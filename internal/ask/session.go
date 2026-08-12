package ask

import (
	"context"
	"fmt"
	"sync"
	"time"

	llm "github.com/archer-developer/miranda-llm"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

const (
	// sessionIdleTTL bounds how long a sessionId can sit unused before
	// LoadHistory treats it as a fresh conversation rather than resuming it
	// — long enough to span a same-day back-and-forth about one health
	// topic, short enough that a session from days ago shouldn't silently
	// resume. The row itself isn't deleted at this point (see
	// sessionGCRetention below) — the same sessionId naturally starts a new
	// conversation once Miranda's own idle timeout has also lapsed and it
	// hands back a fresh id.
	sessionIdleTTL = 6 * time.Hour
	// maxSessionMessages caps how much history LoadHistory replays into a
	// turn's messages — bounds prompt growth for a long-lived session
	// without deleting anything (~20 user/assistant turns, including
	// intermediate tool-call/tool-result pairs).
	maxSessionMessages = 40
	// sessionGCInterval/sessionGCRetention govern the opportunistic cleanup
	// in maybeSweep — no background goroutine, just a cheap check on every
	// EnsureSession call (see that method).
	sessionGCInterval  = time.Hour
	sessionGCRetention = 30 * 24 * time.Hour
)

// SessionStore is the llm.Message translation layer over
// storage.AskSessionRepository, kept in this package (rather than
// internal/storage) so that package stays free of an llm dependency,
// matching this codebase's existing storage/domain separation. Every
// method is a no-op when sessionID == "" — that's what makes a direct,
// non-MCP Ask call with no session (e.g. cmd/medical-dev's `ask` CLI, or a
// test) fully stateless, with zero DB writes, rather than merely "empty
// history." Real MCP calls always have a non-empty sessionID by the time
// they reach here — internal/mcpserver/ask.go's AskInput.SessionID is a
// required field, rejected up front if empty.
type SessionStore struct {
	repo storage.AskSessionRepository
	now  func() time.Time // test seam, defaults to time.Now

	mu        sync.Mutex
	lastSweep time.Time
	locks     map[string]*sync.Mutex // per-sessionID lock, see LockSession
}

// NewSessionStore builds a SessionStore backed by repo.
func NewSessionStore(repo storage.AskSessionRepository) *SessionStore {
	return &SessionStore{repo: repo, now: time.Now}
}

// LockSession acquires sessionID's own dedicated lock and returns a
// function that releases it — call sites should acquire it once, up front,
// for the full duration of one Ask() call (see agent_loop.go's Ask) and
// defer the returned unlock immediately.
//
// Without this, two concurrent Ask() calls sharing the same sessionID (a
// Miranda retry, a double-submit) can interleave their
// LoadHistory/EnsureSession/Record* sequences: storage.go's
// SetMaxOpenConns(1) only serializes individual SQL statements, not the
// whole multi-statement sequence one Ask() call makes, so two overlapping
// calls' message inserts can land in an order that mixes both
// conversations together — e.g. one call's RoleTool result landing between
// the other call's user message and its own tool-call message. Replayed
// back as history on a later turn, that's the same
// orphaned-tool-result-message shape that breaks Gemini's function-name
// resolution (see storage.AskSessionRepository.Messages' own doc comment).
//
// No-op (returns a no-op unlock) when sessionID == "", matching every
// other SessionStore method's no-op-on-empty-sessionID contract — a
// stateless call touches no shared row, so there's nothing to protect.
//
// Locks accumulate one entry per distinct sessionID ever seen and are
// never evicted. Acceptable at this service's household scale (a handful
// of concurrent sessions, each mutex a few dozen bytes) — not worth the
// complexity of tying eviction to maybeSweep's own DeleteInactiveSessions
// for what amounts to a bounded, slow leak in practice.
func (s *SessionStore) LockSession(sessionID string) func() {
	if sessionID == "" {
		return func() {}
	}
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	l, ok := s.locks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[sessionID] = l
	}
	s.mu.Unlock()

	l.Lock()
	return l.Unlock
}

// EnsureSession records sessionID as active (creating it on first use,
// otherwise bumping last_active_at and the stored userID/subjectID
// snapshot — see storage.AskSessionRepository.EnsureSession), then
// opportunistically sweeps old sessions (see maybeSweep). No-op when
// sessionID == "".
func (s *SessionStore) EnsureSession(ctx context.Context, sessionID, userID, subjectID string) error {
	if sessionID == "" {
		return nil
	}
	if err := s.repo.EnsureSession(ctx, sessionID, userID, subjectID); err != nil {
		return fmt.Errorf("ask: ensure session: %w", err)
	}
	s.maybeSweep(ctx)
	return nil
}

// maybeSweep deletes sessions idle longer than sessionGCRetention, at most
// once every sessionGCInterval regardless of request volume — deliberately
// not a background goroutine (nothing to manage the lifecycle of), and
// best-effort: a failed sweep just tries again on the next EnsureSession
// call rather than failing the caller's Ask.
func (s *SessionStore) maybeSweep(ctx context.Context) {
	s.mu.Lock()
	due := s.now().Sub(s.lastSweep) > sessionGCInterval
	if due {
		s.lastSweep = s.now()
	}
	s.mu.Unlock()
	if !due {
		return
	}
	_, _ = s.repo.DeleteInactiveSessions(ctx, s.now().Add(-sessionGCRetention))
}

// LoadHistory returns sessionID's prior turns as []llm.Message, ready to
// prepend to a new turn — nil (no error) for sessionID == "", an unknown
// session, or one idle longer than sessionIdleTTL, all of which mean "start
// fresh" rather than an error.
func (s *SessionStore) LoadHistory(ctx context.Context, sessionID string) ([]llm.Message, error) {
	if sessionID == "" {
		return nil, nil
	}
	lastActive, found, err := s.repo.SessionActiveSince(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ask: session active since: %w", err)
	}
	if !found || s.now().Sub(lastActive) > sessionIdleTTL {
		return nil, nil
	}
	rows, err := s.repo.Messages(ctx, sessionID, maxSessionMessages)
	if err != nil {
		return nil, fmt.Errorf("ask: load session history: %w", err)
	}
	return toLLMMessages(rows), nil
}

// RecordUserMessage persists the turn's incoming question.
func (s *SessionStore) RecordUserMessage(ctx context.Context, sessionID, content string) error {
	if sessionID == "" {
		return nil
	}
	return s.appendMessage(ctx, sessionID, storage.AskMessage{Role: string(llm.RoleUser), Content: content})
}

// RecordAssistantToolCallMessage persists an assistant turn that requested
// one or more tool calls — calls are stored on this message itself (not
// only on the resulting tool-result rows), which is what lets LoadHistory
// replay the exact assistant/tool message pair on a later turn, including
// any ProviderMetadata a provider needs echoed back verbatim.
func (s *SessionStore) RecordAssistantToolCallMessage(ctx context.Context, sessionID, content string, calls []llm.ToolCall) error {
	if sessionID == "" {
		return nil
	}
	return s.appendMessage(ctx, sessionID, storage.AskMessage{
		Role: string(llm.RoleAssistant), Content: content, ToolCalls: toStorageToolCalls(calls),
	})
}

// RecordToolResultMessage persists one tool call's result.
func (s *SessionStore) RecordToolResultMessage(ctx context.Context, sessionID, toolCallID, content string) error {
	if sessionID == "" {
		return nil
	}
	return s.appendMessage(ctx, sessionID, storage.AskMessage{
		Role: string(llm.RoleTool), ToolCallID: toolCallID, Content: content,
	})
}

// RecordAssistantFinalMessage persists the turn's final, no-further-tool-call answer.
func (s *SessionStore) RecordAssistantFinalMessage(ctx context.Context, sessionID, content string) error {
	if sessionID == "" {
		return nil
	}
	return s.appendMessage(ctx, sessionID, storage.AskMessage{Role: string(llm.RoleAssistant), Content: content})
}

func (s *SessionStore) appendMessage(ctx context.Context, sessionID string, m storage.AskMessage) error {
	if err := s.repo.AppendMessage(ctx, sessionID, m); err != nil {
		return fmt.Errorf("ask: append session message: %w", err)
	}
	return nil
}

func toLLMMessages(rows []storage.AskMessage) []llm.Message {
	msgs := make([]llm.Message, len(rows))
	for i, r := range rows {
		m := llm.Message{Role: llm.Role(r.Role), Content: r.Content, ToolCallID: r.ToolCallID}
		if len(r.ToolCalls) > 0 {
			m.ToolCalls = make([]llm.ToolCall, len(r.ToolCalls))
			for j, tc := range r.ToolCalls {
				m.ToolCalls[j] = llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments, ProviderMetadata: tc.ProviderMetadata}
			}
		}
		msgs[i] = m
	}
	return msgs
}

func toStorageToolCalls(calls []llm.ToolCall) []storage.AskToolCallRef {
	refs := make([]storage.AskToolCallRef, len(calls))
	for i, c := range calls {
		refs[i] = storage.AskToolCallRef{ID: c.ID, Name: c.Name, Arguments: c.Arguments, ProviderMetadata: c.ProviderMetadata}
	}
	return refs
}
