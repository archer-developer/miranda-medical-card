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

func TestServer_ResolveDiagnosis_HappyPath(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Консультация. Диагноз: ОРВИ."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"consultation","diagnoses":[{"name":"ОРВИ","status":"active"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
		// index 2: rebuildProfile's Nutrition Guidance call
		// (docs/adr/006-nutrition-guidance.md) — the uploaded document's
		// active "ОРВИ" diagnosis makes Input non-empty, so this fires.
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"restrictions":[],"recommendations":[]}`)},
	)
	session, store := newTestSessionWithStore(t, provider, []config.UserConfig{{ID: "alex"}})

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="consult.pdf"`)
		_, _ = w.Write([]byte("pdf bytes"))
	}))
	t.Cleanup(fileServer.Close)

	uploadResult := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": fileServer.URL})
	require.False(t, uploadResult.IsError, "%v", uploadResult.Content)
	uploadOut := requireContentMirrorsStructured[struct {
		DocumentID string `json:"documentId"`
	}](t, uploadResult)
	// No MCP tool surfaces a raw diagnosisId (by design: Miranda always
	// passes free text, never an id, to medical.resolve_diagnosis, same as
	// medical.decline_planned_action), and storage — not
	// normalization.Normalize — is what actually assigns Diagnosis.ID (a
	// fresh uuid minted in ReplaceForDocument, see
	// internal/storage/diagnosis.go), so the test reads the real id
	// straight out of the store rather than reconstructing it.
	diagnoses, err := storage.NewDiagnosisRepository(store).ListByDocument(context.Background(), uploadOut.DocumentID)
	require.NoError(t, err)
	require.Len(t, diagnoses, 1)
	diagnosisID := diagnoses[0].ID

	provider.WithStructured(
		llmtest.StructuredResponse{}, // index 0, already consumed by upload's OCR/structured calls, never re-read
		llmtest.StructuredResponse{}, // index 1, ditto (instrumentalFindings)
		llmtest.StructuredResponse{}, // index 2, ditto (upload's rebuildProfile nutrition call)
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"matchId":"` + diagnosisID + `"}`)},
	)

	resolveResult := callTool(t, session, "medical.resolve_diagnosis", map[string]any{"userId": "alex", "text": "да, ОРВИ прошла"})
	require.False(t, resolveResult.IsError, "%v", resolveResult.Content)
	resolveOut := requireContentMirrorsStructured[struct {
		DiagnosisID string `json:"diagnosisId"`
		Status      string `json:"status"`
	}](t, resolveResult)
	require.Equal(t, diagnosisID, resolveOut.DiagnosisID)
	require.Equal(t, "resolved", resolveOut.Status)
}

func TestServer_ResolveDiagnosis_NoDiagnosesReturnsNotFound(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.resolve_diagnosis", map[string]any{"userId": "alex", "text": "да, всё прошло"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "DIAGNOSIS_NOT_FOUND")
}

func TestServer_ResolveDiagnosis_EmptyTextRejected(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.resolve_diagnosis", map[string]any{"userId": "alex", "text": "   "})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "INVALID_EVENT")
}
