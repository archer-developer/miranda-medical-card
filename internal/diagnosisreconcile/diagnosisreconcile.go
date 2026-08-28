// Package diagnosisreconcile implements the cross-document Diagnosis
// Reconciliation step (docs/adr/008-diagnosis-cross-document-reconciliation.md):
// deciding, when a newly-extracted diagnosis appears, whether it changes the
// status of one of a household member's other currently active/chronic
// diagnoses. One small Structured LLM call over an explicit, short candidate
// list — the same "never guess when ambiguous" shape as internal/decline —
// but unlike decline.Match, which only picks which candidate a free-text
// message refers to, this also classifies HOW the new diagnosis relates to
// the match (refines it, more specifically/precisely; or cancels it,
// invalidating it outright), since that distinction decides which storage
// mutation (MarkSuperseded vs MarkResolved) the caller should perform. Has
// exactly one call site (internal/pipeline), so — unlike decline — the
// prompt bakes in diagnosis-specific instructions directly rather than
// taking a generic "kind" parameter.
package diagnosisreconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	llm "github.com/archer-developer/miranda-llm"
)

// SchemaName is passed as llm.StructuredRequest.SchemaName.
const SchemaName = "diagnosis_reconciliation"

// RelationRefines and RelationCancels are Result.Relation's only valid
// values. Any other value (including empty, when TargetID is also empty)
// means "the new diagnosis is unrelated to every candidate, or the model
// wasn't confident enough to classify the relationship" — a normal outcome,
// not an error.
const (
	RelationRefines = "refines"
	RelationCancels = "cancels"
)

const promptTemplate = `You are comparing a newly extracted medical diagnosis against a person's existing current diagnoses (each from an earlier document) to decide whether the new one changes the status of any of them.

Given the new diagnosis (name, status) and a list of candidates (id, name, status, diagnosed date), decide:

- "refines": the new diagnosis describes the same underlying condition as one candidate, but more specifically, more precisely, or with updated clinical detail (e.g. a general/unspecified diagnosis followed by a graded or clinically precise version of the same condition). The candidate should be considered replaced by this more informative diagnosis.
- "cancels": the new diagnosis indicates a candidate is no longer valid, was a diagnostic error, or has been ruled out. Do NOT use this just because a document says a condition has resolved — that is decided separately, per document, from the document's own text.

Omit targetId and relation entirely if the new diagnosis is unrelated to every candidate, or if you are not confident enough to classify the relationship — never guess when it's ambiguous or none fit.`

// Candidate is one of the person's other currently active/chronic diagnoses
// offered to the model for comparison against the new one.
type Candidate struct {
	ID          string
	Name        string
	Status      string // "active" or "chronic" — see call site
	DiagnosedAt *time.Time
}

// Schema is the JSON Schema passed as llm.StructuredRequest.Schema —
// targetId is constrained to candidateIDs so the model can only ever return
// an id it was actually offered, never invent one.
func Schema(candidateIDs []string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"targetId": map[string]any{
				"type":        "string",
				"enum":        candidateIDs,
				"description": "The id of the existing diagnosis the new one relates to. Omit entirely if none confidently relate.",
			},
			"relation": map[string]any{
				"type":        "string",
				"enum":        []string{RelationRefines, RelationCancels},
				"description": "How the new diagnosis relates to targetId. Only meaningful together with targetId.",
			},
		},
	}
}

// Result is the Go-side mirror of Schema.
type Result struct {
	TargetID string `json:"targetId,omitempty"`
	Relation string `json:"relation,omitempty"`
}

// StructuredProvider is the subset of llm.StructuredProvider Reconcile needs.
type StructuredProvider interface {
	Structured(ctx context.Context, req llm.StructuredRequest) (json.RawMessage, error)
}

// Reconcile returns the empty Result if candidates is empty or the model
// found no confident relation — never an error for "no relation," only for
// an actual call/parse failure (mirrors decline.Match's posture). Callers
// must treat a TargetID set together with an unrecognized Relation
// (including empty) identically to no match at all — the model omitted the
// classification, or, despite the schema's enum constraint, returned
// something else; either way, guessing which mutation to apply would be
// worse than doing nothing.
func Reconcile(ctx context.Context, provider StructuredProvider, newName, newStatus string, candidates []Candidate) (Result, error) {
	if len(candidates) == 0 {
		return Result{}, nil
	}

	ids := make([]string, len(candidates))
	var listing strings.Builder
	for i, c := range candidates {
		ids[i] = c.ID
		diagnosedAt := "unknown"
		if c.DiagnosedAt != nil {
			diagnosedAt = c.DiagnosedAt.Format("2006-01-02")
		}
		fmt.Fprintf(&listing, "- id=%s status=%s diagnosed=%s: %s\n", c.ID, c.Status, diagnosedAt, c.Name)
	}

	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: promptTemplate},
			{Role: llm.RoleUser, Content: fmt.Sprintf("New diagnosis: %q (status: %s)\n\nCandidates:\n%s", newName, newStatus, listing.String())},
		},
		Schema:     Schema(ids),
		SchemaName: SchemaName,
	}

	raw, err := provider.Structured(ctx, req)
	if err != nil {
		return Result{}, fmt.Errorf("diagnosisreconcile: structured: %w", err)
	}

	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, fmt.Errorf("diagnosisreconcile: structured: unmarshal result: %w", err)
	}
	return result, nil
}
