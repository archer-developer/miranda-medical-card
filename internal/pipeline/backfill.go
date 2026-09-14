package pipeline

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/diagnosisreconcile"
	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// BackfillStudyTitle is a one-off migration operation, not an MCP tool
// (contrast with reads.go's methods, which back docs/mcp/*): it re-derives
// MedicalDocument.Title for a document that was processed before
// extraction.Schema had a studyTitle field (added because the fixed
// documentType label alone — "Лабораторное исследование" for every lab
// report — gives a user nothing to reference a specific document by, see
// that field's doc comment).
//
// Deliberately does NOT re-run the full Pipeline (no OCR, no Normalize, no
// Timeline/Profile/FTS/Embeddings rebuild, no new Extraction version) —
// MedicalDocument.RecognizedText already holds Stage 1's output from the
// original run, and Stage 2a (Structured, the only stage that can produce
// studyTitle) can be replayed directly against it. Every other field
// (DocumentType, DocumentDate, Organization, Doctor, RecognizedText,
// Summary) is carried through unchanged from the existing row — only
// studyTitle is new information this call can add, so only Title is
// allowed to change, via the same buildTitle logic run already uses.
//
// Returns changed=false, nil for a document whose title doesn't move (no
// studyTitle came back, or the document has no RecognizedText to replay
// against) — not an error, since "nothing to backfill" is an expected,
// common outcome, not a failure.
func (p *Pipeline) BackfillStudyTitle(ctx context.Context, userID, documentID string) (changed bool, newTitle string, err error) {
	doc, err := p.documentRepo.Get(ctx, documentID, userID)
	if err != nil {
		return false, "", fmt.Errorf("pipeline: backfill study title: %w", err)
	}
	if doc.RecognizedText == "" {
		return false, "", nil
	}

	result, _, err := extraction.StructuredWithRetry(ctx, p.extractionProvider, p.extractionEscalation, doc.RecognizedText, p.logger)
	if err != nil {
		return false, "", fmt.Errorf("pipeline: backfill study title: structured: %w", err)
	}
	if result.StudyTitle == "" {
		return false, "", nil
	}

	title := buildTitle(extraction.Result{
		DocumentType: doc.DocumentType,
		Organization: doc.Organization,
		StudyTitle:   result.StudyTitle,
	})
	if title == doc.Title {
		return false, "", nil
	}

	update := storage.DocumentExtractedUpdate{
		DocumentType:   doc.DocumentType,
		DocumentDate:   doc.DocumentDate,
		Title:          title,
		Organization:   doc.Organization,
		Doctor:         doc.Doctor,
		RecognizedText: doc.RecognizedText,
		Summary:        doc.Summary,
	}
	if err := p.documentRepo.UpdateExtracted(ctx, documentID, userID, update); err != nil {
		return false, "", fmt.Errorf("pipeline: backfill study title: update: %w", err)
	}
	return true, title, nil
}

// ReindexDocumentFTS is another one-off migration operation (see
// BackfillStudyTitle's doc comment above for why this lives here rather
// than as an MCP tool): rebuilds documentID's FTS index entry from data
// already persisted — no OCR, no Structured re-extraction, no LLM call at
// all — for a document imported before documentTypesWithoutFreeTextContent
// existed (see that var's doc comment in pipeline.go), whose FTS index may
// still include a lab_report/prescription's raw OCR boilerplate. doc.Summary
// already carries any real recommendations Extraction found for the
// document (see buildSummary), so dropping RecognizedText from the
// reindexed text for these two types loses nothing but the noise that
// caused a real false-positive FTS match (see documentTypesWithoutFreeTextContent).
func (p *Pipeline) ReindexDocumentFTS(ctx context.Context, userID, documentID string) error {
	doc, err := p.documentRepo.Get(ctx, documentID, userID)
	if err != nil {
		return fmt.Errorf("pipeline: reindex document fts: %w", err)
	}

	ftsContent := doc.Summary
	if !documentTypesWithoutFreeTextContent[doc.DocumentType] {
		ftsContent = doc.RecognizedText + "\n" + doc.Summary
	}
	if err := p.fts.IndexDocument(ctx, userID, documentID, doc.Title, ftsContent); err != nil {
		return fmt.Errorf("pipeline: reindex document fts: %w", err)
	}
	return nil
}

// DiagnosisReconciliationChange describes one transition
// ReconcileDiagnosisHistory found — or, in dry-run mode, would apply.
type DiagnosisReconciliationChange struct {
	DiagnosisID   string
	DiagnosisName string
	TargetID      string
	TargetName    string
	Relation      string // diagnosisreconcile.RelationRefines or RelationCancels
}

// ReconcileDiagnosisHistory is a one-off migration operation (see
// BackfillStudyTitle's doc comment above for the general shape/rationale):
// replays userID's existing active/chronic diagnoses through the same
// decideDiagnosisRelation/applyDiagnosisRelation pair the live pipeline hook
// uses (diagnoses.go's reconcileOneDiagnosis), in chronological order, as if
// Diagnosis Reconciliation (docs/adr/008-diagnosis-cross-document-reconciliation.md)
// had been in place from the start — for diagnoses persisted before that
// mechanism existed.
//
// Sorted by DiagnosedAt; a diagnosis with no DiagnosedAt falls back to its
// source document's own DocumentDate (documents almost always carry one even
// when the diagnosis line itself didn't parse a date), so the replay order
// stays historically meaningful, and finally to the zero time (sorted
// first) if neither is known.
//
// dryRun=true reports every transition Reconcile would make without calling
// MarkSuperseded/MarkResolved — safe to run repeatedly before committing to
// --apply. Either way, a found transition still updates the in-memory
// "currently active" working set for the rest of the replay (the superseded/
// cancelled candidate is removed, the new diagnosis is added) so later
// diagnoses in the same replay are compared against what the state would
// actually be at that point, not the pre-backfill snapshot.
func (p *Pipeline) ReconcileDiagnosisHistory(ctx context.Context, userID string, dryRun bool) ([]DiagnosisReconciliationChange, error) {
	all, err := p.diagnoses.ListByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: reconcile diagnosis history: list: %w", err)
	}

	var ordered []normalization.Diagnosis
	for _, d := range all {
		if d.Status == "active" || d.Status == "chronic" {
			ordered = append(ordered, d)
		}
	}

	sortDate := make(map[string]time.Time, len(ordered))
	for _, d := range ordered {
		sortDate[d.ID] = p.diagnosisSortDate(ctx, d)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return sortDate[ordered[i].ID].Before(sortDate[ordered[j].ID])
	})

	var changes []DiagnosisReconciliationChange
	var working []normalization.Diagnosis
	for _, d := range ordered {
		result, err := p.decideDiagnosisRelation(ctx, d, working)
		if err != nil {
			return changes, fmt.Errorf("pipeline: reconcile diagnosis history: %w", err)
		}

		if result.TargetID != "" && (result.Relation == diagnosisreconcile.RelationRefines || result.Relation == diagnosisreconcile.RelationCancels) {
			targetName := ""
			remaining := working[:0:0]
			for _, w := range working {
				if w.ID == result.TargetID {
					targetName = w.Name
					continue
				}
				remaining = append(remaining, w)
			}
			changes = append(changes, DiagnosisReconciliationChange{
				DiagnosisID: d.ID, DiagnosisName: d.Name,
				TargetID: result.TargetID, TargetName: targetName,
				Relation: result.Relation,
			})
			if !dryRun {
				if err := p.applyDiagnosisRelation(ctx, userID, d, result, sortDate[d.ID]); err != nil {
					return changes, fmt.Errorf("pipeline: reconcile diagnosis history: apply: %w", err)
				}
			}
			working = remaining
		}
		working = append(working, d)
	}
	return changes, nil
}

// LabResultDateBackfillChange describes one LabResult BackfillLabResultDates
// found with no TakenAt — or, in dry-run mode, would give one.
type LabResultDateBackfillChange struct {
	LabResultID   string
	IndicatorName string
	DocumentID    string
	TakenAt       time.Time
}

// BackfillLabResultDates is a one-off migration operation (see
// BackfillStudyTitle's doc comment above for the general shape/rationale):
// gives every userID LabResult with no TakenAt its source document's own
// DocumentDate, for rows persisted before Normalize's LabResults loop
// (internal/normalization/normalization.go) fell back to it itself.
//
// A multi-page lab report often states its order/collection date once in
// the document header rather than repeating it next to every individual
// result row, so Structured Extraction sometimes couldn't attach a per-item
// date even though the document's own date was known — leaving TakenAt nil
// for that LabResult forever after, which in turn made
// LabResultRepository.LatestByIndicator's "nil TakenAt never wins over a
// dated one" tie-break (internal/storage/lab_result.go) prefer a truly
// older, merely-better-dated result for that indicator. Found investigating
// a wrong "current ferritin level" answer on 2026-09-14 (see git log around
// this function).
//
// A LabResult whose document itself has no DocumentDate either is left
// alone (nothing to backfill from) and not counted as a change.
//
// dryRun=true reports every row that would change without calling
// ReplaceForDocument — safe to run repeatedly before committing to --apply.
func (p *Pipeline) BackfillLabResultDates(ctx context.Context, userID string, dryRun bool) ([]LabResultDateBackfillChange, error) {
	all, err := p.labResults.ListByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("pipeline: backfill lab result dates: list: %w", err)
	}

	byDocument := make(map[string][]normalization.LabResult)
	for _, l := range all {
		byDocument[l.DocumentID] = append(byDocument[l.DocumentID], l)
	}

	var changes []LabResultDateBackfillChange
	for documentID, results := range byDocument {
		hasUndated := false
		for _, l := range results {
			if l.TakenAt == nil {
				hasUndated = true
				break
			}
		}
		if !hasUndated {
			continue
		}

		doc, err := p.documentRepo.Get(ctx, documentID, userID)
		if err != nil {
			return changes, fmt.Errorf("pipeline: backfill lab result dates: get document %s: %w", documentID, err)
		}
		if doc.DocumentDate == nil {
			continue
		}

		changed := false
		for i, l := range results {
			if l.TakenAt != nil {
				continue
			}
			results[i].TakenAt = doc.DocumentDate
			changed = true
			changes = append(changes, LabResultDateBackfillChange{
				LabResultID:   l.ID,
				IndicatorName: l.IndicatorName,
				DocumentID:    documentID,
				TakenAt:       *doc.DocumentDate,
			})
		}
		if changed && !dryRun {
			if err := p.labResults.ReplaceForDocument(ctx, documentID, results); err != nil {
				return changes, fmt.Errorf("pipeline: backfill lab result dates: replace %s: %w", documentID, err)
			}
		}
	}
	return changes, nil
}

// diagnosisSortDate resolves the chronological key ReconcileDiagnosisHistory
// replays d in: d.DiagnosedAt if known, else its source document's own
// DocumentDate, else the zero time (sorts first — an undated diagnosis with
// an undated document is treated as the oldest, unknown-provenance data
// rather than arbitrarily "most recent").
func (p *Pipeline) diagnosisSortDate(ctx context.Context, d normalization.Diagnosis) time.Time {
	if d.DiagnosedAt != nil {
		return *d.DiagnosedAt
	}
	doc, err := p.documentRepo.Get(ctx, d.DocumentID, d.UserID)
	if err != nil || doc.DocumentDate == nil {
		return time.Time{}
	}
	return *doc.DocumentDate
}
