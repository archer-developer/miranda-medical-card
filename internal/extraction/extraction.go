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
- "labResults" is for laboratory (blood/urine/etc.) test panels only. Measurements and observations from imaging or instrumental studies (ultrasound, MRI, CT, X-ray, ECG, echo-KG) are handled by a separate extraction pass — ignore them here even if the text contains them.
- If you are not confident you can read a value correctly, omit that specific field rather than guessing.`

// InstrumentalPrompt is Stage 2b's instruction — a separate, narrowly
// scoped call from Prompt/Schema (Stage 2a), asked only to extract
// instrumental-study findings. Originally instrumentalFindings was one more
// property on the same schema as everything else, but real-document testing
// showed that made the *whole* result unreliable, not just the new field:
// on a dense ultrasound report (~30 structure/parameter combinations across
// 8 organs), diagnoses/doctor/documentDate — fields that extracted
// perfectly reliably before instrumentalFindings existed — started coming
// back empty too, repeatedly. Splitting the sheer volume of requested
// structured output across two calls, each with a narrower single
// responsibility, restored reliability — the same lesson as OCR vs.
// Structured (see this package's doc comment), applied a second time.
const InstrumentalPrompt = `You are a medical document extraction assistant. You will be given the transcribed text of one medical document from an instrumental study (ultrasound, MRI, CT, X-ray, ECG, echo-KG, or similar).

Extract every measured or observed parameter for every anatomical structure/organ mentioned, as a structured JSON object matching the provided schema. You must NOT interpret, diagnose, or draw medical conclusions — record only what is literally stated.

Rules:
- One entry per (structure, parameter) combination. A report describing 8 organs with 3-5 parameters each must produce 24-40 entries, not a summary or a subset.
- Use "value"+"unit" for a quantitative parameter (e.g. a size in mm), "qualitativeValue" for a descriptive one (e.g. echogenicity, contours) — never force a description into "value".
- Omit a field entirely if the text does not state it — never fabricate a value.
- If the text contains no instrumental findings at all, return an empty array, not null.
- If you are not confident you can read a specific value correctly, omit that one field rather than guessing — but still include the entry for the (structure, parameter) itself if it's clearly mentioned.`

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

// InstrumentalSchemaName is passed as llm.StructuredRequest.SchemaName for
// Stage 2b (see InstrumentalPrompt's doc comment for why this is a
// separate call from Schema/Prompt).
const InstrumentalSchemaName = "medical_instrumental_findings"

// InstrumentalSchema is the JSON Schema for Stage 2b — deliberately just
// one property, instrumentalFindings, unlike Schema's many. See
// InstrumentalPrompt's doc comment for why this is split out rather than a
// property on Schema.
func InstrumentalSchema() map[string]any {
	dateProp := map[string]any{"type": "string", "description": "ISO 8601 date (YYYY-MM-DD)"}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"instrumentalFindings": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"structure":        map[string]any{"type": "string", "description": "Anatomical structure/organ, as written, e.g. 'Печень', 'Желчный пузырь', 'Почка правая'."},
						"parameter":        map[string]any{"type": "string", "description": "The specific measured or observed attribute within that structure, as written, e.g. 'правая доля КВР', 'толщина стенки', 'эхогенность', 'особенности'."},
						"value":            map[string]any{"type": "number", "description": "Numeric value, if the parameter is quantitative."},
						"unit":             map[string]any{"type": "string", "description": "e.g. мм, см, см/с — only if value is set."},
						"qualitativeValue": map[string]any{"type": "string", "description": "Descriptive value when the parameter is not numeric, e.g. 'однородная', 'не расширена', 'обычная'."},
						"referenceLow":     map[string]any{"type": "number"},
						"referenceHigh":    map[string]any{"type": "number"},
						"measuredAt":       dateProp,
					},
					"required": []string{"structure", "parameter"},
				},
			},
		},
		"required": []string{"instrumentalFindings"},
	}
}

// Result is the Go-side mirror of Schema — used to unmarshal and
// pretty-print a Structured call's output for review. FullText is not part
// of Schema (it comes from Stage 1, OCR) but is included here since callers
// need both together.
type Result struct {
	DocumentType         string                `json:"documentType"`
	DocumentDate         string                `json:"documentDate,omitempty"`
	Organization         string                `json:"organization,omitempty"`
	Doctor               string                `json:"doctor,omitempty"`
	Language             string                `json:"language,omitempty"`
	FullText             string                `json:"fullText"`
	Diagnoses            []Diagnosis           `json:"diagnoses,omitempty"`
	Medications          []Medication          `json:"medications,omitempty"`
	LabResults           []LabResult           `json:"labResults,omitempty"`
	InstrumentalFindings []InstrumentalFinding `json:"instrumentalFindings,omitempty"`
	Procedures           []Procedure           `json:"procedures,omitempty"`
	Allergies            []Allergy             `json:"allergies,omitempty"`
	VitalSigns           []VitalSign           `json:"vitalSigns,omitempty"`
	Recommendations      []string              `json:"recommendations,omitempty"`
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

// InstrumentalFinding mirrors docs/domain/13-instrumental-finding.md — a
// measured or observed parameter from an instrumental study (ultrasound,
// MRI, CT, ECG, ...), as opposed to LabResult's laboratory test panels.
type InstrumentalFinding struct {
	Structure        string  `json:"structure"`
	Parameter        string  `json:"parameter"`
	Value            float64 `json:"value,omitempty"`
	Unit             string  `json:"unit,omitempty"`
	QualitativeValue string  `json:"qualitativeValue,omitempty"`
	ReferenceLow     float64 `json:"referenceLow,omitempty"`
	ReferenceHigh    float64 `json:"referenceHigh,omitempty"`
	MeasuredAt       string  `json:"measuredAt,omitempty"`
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
//
// A single call to this function does not retry on a suspiciously empty
// result — see StructuredWithRetry for that. Structured stays a thin,
// honest wrapper around one LLM call so callers that want raw, unretried
// behavior (e.g. a test asserting on a specific attempt) still have it.
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

// minFullTextForSuspicion is the fullText length (characters) above which
// an entirely empty structured result is treated as a likely extraction
// failure rather than a genuinely sparse document. Below this length, a
// short document (e.g. a one-line note) legitimately having nothing to
// extract is plausible and not retried.
const minFullTextForSuspicion = 300

// maxStructuredRetries caps how many *additional* attempts
// StructuredWithRetry makes beyond the first when a result looks
// suspiciously empty. Chosen empirically: on a real, previously-reliable
// lab report (Helix biochemistry/lipid/CBC panel, ~40 result rows), 3
// consecutive attempts at the API's low-but-nonzero temperature (see
// llm.StructuredRequest.Temperature) all returned every category empty
// despite a complete, correct fullText and a clean finishReason=STOP (no
// error, no truncation) — worse than the roughly-even odds seen earlier
// the same day on other documents. 2 extra attempts is a starting point,
// not a value derived from a rigorous failure-rate measurement; revisit if
// real usage shows it's not enough (or is overkill).
const maxStructuredRetries = 2

// isSuspiciouslyEmpty reports whether result looks like the observed
// failure mode described on maxStructuredRetries: every structured
// category empty despite a substantial transcribed text. Deliberately
// ignores InstrumentalFindings (populated by a separate call, see
// InstrumentalStructured) and metadata fields (documentType/doctor/etc.) —
// a document can legitimately have, say, no diagnoses while still having
// lab results, so this only fires when *every* clinical category is empty
// at once, not when any single one is.
func isSuspiciouslyEmpty(result Result) bool {
	if len(result.FullText) < minFullTextForSuspicion {
		return false
	}
	return len(result.Diagnoses) == 0 &&
		len(result.Medications) == 0 &&
		len(result.LabResults) == 0 &&
		len(result.Procedures) == 0 &&
		len(result.Allergies) == 0 &&
		len(result.VitalSigns) == 0 &&
		len(result.Recommendations) == 0
}

// StructuredWithRetry calls Structured, and retries (up to
// maxStructuredRetries additional times) if the result looks suspiciously
// empty (see isSuspiciouslyEmpty) — this is the function real Pipeline code
// and Extract call; Structured itself stays retry-free for callers that
// need the raw single-attempt behavior. Returns the last attempt's result
// even if every attempt was suspicious — StructuredWithRetry cannot tell
// the difference between "the model keeps failing" and "this document
// genuinely has a lot of text and legitimately nothing structured to
// extract from it," so it never turns "suspicious" into a hard error;
// see docs/mcp/03-documents.md's "Автоматический ретрай" section for how
// the caller (upload_document) is expected to still surface this via
// extractedCounts for the user to notice and request
// medical.reprocess_document if genuinely wrong.
func StructuredWithRetry(ctx context.Context, provider StructuredProvider, text string) (Result, json.RawMessage, error) {
	var (
		result Result
		raw    json.RawMessage
		err    error
	)
	for attempt := 0; attempt <= maxStructuredRetries; attempt++ {
		result, raw, err = Structured(ctx, provider, text)
		if err != nil {
			return Result{}, nil, err
		}
		if !isSuspiciouslyEmpty(result) {
			break
		}
	}
	return result, raw, nil
}

// InstrumentalStructured runs Stage 2b: extract only instrumental-study
// findings from already-transcribed text. Kept separate from Structured
// (Stage 2a) — see InstrumentalPrompt's doc comment for why combining them
// into one call turned out to be unreliable on real, dense documents.
func InstrumentalStructured(ctx context.Context, provider StructuredProvider, text string) ([]InstrumentalFinding, json.RawMessage, error) {
	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: InstrumentalPrompt},
			{Role: llm.RoleUser, Content: text},
		},
		Schema:     InstrumentalSchema(),
		SchemaName: InstrumentalSchemaName,
	}

	raw, err := provider.Structured(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("extraction: instrumental structured: %w", err)
	}

	var parsed struct {
		InstrumentalFindings []InstrumentalFinding `json:"instrumentalFindings"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, raw, fmt.Errorf("extraction: instrumental structured: unmarshal result: %w", err)
	}
	return parsed.InstrumentalFindings, raw, nil
}

// InstrumentalStructuredWithRetry is InstrumentalStructured's retrying
// counterpart — but unlike StructuredWithRetry, an empty result here is the
// *expected* outcome for most documents (a lab report or prescription
// legitimately has zero instrumental findings), so retrying on emptiness
// unconditionally would waste a call on every non-imaging document for no
// benefit. expectFindings gates this: pass true only when the document is
// already known (typically from Stage 2a's DocumentType, e.g.
// "imaging_report") to actually be an instrumental study — see Extract.
// When expectFindings is false, this makes exactly one attempt, same as
// calling InstrumentalStructured directly.
func InstrumentalStructuredWithRetry(ctx context.Context, provider StructuredProvider, text string, expectFindings bool) ([]InstrumentalFinding, json.RawMessage, error) {
	attempts := 1
	if expectFindings {
		attempts += maxStructuredRetries
	}

	var (
		findings []InstrumentalFinding
		raw      json.RawMessage
		err      error
	)
	for attempt := 0; attempt < attempts; attempt++ {
		findings, raw, err = InstrumentalStructured(ctx, provider, text)
		if err != nil {
			return nil, nil, err
		}
		if len(findings) > 0 {
			break
		}
	}
	return findings, raw, nil
}

// Provider is the union Extract needs: something that can both Chat (Stage
// 1) and Structured (Stage 2) — gemini.Provider satisfies this directly;
// router.Router does too once at least one configured provider implements
// llm.StructuredProvider.
type Provider interface {
	ChatProvider
	StructuredProvider
}

// Extract runs all stages against one document image: OCR, then
// StructuredWithRetry (Stage 2a, retried automatically if suspiciously
// empty) and InstrumentalStructured (Stage 2b) as two independent calls
// over the same transcribed text, merged into one Result. This is the
// function real Pipeline code should call; the individual stages are
// exported separately for tests/tooling that want to inspect or reuse an
// intermediate result (e.g. cmd/extract-test).
func Extract(ctx context.Context, provider Provider, imageBase64, mimeType string) (Result, json.RawMessage, error) {
	text, err := OCR(ctx, provider, imageBase64, mimeType)
	if err != nil {
		return Result{}, nil, err
	}

	result, _, err := StructuredWithRetry(ctx, provider, text)
	if err != nil {
		return Result{}, nil, err
	}

	findings, _, err := InstrumentalStructuredWithRetry(ctx, provider, text, result.DocumentType == "imaging_report")
	if err != nil {
		return Result{}, nil, err
	}
	result.InstrumentalFindings = findings

	merged, err := json.Marshal(result)
	if err != nil {
		return Result{}, nil, fmt.Errorf("extraction: marshal merged result: %w", err)
	}
	return result, merged, nil
}
