package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// AskToolCallRef is one tool call an assistant turn requested, stored as
// part of an AskMessage — mirrors miranda's own history.ToolCallRef.
// ProviderMetadata is opaque per-provider replay data (e.g. Gemini's
// "thought signature") that must be echoed back verbatim on a later turn;
// see llm.ToolCall's own doc comment in miranda-llm for why it exists.
type AskToolCallRef struct {
	ID               string
	Name             string
	Arguments        string
	ProviderMetadata string
}

// AskMessage is one turn in a medical.ask agent-loop conversation. Role is
// "user", "assistant", or "tool" (mirrors llm.Role). ToolCallID is set only
// on Role=="tool" (which tool call this answers); ToolCalls is set only on
// an assistant turn that requested one or more tool calls.
type AskMessage struct {
	ID         int64
	SessionID  string
	Role       string
	Content    string
	ToolCallID string
	ToolCalls  []AskToolCallRef
	CreatedAt  time.Time
}

// askRoleTool and askRoleAssistant are AskMessage.Role's values for a
// tool-result message and an assistant turn respectively — kept as
// unexported constants rather than importing miranda-llm here (this package
// stays free of an llm dependency, see AskSessionRepository's own doc
// comment); the strings themselves are dictated by internal/ask/session.go's
// toLLMMessages, which does string(llm.RoleTool) == "tool" and
// string(llm.RoleAssistant) == "assistant".
const (
	askRoleTool      = "tool"
	askRoleAssistant = "assistant"
)

// AskSessionRepository persists medical.ask agent-loop conversation state —
// see internal/ask.SessionStore for the llm.Message translation layer built
// on top of this (kept separate so this package stays free of an llm
// dependency, matching the rest of this package's conventions).
type AskSessionRepository interface {
	// EnsureSession inserts sessionID if absent, or bumps last_active_at and
	// overwrites userID/subjectID with the latest call's values if already
	// present (informational "most recent" snapshot, not accumulated — see
	// ask_sessions' own doc comment in storage.go's schema).
	EnsureSession(ctx context.Context, sessionID, userID, subjectID string) error
	AppendMessage(ctx context.Context, sessionID string, m AskMessage) error
	// Messages returns sessionID's most recent limit messages (0 means no
	// limit), in chronological order — always a structurally valid window,
	// safe to replay as-is: see the doc comment on the
	// trimOrphanedLeadingToolMessages helper this delegates to for what
	// "valid" rules out.
	Messages(ctx context.Context, sessionID string, limit int) ([]AskMessage, error)
	// SessionActiveSince returns sessionID's last_active_at, or found=false
	// if no such session exists.
	SessionActiveSince(ctx context.Context, sessionID string) (lastActive time.Time, found bool, err error)
	// DeleteInactiveSessions removes every session whose last_active_at is
	// older than olderThan, cascading to its messages (ask_messages'
	// ON DELETE CASCADE), and returns how many sessions were removed.
	DeleteInactiveSessions(ctx context.Context, olderThan time.Time) (int64, error)
}

type sqliteAskSessionRepository struct {
	db *sql.DB
}

// NewAskSessionRepository builds an AskSessionRepository backed by s.
func NewAskSessionRepository(s *Store) AskSessionRepository {
	return &sqliteAskSessionRepository{db: s.db}
}

func (r *sqliteAskSessionRepository) EnsureSession(ctx context.Context, sessionID, userID, subjectID string) error {
	now := time.Now().Unix()
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO ask_sessions (id, user_id, subject_id, created_at, last_active_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			user_id = excluded.user_id,
			subject_id = excluded.subject_id,
			last_active_at = excluded.last_active_at`,
		sessionID, userID, nullString(subjectID), now, now,
	)
	if err != nil {
		return fmt.Errorf("storage: ensure ask session: %w", err)
	}
	return nil
}

func (r *sqliteAskSessionRepository) AppendMessage(ctx context.Context, sessionID string, m AskMessage) error {
	toolCallsJSON := ""
	if len(m.ToolCalls) > 0 {
		b, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return fmt.Errorf("storage: marshal ask message tool calls: %w", err)
		}
		toolCallsJSON = string(b)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO ask_messages (session_id, role, content, tool_call_id, tool_calls_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, m.Role, m.Content, m.ToolCallID, toolCallsJSON, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("storage: append ask message: %w", err)
	}
	return nil
}

func (r *sqliteAskSessionRepository) Messages(ctx context.Context, sessionID string, limit int) ([]AskMessage, error) {
	query := `
		SELECT id, session_id, role, content, tool_call_id, tool_calls_json, created_at
		FROM (
			SELECT id, session_id, role, content, tool_call_id, tool_calls_json, created_at
			FROM ask_messages
			WHERE session_id = ?
			ORDER BY id DESC`
	args := []any{sessionID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	query += `
		) ORDER BY id ASC`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list ask messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []AskMessage
	for rows.Next() {
		var m AskMessage
		var toolCallsJSON string
		var createdAt int64
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.ToolCallID, &toolCallsJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("storage: scan ask message: %w", err)
		}
		if toolCallsJSON != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("storage: unmarshal ask message tool calls: %w", err)
			}
		}
		m.CreatedAt = time.Unix(createdAt, 0).UTC()
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate ask messages: %w", err)
	}
	return trimOrphanedLeadingToolMessages(result), nil
}

// trimOrphanedLeadingToolMessages drops any leading run of role=="tool"
// messages, and any leading assistant tool-call ("function call") turn that
// has nothing valid before it, from msgs.
//
// Messages' own limit truncates to the most recent N rows by id with no
// awareness of assistant-tool-call/tool-result group boundaries — if the
// cut lands inside such a group, the window can start with a tool-result
// message whose originating assistant tool-call message was in the dropped
// older rows. Replayed back into []llm.Message (see internal/ask/session.go's
// toLLMMessages) and on to a real provider, that orphaned entry has no name
// to resolve against: miranda-llm/gemini's toGeminiContents recovers a
// RoleTool message's function name only by walking earlier assistant
// messages in the SAME slice (toolNames[m.ToolCallID]), so an orphan
// resolves to an empty name and the provider's API is very likely to reject
// the whole turn outright — silently breaking every future turn of that
// session.
//
// The same cut can just as easily land one row later, leaving an assistant
// tool-call message itself (role=="assistant" with ToolCalls set) as the
// window's first entry — a *complete*, well-formed call/result pair, but
// still invalid as the very first thing replayed: Gemini requires a
// function-call turn be immediately preceded by a user or function-response
// turn (see the "function call turn comes immediately after a user turn or
// after a function response turn" error this produced in production,
// 2026-08-14 — a session that ballooned past maxSessionMessages from a burst
// of retried tool calls got wedged permanently this way, for every future
// turn of that session, not just the one that tripped it). The window is
// prepended after the system prompt, which Gemini doesn't count as a valid
// predecessor, so a leading tool-call turn has no legal predecessor at all
// once older rows are trimmed away — it must be dropped, together with the
// tool-result messages answering it (which the loop below then finds newly
// leading, and drops in turn as plain orphans).
//
// Any role=="tool" message at the very front of a chronologically-ordered
// window is, by construction, always an orphan: its owning assistant
// message, if it were present in the window at all, would necessarily sort
// before it (lower id). So both checks are unconditional and don't need to
// inspect ToolCallIDs at all. This can never remove a genuinely safe
// opener — a plain user message or a tool-call-free assistant message stops
// the loop immediately, since neither check matches it.
func trimOrphanedLeadingToolMessages(msgs []AskMessage) []AskMessage {
	for len(msgs) > 0 {
		if msgs[0].Role == askRoleTool {
			msgs = msgs[1:]
			continue
		}
		if msgs[0].Role == askRoleAssistant && len(msgs[0].ToolCalls) > 0 {
			msgs = msgs[1:]
			continue
		}
		break
	}
	return msgs
}

func (r *sqliteAskSessionRepository) SessionActiveSince(ctx context.Context, sessionID string) (time.Time, bool, error) {
	var lastActive int64
	err := r.db.QueryRowContext(ctx, `SELECT last_active_at FROM ask_sessions WHERE id = ?`, sessionID).Scan(&lastActive)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("storage: ask session active since: %w", err)
	}
	return time.Unix(lastActive, 0).UTC(), true, nil
}

func (r *sqliteAskSessionRepository) DeleteInactiveSessions(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM ask_sessions WHERE last_active_at < ?`, olderThan.Unix())
	if err != nil {
		return 0, fmt.Errorf("storage: delete inactive ask sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("storage: delete inactive ask sessions: rows affected: %w", err)
	}
	return n, nil
}

// nullString converts an empty string to SQL NULL — subject_id has no
// NOT NULL constraint since it's genuinely optional (AskInput.SubjectID is
// itself optional).
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
