package pipeline

import (
	"context"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
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

	result, _, err := extraction.StructuredWithRetry(ctx, p.provider, p.escalationProvider, doc.RecognizedText, p.logger)
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
