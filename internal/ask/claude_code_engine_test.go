package ask_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/claudecode"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// fakeClaudeEnv switches this package's test binary into a fake `claude` CLI
// (see TestMain/runFakeClaude) — EnableClaudeCode is pointed at os.Args[0],
// so the claude_code engine is exercised end to end (process, stream-json,
// Medical MCP over real HTTP, callTool, session history) without the real
// CLI or any network.
const fakeClaudeEnv = "MEDICAL_FAKE_CLAUDE"

// fakePIDDirEnv names a directory the fake writes its PID into, so a test
// can check the process is gone once Ask returns.
const fakePIDDirEnv = "MEDICAL_FAKE_CLAUDE_PIDDIR"

func TestMain(m *testing.M) {
	if os.Getenv(fakeClaudeEnv) == "1" {
		runFakeClaude()
		return
	}
	os.Exit(m.Run())
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// runFakeClaude connects to the MCP server named in --mcp-config, like the
// real CLI, and answers the one stdin message with a result whose text
// reports what it saw: "tools=<names>", "replayed=<bool>",
// "prompt_note=<bool>" and — for a message ending in "CALL <tool> <json>" —
// "result=<tool result>" (the call also goes through the stream as a
// tool_use/tool_result round, for the trace). "maxturns" ends with
// error_max_turns; "badtoken" reports what a wrong bearer token gets.
func runFakeClaude() {
	if dir := os.Getenv(fakePIDDirEnv); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	args := os.Args[1:]
	argAfter := func(flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	raw, err := os.ReadFile(argAfter("--mcp-config"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read mcp config:", err)
		os.Exit(2)
	}
	var cfg struct {
		MCPServers map[string]claudecode.MCPServer `json:"mcpServers"`
	}
	_ = json.Unmarshal(raw, &cfg)
	srv := cfg.MCPServers["medical"]
	token := strings.TrimPrefix(srv.Headers["Authorization"], "Bearer ")
	prompt, _ := os.ReadFile(argAfter("--system-prompt-file"))

	ctx := context.Background()
	connect := func(token string) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "fake-claude", Version: "1"}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: bearerTransport{token}}}, nil)
	}
	session, err := connect(token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(2)
	}
	defer session.Close()

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var msg struct {
			Message struct {
				Content []map[string]any `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal(in.Bytes(), &msg)
		var text string
		for _, c := range msg.Message.Content {
			if c["type"] == "text" {
				text, _ = c["text"].(string)
			}
		}
		if strings.HasSuffix(text, "hang") {
			// One model call's output, then nothing — like a stuck CLI.
			_ = out.Encode(map[string]any{"type": "assistant", "message": map[string]any{
				"id": "msg_1", "content": []any{map[string]any{"type": "text", "text": "thinking…"}}}})
			select {}
		}
		if strings.HasSuffix(text, "maxturns") {
			_ = out.Encode(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "num_turns": 3})
			continue
		}
		if strings.HasSuffix(text, "badtoken") {
			_, err := connect("wrong")
			_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "result": fmt.Sprintf("badtoken_err=%v", err != nil)})
			continue
		}

		tools, _ := session.ListTools(ctx, nil)
		var names []string
		for _, t := range tools.Tools {
			names = append(names, t.Name)
		}
		reply := fmt.Sprintf("tools=%s replayed=%v prompt_note=%v",
			strings.Join(names, ","), strings.Contains(text, "<previous_conversation>"), strings.Contains(string(prompt), "mcp__medical__"))

		if i := strings.LastIndex(text, "CALL "); i >= 0 {
			name, callArgs, _ := strings.Cut(text[i+len("CALL "):], " ")
			_ = out.Encode(map[string]any{"type": "assistant", "message": map[string]any{
				"id": "msg_1", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "mcp__medical__" + name, "input": json.RawMessage(callArgs)}}}})
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name: name, Arguments: json.RawMessage(callArgs), Meta: mcp.Meta{"claudecode/toolUseId": "toolu_1"},
			})
			result := fmt.Sprint(err)
			if err == nil && len(res.Content) > 0 {
				result = res.Content[0].(*mcp.TextContent).Text
			}
			_ = out.Encode(map[string]any{"type": "user", "message": map[string]any{
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": result}}}})
			reply += " result=" + result
		}
		_ = out.Encode(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg_2", "content": []any{map[string]any{"type": "text", "text": reply}}}})
		_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "result": reply, "num_turns": 2})
	}
}

type recordingTracer struct {
	mu     sync.Mutex
	blocks []string
}

func (r *recordingTracer) Trace(_ context.Context, provider, request, response string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocks = append(r.blocks, provider+" "+response)
}

func newClaudeCodeAsker(t *testing.T, registry *ask.Registry, sessions *ask.SessionStore, tracer *recordingTracer) *ask.Asker {
	t.Helper()
	t.Setenv(fakeClaudeEnv, "1")
	asker := ask.NewAsker(nil, registry, sessions, testProviderTimeout, 20, 8, nil)
	var tr llm.Tracer
	if tracer != nil {
		tr = tracer
	}
	require.NoError(t, asker.EnableClaudeCode(ask.ClaudeCodeConfig{Binary: os.Args[0], MaxTurns: 5}, tr))
	t.Cleanup(asker.Close)
	return asker
}

func TestClaudeCodeEngine_ToolCallEndToEnd(t *testing.T) {
	provider := &scriptedProvider{name: "lab_results", chunks: []ask.KnowledgeChunk{{Source: "lab_results", Content: "ALT 42", DocumentID: "doc1", Title: "Биохимия"}}}
	store := newTestStore(t)
	sessions := ask.NewSessionStore(storage.NewAskSessionRepository(store))
	tracer := &recordingTracer{}
	asker := newClaudeCodeAsker(t, ask.NewRegistry(provider, &scriptedProvider{name: "timeline"}), sessions, tracer)

	pidDir := t.TempDir()
	t.Setenv(fakePIDDirEnv, pidDir)
	result, err := asker.Ask(context.Background(), "alex", "anna", "sess-1", `CALL lab_results {"indicatorName":"ALT"}`)
	require.NoError(t, err)

	require.Contains(t, result.Answer, "tools=lab_results,timeline", "the process must see exactly the registry's tools")
	require.Contains(t, result.Answer, "prompt_note=true")
	require.Contains(t, result.Answer, "result=")
	require.Contains(t, result.Answer, "ALT 42")
	require.Len(t, provider.calls, 1)
	require.Equal(t, "anna", provider.calls[0].UserID, "tool calls must run as the resolved subject, never anything model-supplied")
	require.Equal(t, "ALT", provider.calls[0].IndicatorName)
	require.Len(t, result.Sources, 1)
	require.Equal(t, "doc1", result.Sources[0].DocumentID)

	// The process lives for exactly one question.
	pidRaw, err := os.ReadFile(filepath.Join(pidDir, "pid"))
	require.NoError(t, err)
	pid, _ := strconv.Atoi(string(pidRaw))
	require.ErrorIs(t, syscall.Kill(pid, 0), syscall.ESRCH, "the CLI process must be gone once Ask returns")

	// History is recorded exactly like the miranda engine records it, with
	// the CLI's own tool_use id.
	history, err := sessions.LoadHistory(context.Background(), "sess-1")
	require.NoError(t, err)
	require.Len(t, history, 4) // user, assistant tool call, tool result, final answer
	require.Equal(t, "toolu_1", history[1].ToolCalls[0].ID)
	require.Equal(t, "lab_results", history[1].ToolCalls[0].Name)
	require.Equal(t, "toolu_1", history[2].ToolCallID)
	require.Equal(t, result.Answer, history[3].Content)

	// llm.log: one block per model call, tool names without the CLI's MCP
	// namespace.
	require.Len(t, tracer.blocks, 2)
	require.True(t, strings.HasPrefix(tracer.blocks[0], "claude_code "))
	require.Contains(t, tracer.blocks[0], `"name":"lab_results"`)
	require.NotContains(t, tracer.blocks[0], "mcp__medical__")
}

func TestClaudeCodeEngine_FreshProcessGetsSessionTranscript(t *testing.T) {
	sessions := ask.NewSessionStore(storage.NewAskSessionRepository(newTestStore(t)))
	asker := newClaudeCodeAsker(t, ask.NewRegistry(&scriptedProvider{name: "timeline"}), sessions, nil)

	first, err := asker.Ask(context.Background(), "alex", "alex", "sess-2", "Первый вопрос")
	require.NoError(t, err)
	require.Contains(t, first.Answer, "replayed=false")

	second, err := asker.Ask(context.Background(), "alex", "alex", "sess-2", "Второй вопрос")
	require.NoError(t, err)
	require.Contains(t, second.Answer, "replayed=true", "a follow-up in the same session must carry the earlier turns")

	stateless, err := asker.Ask(context.Background(), "alex", "alex", "", "Без сессии")
	require.NoError(t, err)
	require.Contains(t, stateless.Answer, "replayed=false")
}

func TestClaudeCodeEngine_MaxTurnsIsAnIterationCapAnomaly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "anomalies")
	tracer := &recordingTracer{}
	asker := newClaudeCodeAsker(t, ask.NewRegistry(&scriptedProvider{name: "timeline"}), ask.NewSessionStore(nil), tracer)
	asker.SetAnomalyConfig(ask.AnomalyConfig{Dir: dir})

	_, err := asker.Ask(context.Background(), "alex", "alex", "", "maxturns")
	require.ErrorContains(t, err, "exceeded 5 turns")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Name(), "iteration_cap")
}

func TestClaudeCodeEngine_RejectsForeignToken(t *testing.T) {
	asker := newClaudeCodeAsker(t, ask.NewRegistry(&scriptedProvider{name: "timeline"}), ask.NewSessionStore(nil), nil)
	result, err := asker.Ask(context.Background(), "alex", "alex", "", "badtoken")
	require.NoError(t, err)
	require.Equal(t, "badtoken_err=true", result.Answer)
}

func TestClaudeCodeEngine_TimeoutKillsTheProcessAndCountsCalls(t *testing.T) {
	t.Setenv(fakeClaudeEnv, "1")
	dir := filepath.Join(t.TempDir(), "anomalies")
	asker := ask.NewAsker(nil, ask.NewRegistry(&scriptedProvider{name: "timeline"}), ask.NewSessionStore(nil), testProviderTimeout, 20, 8, nil)
	require.NoError(t, asker.EnableClaudeCode(ask.ClaudeCodeConfig{Binary: os.Args[0], MaxTurns: 5, Timeout: 500 * time.Millisecond}, &recordingTracer{}))
	t.Cleanup(asker.Close)
	asker.SetAnomalyConfig(ask.AnomalyConfig{Dir: dir})

	_, err := asker.Ask(context.Background(), "alex", "alex", "", "hang")
	require.ErrorIs(t, err, context.DeadlineExceeded)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Name(), "timeout")
}
