// Package events implements Structured Extraction for Self-Reported Events
// (docs/mcp/07-events.md §3, docs/architecture/02-processing-pipeline.md
// §17) — the text-only counterpart to internal/extraction's document
// pipeline: no OCR (the input is already text), one Structured call instead
// of two/three, and — unlike internal/extraction — a failed or empty result
// here is never treated as a Pipeline failure (see Extract's doc comment).
package events

import (
	"context"
	"encoding/json"
	"fmt"

	llm "github.com/archer-developer/miranda-llm"

	"github.com/archer-developer/miranda-medical-card/internal/planning"
)

// SchemaName is passed as llm.StructuredRequest.SchemaName.
const SchemaName = "self_reported_event_extraction"

// Prompt classifies rawText and, if it mentions taking a medication or a
// future action, extracts that separately as MedicationIntake/PlannedAction
// — both independent of category, since a single report can be a symptom
// AND a medication intake at once (see
// docs/domain/12-self-reported-events.md §5's headache+ibuprofen example),
// and equally can describe something the user needs to do in the future
// regardless of how the note itself is classified.
const Prompt = `You are a medical assistant classifying a short, free-text note a patient wrote about themselves (not a document, not OCR output).

Your job:
1. Classify the note into exactly one category: "symptom" (a complaint, pain, discomfort), "observation" (a measurement or noted fact not equivalent to a symptom, e.g. "давление 145/95"), "medication_intake" (the note is only about taking a medication, nothing else), or "other".
2. Write a short, factual one-sentence description of what the note says — no medical interpretation, no advice, just what was written.
3. If the note mentions taking a specific medication (even in passing, alongside a symptom), extract it separately as medicationIntake — this applies regardless of which category you chose in step 1.
4. If the note describes something the user needs to do in the future with a rough timeframe (a follow-up test, a vaccination, a visit) — e.g. "нужно сделать прививку от бешенства в течение полугода" — extract it as plannedAction, independent of category, same as medicationIntake. Express the timeframe as a relative amount + unit (dueAmountMin/dueAmountMax/dueUnit), never as a calendar date you compute yourself — you don't reliably know today's date, only the offset the user stated.

Rules:
- Do not diagnose, interpret, or draw medical conclusions.
- Do not infer a fact that is not written in the text.
- Omit medicationIntake entirely if no medication is mentioned.
- Omit plannedAction entirely if the note describes nothing forward-looking.
- If you cannot confidently classify the note or extract anything structured, it is fine to omit category/description/medicationIntake/plannedAction — the original text is preserved regardless of what you return here.`

// Schema is the JSON Schema passed as llm.StructuredRequest.Schema.
func Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"category": map[string]any{
				"type": "string",
				"enum": []string{"symptom", "observation", "medication_intake", "other"},
			},
			"description": map[string]any{"type": "string"},
			"medicationIntake": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"drugName":   map[string]any{"type": "string", "description": "As written, e.g. 'ибупрофен'."},
					"doseAmount": map[string]any{"type": "number"},
					"doseUnit":   map[string]any{"type": "string", "description": "e.g. mg — only if a numeric dose is stated."},
					"reason":     map[string]any{"type": "string", "description": "Why it was taken, if stated, e.g. 'головная боль'."},
				},
				"required": []string{"drugName"},
			},
			"plannedAction": planning.SchemaObject(),
		},
	}
}

// Result is the Go-side mirror of Schema.
type Result struct {
	Category         string                  `json:"category,omitempty"`
	Description      string                  `json:"description,omitempty"`
	MedicationIntake *MedicationIntake       `json:"medicationIntake,omitempty"`
	PlannedAction    *planning.PlannedAction `json:"plannedAction,omitempty"`
}

type MedicationIntake struct {
	DrugName   string  `json:"drugName"`
	DoseAmount float64 `json:"doseAmount,omitempty"`
	DoseUnit   string  `json:"doseUnit,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// StructuredProvider is the subset of llm.StructuredProvider Extract needs.
type StructuredProvider interface {
	Structured(ctx context.Context, req llm.StructuredRequest) (json.RawMessage, error)
}

// Extract runs Structured Extraction over rawText. Unlike
// internal/extraction.Structured, a failure here (LLM error, or a result
// with empty Category/Description/MedicationIntake/PlannedAction) is not escalated as a
// Pipeline failure by this function's caller — see
// docs/mcp/07-events.md §3: the user's original text must never be lost
// just because automatic structuring didn't work, so Extract returns
// whatever error occurred and lets the caller decide to swallow it (see
// pipeline.Pipeline.LogEvent).
func Extract(ctx context.Context, provider StructuredProvider, rawText string) (Result, error) {
	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: Prompt},
			{Role: llm.RoleUser, Content: rawText},
		},
		Schema:     Schema(),
		SchemaName: SchemaName,
	}

	raw, err := provider.Structured(ctx, req)
	if err != nil {
		return Result{}, fmt.Errorf("events: structured: %w", err)
	}

	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, fmt.Errorf("events: structured: unmarshal result: %w", err)
	}
	return result, nil
}
