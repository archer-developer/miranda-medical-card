package ask

import (
	"context"
	"encoding/json"
	"fmt"

	llm "github.com/archer-developer/miranda-llm"
)

// AnswerSchemaName is passed as llm.StructuredRequest.SchemaName.
const AnswerSchemaName = "medical_ask_answer"

// answerPrompt mirrors docs/mcp/04-medical.md §16-17's requirements:
// grounded only in the given context, honest about missing information,
// distinguishes document-derived facts from self-reported ones, and never
// substitutes for a doctor.
//
// `sources` is deliberately NOT part of this call's output — it's computed
// separately by CollectSources from the same chunks that built the
// context, rather than asked of the model, since the model reproducing an
// exact documentId/eventId string correctly (not paraphrased, not
// invented) is exactly the kind of mechanical task
// docs/architecture/05-llm.md §4 says code should do, not LLM.
const answerPrompt = `You are a medical information assistant answering a user's question about their own medical history, using ONLY the context provided below — never anything else.

Rules:
- Base your answer strictly on the given context. Never invent or assume a fact not present in it.
- If the context doesn't contain enough information to answer, say so plainly (e.g. "В медицинской истории нет информации о ..."). Do not guess.
- When you state a fact, make its source clear in the wording: a fact from "Timeline", "Lab Results", "Medications", "Diagnoses", "Procedures", "Instrumental Findings", "Medical Profile", or "Documents" came from a document or verified structured record — you may state it directly ("Согласно результатам анализа от..."). A fact whose context section marked it as self-reported (see the context — self-reported Timeline entries and any "Related (semantic search)" entry sourced from a user's own note) must be attributed as the user's own unverified account (e.g. "по вашей записи от...", "по вашим словам..."), not stated as a confirmed medical fact.
- You are not a doctor. Do not diagnose, do not recommend treatment, do not interpret whether a lab value is dangerous — state facts and, if relevant, suggest discussing interpretation with a doctor.
- Answer in the same language as the question, concisely and clearly.`

type answerResult struct {
	Answer string `json:"answer"`
}

// GenerateAnswer runs the Answer Generator
// (docs/architecture/03-knowledge-providers.md §14): the second and last
// LLM call, question + the Context Builder's output in, a grounded answer
// out.
func GenerateAnswer(ctx context.Context, provider StructuredProvider, question, builtContext string) (string, error) {
	req := llm.StructuredRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: answerPrompt},
			{Role: llm.RoleUser, Content: fmt.Sprintf("%s\n\nQuestion to answer: %s", builtContext, question)},
		},
		Schema:     answerSchema(),
		SchemaName: AnswerSchemaName,
	}

	raw, err := provider.Structured(ctx, req)
	if err != nil {
		return "", fmt.Errorf("ask: generate answer: %w", err)
	}
	var result answerResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("ask: generate answer: unmarshal result: %w", err)
	}
	return result.Answer, nil
}

func answerSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
		},
		"required": []string{"answer"},
	}
}
