package mcpserver_test

import (
	"context"
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
	"github.com/archer-developer/miranda-medical-card/internal/linkstore"
	"github.com/archer-developer/miranda-medical-card/internal/mcpserver"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// newAskTestSession mirrors newTestSession (server_test.go) but hands back
// the *llmtest.FakeProvider backing the agent loop too — newTestSession's
// wiring is opaque by design, but these tests need to inspect exactly what
// each Chat call's request looked like to prove sessionId round-trips into
// Asker.Ask and its conversation history actually persists/replays across
// separate medical.ask calls over the real MCP wire protocol.
func newAskTestSession(t *testing.T, fake *llmtest.FakeProvider, users []config.UserConfig) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	fs, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	pl := pipeline.New(fake, nil, fake, nil, llmtest.NewFakeEmbedder([]float32{0.1, 0.2}), "fake", "fake-model", fs, s, nil, nil)
	registry := ask.NewRegistry(ask.NewTimelineProvider(storage.NewTimelineRepository(s)))
	askRouter, err := router.New([]llm.Provider{fake}, nil, "fake")
	require.NoError(t, err)
	asker := ask.NewAsker(askRouter, registry, ask.NewSessionStore(storage.NewAskSessionRepository(s)), 5*time.Second, 20, 8, nil)

	links := linkstore.New(storage.NewEphemeralLinkRepository(s))
	server := mcpserver.New(pl, asker, users, 50*1024*1024, testPublicBaseURL, links, nil)
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

type askOutput struct {
	Answer  string `json:"answer"`
	Sources []struct {
		DocumentID string `json:"documentId,omitempty"`
		EventID    string `json:"eventId,omitempty"`
		Title      string `json:"title,omitempty"`
		Excerpt    string `json:"excerpt,omitempty"`
	} `json:"sources"`
}

func TestAskHandler_OutputShapeUnchanged(t *testing.T) {
	fake := llmtest.New("fake", llmtest.Response{Text: "Ответ."})
	session := newAskTestSession(t, fake, []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.ask", map[string]any{"userId": "alex", "question": "Вопрос?", "sessionId": "sess1"})
	require.False(t, result.IsError, "%v", result.Content)
	out := decodeStructured[askOutput](t, result)
	require.Equal(t, "Ответ.", out.Answer)
	require.Empty(t, out.Sources)
}

// TestAskHandler_SessionIdOmittedRejectedBySchema proves sessionId is a
// required MCP argument now (internal/mcpserver/ask.go's AskInput.SessionID
// has no omitempty) — a call that leaves it out entirely never reaches
// askHandler, it's rejected by the go-sdk's own schema validation
// (applySchema, see mcp/server.go) before the handler runs at all. This is
// deliberate: Miranda's own backend now unconditionally injects its
// resolved conversation id into every medical.ask call (see
// miranda/docs/medical-card-session-injection.md) before the request ever
// reaches this service, so an omitted sessionId on a real call means
// Miranda's own injection config for this tool has drifted — that should
// fail loudly, not silently fall back to a stateless answer.
func TestAskHandler_SessionIdOmittedRejectedBySchema(t *testing.T) {
	session := newAskTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.ask", map[string]any{"userId": "alex", "question": "Вопрос?"})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "sessionId")
}

// TestAskHandler_SessionIdEmptyRejected covers the gap schema "required"
// alone doesn't: required only guarantees the property is present, not
// non-empty — an explicit "" would satisfy the schema but is exactly as
// useless as an omitted one, so askHandler has its own explicit check.
func TestAskHandler_SessionIdEmptyRejected(t *testing.T) {
	session := newAskTestSession(t, llmtest.New("fake"), []config.UserConfig{{ID: "alex"}})

	result := callTool(t, session, "medical.ask", map[string]any{"userId": "alex", "question": "Вопрос?", "sessionId": ""})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*mcp.TextContent).Text, "INVALID_EVENT")
}

// TestAskHandler_SessionIdPersistsAndReplaysHistory proves the opposite:
// passing the same sessionId across two medical.ask calls carries the first
// call's question and answer into the second call's request — end-to-end
// through the real MCP JSON-RPC transport, not just a direct Go call into
// Asker.Ask (see internal/ask/agent_loop_test.go's
// TestAsk_SessionContinuity_ReplaysHistoryIntoSecondCall for that narrower
// check).
func TestAskHandler_SessionIdPersistsAndReplaysHistory(t *testing.T) {
	fake := llmtest.New("fake",
		llmtest.Response{Text: "Первый ответ."},
		llmtest.Response{Text: "Второй ответ с учётом контекста."},
	)
	session := newAskTestSession(t, fake, []config.UserConfig{{ID: "alex"}})

	result1 := callTool(t, session, "medical.ask", map[string]any{
		"userId": "alex", "question": "Первый вопрос", "sessionId": "sess1",
	})
	require.False(t, result1.IsError, "%v", result1.Content)

	result2 := callTool(t, session, "medical.ask", map[string]any{
		"userId": "alex", "question": "Второй вопрос", "sessionId": "sess1",
	})
	require.False(t, result2.IsError, "%v", result2.Content)
	out2 := decodeStructured[askOutput](t, result2)
	require.Equal(t, "Второй ответ с учётом контекста.", out2.Answer)

	require.Len(t, fake.Requests, 2)
	// [0]=system, [1]=first user, [2]=first assistant, [3]=second user
	msgs := fake.Requests[1].Messages
	require.Len(t, msgs, 4)
	require.Equal(t, "Первый вопрос", msgs[1].Content)
	require.Equal(t, "Первый ответ.", msgs[2].Content)
	require.Equal(t, "Второй вопрос", msgs[3].Content)
}
