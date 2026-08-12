package ask

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// This file is package ask (white-box), unlike the rest of this package's
// tests (package ask_test) — SessionStore's idle-expiry test needs direct
// access to its unexported now clock seam, which an external test package
// can't reach.

func newSessionTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionStore_EmptySessionIDIsFullNoOp(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(panicRepo{})

	require.NoError(t, store.EnsureSession(ctx, "", "alex", ""))
	require.NoError(t, store.RecordUserMessage(ctx, "", "question"))
	require.NoError(t, store.RecordAssistantToolCallMessage(ctx, "", "text", []llm.ToolCall{{ID: "1", Name: "timeline"}}))
	require.NoError(t, store.RecordToolResultMessage(ctx, "", "1", "result"))
	require.NoError(t, store.RecordAssistantFinalMessage(ctx, "", "final"))

	history, err := store.LoadHistory(ctx, "")
	require.NoError(t, err)
	require.Nil(t, history)
}

// panicRepo implements storage.AskSessionRepository by panicking on every
// call — used to prove SessionStore's sessionID == "" branches return
// before ever reaching the repository, not just that they happen to write
// harmless empty-key rows.
type panicRepo struct{}

func (panicRepo) EnsureSession(context.Context, string, string, string) error {
	panic("must not be called for an empty sessionID")
}
func (panicRepo) AppendMessage(context.Context, string, storage.AskMessage) error {
	panic("must not be called for an empty sessionID")
}
func (panicRepo) Messages(context.Context, string, int) ([]storage.AskMessage, error) {
	panic("must not be called for an empty sessionID")
}
func (panicRepo) SessionActiveSince(context.Context, string) (time.Time, bool, error) {
	panic("must not be called for an empty sessionID")
}
func (panicRepo) DeleteInactiveSessions(context.Context, time.Time) (int64, error) {
	panic("must not be called for an empty sessionID")
}

func TestSessionStore_RoundTripPreservesExactMessageShape(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(storage.NewAskSessionRepository(newSessionTestStore(t)))

	require.NoError(t, store.EnsureSession(ctx, "sess1", "alex", "kid"))
	require.NoError(t, store.RecordUserMessage(ctx, "sess1", "Когда впервые повысился ALT?"))
	require.NoError(t, store.RecordAssistantToolCallMessage(ctx, "sess1", "", []llm.ToolCall{
		{ID: "call_1", Name: "lab_results", Arguments: `{"indicatorName":"ALT"}`, ProviderMetadata: "thought-sig"},
	}))
	require.NoError(t, store.RecordToolResultMessage(ctx, "sess1", "call_1", "- ALT 54.7 (2025-03-14)"))
	require.NoError(t, store.RecordAssistantFinalMessage(ctx, "sess1", "Впервые повышен 14 марта 2025."))

	history, err := store.LoadHistory(ctx, "sess1")
	require.NoError(t, err)
	require.Len(t, history, 4)

	require.Equal(t, llm.RoleUser, history[0].Role)
	require.Equal(t, "Когда впервые повысился ALT?", history[0].Content)

	require.Equal(t, llm.RoleAssistant, history[1].Role)
	require.Len(t, history[1].ToolCalls, 1)
	require.Equal(t, "call_1", history[1].ToolCalls[0].ID)
	require.Equal(t, "lab_results", history[1].ToolCalls[0].Name)
	require.Equal(t, `{"indicatorName":"ALT"}`, history[1].ToolCalls[0].Arguments)
	require.Equal(t, "thought-sig", history[1].ToolCalls[0].ProviderMetadata)

	require.Equal(t, llm.RoleTool, history[2].Role)
	require.Equal(t, "call_1", history[2].ToolCallID)
	require.Equal(t, "- ALT 54.7 (2025-03-14)", history[2].Content)

	require.Equal(t, llm.RoleAssistant, history[3].Role)
	require.Equal(t, "Впервые повышен 14 марта 2025.", history[3].Content)
	require.Empty(t, history[3].ToolCalls)
}

func TestSessionStore_MessageCountCapTrimsToMostRecent(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(storage.NewAskSessionRepository(newSessionTestStore(t)))
	require.NoError(t, store.EnsureSession(ctx, "sess1", "alex", ""))

	total := maxSessionMessages + 5
	for i := 0; i < total; i++ {
		require.NoError(t, store.RecordUserMessage(ctx, "sess1", string(rune('a'+(i%26)))+string(rune(i))))
	}

	history, err := store.LoadHistory(ctx, "sess1")
	require.NoError(t, err)
	require.Len(t, history, maxSessionMessages, "must cap to maxSessionMessages, not return every stored message")
}

func TestSessionStore_IdleExpiry_EmptiesHistoryForStaleSession(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewAskSessionRepository(newSessionTestStore(t))
	// Anchored to real wall-clock time, not a fixed date: the repository
	// itself stamps last_active_at with the real time.Now() (see
	// storage/ask_session.go's EnsureSession — it has no clock seam of its
	// own), so this test's fake `now` must start near the real "now" for
	// the "well within TTL" assertion below to hold, then advance forward
	// from there for the "past TTL" assertion.
	now := time.Now()
	store := &SessionStore{repo: repo, now: func() time.Time { return now }}

	require.NoError(t, store.EnsureSession(ctx, "sess1", "alex", ""))
	require.NoError(t, store.RecordUserMessage(ctx, "sess1", "hello"))

	history, err := store.LoadHistory(ctx, "sess1")
	require.NoError(t, err)
	require.Len(t, history, 1, "well within sessionIdleTTL, history must replay normally")

	now = now.Add(sessionIdleTTL + time.Hour)
	history, err = store.LoadHistory(ctx, "sess1")
	require.NoError(t, err)
	require.Empty(t, history, "a session idle longer than sessionIdleTTL must start fresh, not replay stale history")
}

// TestSessionStore_LockSession_SerializesSameSessionID guards against a
// review finding: without this, two concurrent Ask() calls sharing a
// sessionID could interleave their LoadHistory/EnsureSession/Record*
// sequences and corrupt message ordering (see
// storage/ask_session.go's Messages doc comment for the resulting
// orphaned-tool-message failure mode). A second LockSession call for the
// SAME sessionID must block until the first is released.
func TestSessionStore_LockSession_SerializesSameSessionID(t *testing.T) {
	store := NewSessionStore(nil) // no DB calls made — locking is pure in-memory

	unlock1 := store.LockSession("sess1")

	acquired := make(chan struct{})
	go func() {
		unlock2 := store.LockSession("sess1")
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("second LockSession for the same sessionID must not acquire while the first is still held")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	unlock1()

	select {
	case <-acquired:
		// expected: unblocked once the first lock was released
	case <-time.After(time.Second):
		t.Fatal("second LockSession never acquired after the first was released")
	}
}

// TestSessionStore_LockSession_DifferentSessionsDontBlockEachOther ensures
// the lock is scoped per sessionID, not a single global lock that would
// needlessly serialize unrelated households' concurrent conversations.
func TestSessionStore_LockSession_DifferentSessionsDontBlockEachOther(t *testing.T) {
	store := NewSessionStore(nil)

	unlock1 := store.LockSession("sess1")
	defer unlock1()

	acquired := make(chan struct{})
	go func() {
		unlock2 := store.LockSession("sess2")
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		// expected: a different sessionID's lock is independent
	case <-time.After(time.Second):
		t.Fatal("LockSession for a different sessionID must not be blocked by sess1's lock")
	}
}

// TestSessionStore_LockSession_EmptySessionIDIsNoOp matches every other
// SessionStore method's no-op-on-empty-sessionID contract — a stateless
// call touches no shared row, so there's nothing to serialize, and this
// must never block regardless of what other locks are held.
func TestSessionStore_LockSession_EmptySessionIDIsNoOp(t *testing.T) {
	store := NewSessionStore(nil)

	done := make(chan struct{})
	go func() {
		unlock := store.LockSession("")
		unlock()
		close(done)
	}()

	select {
	case <-done:
		// expected: immediate no-op
	case <-time.After(time.Second):
		t.Fatal("LockSession(\"\") must never block")
	}
}

func TestSessionStore_LoadHistory_UnknownSessionReturnsEmptyNotError(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(storage.NewAskSessionRepository(newSessionTestStore(t)))

	history, err := store.LoadHistory(ctx, "never-seen-before")
	require.NoError(t, err)
	require.Empty(t, history)
}
