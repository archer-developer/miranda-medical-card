// Package nutrition implements the Nutrition Advisor Domain Service
// (docs/adr/006-nutrition-guidance.md): a small, one-shot Structured LLM
// call that turns a household member's active diagnoses/allergies/age/sex
// and symptoms reported in the last month into short, medically-grounded
// dietary restrictions and recommendations for
// profile.MedicalProfile.NutritionGuidance. Built like internal/decline —
// one narrow Structured task, its own prompt/Schema, no general-purpose
// reasoning surface.
//
// Deliberately scoped to medical constraints, not meal planning: turning
// these into actual dishes with calorie/macro targets is Miranda's job, not
// this service's (docs/adr/006-nutrition-guidance.md §8, ../CLAUDE.md's
// "Miranda — оркестратор... Medical Service — медицинский эксперт" split).
// promptTemplate instructs the model accordingly.
package nutrition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	llm "github.com/archer-developer/miranda-llm"

	"github.com/archer-developer/miranda-medical-card/internal/profile"
)

// SchemaName is passed as llm.StructuredRequest.SchemaName.
const SchemaName = "nutrition_guidance"

// promptTemplate is deliberately strict about two things: every item must
// trace back to something in the input (no generic "eat balanced meals"
// filler — see Input.Empty's own doc comment for why the caller should
// skip calling Generate at all when there's nothing to reason over), and
// the output must stay at the level of a direction ("limit fatty food"),
// never a specific dish/food/quantity — that step is explicitly left to
// whatever downstream system builds an actual meal plan (Miranda).
const promptTemplate = `You are a clinical nutrition assistant for a household medical record service.

Given one household member's active diagnoses/chronic conditions, allergies, age, sex, active medications, past surgeries, and symptoms they've reported in the last month, produce short dietary restrictions and recommendations that follow directly from those medical facts.

Rules:
- Every item must be grounded in a specific diagnosis, allergy, active medication, past surgery, or symptom given below — never invent an item that isn't traceable to the input.
- A past surgery can matter just as much as an active diagnosis even if it happened long ago and nothing about it appears elsewhere in the input — e.g. gallbladder removal (cholecystectomy) permanently reduces bile output and calls for a lasting fat restriction; a medication can matter on its own too — e.g. iron supplementation interacts with foods/drinks that inhibit its absorption (tea, coffee, dairy taken at the same time).
- Keep each item to one short sentence, plus a one-sentence medical reason referencing what it follows from.
- Do NOT suggest specific dishes, foods, brands, recipes, or calorie/macro targets — only the direction of a restriction or recommendation (e.g. "limit fatty food," never "avoid butter, use olive oil instead, target 20g fat/day"). A separate system turns this into an actual meal plan.
- If nothing below has clear dietary relevance, return empty lists for both — do not pad the answer with generic advice that isn't tied to a given fact.
- If a "Response language" line is given below, write every "text" and "reason" strictly in that language, regardless of what language the medical facts themselves are written in. Otherwise, write them in the same language as those facts.`

// Input is everything Generate needs about one user — deliberately narrow
// (docs/adr/006-nutrition-guidance.md §6-7, §9): the active
// diagnoses/chronic conditions and allergies profile.Builder already
// computes, active medications (same, from ActiveMedications — a course
// currently being taken can itself carry a dietary implication, e.g. iron
// supplementation and foods that block its absorption), past surgeries
// (from Procedure, type=="surgery" only — a permanent anatomical change
// like a cholecystectomy stays relevant indefinitely, not just while it's
// a recent Timeline entry), symptoms self-reported in the last month, and
// age/sex/language from config.User via pipeline.UserRepository. Still not
// the whole Profile — lab results and vital signs are out of scope for
// this call (see docs/adr/006-nutrition-guidance.md §8).
type Input struct {
	AgeYears *int
	Sex      string
	// Language, if set (config.UserConfig.Language), instructs Generate to
	// write every NutritionNote.Text/Reason in that language regardless of
	// what language the diagnoses/symptoms/etc. below happen to be in —
	// see promptTemplate's own rule. Empty means no instruction: the model
	// infers a language from the rest of the input instead, the behavior
	// from before this field existed.
	Language       string
	Diagnoses      []string
	Allergies      []string
	Medications    []string
	PastSurgeries  []string
	RecentSymptoms []string
}

// Empty reports whether i has nothing dietary-relevant to reason over.
// Generate itself always makes the call — it's the caller's job
// (pipeline.rebuildProfile) to check this first and skip Generate entirely,
// per docs/adr/006-nutrition-guidance.md §4: age/sex alone isn't a medical
// restriction, so calling the LLM just to get back "eat balanced meals" for
// a household member with no active diagnosis, allergy, medication, past
// surgery, or recent symptom would be a wasted call for a non-medical
// answer.
func (i Input) Empty() bool {
	return len(i.Diagnoses) == 0 && len(i.Allergies) == 0 && len(i.Medications) == 0 &&
		len(i.PastSurgeries) == 0 && len(i.RecentSymptoms) == 0
}

// note mirrors one NutritionNote's JSON shape — Schema constrains the
// model's output to exactly these two fields per item.
type note struct {
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// result is the Go-side mirror of Schema's top-level shape.
type result struct {
	Restrictions    []note `json:"restrictions"`
	Recommendations []note `json:"recommendations"`
}

// Schema is the JSON Schema passed as llm.StructuredRequest.Schema.
func Schema() map[string]any {
	item := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type":        "string",
				"description": "Short (one sentence) restriction or recommendation — no specific foods, dishes, brands, or quantities.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "One-sentence medical reason, referencing the diagnosis/allergy/symptom it follows from.",
			},
		},
		"required": []string{"text", "reason"},
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"restrictions":    map[string]any{"type": "array", "items": item},
			"recommendations": map[string]any{"type": "array", "items": item},
		},
	}
}

// StructuredProvider is the subset of llm.StructuredProvider Generate
// needs — mirrors internal/decline.StructuredProvider.
type StructuredProvider interface {
	Structured(ctx context.Context, req llm.StructuredRequest) (json.RawMessage, error)
}

// Generate calls provider once to turn input into a profile.NutritionGuidance,
// stamped with now as GeneratedAt. Callers should check Input.Empty() first
// (see its own doc comment) — Generate doesn't short-circuit itself, so
// calling it is always a real LLM call.
func Generate(ctx context.Context, provider StructuredProvider, input Input, now time.Time) (profile.NutritionGuidance, error) {
	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: promptTemplate},
			{Role: llm.RoleUser, Content: formatInput(input)},
		},
		Schema:     Schema(),
		SchemaName: SchemaName,
	}

	raw, err := provider.Structured(ctx, req)
	if err != nil {
		return profile.NutritionGuidance{}, fmt.Errorf("nutrition: structured: %w", err)
	}

	var r result
	if err := json.Unmarshal(raw, &r); err != nil {
		return profile.NutritionGuidance{}, fmt.Errorf("nutrition: structured: unmarshal result: %w", err)
	}

	return profile.NutritionGuidance{
		Restrictions:    toNotes(r.Restrictions),
		Recommendations: toNotes(r.Recommendations),
		GeneratedAt:     now,
	}, nil
}

func toNotes(notes []note) []profile.NutritionNote {
	if len(notes) == 0 {
		return nil
	}
	result := make([]profile.NutritionNote, len(notes))
	for i, n := range notes {
		result[i] = profile.NutritionNote{Text: n.Text, Reason: n.Reason}
	}
	return result
}

func formatInput(input Input) string {
	var b strings.Builder
	if input.Language != "" {
		fmt.Fprintf(&b, "Response language: %s\n", input.Language)
	}
	if input.AgeYears != nil {
		fmt.Fprintf(&b, "Age: %d\n", *input.AgeYears)
	}
	if input.Sex != "" {
		fmt.Fprintf(&b, "Sex: %s\n", input.Sex)
	}
	writeList(&b, "Active diagnoses/chronic conditions", input.Diagnoses)
	writeList(&b, "Allergies", input.Allergies)
	writeList(&b, "Active medications", input.Medications)
	writeList(&b, "Past surgeries", input.PastSurgeries)
	writeList(&b, "Symptoms reported in the last month", input.RecentSymptoms)
	return b.String()
}

func writeList(b *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", label)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
}
