package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
