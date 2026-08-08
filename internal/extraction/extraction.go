// Package extraction implements the OCR/Vision and Structured Extraction
// stages of the Processing Pipeline (see
// docs/architecture/02-processing-pipeline.md §4.3 and §5) as two separate
// LLM calls: OCR (image in, plain text out) and Structured Extraction (text
// in, schema-constrained JSON out, per docs/domain/03-files-and-documents.md
// §4).
//
// These were originally combined into a single vision call for simplicity.
// That was reverted after real-document testing: the same document, same
// prompt/schema, at the API's default temperature, non-deterministically
// returned either a fully-populated result or one with every structured
// array empty (while "fullText" — the easier, transcription-only part of
// the task — succeeded reliably every time). Lowering the sampling
// temperature (see llm.StructuredRequest.Temperature) reduced but did not
// eliminate the failure. Splitting transcription and structuring into two
// calls — so the second call only has to reason over already-clean text,
// never simultaneously with reading the image — resolved it in repeated
// testing against a real 4-page, ~40-result lab report. See this package's
// git history / CLAUDE.md for the specific test results if this decision
// needs revisiting.
package extraction

import (
	"context"
	"encoding/json"
	"fmt"

	llm "github.com/archer-developer/miranda-llm"
)

// SchemaName is passed as llm.StructuredRequest.SchemaName.
const SchemaName = "medical_document_extraction"

// OCRPrompt is Stage 1: transcribe only, no structuring, no judgment calls
// beyond faithful transcription. Deliberately the only thing this call is
// asked to do — see this package's doc comment for why keeping it separate
// from Structured Extraction turned out to matter.
const OCRPrompt = `Transcribe the complete text of this document, preserving reading order. Render any tables as plain text rows (one row per line), keeping every column's value. Do not summarize, skip, or omit any part of the document, including headers, footers, disclaimers, and page numbers. Output only the transcribed text, nothing else — no commentary, no markdown formatting.`

// Prompt is Stage 2's instruction, applied to the already-transcribed text
// from Stage 1 (OCR). It is deliberately conservative: extract only what's
// in the text, don't interpret, don't diagnose, don't fill in anything not
// actually present.
//
// Fields intentionally capture facts as written (verbatim drug names, no
// canonicalization) — canonicalization is a Normalization concern
// (docs/architecture/02-processing-pipeline.md §6), not Extraction's;
// keeping Extraction "dumb" makes its output independently checkable
// against the source document without also having to judge whether a
// canonicalization decision was medically correct.
const Prompt = `You are a medical document extraction assistant. You will be given the transcribed text of one medical document (a lab report, prescription, discharge summary, referral, or similar).

Your only job is to extract facts that are literally present in the text, as a structured JSON object matching the provided schema. You must NOT:
- interpret, diagnose, or draw medical conclusions;
- infer a fact that is not written in the text, even if it seems medically obvious;
- translate names of drugs, diagnoses, or organizations into another language — copy them as written;
- normalize or canonicalize drug/diagnosis names — record them exactly as they appear.

Rules:
- Dates must be normalized to ISO 8601 (YYYY-MM-DD). If only a partial date is present (e.g. just a month and year), record what is certain, but do not guess a day.
- Omit a field entirely (do not include it, do not set it to an empty string) if the text does not state it — never fabricate a value.
- If the text contains no instances of a given category (e.g. no medications mentioned), return an empty array for that field, not null.
- You must attempt to populate every category the text actually contains data for, no matter how many entries there are — e.g. a lab report with 40 result rows must produce 40 entries in "labResults", not a subset and not an empty array. Omitting an individual field within one entry because you're unsure of it is fine; omitting the entire entry, or the entire array, when the text clearly contains the data is not.
- If you are not confident you can read a value correctly, omit that specific field rather than guessing.`

// Schema is the JSON Schema passed as llm.StructuredRequest.Schema for
// Stage 2. Field shapes intentionally track the domain entities in
// docs/domain/ (Medication, Diagnosis, Procedure, LabResult, Allergy,
// VitalSign) closely enough that Normalization's job is closer to "copy +
// canonicalize" than "reinterpret," but every field here is still what
// Extraction itself is expected to produce — not the domain entity shape
// verbatim (no ids, no userId/documentId, no derived status beyond what's
// explicitly stated in the text).
func Schema() map[string]any {
	dateProp := map[string]any{"type": "string", "description": "ISO 8601 date (YYYY-MM-DD)"}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"documentType": map[string]any{
				"type": "string",
				"enum": []string{"lab_report", "consultation", "discharge_summary", "prescription", "imaging_report", "referral", "other"},
			},
			"documentDate": dateProp,
			"organization": map[string]any{"type": "string", "description": "Clinic/lab/hospital that issued the document, as written."},
			"doctor":       map[string]any{"type": "string", "description": "Doctor's name, as written."},
			"language":     map[string]any{"type": "string", "description": "ISO 639-1 language code of the document's main text."},
			"diagnoses": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string"},
						"code":        map[string]any{"type": "string", "description": "Diagnosis code, if printed (e.g. ICD-10)."},
						"codeSystem":  map[string]any{"type": "string", "description": "e.g. icd10 — only if a code is present."},
						"diagnosedAt": dateProp,
						"status": map[string]any{
							"type":        "string",
							"enum":        []string{"suspected", "active", "chronic", "resolved"},
							"description": "Only set if the text's own wording clearly indicates one of these; omit otherwise.",
						},
						"notes": map[string]any{"type": "string"},
					},
					"required": []string{"name"},
				},
			},
			"medications": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string", "description": "As written in the text (brand or generic, whichever is printed)."},
						"doseAmount": map[string]any{"type": "number"},
						"doseUnit":   map[string]any{"type": "string", "description": "e.g. mg, ml, IU"},
						"frequency":  map[string]any{"type": "string", "description": "Free text as written, e.g. '1 раз в день'."},
						"route":      map[string]any{"type": "string", "description": "e.g. oral, injection, topical — only if stated."},
						"startedAt":  dateProp,
						"endedAt":    dateProp,
						"status": map[string]any{
							"type": "string",
							"enum": []string{"active", "discontinued", "completed"},
						},
						"reason":       map[string]any{"type": "string"},
						"prescribedBy": map[string]any{"type": "string"},
					},
					"required": []string{"name"},
				},
			},
			"labResults": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":          map[string]any{"type": "string", "description": "Indicator name as printed, e.g. ALT, LDL."},
						"value":         map[string]any{"type": "number"},
						"unit":          map[string]any{"type": "string"},
						"referenceLow":  map[string]any{"type": "number"},
						"referenceHigh": map[string]any{"type": "number"},
						"takenAt":       dateProp,
					},
					"required": []string{"name", "value"},
				},
			},
			"procedures": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{
							"type": "string",
							"enum": []string{"surgery", "examination", "hospitalization", "vaccination", "consultation", "other"},
						},
						"name":        map[string]any{"type": "string"},
						"performedAt": dateProp,
						"performedBy": map[string]any{"type": "string"},
						"notes":       map[string]any{"type": "string"},
					},
					"required": []string{"name"},
				},
			},
			"allergies": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"substance": map[string]any{"type": "string"},
						"reaction":  map[string]any{"type": "string"},
						"severity":  map[string]any{"type": "string", "enum": []string{"mild", "moderate", "severe"}},
					},
					"required": []string{"substance"},
				},
			},
			"vitalSigns": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{
							"type": "string",
							"enum": []string{"blood_pressure", "weight", "height", "pulse", "temperature"},
						},
						"systolic":   map[string]any{"type": "number", "description": "blood_pressure only"},
						"diastolic":  map[string]any{"type": "number", "description": "blood_pressure only"},
						"value":      map[string]any{"type": "number", "description": "all types except blood_pressure"},
						"unit":       map[string]any{"type": "string"},
						"measuredAt": dateProp,
					},
					"required": []string{"type"},
				},
			},
			"recommendations": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"required": []string{"documentType"},
	}
}

// Result is the Go-side mirror of Schema — used to unmarshal and
// pretty-print a Structured call's output for review. FullText is not part
// of Schema (it comes from Stage 1, OCR) but is included here since callers
// need both together.
type Result struct {
	DocumentType    string       `json:"documentType"`
	DocumentDate    string       `json:"documentDate,omitempty"`
	Organization    string       `json:"organization,omitempty"`
	Doctor          string       `json:"doctor,omitempty"`
	Language        string       `json:"language,omitempty"`
	FullText        string       `json:"fullText"`
	Diagnoses       []Diagnosis  `json:"diagnoses,omitempty"`
	Medications     []Medication `json:"medications,omitempty"`
	LabResults      []LabResult  `json:"labResults,omitempty"`
	Procedures      []Procedure  `json:"procedures,omitempty"`
	Allergies       []Allergy    `json:"allergies,omitempty"`
	VitalSigns      []VitalSign  `json:"vitalSigns,omitempty"`
	Recommendations []string     `json:"recommendations,omitempty"`
}

type Diagnosis struct {
	Name        string `json:"name"`
	Code        string `json:"code,omitempty"`
	CodeSystem  string `json:"codeSystem,omitempty"`
	DiagnosedAt string `json:"diagnosedAt,omitempty"`
	Status      string `json:"status,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

type Medication struct {
	Name         string  `json:"name"`
	DoseAmount   float64 `json:"doseAmount,omitempty"`
	DoseUnit     string  `json:"doseUnit,omitempty"`
	Frequency    string  `json:"frequency,omitempty"`
	Route        string  `json:"route,omitempty"`
	StartedAt    string  `json:"startedAt,omitempty"`
	EndedAt      string  `json:"endedAt,omitempty"`
	Status       string  `json:"status,omitempty"`
	Reason       string  `json:"reason,omitempty"`
	PrescribedBy string  `json:"prescribedBy,omitempty"`
}

type LabResult struct {
	Name          string  `json:"name"`
	Value         float64 `json:"value"`
	Unit          string  `json:"unit,omitempty"`
	ReferenceLow  float64 `json:"referenceLow,omitempty"`
	ReferenceHigh float64 `json:"referenceHigh,omitempty"`
	TakenAt       string  `json:"takenAt,omitempty"`
}

type Procedure struct {
	Type        string `json:"type,omitempty"`
	Name        string `json:"name"`
	PerformedAt string `json:"performedAt,omitempty"`
	PerformedBy string `json:"performedBy,omitempty"`
	Notes       string `json:"notes,omitempty"`
}

type Allergy struct {
	Substance string `json:"substance"`
	Reaction  string `json:"reaction,omitempty"`
	Severity  string `json:"severity,omitempty"`
}

type VitalSign struct {
	Type       string  `json:"type"`
	Systolic   float64 `json:"systolic,omitempty"`
	Diastolic  float64 `json:"diastolic,omitempty"`
	Value      float64 `json:"value,omitempty"`
	Unit       string  `json:"unit,omitempty"`
	MeasuredAt string  `json:"measuredAt,omitempty"`
}

// ChatProvider is the subset of llm.Provider Stage 1 (OCR) needs.
type ChatProvider interface {
	Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error)
}

// StructuredProvider is the subset of llm.StructuredProvider (or
// router.Router) Stage 2 needs.
type StructuredProvider interface {
	Structured(ctx context.Context, req llm.StructuredRequest) (json.RawMessage, error)
}

// ocrTemperature keeps transcription low-temperature for the same reason
// Structured defaults to low temperature (see llm.StructuredRequest.Temperature's
// doc comment) — observed directly on a real document: at the provider's
// default (higher) temperature, OCR stopped mid-sentence right after a
// document's boilerplate consent-form preamble, never transcribing the
// actual clinical content (diagnoses, plan) that followed on the same
// visible, legible page. ChatRequest.Temperature has no forced default of
// its own (see its doc comment — Chat covers many use cases, some of which
// want the provider's own default), so OCR sets it explicitly here rather
// than relying on one.
var ocrTemperature = 0.1

// OCR runs Stage 1: transcribe imageBase64 (mimeType e.g. "image/jpeg",
// "application/pdf") into plain text.
func OCR(ctx context.Context, provider ChatProvider, imageBase64, mimeType string) (string, error) {
	req := llm.ChatRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: OCRPrompt},
			{Role: llm.RoleUser, Parts: []llm.ContentPart{{ImageBase64: imageBase64, MIMEType: mimeType}}},
		},
		Temperature: &ocrTemperature,
	}
	stream, err := provider.Chat(ctx, req)
	if err != nil {
		return "", fmt.Errorf("extraction: ocr: %w", err)
	}
	var text string
	for chunk := range stream {
		if chunk.Err != nil {
			return "", fmt.Errorf("extraction: ocr: %w", chunk.Err)
		}
		text += chunk.TextDelta
	}
	return text, nil
}

// Structured runs Stage 2: structure already-transcribed text against
// Schema. Returns both the typed Result (FullText populated from text, the
// rest unmarshaled from the model's JSON) and the raw JSON for callers that
// want to persist it verbatim as Extraction.raw (see
// docs/domain/03-files-and-documents.md §4).
func Structured(ctx context.Context, provider StructuredProvider, text string) (Result, json.RawMessage, error) {
	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: Prompt},
			{Role: llm.RoleUser, Content: text},
		},
		Schema:     Schema(),
		SchemaName: SchemaName,
	}

	raw, err := provider.Structured(ctx, req)
	if err != nil {
		return Result{}, nil, fmt.Errorf("extraction: structured: %w", err)
	}

	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{}, raw, fmt.Errorf("extraction: structured: unmarshal result: %w", err)
	}
	result.FullText = text
	return result, raw, nil
}

// Provider is the union Extract needs: something that can both Chat (Stage
// 1) and Structured (Stage 2) — gemini.Provider satisfies this directly;
// router.Router does too once at least one configured provider implements
// llm.StructuredProvider.
type Provider interface {
	ChatProvider
	StructuredProvider
}

// Extract runs both stages against one document image: OCR then Structured.
// This is the function real Pipeline code should call; OCR and Structured
// are exported separately for tests/tooling that want to inspect or reuse
// an intermediate transcription (e.g. cmd/extract-test).
func Extract(ctx context.Context, provider Provider, imageBase64, mimeType string) (Result, json.RawMessage, error) {
	text, err := OCR(ctx, provider, imageBase64, mimeType)
	if err != nil {
		return Result{}, nil, err
	}
	return Structured(ctx, provider, text)
}
