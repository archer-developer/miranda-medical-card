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

func TestServer_CompleteMedication_HappyPath(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Рецепт. Амоксициллин 500мг."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"prescription","medications":[{"name":"Амоксициллин","doseAmount":500,"doseUnit":"мг","status":"active"}]}`)},
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
	// No MCP tool surfaces a raw medicationId (by design: Miranda always
	// passes free text, never an id, to medical.complete_medication, same as
	// medical.resolve_diagnosis), and storage — not normalization.Normalize
	// — is what actually assigns Medication.ID (a fresh uuid minted in
	// ReplaceForDocument, see internal/storage/medication.go), so the test
	// reads the real id straight out of the store rather than reconstructing it.
	meds, err := storage.NewMedicationRepository(store).ListByDocument(context.Background(), uploadOut.DocumentID)
	require.NoError(t, err)
	require.Len(t, meds, 1)
	medicationID := meds[0].ID

	provider.WithStructured(
		llmtest.StructuredResponse{}, // index 0, already consumed by upload's OCR/structured calls, never re-read
		llmtest.StructuredResponse{}, // index 1, ditto (instrumentalFindings)
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"matchId":"` + medicationID + `"}`)},
	)

	completeResult := callTool(t, session, "medical.complete_medication", map[string]any{"userId": "alex", "text": "я закончил принимать амоксициллин"})
	require.False(t, completeResult.IsError, "%v", completeResult.Content)
	completeOut := requireContentMirrorsStructured[struct {
		MedicationID string `json:"medicationId"`
		Status       string `json:"status"`
	}](t, completeResult)
	require.Equal(t, medicationID, completeOut.MedicationID)
	require.Equal(t, "completed", completeOut.Status)
}

func TestServer_CompleteMedication_NoActiveMedicationsReturnsNotFound(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.complete_medication", map[string]any{"userId": "alex", "text": "я закончил курс"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "MEDICATION_NOT_FOUND")
}

func TestServer_CompleteMedication_EmptyTextRejected(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.complete_medication", map[string]any{"userId": "alex", "text": "   "})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "INVALID_EVENT")
}
