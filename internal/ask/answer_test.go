package ask_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
)

func TestGenerateAnswer_ReturnsAnswerText(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"answer":"Согласно результатам анализа, ALT впервые повысился 12 марта 2025."}`),
	})

	answer, err := ask.GenerateAnswer(context.Background(), provider, "Когда впервые повысился ALT?", "QUESTION\n\n...")
	require.NoError(t, err)
	require.Contains(t, answer, "12 марта 2025")
}
