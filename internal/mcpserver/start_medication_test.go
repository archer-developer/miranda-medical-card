package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestServer_StartMedication_HappyPath(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Рецепт. Амоксициллин 500мг."},
	).WithStructured(
		// No explicit status — a plain prescription defaults to "prescribed"
		// (see normalization.Normalize).
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"prescription","medications":[{"name":"Амоксициллин","doseAmount":500,"doseUnit":"мг"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	session, store := newTestSessionWithStore(t, provider, []config.UserConfig{{ID: "alex"}})

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="prescription.pdf"`)
		_, _ = w.Write([]byte("pdf bytes"))
	}))
	t.Cleanup(fileServer.Close)

	uploadResult := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": fileServer.URL})
	require.False(t, uploadResult.IsError, "%v", uploadResult.Content)
	uploadOut := requireContentMirrorsStructured[struct {
		DocumentID string `json:"documentId"`
	}](t, uploadResult)
	meds, err := storage.NewMedicationRepository(store).ListByDocument(context.Background(), uploadOut.DocumentID)
	require.NoError(t, err)
	require.Len(t, meds, 1)
	require.Equal(t, "prescribed", string(meds[0].Status), "a plain prescription with no confirmed intake defaults to prescribed")
	medicationID := meds[0].ID

	provider.WithStructured(
		llmtest.StructuredResponse{}, // index 0, already consumed by upload's OCR/structured calls, never re-read
		llmtest.StructuredResponse{}, // index 1, ditto (instrumentalFindings)
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"matchId":"` + medicationID + `"}`)},
	)

	startResult := callTool(t, session, "medical.start_medication", map[string]any{"userId": "alex", "text": "я начал принимать амоксициллин"})
	require.False(t, startResult.IsError, "%v", startResult.Content)
	startOut := requireContentMirrorsStructured[struct {
		MedicationID string `json:"medicationId"`
		Status       string `json:"status"`
	}](t, startResult)
	require.Equal(t, medicationID, startOut.MedicationID)
	require.Equal(t, "active", startOut.Status)
}

func TestServer_StartMedication_NoPrescribedMedicationsReturnsNotFound(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.start_medication", map[string]any{"userId": "alex", "text": "я начал курс"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "MEDICATION_NOT_FOUND")
}

func TestServer_StartMedication_EmptyTextRejected(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.start_medication", map[string]any{"userId": "alex", "text": "   "})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "INVALID_EVENT")
}
