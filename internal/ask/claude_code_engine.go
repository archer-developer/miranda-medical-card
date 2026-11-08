package ask

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/claudecode"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

// claudeCodeProviderName tags a claude_code Ask call's llm.log blocks.
const claudeCodeProviderName = "claude_code"

// medicalMCPServerName is the MCP server name each Claude Code process sees
// the Knowledge Providers under — the CLI exposes them to the model as
// "mcp__medical__<tool>".
const medicalMCPServerName = "medical"

// claudeCodeDefaultTimeout bounds one Ask call when ClaudeCodeConfig.Timeout
// is unset.
const claudeCodeDefaultTimeout = 5 * time.Minute

// claudeCodeToolNote is appended to the system prompt for this engine only:
// the prompt names tools by their registry names, the CLI shows them
// namespaced.
const claudeCodeToolNote = "\n\nThe knowledge-source tools above are available to you as MCP tools named mcp__" + medicalMCPServerName + "__<name> (e.g. mcp__" + medicalMCPServerName + "__timeline). They are your only tools."

// transcriptToolResultMax bounds one replayed tool result in the transcript
// a fresh process gets of an earlier session — enough for the model to know
// what came back without re-sending whole documents.
const transcriptToolResultMax = 1000

// ClaudeCodeConfig configures the claude_code engine (agent.engine:
// claude_code — see config.AgentClaudeCodeConfig, which main.go maps here).
type ClaudeCodeConfig struct {
	Binary   string
	Model    string
	Effort   string
	MaxTurns int
	// Timeout is the hard ceiling for one Ask call, process start included.
	// <= 0 uses claudeCodeDefaultTimeout.
	Timeout        time.Duration
	AllowAPIKeyEnv bool
	ExtraArgs      []string
}

// ClaudeCodeConfigFrom maps agent.claude_code onto ClaudeCodeConfig — the
// one mapping both cmd/miranda-medical-card and cmd/medical-dev use.
func ClaudeCodeConfigFrom(c config.AgentClaudeCodeConfig) ClaudeCodeConfig {
	return ClaudeCodeConfig{
		Binary:         c.Binary,
		Model:          c.Model,
		Effort:         c.Effort,
		MaxTurns:       c.MaxTurns,
		Timeout:        time.Duration(c.TimeoutSeconds) * time.Second,
		AllowAPIKeyEnv: c.AllowAPIKeyEnv,
		ExtraArgs:      c.ExtraArgs,
	}
}

// claudeCodeEngine is the claude_code engine (see engine and
// docs/adr/010-claude-code-engine.md): every Ask call starts a fresh headless
// Claude Code process, which does the reasoning and tool selection and calls
// the Knowledge Providers back through a loopback MCP endpoint ("Medical
// MCP") served by this engine; the process is closed as soon as it has
// answered. Every tool call still goes through Asker.callTool — exactly the
// path mirandaEngine uses — so no tool logic exists twice.
//
// Unlike miranda's orchestrator, which keeps one long-lived process per user,
// a process here lives for exactly one question: medical.ask calls are rare
// and independent, sessions are optional (SessionStore is the source of truth
// either way — a fresh process gets earlier turns as a transcript), and a
// process that never outlives its call needs no idle reaping, no restart
// rules and can't leak one question's state into another's.
type claudeCodeEngine struct {
	a      *Asker
	cfg    ClaudeCodeConfig
	tracer llm.Tracer

	httpSrv *http.Server
	baseURL string

	mu sync.Mutex
	// calls maps each in-flight Ask call's bearer token to it — the token is
	// what binds an inbound tool call to a call (and so to its subjectID):
	// a process can only ever read the subject it was started for.
	calls map[string]*ccCall
}

// ccCall is one in-flight Ask call as Medical MCP sees it.
type ccCall struct {
	// ctx is the Ask call's own ctx (deadline, conversation id, anomaly
	// Recorder) — tool calls run under it, not under the MCP request's.
	ctx     context.Context
	turn    *askTurn
	handler http.Handler
	// toolMu serializes tool execution within a call: the CLI may run
	// several MCP calls in parallel, but the session history must stay in
	// "assistant tool call → its result" order for a later replay.
	toolMu sync.Mutex
}

// EnableClaudeCode switches this Asker to the claude_code engine. tracer
// should be the same tracer the router writes llm.log through
// (llmtrace.ContextTracer), so a claude_code call's blocks land in the same
// file and reach the per-turn anomaly Recorder; nil disables tracing. Call
// once at startup, before serving; Close stops the loopback listener.
//
// Fails if the CLI can't be found or the listener can't be opened. Whether
// the CLI is logged in is only logged (claudecode.LogAuthStatus, by the
// caller), since a login can be fixed without a restart.
func (a *Asker) EnableClaudeCode(cfg ClaudeCodeConfig, tracer llm.Tracer) error {
	bin, err := claudecode.ResolveBinary(cfg.Binary)
	if err != nil {
		return fmt.Errorf("ask: claude_code: %w", err)
	}
	cfg.Binary = bin
	if cfg.Timeout <= 0 {
		cfg.Timeout = claudeCodeDefaultTimeout
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("ask: claude_code: listen for medical mcp: %w", err)
	}
	e := &claudeCodeEngine{
		a:       a,
		cfg:     cfg,
		tracer:  tracer,
		baseURL: "http://" + ln.Addr().String() + "/mcp",
		calls:   make(map[string]*ccCall),
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", e.mcpHandler())
	e.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = e.httpSrv.Serve(ln) }()
	a.engine = e
	return nil
}

// mcpHandler serves Medical MCP: the bearer token picks the call, and with it
// that call's own MCP handler. Without a valid token — 401.
func (e *claudeCodeEngine) mcpHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := e.callByToken(r)
		if call == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		call.handler.ServeHTTP(w, r)
	})
}

func (e *claudeCodeEngine) callByToken(r *http.Request) *ccCall {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil
	}
	// A plain map lookup: the token is 256 random bits, regenerated per
	// call, so timing on its lookup gives nothing guessable away.
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[token]
}

// run implements engine.
func (e *claudeCodeEngine) run(ctx context.Context, t *askTurn) (string, error) {
	t.outcome.MaxIterations = e.cfg.MaxTurns
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	token, err := randomToken()
	if err != nil {
		return "", err
	}
	call := &ccCall{ctx: ctx, turn: t}
	// A per-call MCP server and HTTP handler (rather than one shared
	// handler choosing a server per request): when the call ends, both are
	// simply dropped, taking any MCP session state the SDK keeps with them.
	server := mcp.NewServer(&mcp.Implementation{Name: medicalMCPServerName, Version: "1"}, nil)
	for _, td := range t.tools {
		server.AddTool(&mcp.Tool{Name: td.Name, Description: td.Description, InputSchema: td.Parameters}, e.toolHandler(call, td.Name))
	}
	call.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	e.mu.Lock()
	e.calls[token] = call
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.calls, token)
		e.mu.Unlock()
	}()

	system := t.system + claudeCodeToolNote
	proc, err := claudecode.Start(claudecode.Options{
		Binary:       e.cfg.Binary,
		Model:        e.cfg.Model,
		Effort:       e.cfg.Effort,
		SystemPrompt: system,
		MCPServers: map[string]claudecode.MCPServer{
			medicalMCPServerName: {Type: "http", URL: e.baseURL, Headers: map[string]string{"Authorization": "Bearer " + token}},
		},
		// No built-in tools at all (no web, no files): an answer about the
		// user's medical history must come from their own records only.
		AllowedTools:   []string{"mcp__" + medicalMCPServerName + "__*"},
		MaxTurns:       e.cfg.MaxTurns,
		AllowAPIKeyEnv: e.cfg.AllowAPIKeyEnv,
		ExtraArgs:      e.cfg.ExtraArgs,
	})
	if err != nil {
		return "", fmt.Errorf("claude_code: %w", err)
	}
	// Closed as soon as this call has its answer (or failed): the process
	// lives for exactly one question.
	defer func() { _ = proc.Close() }()

	msg := claudecode.UserMessage{Text: composeClaudeCodeMessage(t)}
	tb := claudecode.NewTraceBuilder(e.tracer, claudeCodeProviderName, "mcp__"+medicalMCPServerName+"__", system, msg, t.tools)
	res, err := proc.Turn(ctx, msg, claudecode.Handler{
		OnAssistant:   func(id string, b claudecode.ContentBlock) { tb.AddResponse(ctx, id, b) },
		OnToolResults: tb.ToolResults,
	})
	t.outcome.IterationCount = res.NumTurns
	tb.Flush(ctx, err)
	if err != nil {
		// No result event, so no num_turns: count the model calls the
		// trace saw, so a timed-out turn's anomaly report shows how far it got.
		t.outcome.IterationCount = tb.Calls()
		t.outcome.TimedOut = errors.Is(err, context.DeadlineExceeded)
		return "", fmt.Errorf("claude_code: %w", err)
	}

	switch {
	case res.Subtype == "error_max_turns":
		t.outcome.HitIterationCap = true
		return "", fmt.Errorf("claude_code: exceeded %d turns without a final reply", e.cfg.MaxTurns)
	case res.IsError:
		return "", fmt.Errorf("claude_code: call failed (%s): %s", res.Subtype, res.Text)
	case strings.TrimSpace(res.Text) == "":
		// Same reasoning as the miranda engine relaying whatever text the
		// model ended on: never hand Miranda a silent blank answer.
		return "", fmt.Errorf("claude_code: empty final reply")
	}
	return res.Text, nil
}

// close implements engine.
func (e *claudeCodeEngine) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = e.httpSrv.Shutdown(ctx)
}

// toolHandler is Medical MCP's handler for one Knowledge Provider tool: it
// records the call and runs it through Asker.callTool under the Ask call's
// own ctx.
func (e *claudeCodeEngine) toolHandler(call *ccCall, name string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		call.toolMu.Lock()
		defer call.toolMu.Unlock()
		if call.ctx.Err() != nil {
			// "error:"-prefixed like executeToolCall's own failures, so the
			// anomaly detector's tool_error check sees it.
			return toolErrorResult("error: this medical.ask call has already ended"), nil
		}

		args := "{}"
		if len(req.Params.Arguments) > 0 {
			args = string(req.Params.Arguments)
		}
		tc := llm.ToolCall{ID: toolUseID(req), Name: name, Arguments: args}
		// Recorded as an assistant tool-call message followed by its result,
		// exactly like mirandaEngine does — so the session replays the same
		// way under either engine. Text the model wrote alongside the call
		// isn't attached (it arrives on stdout, the call over MCP); the
		// final answer is recorded by Ask as usual.
		if err := e.a.sessions.RecordAssistantToolCallMessage(call.ctx, call.turn.sessionID, "", []llm.ToolCall{tc}); err != nil {
			e.a.logger.Warn("ask: record assistant tool-call message failed", "error", err)
		}
		result := e.a.callTool(call.ctx, call.turn, tc)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil
	}
}

// toolUseID reuses the CLI's own tool_use id (sent in _meta) so the session
// history and llm.log refer to the same call by the same id; falls back to a
// fresh one.
func toolUseID(req *mcp.CallToolRequest) string {
	if id, ok := req.Params.Meta["claudecode/toolUseId"].(string); ok && id != "" {
		return id
	}
	return "call_" + uuid.NewString()
}

func toolErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("claude_code: token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// composeClaudeCodeMessage builds the one message a call's process gets: the
// question, preceded — for a session with earlier turns — by those turns
// rendered from SessionStore (a fresh process has seen none of them).
func composeClaudeCodeMessage(t *askTurn) string {
	if len(t.history) == 0 {
		return t.question
	}
	return "Below is the earlier part of this conversation, restored from the session history. Continue it with that context; don't retell it.\n<previous_conversation>\n" +
		claudecode.RenderTranscript(t.history, transcriptToolResultMax) +
		"</previous_conversation>\n\n" + t.question
}
