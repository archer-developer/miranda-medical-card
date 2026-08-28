package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/decline"
	"github.com/archer-developer/miranda-medical-card/internal/diagnosisreconcile"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// DiagnosisNotFoundError is returned by ResolveDiagnosis when the user has
// no non-resolved Diagnoses at all, or none of them confidently matches the
// given text — mirrors PlannedActionNotFoundError (see that type's doc
// comment for why this carries the user's current candidates rather than
// just failing silently).
type DiagnosisNotFoundError struct {
	CurrentNames []string
}

func (e *DiagnosisNotFoundError) Error() string {
	if len(e.CurrentNames) == 0 {
		return "pipeline: resolve diagnosis: no non-resolved diagnoses"
	}
	return fmt.Sprintf("pipeline: resolve diagnosis: no confident match among current diagnoses: %s",
		strings.Join(e.CurrentNames, "; "))
}

// ResolveDiagnosis implements docs/mcp/09-diagnoses.md's
// medical.resolve_diagnosis: finds the single non-resolved Diagnosis text
// most clearly refers to (via decline.Match, the same small Structured LLM
// call over a short candidate list DeclinePlannedAction uses — Miranda
// passes text exactly as the user said it, e.g. "да, ОРВИ прошла", never a
// specific diagnosisId) and marks it resolved. Returns
// *DiagnosisNotFoundError (never a plain error) when nothing non-resolved
// exists or nothing matched confidently.
func (p *Pipeline) ResolveDiagnosis(ctx context.Context, userID, text string) (normalization.Diagnosis, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: text must not be empty")
	}

	all, err := p.diagnoses.ListByUser(ctx, userID)
	if err != nil {
		return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: list: %w", err)
	}
	var current []normalization.Diagnosis
	for _, d := range all {
		// A superseded diagnosis already has a successor recorded by
		// Diagnosis Reconciliation (see reconcileOneDiagnosis below) — it's
		// no longer something the user would sensibly say "прошло" about,
		// so it's excluded from candidates exactly like a resolved one.
		if d.Status != "resolved" && d.Status != "superseded" {
			current = append(current, d)
		}
	}
	if len(current) == 0 {
		return normalization.Diagnosis{}, &DiagnosisNotFoundError{}
	}

	names := make([]string, len(current))
	candidates := make([]decline.Candidate, len(current))
	for i, d := range current {
		names[i] = d.Name
		candidates[i] = decline.Candidate{ID: d.ID, Description: d.Name, Context: d.Status}
	}

	matchedID, err := decline.Match(ctx, p.extractionProvider,
		"The user is saying one of their current diagnoses has now resolved.", text, candidates)
	if err != nil {
		return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: match: %w", err)
	}

	for _, d := range current {
		if d.ID == matchedID && matchedID != "" {
			now := time.Now().UTC()
			reasoning := fmt.Sprintf("Пользователь подтвердил разрешение диагноза в диалоге: %q.", text)
			if err := p.diagnoses.MarkResolved(ctx, d.ID, userID, now, reasoning); err != nil {
				return normalization.Diagnosis{}, fmt.Errorf("pipeline: resolve diagnosis: %w", err)
			}
			d.Status = "resolved"
			d.ActualResolutionAt = &now
			d.StatusReasoning = reasoning
			return d, nil
		}
	}
	// Either the model omitted matchId (no confident match), or — despite
	// the schema's enum constraint — returned an id outside the candidate
	// set; both are treated identically as "nothing matched" rather than
	// trusting an id that was never actually offered (mirrors
	// DeclinePlannedAction's own posture).
	return normalization.Diagnosis{}, &DiagnosisNotFoundError{CurrentNames: names}
}

// reconcileDiagnosesForDocument compares every diagnosis documentID just
// produced against userID's other currently active/chronic diagnoses (see
// internal/diagnosisreconcile) and updates whichever existing diagnosis, if
// any, a new one refines or cancels — docs/adr/008-diagnosis-cross-document-reconciliation.md.
// Best-effort like generateDocumentEmbedding (pipeline.go): an external LLM
// call whose failure must not fail the whole document, since the new
// diagnosis itself is already correctly persisted regardless — only the
// historical linkage to an older diagnosis would be missing.
func (p *Pipeline) reconcileDiagnosesForDocument(ctx context.Context, userID, documentID string) {
	all, err := p.diagnoses.ListByUser(ctx, userID)
	if err != nil {
		p.logger.Warn("pipeline: reconcile diagnoses: list", "documentId", documentID, "error", err)
		return
	}
	var fresh, current []normalization.Diagnosis
	for _, d := range all {
		if d.DocumentID == documentID {
			fresh = append(fresh, d)
		} else if d.Status == "active" || d.Status == "chronic" {
			current = append(current, d)
		}
	}
	// current is computed once, before this document's own diagnoses are
	// considered — a known, accepted limitation (same posture as
	// docs/adr/005-planned-action-cross-source-dedup.md's own "rarer v1
	// gap"): if a single document introduces two related new diagnoses in
	// one pass, the second won't reconcile against the first, only against
	// diagnoses from other documents. Not worth solving now — a single
	// document rarely states both a general and a refined version of the
	// same diagnosis.
	// No filter on the new diagnosis's own Status here (unlike current's
	// active/chronic filter above): a document explicitly ruling out an
	// earlier suspicion typically extracts with Status "resolved" for its
	// own line, yet is exactly the "cancels" case that must still flip the
	// older, still-active candidate — the new diagnosis's own status and its
	// relationship to an existing one are independent questions.
	for _, d := range fresh {
		if err := p.reconcileOneDiagnosis(ctx, userID, d, current, time.Now().UTC()); err != nil {
			p.logger.Warn("pipeline: reconcile diagnoses", "documentId", documentID, "diagnosisId", d.ID, "error", err)
		}
	}
}

// reconcileOneDiagnosis decides newDx's relationship to candidates and, if
// one was found, immediately applies it — the live pipeline hook's shape
// (reconcileDiagnosesForDocument above). medical-dev's reconcile-diagnoses
// backfill command needs to report a decision before choosing whether to
// apply it (dry-run), so it calls decideDiagnosisRelation/
// applyDiagnosisRelation separately instead — see backfill.go's
// ReconcileDiagnosisHistory.
func (p *Pipeline) reconcileOneDiagnosis(ctx context.Context, userID string, newDx normalization.Diagnosis, candidates []normalization.Diagnosis, asOf time.Time) error {
	result, err := p.decideDiagnosisRelation(ctx, newDx, candidates)
	if err != nil {
		return err
	}
	return p.applyDiagnosisRelation(ctx, userID, newDx, result, asOf)
}

// decideDiagnosisRelation is the pure, no-mutation half of diagnosis
// reconciliation: one Structured LLM call (internal/diagnosisreconcile)
// deciding whether newDx refines or cancels one of candidates. candidates
// need not exclude newDx.ID/same-document siblings itself — this function
// does it. Excluding same-document candidates is not just an optimization:
// diagnoses from the same document are independent facts extracted from one
// visit (e.g. a checkup listing "Дислипидемия", "Гиперхолестеринемия", and
// unrelated findings side by side), never one superseding another the way
// two different visits over time can — reconcileDiagnosesForDocument's own
// current/fresh split already guarantees this for the live pipeline hook,
// but ReconcileDiagnosisHistory's backfill replay walks one flat,
// cross-document list, so without this check here it would happily compare
// same-document siblings against each other purely because they share a
// sort date, and — found in practice on real data — can chain a completely
// unrelated diagnosis (e.g. "Холестероз ЖП", a gallbladder finding) into a
// lipid-disorder cluster just because both appeared in the same report.
func (p *Pipeline) decideDiagnosisRelation(ctx context.Context, newDx normalization.Diagnosis, candidates []normalization.Diagnosis) (diagnosisreconcile.Result, error) {
	var offered []diagnosisreconcile.Candidate
	for _, d := range candidates {
		if d.ID == newDx.ID || d.DocumentID == newDx.DocumentID {
			continue
		}
		offered = append(offered, diagnosisreconcile.Candidate{ID: d.ID, Name: d.Name, Status: d.Status, DiagnosedAt: d.DiagnosedAt})
	}

	result, err := diagnosisreconcile.Reconcile(ctx, p.extractionProvider, newDx.Name, newDx.Status, offered)
	if err != nil {
		return diagnosisreconcile.Result{}, fmt.Errorf("pipeline: reconcile diagnosis: %w", err)
	}
	return result, nil
}

// applyDiagnosisRelation performs the storage mutation decideDiagnosisRelation's
// result calls for, if any: "refines" marks result.TargetID superseded,
// "cancels" marks it resolved (asOf is the resolution timestamp —
// time.Now() from the live pipeline hook, or the replayed diagnosis's own
// DiagnosedAt from a backfill run, so a historical replay doesn't stamp
// every transition with today's date). A TargetID set together with an
// empty/unrecognized Relation (the model omitted it, or — despite the
// schema's enum constraint — returned something else) is treated identically
// to no match at all, same posture as ResolveDiagnosis above: guessing which
// mutation to apply would be worse than doing nothing.
func (p *Pipeline) applyDiagnosisRelation(ctx context.Context, userID string, newDx normalization.Diagnosis, result diagnosisreconcile.Result, asOf time.Time) error {
	switch {
	case result.TargetID == "":
		return nil
	case result.Relation == diagnosisreconcile.RelationRefines:
		reasoning := fmt.Sprintf("Заменён более специфичным диагнозом %q.", newDx.Name)
		return p.diagnoses.MarkSuperseded(ctx, result.TargetID, userID, reasoning)
	case result.Relation == diagnosisreconcile.RelationCancels:
		reasoning := fmt.Sprintf("Отменён диагнозом %q.", newDx.Name)
		return p.diagnoses.MarkResolved(ctx, result.TargetID, userID, asOf, reasoning)
	default:
		return nil
	}
}
