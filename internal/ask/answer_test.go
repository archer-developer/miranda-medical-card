package ask_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
)

func TestGenerateAnswer_ReturnsAnswerText(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"answer":"Согласно результатам анализа, ALT впервые повысился 12 марта 2025."}`),
	})

	answer, err := ask.GenerateAnswer(context.Background(), provider, nil, "Когда впервые повысился ALT?", "QUESTION\n\n...", nil)
	require.NoError(t, err)
	require.Contains(t, answer, "12 марта 2025")
}

// TestGenerateAnswer_EscalatesOnPrimaryFailure covers the config gap found
// in production (2026-08-09): llm.yaml's escalation config on
// answer_provider was silently ignored — Asker never had an escalation
// provider wired at all — so a Gemini 429 failed the whole medical.ask
// call even though Claude was configured as a fallback. GenerateAnswer
// must now retry once against escalate when provider hard-errors.
func TestGenerateAnswer_EscalatesOnPrimaryFailure(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		Err: errors.New("boom: 429 quota exceeded"),
	})
	escalate := llmtest.New("fake-escalate").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"answer":"Ответ от эскалационного провайдера."}`),
	})

	answer, err := ask.GenerateAnswer(context.Background(), provider, escalate, "question", "context", nil)
	require.NoError(t, err)
	require.Equal(t, "Ответ от эскалационного провайдера.", answer)
}

// TestGenerateAnswer_NoEscalationConfiguredFailsOutright confirms nil
// escalate (no escalation configured for this provider in llm.yaml) keeps
// today's plain "primary fails, call fails" behavior — not a regression.
func TestGenerateAnswer_NoEscalationConfiguredFailsOutright(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		Err: errors.New("boom: 429 quota exceeded"),
	})

	_, err := ask.GenerateAnswer(context.Background(), provider, nil, "question", "context", nil)
	require.Error(t, err)
}

// TestGenerateAnswer_BothPrimaryAndEscalationFailReturnsError confirms an
// escalation provider that also fails surfaces a real error rather than
// panicking or silently returning an empty answer.
func TestGenerateAnswer_BothPrimaryAndEscalationFailReturnsError(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		Err: errors.New("boom: primary down"),
	})
	escalate := llmtest.New("fake-escalate").WithStructured(llmtest.StructuredResponse{
		Err: errors.New("boom: escalation also down"),
	})

	_, err := ask.GenerateAnswer(context.Background(), provider, escalate, "question", "context", nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "primary down")
	require.ErrorContains(t, err, "escalation also down")
}
