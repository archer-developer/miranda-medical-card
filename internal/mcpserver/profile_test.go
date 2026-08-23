package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-llm/llmtest"

	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/mcpserver"
)

// TestServer_Profile_ContentMirrorsStructuredContent guards against
// docs/adr/002-structured-profile-response.md's reported bug: Miranda only
// seeing part of a user's profile (and mis-grouping the rest when asked to
// build a PDF from it) because the tool's Content text block used to be a
// lossy one-line count of just 3 of the profile's 7 sections, while the
// full data only lived in StructuredContent. Whatever a caller reads out of
// Content must now be the same complete data as StructuredContent — never a
// summary that drops fields.
func TestServer_Profile_ContentMirrorsStructuredContent(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3,"unit":"U/L"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="cbc.pdf"`)
		_, _ = w.Write([]byte("pdf bytes"))
	}))
	t.Cleanup(fileServer.Close)

	uploadResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "medical.upload_document", Arguments: map[string]any{"userId": "alex", "fileUri": fileServer.URL},
	})
	require.NoError(t, err)
	require.False(t, uploadResult.IsError, "%v", uploadResult.Content)

	result := callTool(t, session, "medical.profile", map[string]any{"userId": "alex"})
	require.False(t, result.IsError, "%v", result.Content)

	structured := decodeStructured[mcpserver.ProfileOutput](t, result)
	require.Len(t, structured.LatestLabResults, 1)
	require.Equal(t, "АЛТ", structured.LatestLabResults[0].Name)
	require.NotEmpty(t, structured.LatestLabResults[0].DocumentSource, "a lab result's DocumentSource must be populated so a caller can tell which document/panel it came from")

	require.Len(t, result.Content, 1)
	textContent, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	var fromText mcpserver.ProfileOutput
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &fromText), "Content must be the full profile JSON, not a hand-written summary sentence")
	require.Equal(t, structured, fromText, "Content and StructuredContent must carry exactly the same data")
}

// TestServer_Profile_FormatFileURI_ResolvesToSameJSONAsInline guards
// docs/adr/007-short-lived-file-links-for-profile-export.md's whole point:
// format=file_uri's link must resolve, byte-for-byte, to exactly the JSON
// format=json would have returned inline — not a summary, not a
// re-derivation, the same bytes — since the entire motivation was a caller
// unreliably retyping a large profile rather than passing a URI through.
func TestServer_Profile_FormatFileURI_ResolvesToSameJSONAsInline(t *testing.T) {
	provider := llmtest.New("fake",
		llmtest.Response{Text: "Общий анализ крови. Дата: 2026-03-12."},
	).WithStructured(
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"documentType":"lab_report","labResults":[{"name":"АЛТ","value":28.3,"unit":"U/L"}]}`)},
		llmtest.StructuredResponse{JSON: json.RawMessage(`{"instrumentalFindings":[]}`)},
	)
	session, store := newTestSessionWithStore(t, provider, []config.UserConfig{{ID: "alex"}})

	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="cbc.pdf"`)
		_, _ = w.Write([]byte("pdf bytes"))
	}))
	t.Cleanup(fileServer.Close)

	uploadResult, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "medical.upload_document", Arguments: map[string]any{"userId": "alex", "fileUri": fileServer.URL},
	})
	require.NoError(t, err)
	require.False(t, uploadResult.IsError, "%v", uploadResult.Content)

	jsonResult := callTool(t, session, "medical.profile", map[string]any{"userId": "alex", "format": "json"})
	require.False(t, jsonResult.IsError, "%v", jsonResult.Content)
	inlineBytes, err := json.Marshal(decodeStructured[mcpserver.ProfileOutput](t, jsonResult))
	require.NoError(t, err)

	fileURIResult := callTool(t, session, "medical.profile", map[string]any{"userId": "alex", "format": "file_uri"})
	require.False(t, fileURIResult.IsError, "%v", fileURIResult.Content)
	fileOut := decodeStructured[mcpserver.ProfileFileOutput](t, fileURIResult)
	require.NotEmpty(t, fileOut.FileURI)
	require.True(t, strings.HasPrefix(fileOut.FileURI, testPublicBaseURL+"/links/"), "fileUri %q must be rooted at PublicBaseURL's /links/ route", fileOut.FileURI)
	expiresAt, err := time.Parse(time.RFC3339, fileOut.ExpiresAt)
	require.NoError(t, err, "expiresAt must be RFC3339")
	require.WithinDuration(t, time.Now().Add(5*time.Minute), expiresAt, time.Minute)

	// The link must resolve through the exact handler production mounts —
	// mirrors httpserver.New's "GET /links/{linkId}" route.
	linksMux := http.NewServeMux()
	linksMux.Handle("GET /links/{linkId}", mcpserver.NewLinkDownloadHandler(linkstoreFor(t, store), nil))
	downloadServer := httptest.NewServer(linksMux)
	t.Cleanup(downloadServer.Close)

	relativePath := strings.TrimPrefix(fileOut.FileURI, testPublicBaseURL)
	resp, err := http.Get(downloadServer.URL + relativePath)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t, string(inlineBytes), string(body), "the linked JSON must be exactly what format=json returns inline")
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
}

// TestServer_Profile_InvalidFormatReturnsError guards that an unrecognized
// format value fails loudly (INVALID_FORMAT) rather than silently falling
// back to the default "json" shape.
func TestServer_Profile_InvalidFormatReturnsError(t *testing.T) {
	provider := llmtest.New("fake")
	session := newTestSession(t, provider, []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.profile", map[string]any{"userId": "alex", "format": "xml"})
	require.True(t, result.IsError)
}
