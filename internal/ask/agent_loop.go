package ask

import (
	"context"
	"fmt"
	"strings"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtrace"
)

// Ask runs the agent loop for one question: callerUserID is Miranda's
// authenticated caller (AskInput.UserID, raw — never used for data access,
// only session bookkeeping); subjectID is the resolved data owner (post
// gate.resolveSubject in internal/mcpserver/ask.go — the same value the old
// Ask(ctx, userID, question)'s single parameter silently was). They're kept
// distinct because they can differ (a parent asking about a child) and
// because session bookkeeping needs the real caller id, not the subject.
// sessionID, if non-empty, is Miranda's own session/conversation id — see
// SessionStore for what an empty one does (nothing persists).
func (a *Asker) Ask(ctx context.Context, callerUserID, subjectID, sessionID, question string) (Result, error) {
	// Held for this whole call, not just around individual SessionStore
	// calls — see LockSession's own doc comment for why a narrower scope
	// wouldn't actually prevent two concurrent calls for the same
	// sessionID from interleaving their message inserts.
	defer a.sessions.LockSession(sessionID)()

	if sessionID != "" {
		ctx = llmtrace.WithConversationID(ctx, sessionID)
	}

	// Order matters: LoadHistory must read the session's staleness against
	// its PRIOR last_active_at before EnsureSession bumps that timestamp to
	// now — reversing this order would make every session look freshly
	// active by the time LoadHistory checks it, defeating idle expiry.
	history, err := a.sessions.LoadHistory(ctx, sessionID)
	if err != nil {
		return Result{}, fmt.Errorf("ask: %w", err)
	}
	if err := a.sessions.EnsureSession(ctx, sessionID, callerUserID, subjectID); err != nil {
		return Result{}, fmt.Errorf("ask: %w", err)
	}
	if err := a.sessions.RecordUserMessage(ctx, sessionID, question); err != nil {
		a.logger.Warn("ask: record user message failed", "error", err)
	}

	messages := make([]llm.Message, 0, len(history)+2)
	messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: agentSystemPrompt(a.registry)})
	messages = append(messages, history...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: question})

	tools := toolDefsForRegistry(a.registry)

	var allChunks []KnowledgeChunk
	var pinnedProvider string

	for i := 0; i < a.maxIterations; i++ {
		text, calls, err := a.streamTurn(ctx, messages, tools, &pinnedProvider)
		if err != nil {
			return Result{}, fmt.Errorf("ask: %w", err)
		}

		if len(calls) == 0 {
			if err := a.sessions.RecordAssistantFinalMessage(ctx, sessionID, text); err != nil {
				a.logger.Warn("ask: record final message failed", "error", err)
			}
			// maxChunks == 0 (unbounded) here deliberately: each call's own
			// contribution to allChunks was already capped in
			// executeToolCall, to exactly what the model was shown for
			// that call — re-capping the merged total here could silently
			// drop a chunk the model actually read and cited, which is
			// worse than a longer sources list. This pass only
			// dedupes/sorts across calls.
			return Result{
				Answer:  text,
				Sources: CollectSources(RankChunks(allChunks, a.registry, 0)),
			}, nil
		}

		if err := a.sessions.RecordAssistantToolCallMessage(ctx, sessionID, text, calls); err != nil {
			a.logger.Warn("ask: record assistant tool-call message failed", "error", err)
		}
		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Content: text, ToolCalls: calls})

		for _, tc := range calls {
			chunks, resultText := a.executeToolCall(ctx, subjectID, tc)
			allChunks = append(allChunks, chunks...)
			if err := a.sessions.RecordToolResultMessage(ctx, sessionID, tc.ID, resultText); err != nil {
				a.logger.Warn("ask: record tool result message failed", "error", err)
			}
			messages = append(messages, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, Content: resultText})
		}
	}

	return Result{}, fmt.Errorf("ask: exceeded %d tool-call iterations without a final reply", a.maxIterations)
}

// streamTurn runs one Chat call and collects its full text + any requested
// tool calls. pinnedProvider starts as "" (unpinned — the router's own
// default fallback order applies) and is updated in place once the first
// turn's provider is known, so every later iteration of THIS loop stays
// pinned to it: a tool result must be answered by the model that requested
// the tool, not whichever provider the router would otherwise pick first.
func (a *Asker) streamTurn(ctx context.Context, messages []llm.Message, tools []llm.ToolDef, pinnedProvider *string) (string, []llm.ToolCall, error) {
	stream, err := a.chat.ChatPinned(ctx, llm.ChatRequest{Messages: messages, Tools: tools}, *pinnedProvider, func(name string) {
		*pinnedProvider = name
	})
	if err != nil {
		return "", nil, fmt.Errorf("chat: %w", err)
	}

	var text strings.Builder
	var calls []llm.ToolCall
	for chunk := range stream {
		if chunk.Err != nil {
			return "", nil, fmt.Errorf("chat stream: %w", chunk.Err)
		}
		text.WriteString(chunk.TextDelta)
		if chunk.ToolCall != nil {
			calls = append(calls, *chunk.ToolCall)
		}
	}
	return text.String(), calls, nil
}

// executeToolCall dispatches one model-requested tool call to its
// registered Provider and formats the result as RoleTool message text.
// Errors — an unknown tool name, bad arguments, a Provider.Collect failure
// or timeout — never abort the loop: they become an "error: ..." result
// fed back to the model like any other tool response, so it can react
// (try different parameters, a different tool, or note the gap in its
// final answer) instead of the whole question failing over one bad call.
func (a *Asker) executeToolCall(ctx context.Context, subjectID string, tc llm.ToolCall) ([]KnowledgeChunk, string) {
	provider, ok := a.registry.Get(tc.Name)
	if !ok {
		return nil, fmt.Sprintf("error: unknown tool %q", tc.Name)
	}

	req, err := decodeKnowledgeRequest(tc.Name, tc.Arguments)
	if err != nil {
		return nil, fmt.Sprintf("error: %v", err)
	}
	// UserID is never taken from the model-supplied arguments (no tool
	// schema exposes one — see toolCallArgs) — always the MCP-authenticated
	// subjectID, closing off any prompt-injection path to cross-user data.
	req.UserID = subjectID

	providerCtx, cancel := context.WithTimeout(ctx, a.providerTimeout)
	defer cancel()

	chunks, err := provider.Collect(providerCtx, req)
	if err != nil {
		a.logger.Warn("ask: tool call failed", "tool", tc.Name, "error", err)
		return nil, fmt.Sprintf("error: %v", err)
	}
	// Dedupe and cap THIS call's own chunks before showing them to the
	// model — the modern equivalent of what maxChunks used to bound once,
	// at the very end of the old one-shot pipeline (overall prompt size),
	// now applied per tool call instead. Returning the same (possibly
	// capped) slice back to the caller — which accumulates it into the
	// turn's allChunks for the final `sources` — is what guarantees
	// `sources` can never omit a chunk the model was actually shown (see
	// Ask's own final RankChunks call, which no longer re-caps).
	ranked := RankChunks(chunks, a.registry, a.maxChunks)
	return ranked, formatToolResult(ranked)
}
