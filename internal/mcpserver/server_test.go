package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/filestore"
	"github.com/archer-developer/miranda-medical-card/internal/mcpserver"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// newTestSession wires a real Pipeline + Asker (both backed by an in-memory
// SQLite Store and fake LLM/embedder) behind mcpserver.New, connects an
// in-process MCP client to it via mcp.NewInMemoryTransports, and returns a
// ready ClientSession — end-to-end through the actual MCP protocol
// (JSON-RPC over an in-memory pipe), not a direct Go function call, so this
// exercises the same request/response marshaling a real client would hit.
func newTestSession(t *testing.T, provider *llmtest.FakeProvider, users []config.UserConfig) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	fs, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	pl := pipeline.New(provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil)

	registry := ask.NewRegistry(
		ask.NewTimelineProvider(storage.NewTimelineRepository(s)),
		ask.NewMedicationProvider(storage.NewMedicationRepository(s)),
	)
	asker := ask.NewAsker(provider, nil, provider, nil, registry, 5*time.Second, 20, nil)

	server := mcpserver.New(pl, asker, users, 50*1024*1024, nil)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err, "transport-level error calling %s", name)
	return result
}

func decodeStructured[T any](t *testing.T, result *mcp.CallToolResult) T {
	t.Helper()
	var out T
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestServer_ListsAllRegisteredTools(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	names := make(map[string]bool, len(result.Tools))
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"medical.download_file",
		"medical.upload_document", "medical.reprocess_document", "medical.list_documents", "medical.get_document",
		"medical.log_event", "medical.delete_event",
		"medical.ask", "medical.profile", "medical.timeline",
	} {
		require.True(t, names[want], "expected tool %s to be registered", want)
	}
	require.False(t, names["medical.delete_document"], "delete_document is explicitly out of scope for this pass")
	require.False(t, names["medical.upload_file"], "medical.upload_file was removed — no MCP tool may accept raw file bytes as an argument, see docs/mcp/02-files.md §2")
}

func TestServer_UnknownUserRejected(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.profile", map[string]any{"userId": "someone_not_configured"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "USER_NOT_FOUND")
}

func TestServer_UploadDocumentFetchesFileURI(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	// Stands in for Miranda's file-hosting endpoint (see
	// docs/mcp/02-files.md §2): serves the file's bytes over plain HTTP,
	// exactly what medical.upload_document is expected to GET itself.
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="cbc.pdf"`)
		_, _ = w.Write([]byte("pdf bytes"))
	}))
	t.Cleanup(fileServer.Close)

	docResult := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": fileServer.URL})
	require.False(t, docResult.IsError, "%v", docResult.Content)
	docOut := decodeStructured[struct {
		DocumentID      string `json:"documentId"`
		Status          string `json:"status"`
		ExtractedCounts struct {
			LabResults int `json:"labResults"`
		} `json:"extractedCounts"`
	}](t, docResult)
	require.Equal(t, "READY", docOut.Status)
	require.Equal(t, 1, docOut.ExtractedCounts.LabResults)

	listResult := callTool(t, session, "medical.list_documents", map[string]any{"userId": "alex"})
	require.False(t, listResult.IsError)
	listOut := decodeStructured[struct {
		Documents []struct{ DocumentID string } `json:"documents"`
	}](t, listResult)
	require.Len(t, listOut.Documents, 1)
	require.Equal(t, docOut.DocumentID, listOut.Documents[0].DocumentID)
}

func TestServer_UploadDocument_InvalidFileURIRejected(t *testing.T) {
	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": "not-a-url"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "INVALID_FILE")
}

func TestServer_UploadDocument_FileURINotFoundRejected(t *testing.T) {
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(fileServer.Close)

	session := newTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": fileServer.URL})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "FILE_NOT_FOUND")
}

func TestServer_LogEventThenDeleteEvent(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"symptom","description":"Головная боль"}`),
	})
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	logResult := callTool(t, session, "medical.log_event", map[string]any{"userId": "alex", "text": "Болит голова"})
	require.False(t, logResult.IsError, "%v", logResult.Content)
	logOut := decodeStructured[struct {
		EventID string `json:"eventId"`
	}](t, logResult)
	require.NotEmpty(t, logOut.EventID)

	deleteResult := callTool(t, session, "medical.delete_event", map[string]any{"userId": "alex", "eventId": logOut.EventID})
	require.False(t, deleteResult.IsError)
	deleteOut := decodeStructured[struct {
		Deleted bool `json:"deleted"`
	}](t, deleteResult)
	require.True(t, deleteOut.Deleted)
}

func TestServer_SharedWithGrantsSubjectIDReadAccess(t *testing.T) {
	users := []config.UserConfig{{ID: "parent"}, {ID: "kid", SharedWith: []string{"parent"}}}
	session := newTestSession(t, llmtest.New("fake"), users)

	// kid has no data yet — MedicalProfile is expected to be empty, not an error.
	result := callTool(t, session, "medical.profile", map[string]any{"userId": "parent", "subjectId": "kid"})
	require.False(t, result.IsError, "%v", result.Content)

	// The reverse direction must be denied: parent hasn't shared with kid.
	denied := callTool(t, session, "medical.profile", map[string]any{"userId": "kid", "subjectId": "parent"})
	require.True(t, denied.IsError)
	require.Contains(t, denied.Content[0].(*mcp.TextContent).Text, "USER_NOT_FOUND")
}
