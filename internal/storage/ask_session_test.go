package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestAskSessionRepository_EnsureSession_InsertsThenUpserts(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAskSessionRepository(newTestStore(t))

	require.NoError(t, repo.EnsureSession(ctx, "sess1", "alex", "kid"))
	_, found, err := repo.SessionActiveSince(ctx, "sess1")
	require.NoError(t, err)
	require.True(t, found)

	// A second call for the same id must upsert, not fail on the PRIMARY
	// KEY conflict — the "most recent wins" contract for user_id/subject_id
	// (see ask_sessions' schema doc comment in storage.go) with a different
	// subjectID than the first call.
	require.NoError(t, repo.EnsureSession(ctx, "sess1", "alex", ""))
	_, found, err = repo.SessionActiveSince(ctx, "sess1")
	require.NoError(t, err)
	require.True(t, found)
}

func TestAskSessionRepository_SessionActiveSince_UnknownSessionNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAskSessionRepository(newTestStore(t))

	_, found, err := repo.SessionActiveSince(ctx, "does-not-exist")
	require.NoError(t, err)
	require.False(t, found)
}

func TestAskSessionRepository_AppendMessage_MessagesReturnsChronologicalOrder(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAskSessionRepository(newTestStore(t))
	require.NoError(t, repo.EnsureSession(ctx, "sess1", "alex", ""))

	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{Role: "user", Content: "first"}))
	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{
		Role: "assistant", Content: "calling a tool",
		ToolCalls: []storage.AskToolCallRef{{ID: "call_1", Name: "lab_results", Arguments: `{"indicatorName":"ALT"}`, ProviderMetadata: "opaque-blob"}},
	}))
	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{Role: "tool", ToolCallID: "call_1", Content: "ALT 28.3"}))
	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{Role: "assistant", Content: "final answer"}))

	messages, err := repo.Messages(ctx, "sess1", 0)
	require.NoError(t, err)
	require.Len(t, messages, 4)
	require.Equal(t, "first", messages[0].Content)
	require.Equal(t, "assistant", messages[1].Role)
	require.Len(t, messages[1].ToolCalls, 1)
	require.Equal(t, "call_1", messages[1].ToolCalls[0].ID)
	require.Equal(t, "lab_results", messages[1].ToolCalls[0].Name)
	require.Equal(t, "opaque-blob", messages[1].ToolCalls[0].ProviderMetadata)
	require.Equal(t, "tool", messages[2].Role)
	require.Equal(t, "call_1", messages[2].ToolCallID)
	require.Equal(t, "final answer", messages[3].Content)
}

func TestAskSessionRepository_Messages_RespectsLimitAndKeepsChronologicalOrder(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAskSessionRepository(newTestStore(t))
	require.NoError(t, repo.EnsureSession(ctx, "sess1", "alex", ""))

	for _, content := range []string{"a", "b", "c", "d"} {
		require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{Role: "user", Content: content}))
	}

	messages, err := repo.Messages(ctx, "sess1", 2)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	// The most recent 2 (c, d), still returned oldest-first.
	require.Equal(t, "c", messages[0].Content)
	require.Equal(t, "d", messages[1].Content)
}

// TestAskSessionRepository_Messages_TruncationNeverOrphansAToolMessage
// guards against a real failure mode found in review: a naive "last N rows"
// cut can land inside an assistant-tool-call/tool-result group, returning a
// window that starts with a role="tool" message whose owning assistant
// message was in the dropped older rows. Replayed into an LLM provider
// (e.g. Gemini, which resolves a tool result's function name only by
// walking earlier assistant messages in the same slice), an orphaned entry
// like that gets rejected outright — breaking every future turn of the
// session.
//
// Five rows total (ids 1-5): user q1, assistant+call_1, tool(call_1) r1,
// assistant+call_2, tool(call_2) r2.
func TestAskSessionRepository_Messages_TruncationNeverOrphansAToolMessage(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAskSessionRepository(newTestStore(t))
	require.NoError(t, repo.EnsureSession(ctx, "sess1", "alex", ""))

	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{Role: "user", Content: "q1"}))
	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{
		Role: "assistant", Content: "", ToolCalls: []storage.AskToolCallRef{{ID: "call_1", Name: "timeline"}},
	}))
	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{Role: "tool", ToolCallID: "call_1", Content: "r1"}))
	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{
		Role: "assistant", Content: "", ToolCalls: []storage.AskToolCallRef{{ID: "call_2", Name: "lab_results"}},
	}))
	require.NoError(t, repo.AppendMessage(ctx, "sess1", storage.AskMessage{Role: "tool", ToolCallID: "call_2", Content: "r2"}))

	// limit=2: naive cut is rows 4-5 (assistant+call_2, tool(call_2)) — a
	// complete, well-formed pair, nothing to trim.
	messages, err := repo.Messages(ctx, "sess1", 2)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "assistant", messages[0].Role)
	require.Equal(t, "tool", messages[1].Role)
	require.Equal(t, "call_2", messages[1].ToolCallID)

	// limit=1: naive cut is just row 5, tool(call_2) alone — orphaned (its
	// assistant call isn't in the window) — must trim to empty, not return
	// a 1-message window starting with an orphan.
	messages, err = repo.Messages(ctx, "sess1", 1)
	require.NoError(t, err)
	require.Empty(t, messages)

	// limit=3: naive cut is rows 3-5 (tool(call_1), assistant+call_2,
	// tool(call_2)) — row 3 is an orphan (call_1's assistant message is
	// row 2, outside this window) and must be trimmed, leaving only the
	// complete trailing pair.
	messages, err = repo.Messages(ctx, "sess1", 3)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "assistant", messages[0].Role)
	require.Equal(t, "call_2", messages[0].ToolCalls[0].ID)
	require.Equal(t, "tool", messages[1].Role)
	require.Equal(t, "call_2", messages[1].ToolCallID)
}

func TestAskSessionRepository_DeleteInactiveSessions_CascadesToMessages(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	repo := storage.NewAskSessionRepository(store)

	require.NoError(t, repo.EnsureSession(ctx, "old", "alex", ""))
	require.NoError(t, repo.AppendMessage(ctx, "old", storage.AskMessage{Role: "user", Content: "stale"}))
	require.NoError(t, repo.EnsureSession(ctx, "fresh", "alex", ""))
	require.NoError(t, repo.AppendMessage(ctx, "fresh", storage.AskMessage{Role: "user", Content: "keep"}))

	n, err := repo.DeleteInactiveSessions(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "both sessions are older than now+1h")

	_, found, err := repo.SessionActiveSince(ctx, "old")
	require.NoError(t, err)
	require.False(t, found)

	messages, err := repo.Messages(ctx, "old", 0)
	require.NoError(t, err)
	require.Empty(t, messages, "ask_messages must cascade-delete via ON DELETE CASCADE")
}
