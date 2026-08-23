package mcpserver_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/router"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/filestore"
	"github.com/archer-developer/miranda-medical-card/internal/mcpserver"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// testPublicBaseURL stands in for config.Config.PublicBaseURL — the origin
// medical.get_document's fileUri is built against (see documents.go's
// fileURI helper).
const testPublicBaseURL = "https://medical-card.test:8791"

// newTestSession wires a real Pipeline + Asker (both backed by an in-memory
// SQLite Store and fake LLM/embedder) behind mcpserver.New, connects an
// in-process MCP client to it via mcp.NewInMemoryTransports, and returns a
// ready ClientSession — end-to-end through the actual MCP protocol
// (JSON-RPC over an in-memory pipe), not a direct Go function call, so this
// exercises the same request/response marshaling a real client would hit.
func newTestSession(t *testing.T, provider *llmtest.FakeProvider, users []config.UserConfig) *mcp.ClientSession {
	t.Helper()
	session, _ := newTestSessionWithStore(t, provider, users)
	return session
}

// newTestSessionWithStore is newTestSession plus the underlying Store, for
// the rare test that needs to read something no MCP tool surfaces (e.g.
// resolve_diagnosis_test.go's real, storage-assigned Diagnosis.ID — storage
// mints that id itself now rather than trusting normalization's
// deterministic one, see internal/storage/diagnosis.go's ReplaceForDocument).
func newTestSessionWithStore(t *testing.T, provider *llmtest.FakeProvider, users []config.UserConfig) (*mcp.ClientSession, *storage.Store) {
	t.Helper()
	ctx := context.Background()

	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	fs, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	pl := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)

	registry := ask.NewRegistry(
		ask.NewTimelineProvider(storage.NewTimelineRepository(s)),
		ask.NewMedicationProvider(storage.NewMedicationRepository(s)),
	)
	askRouter, err := router.New([]llm.Provider{provider}, nil, "fake")
	require.NoError(t, err)
	asker := ask.NewAsker(askRouter, registry, ask.NewSessionStore(storage.NewAskSessionRepository(s)), 5*time.Second, 20, 8, nil)

	server := mcpserver.New(pl, asker, users, 50*1024*1024, testPublicBaseURL, nil)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession, s
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

// requireContentMirrorsStructured guards against
// docs/adr/002-structured-profile-response.md's class of bug: Miranda's
// Gemini relay only reads a tool result's Content, never StructuredContent
// (confirmed from a live Miranda log where list_documents/get_document
// results arrived as a lossy one-line summary, hiding every documentId and
// fileUri from the model). Every tool except medical.ask and
// medical.download_file (see files_test.go for that deliberate exception)
// must leave Content nil so the SDK serializes the exact same JSON as
// StructuredContent into it — this asserts that invariant end-to-end.
func requireContentMirrorsStructured[T any](t *testing.T, result *mcp.CallToolResult) T {
	t.Helper()
	require.Len(t, result.Content, 1)
	textContent, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	var fromText T
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &fromText), "Content must be the full structured JSON, not a hand-written summary")
	structured := decodeStructured[T](t, result)
	require.Equal(t, structured, fromText, "Content and StructuredContent must carry exactly the same data")
	return structured
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
		"medical.log_event", "medical.delete_event", "medical.decline_planned_action", "medical.complete_planned_action", "medical.resolve_diagnosis",
		"medical.ask", "medical.profile", "medical.timeline", "medical.planned_actions",
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
	docOut := requireContentMirrorsStructured[struct {
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
	listOut := requireContentMirrorsStructured[struct {
		Documents []struct{ DocumentID string } `json:"documents"`
	}](t, listResult)
	require.Len(t, listOut.Documents, 1)
	require.Equal(t, docOut.DocumentID, listOut.Documents[0].DocumentID)
}

// TestServer_UploadDocument_ReturnsPlannedActions covers the
// upload_document/reprocess_document response shape added alongside
// docs/adr/004-planned-actions.md's own feature landing: extractedCounts
// must include plannedActions (previously only computed internally by
// internal/pipeline, never surfaced through MCP), and the response's own
// plannedActions array must carry the full description/type/due-date-range
// for each — so Miranda can tell the user what was added to their plan
// straight from the upload response, without a separate
// medical.planned_actions call. Two entries: one with a stated timeframe
// (must produce a dueDateFrom/dueDateTo range) and one without (must not).
func TestServer_UploadDocument_ReturnsPlannedActions(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Консультация эндокринолога. Рекомендован контроль глюкозы через 6 месяцев и консультация невролога."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{
			"documentType": "consultation",
			"documentDate": "2026-03-12",
			"recommendations": ["Контроль глюкозы через 6 месяцев", "Консультация невролога"],
			"plannedActions": [
				{"type": "lab_test", "description": "Контроль глюкозы", "relatedIndicatorName": "Глюкоза", "dueAmountMax": 6, "dueUnit": "month"},
				{"type": "consultation", "description": "Консультация невролога", "relatedProcedureName": "Консультация невролога"}
			]
		}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="consult.pdf"`)
		_, _ = w.Write([]byte("pdf bytes"))
	}))
	t.Cleanup(fileServer.Close)

	docResult := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": fileServer.URL})
	require.False(t, docResult.IsError, "%v", docResult.Content)
	docOut := requireContentMirrorsStructured[struct {
		ExtractedCounts struct {
			PlannedActions int `json:"plannedActions"`
		} `json:"extractedCounts"`
		PlannedActions []struct {
			PlannedActionID string `json:"plannedActionId"`
			Type            string `json:"type"`
			Description     string `json:"description"`
			DueDateFrom     string `json:"dueDateFrom"`
			DueDateTo       string `json:"dueDateTo"`
			Status          string `json:"status"`
		} `json:"plannedActions"`
	}](t, docResult)

	require.Equal(t, 2, docOut.ExtractedCounts.PlannedActions)
	require.Len(t, docOut.PlannedActions, 2)

	byDescription := make(map[string]struct {
		PlannedActionID string `json:"plannedActionId"`
		Type            string `json:"type"`
		Description     string `json:"description"`
		DueDateFrom     string `json:"dueDateFrom"`
		DueDateTo       string `json:"dueDateTo"`
		Status          string `json:"status"`
	})
	for _, a := range docOut.PlannedActions {
		byDescription[a.Description] = a
	}

	glucose := byDescription["Контроль глюкозы"]
	require.Equal(t, "lab_test", glucose.Type)
	require.NotEmpty(t, glucose.PlannedActionID)
	require.NotEmpty(t, glucose.DueDateFrom, "a recommendation with a stated timeframe must get a due date")
	require.Equal(t, "pending", glucose.Status)

	neurologist := byDescription["Консультация невролога"]
	require.Equal(t, "consultation", neurologist.Type)
	require.Empty(t, neurologist.DueDateFrom, "a recommendation with no stated timeframe must stay undated")
}

// TestServer_GetDocumentReturnsFileURI exercises the replacement for
// medical.download_file (see docs/mcp/02-files.md §5): medical.get_document
// must embed an absolute fileUri built from PublicBaseURL, and that URI
// must actually resolve — via NewFileDownloadHandler, mounted the same way
// httpserver.New mounts it in production — to the exact bytes originally
// uploaded.
func TestServer_GetDocumentReturnsFileURI(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)

	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	fs, err := filestore.New(t.TempDir())
	require.NoError(t, err)
	pl := pipeline.New(provider, nil, provider, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)
	registry := ask.NewRegistry(ask.NewTimelineProvider(storage.NewTimelineRepository(s)))
	askRouter, err := router.New([]llm.Provider{provider}, nil, "fake")
	require.NoError(t, err)
	asker := ask.NewAsker(askRouter, registry, ask.NewSessionStore(storage.NewAskSessionRepository(s)), 5*time.Second, 20, 8, nil)

	server := mcpserver.New(pl, asker, []config.UserConfig{{ID: "alex"}}, 50*1024*1024, testPublicBaseURL, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	const originalBytes = "pdf bytes"
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="cbc.pdf"`)
		_, _ = w.Write([]byte(originalBytes))
	}))
	t.Cleanup(fileServer.Close)

	docResult := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": fileServer.URL})
	require.False(t, docResult.IsError, "%v", docResult.Content)
	docOut := requireContentMirrorsStructured[struct {
		DocumentID string `json:"documentId"`
	}](t, docResult)

	getResult := callTool(t, session, "medical.get_document", map[string]any{"userId": "alex", "documentId": docOut.DocumentID})
	require.False(t, getResult.IsError, "%v", getResult.Content)
	getOut := requireContentMirrorsStructured[struct {
		FileURI string `json:"fileUri"`
	}](t, getResult)
	require.True(t, strings.HasPrefix(getOut.FileURI, testPublicBaseURL+"/files/"), "fileUri %q must be rooted at PublicBaseURL", getOut.FileURI)

	// The URI must resolve through the exact handler production mounts —
	// mirrors httpserver.New's "GET /files/{fileId}" route.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files/{fileId}", mcpserver.NewFileDownloadHandler(pl, nil))
	downloadServer := httptest.NewServer(mux)
	t.Cleanup(downloadServer.Close)

	relativePath := strings.TrimPrefix(getOut.FileURI, testPublicBaseURL)
	resp, err := http.Get(downloadServer.URL + relativePath)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, originalBytes, string(body))
	require.Equal(t, "application/pdf", resp.Header.Get("Content-Type"))
	require.Equal(t, strconv.Itoa(len(originalBytes)), resp.Header.Get("Content-Length"), "Content-Length must match the bytes actually written, not DB metadata")
	require.Equal(t, `attachment; filename="cbc.pdf"; filename*=UTF-8''cbc.pdf`, resp.Header.Get("Content-Disposition"))
}

// TestServer_DownloadFileEnforcesOwnershipOnEveryCall is the reason
// medical.download_file was kept alongside fileUri: unlike fileUri (whose
// access check happens once, at the moment get_document mints it),
// medical.download_file re-validates ownership/shared_with on every call —
// this proves that a user with no relation to the document is rejected
// even though the fileId itself is valid.
func TestServer_DownloadFileEnforcesOwnershipOnEveryCall(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}, {ID: "stranger"}})

	const originalBytes = "pdf bytes"
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="анализ крови.pdf"`)
		_, _ = w.Write([]byte(originalBytes))
	}))
	t.Cleanup(fileServer.Close)

	docResult := callTool(t, session, "medical.upload_document", map[string]any{"userId": "alex", "fileUri": fileServer.URL})
	require.False(t, docResult.IsError, "%v", docResult.Content)
	docOut := decodeStructured[struct {
		DocumentID string `json:"documentId"`
	}](t, docResult)

	getResult := callTool(t, session, "medical.get_document", map[string]any{"userId": "alex", "documentId": docOut.DocumentID})
	require.False(t, getResult.IsError, "%v", getResult.Content)
	getOut := decodeStructured[struct {
		FileURI string `json:"fileUri"`
	}](t, getResult)
	fileID := strings.TrimPrefix(getOut.FileURI, testPublicBaseURL+"/files/")
	require.NotEmpty(t, fileID)

	ownResult := callTool(t, session, "medical.download_file", map[string]any{"userId": "alex", "fileId": fileID})
	require.False(t, ownResult.IsError, "%v", ownResult.Content)
	ownOut := decodeStructured[struct {
		Filename string `json:"filename"`
		Data     string `json:"data"`
	}](t, ownResult)
	require.Equal(t, "анализ крови.pdf", ownOut.Filename)
	decoded, err := base64.StdEncoding.DecodeString(ownOut.Data)
	require.NoError(t, err)
	require.Equal(t, originalBytes, string(decoded))

	// Deliberate exception to requireContentMirrorsStructured (see
	// files.go's downloadFileHandler): unlike every other tool, Content
	// here must NOT mirror StructuredContent, because StructuredContent
	// carries the full base64 file body — auto-serializing that into
	// Content would dump potentially megabytes of base64 into Miranda's
	// LLM context, which only reads Content.
	require.Len(t, ownResult.Content, 1)
	ownText, ok := ownResult.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.NotContains(t, ownText.Text, ownOut.Data, "Content must never carry the base64 file body")

	deniedResult := callTool(t, session, "medical.download_file", map[string]any{"userId": "stranger", "fileId": fileID})
	require.True(t, deniedResult.IsError)
	require.Contains(t, deniedResult.Content[0].(*mcp.TextContent).Text, "FILE_NOT_FOUND")
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
	}, llmtest.StructuredResponse{
		// log_event's rebuildProfile Nutrition Guidance call
		// (docs/adr/006-nutrition-guidance.md) — the freshly logged
		// symptom makes Input non-empty. delete_event's own rebuildProfile
		// doesn't need a second scripted response: by the time it runs the
		// event is already removed, so Input is empty again and Generate
		// is short-circuited.
		JSON: json.RawMessage(`{"restrictions":[],"recommendations":[]}`),
	})
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	logResult := callTool(t, session, "medical.log_event", map[string]any{"userId": "alex", "text": "Болит голова"})
	require.False(t, logResult.IsError, "%v", logResult.Content)
	logOut := requireContentMirrorsStructured[struct {
		EventID  string `json:"eventId"`
		Category string `json:"category"`
	}](t, logResult)
	require.NotEmpty(t, logOut.EventID)
	require.Equal(t, "symptom", logOut.Category, "Content must carry category, not just a bare eventId summary")

	deleteResult := callTool(t, session, "medical.delete_event", map[string]any{"userId": "alex", "eventId": logOut.EventID})
	require.False(t, deleteResult.IsError)
	deleteOut := requireContentMirrorsStructured[struct {
		Deleted bool `json:"deleted"`
	}](t, deleteResult)
	require.True(t, deleteOut.Deleted)
}

func TestServer_TimelineContentMirrorsStructuredContent(t *testing.T) {
	provider := llmtest.New("fake").WithStructured(llmtest.StructuredResponse{
		JSON: json.RawMessage(`{"category":"symptom","description":"Головная боль"}`),
	}, llmtest.StructuredResponse{
		// log_event's rebuildProfile Nutrition Guidance call — see the
		// identical comment in TestServer_LogEventThenDeleteEvent above.
		JSON: json.RawMessage(`{"restrictions":[],"recommendations":[]}`),
	})
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	logResult := callTool(t, session, "medical.log_event", map[string]any{"userId": "alex", "text": "Болит голова"})
	require.False(t, logResult.IsError, "%v", logResult.Content)

	result := callTool(t, session, "medical.timeline", map[string]any{"userId": "alex"})
	require.False(t, result.IsError, "%v", result.Content)
	out := requireContentMirrorsStructured[struct {
		Events []struct {
			EventID string `json:"eventId"`
			Title   string `json:"title"`
		} `json:"events"`
	}](t, result)
	require.NotEmpty(t, out.Events, "Content must carry the actual events, not just a count")
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
