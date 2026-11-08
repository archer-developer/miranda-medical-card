package ask

import (
	"context"
	"sync"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
)

// engine is the swappable "brain" of one Ask call: given everything Ask has
// already prepared (the system prompt, prior session history, the question,
// the Knowledge Provider tools), it decides which tools to call — executing
// each one through Asker.callTool — and produces the final answer text.
//
// Everything outside the engine is shared, deliberately: the per-session
// lock, the anomaly Recorder, loading/recording session history, the system
// prompt and tool set, and turning the collected chunks into `sources` all
// live in Ask, so switching agent.engine in config changes only how a
// question is reasoned about, never what is persisted or returned. Mirrors
// miranda's own turnEngine (miranda/docs/adr/claude-code-orchestrator.md);
// see docs/adr/010-claude-code-engine.md.
type engine interface {
	// run returns the final answer. It must record every tool call to the
	// session itself — the assistant tool-call message via
	// SessionStore.RecordAssistantToolCallMessage, the result via
	// Asker.callTool — since only the engine sees them; Ask records just the
	// user message and the final answer. It also fills t.outcome for anomaly
	// detection.
	run(ctx context.Context, t *askTurn) (string, error)
	// close releases whatever the engine holds (listeners) at shutdown.
	close()
}

// askTurn is one Ask call's input to an engine, plus the state engines
// accumulate into it.
type askTurn struct {
	subjectID string
	sessionID string
	// system is agentSystemPrompt's output for this call.
	system string
	// history is the session's conversation so far (SessionStore.LoadHistory),
	// without the system prompt and without this call's question.
	history  []llm.Message
	question string
	tools    []llm.ToolDef
	outcome  *anomaly.Outcome

	mu sync.Mutex
	// chunks accumulates every chunk any tool call showed the model — what
	// Ask turns into the answer's `sources` (see callTool).
	chunks []KnowledgeChunk
}

// callTool executes one model-requested tool call (executeToolCall), records
// its result to the session and remembers its chunks for `sources`. Safe for
// concurrent use, though the claude_code engine still serializes calls so a
// session's history keeps the call → result order.
func (a *Asker) callTool(ctx context.Context, t *askTurn, tc llm.ToolCall) string {
	chunks, resultText := a.executeToolCall(ctx, t.subjectID, tc)
	t.mu.Lock()
	t.chunks = append(t.chunks, chunks...)
	t.mu.Unlock()
	if err := a.sessions.RecordToolResultMessage(ctx, t.sessionID, tc.ID, resultText); err != nil {
		a.logger.Warn("ask: record tool result message failed", "error", err)
	}
	return resultText
}

// Close releases the active engine's resources (for the claude_code engine:
// its loopback MCP listener). Call once at shutdown; a no-op for the default
// engine.
func (a *Asker) Close() {
	a.engine.close()
}
