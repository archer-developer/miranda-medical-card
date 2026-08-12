// This file implements `medical-dev llm-trace` — a reader for logs/llm.log
// (see internal/llmtrace's block format: one "=== <time> provider=<name>
// [conversation=<id>] ===" section per Provider.Chat/Structured call, with
// the exact request/response each provider sent, added by
// cmd/miranda-medical-card/main.go's buildLLMTraceWriter when
// logging.level is debug). Turned into a standing command after manually
// reconstructing this same step-by-step table by hand (scp the log, write a
// throwaway parser) while debugging a medical.ask "exceeded N tool-call
// iterations" failure — worth having on tap for whenever a tool's schema or
// the agent's system prompt needs correcting based on what the model
// actually tried, not just the final MCP error Miranda saw.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// traceBlock is one llmtrace.Logger.Trace block, parsed. Conversation is
// empty for calls with no llmtrace.WithConversationID in scope — the
// Document Pipeline's Structured/Chat calls never tag one, only the Agent
// Loop's do (see internal/ask/agent_loop.go), so an empty Conversation
// reliably means "not a medical.ask turn".
type traceBlock struct {
	Time         time.Time
	Provider     string
	Conversation string
	Request      string
	Response     string
	ErrorText    string // set instead of Response when the call itself failed
}

var traceHeaderRe = regexp.MustCompile(`^(\S+) provider=(\S+)(?: conversation=(\S+))? ===$`)

// blockSplitRe finds the start of each block — anchored to line start so a
// request/response body that happens to contain the literal text "=== "
// mid-line (astronomically unlikely in practice, but cheap to guard) can
// never be mistaken for a new block.
var blockSplitRe = regexp.MustCompile(`(?m)^=== `)

func runLLMTrace(args []string) error {
	fs := flag.NewFlagSet("llm-trace", flag.ExitOnError)
	file := fs.String("file", "logs/llm.log", "path to the LLM trace log")
	conversation := fs.String("conversation", "", "show every turn of this conversation id")
	latest := fs.Bool("latest", false, "show every turn of the most recently started conversation")
	if err := fs.Parse(args); err != nil {
		return err
	}

	blocks, err := parseTraceFile(*file)
	if err != nil {
		return err
	}

	if *latest {
		id := latestConversation(blocks)
		if id == "" {
			return fmt.Errorf("no conversation-tagged blocks found in %s", *file)
		}
		*conversation = id
	}

	if *conversation == "" {
		printConversationSummary(blocks)
		return nil
	}
	return printConversationDetail(blocks, *conversation)
}

func parseTraceFile(path string) ([]traceBlock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trace log %s: %w", path, err)
	}

	const reqMarker = "--- request ---\n"
	const respMarker = "--- response ---\n"

	var blocks []traceBlock
	for _, part := range blockSplitRe.Split(string(data), -1) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		lines := strings.SplitN(part, "\n", 2)
		if len(lines) < 2 {
			continue
		}
		m := traceHeaderRe.FindStringSubmatch(lines[0])
		if m == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, m[1])
		if err != nil {
			continue
		}
		block := traceBlock{Time: t, Provider: m[2], Conversation: m[3]}

		body := lines[1]
		reqStart := strings.Index(body, reqMarker)
		respStart := strings.Index(body, respMarker)
		if reqStart == -1 || respStart == -1 {
			continue
		}
		block.Request = strings.TrimSpace(body[reqStart+len(reqMarker) : respStart])
		respText := strings.TrimSpace(body[respStart+len(respMarker):])
		if rest, ok := strings.CutPrefix(respText, "error: "); ok {
			block.ErrorText = rest
		} else {
			block.Response = respText
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func latestConversation(blocks []traceBlock) string {
	var id string
	var latest time.Time
	for _, b := range blocks {
		if b.Conversation != "" && b.Time.After(latest) {
			latest, id = b.Time, b.Conversation
		}
	}
	return id
}

type conversationSummary struct {
	ID        string
	Provider  string
	FirstSeen time.Time
	LastSeen  time.Time
	Turns     int
	Errors    int
}

func printConversationSummary(blocks []traceBlock) {
	summaries := map[string]*conversationSummary{}
	var order []string
	untagged := 0
	for _, b := range blocks {
		if b.Conversation == "" {
			untagged++
			continue
		}
		s, ok := summaries[b.Conversation]
		if !ok {
			s = &conversationSummary{ID: b.Conversation, Provider: b.Provider, FirstSeen: b.Time, LastSeen: b.Time}
			summaries[b.Conversation] = s
			order = append(order, b.Conversation)
		}
		s.Turns++
		if b.Time.Before(s.FirstSeen) {
			s.FirstSeen = b.Time
		}
		if b.Time.After(s.LastSeen) {
			s.LastSeen = b.Time
		}
		if b.ErrorText != "" {
			s.Errors++
		}
	}
	sort.Slice(order, func(i, j int) bool { return summaries[order[i]].FirstSeen.Before(summaries[order[j]].FirstSeen) })

	if len(order) == 0 {
		fmt.Println("No conversation-tagged blocks found (medical.ask wasn't called while tracing was on).")
	} else {
		fmt.Printf("%-28s %-16s %-6s %-7s %s\n", "CONVERSATION", "PROVIDER", "TURNS", "ERRORS", "STARTED")
		for _, id := range order {
			s := summaries[id]
			fmt.Printf("%-28s %-16s %-6d %-7d %s\n", s.ID, s.Provider, s.Turns, s.Errors, s.FirstSeen.Format(time.RFC3339))
		}
	}
	if untagged > 0 {
		fmt.Printf("\n(%d block(s) with no conversation id — Document Pipeline calls, not shown here)\n", untagged)
	}
	fmt.Println("\nPass --conversation <id> or --latest for the turn-by-turn table.")
}

func printConversationDetail(blocks []traceBlock, conversationID string) error {
	var turns []traceBlock
	for _, b := range blocks {
		if b.Conversation == conversationID {
			turns = append(turns, b)
		}
	}
	if len(turns) == 0 {
		return fmt.Errorf("no blocks found for conversation %q", conversationID)
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].Time.Before(turns[j].Time) })

	fmt.Printf("Conversation %s — %d turn(s), provider=%s\n\n", conversationID, len(turns), turns[0].Provider)

	for i, b := range turns {
		ts := b.Time.Format("15:04:05")
		in, _ := describeIncoming(b, i == 0)
		if len(in) == 0 {
			fmt.Printf("%2d %s\n", i+1, ts)
		} else {
			fmt.Printf("%2d %s  <- %s\n", i+1, ts, in[0])
			for _, extra := range in[1:] {
				fmt.Printf("      <- %s\n", extra)
			}
		}
		if b.ErrorText != "" {
			fmt.Printf("      !! ERROR: %s\n", truncate(b.ErrorText, 300))
			continue
		}
		for _, out := range describeOutgoing(b) {
			fmt.Printf("      -> %s\n", out)
		}
	}

	last := turns[len(turns)-1]
	if last.ErrorText == "" && endsOnToolCallWithNoAnswer(last) {
		fmt.Println("\nNote: conversation ends on a tool call with no final answer — likely hit the agent loop's iteration cap or the request was cut off before finishing.")
	}
	return nil
}

// --- Gemini trace shape (gemini.Provider.trace) — the only provider
// actually seeing agent-loop traffic today (llm.agent_provider in
// config/llm.yaml); fully decoded. ---

type geminiPart struct {
	Text             string          `json:"text"`
	FunctionCall     *geminiCall     `json:"functionCall"`
	FunctionResponse *geminiResponse `json:"functionResponse"`
}
type geminiCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}
type geminiResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}
type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}
type geminiToolCall struct {
	Name      string `json:"Name"`
	Arguments string `json:"Arguments"`
}
type geminiChatResponse struct {
	Text      string           `json:"text"`
	ToolCalls []geminiToolCall `json:"tool_calls"`
}

// --- Anthropic trace shape (anthropic.Provider.trace) — used only for the
// (rarely triggered) claude-escalation provider (config/llm.yaml's
// escalation.target_provider). Decoded generically via the well-known
// Messages API JSON shape rather than importing anthropic-sdk-go's request
// param types, which aren't meant to be read back from JSON. ---

func anthropicBlockText(block map[string]any, key string) (string, bool) {
	v, ok := block[key].(string)
	return v, ok
}

// --- shared decoding ---

// describeIncoming summarizes every item this block's request adds to the
// conversation since the previous turn — the original question (first
// block only), or one entry per tool result a parallel multi-tool-call turn
// produced (a single turn commonly calls more than one tool at once — e.g.
// profile+lab_results together — and each gets its own result; showing
// only the last would silently drop every result but one). ok is false when
// the request doesn't match any recognized shape, so the caller can fall
// back to a plain turn number.
func describeIncoming(b traceBlock, isFirst bool) ([]string, bool) {
	var req geminiRequest
	if json.Unmarshal([]byte(b.Request), &req) == nil && len(req.Contents) > 0 {
		return describeIncomingGemini(req.Contents, isFirst), true
	}

	var generic struct {
		Messages []map[string]any `json:"messages"`
	}
	if json.Unmarshal([]byte(b.Request), &generic) == nil && len(generic.Messages) > 0 {
		return describeIncomingAnthropic(generic.Messages[len(generic.Messages)-1], isFirst), true
	}

	return nil, false
}

// describeIncomingGemini handles the first block (a single "user"/text
// content — the question) and every later block, where a parallel
// multi-tool-call turn adds one whole "user"/functionResponse content entry
// per tool called (see gemini.Provider's own request-building — unlike
// Anthropic, which bundles multiple tool_results into one message, see
// describeIncomingAnthropic) rather than multiple parts on a single entry.
// Walking backward from the end and collecting every consecutive
// all-functionResponse entry covers exactly that, however many tools the
// previous turn called.
func describeIncomingGemini(contents []geminiContent, isFirst bool) []string {
	if len(contents) == 0 {
		return nil
	}
	if isFirst {
		for _, p := range contents[len(contents)-1].Parts {
			if p.Text != "" {
				return []string{"question: " + truncate(p.Text, 220)}
			}
		}
		return nil
	}

	var reversed []string
	for i := len(contents) - 1; i >= 0; i-- {
		parts := contents[i].Parts
		if len(parts) == 0 {
			break
		}
		allResponses := true
		for _, p := range parts {
			if p.FunctionResponse == nil {
				allResponses = false
				break
			}
		}
		if !allResponses {
			break
		}
		for _, p := range parts {
			reversed = append(reversed, fmt.Sprintf("%s -> %s", p.FunctionResponse.Name, truncate(fmt.Sprint(p.FunctionResponse.Response["result"]), 220)))
		}
	}
	// Collected walking backward (most recent tool call's result first);
	// reverse to match call order.
	for l, r := 0, len(reversed)-1; l < r; l, r = l+1, r-1 {
		reversed[l], reversed[r] = reversed[r], reversed[l]
	}
	return reversed
}

// describeIncomingAnthropic handles Anthropic's own bundling: a
// multi-tool-call turn's results all land as separate tool_result blocks
// within the one following user message (unlike Gemini — see
// describeIncomingGemini), so collecting every matching block in that
// single message already covers a parallel turn.
func describeIncomingAnthropic(last map[string]any, isFirst bool) []string {
	var out []string
	content, _ := last["content"].([]any)
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "tool_result":
			text, _ := anthropicBlockText(block, "content")
			out = append(out, "tool result -> "+truncate(text, 220))
		case "text":
			if isFirst {
				if text, ok := anthropicBlockText(block, "text"); ok {
					out = append(out, "question: "+truncate(text, 220))
				}
			}
		}
	}
	// A plain-string content (no blocks) is valid Anthropic API shape too,
	// used for simple single-turn text.
	if len(out) == 0 {
		if text, ok := last["content"].(string); ok && isFirst {
			out = append(out, "question: "+truncate(text, 220))
		}
	}
	return out
}

// describeOutgoing lists what the model did this turn — one entry per tool
// call, or its final answer text, or a raw preview when the response
// doesn't match a recognized shape at all.
func describeOutgoing(b traceBlock) []string {
	var resp geminiChatResponse
	if json.Unmarshal([]byte(b.Response), &resp) == nil && (resp.Text != "" || len(resp.ToolCalls) > 0) {
		var out []string
		if resp.Text != "" {
			out = append(out, "answer: "+truncate(resp.Text, 400))
		}
		for _, tc := range resp.ToolCalls {
			out = append(out, fmt.Sprintf("call %s(%s)", tc.Name, truncate(tc.Arguments, 200)))
		}
		return out
	}

	var anth struct {
		Content []map[string]any `json:"content"`
	}
	if json.Unmarshal([]byte(b.Response), &anth) == nil && len(anth.Content) > 0 {
		var out []string
		for _, block := range anth.Content {
			switch block["type"] {
			case "text":
				if text, ok := anthropicBlockText(block, "text"); ok && text != "" {
					out = append(out, "answer: "+truncate(text, 400))
				}
			case "tool_use":
				name, _ := block["name"].(string)
				input, _ := json.Marshal(block["input"])
				out = append(out, fmt.Sprintf("call %s(%s)", name, truncate(string(input), 200)))
			}
		}
		if len(out) > 0 {
			return out
		}
	}

	return []string{"(unrecognized response shape) " + truncate(b.Response, 200)}
}

// endsOnToolCallWithNoAnswer reports whether b's response is entirely tool
// call(s) with no final text — the shape a conversation that got cut off
// mid-loop (iteration cap, timeout, crash) always ends on.
func endsOnToolCallWithNoAnswer(b traceBlock) bool {
	// json.Unmarshal into either shape below "succeeds" even on a response
	// that matches neither (unknown fields are ignored, missing ones stay
	// zero) — checking for actual signal (a non-empty ToolCalls/Content),
	// not just a nil error, is what lets an anthropic-shaped response fall
	// through to its own branch instead of being misread as an
	// empty-but-valid gemini one.
	var resp geminiChatResponse
	if json.Unmarshal([]byte(b.Response), &resp) == nil && (resp.Text != "" || len(resp.ToolCalls) > 0) {
		return resp.Text == "" && len(resp.ToolCalls) > 0
	}
	var anth struct {
		Content []map[string]any `json:"content"`
	}
	if json.Unmarshal([]byte(b.Response), &anth) == nil && len(anth.Content) > 0 {
		sawToolUse, sawText := false, false
		for _, block := range anth.Content {
			switch block["type"] {
			case "tool_use":
				sawToolUse = true
			case "text":
				sawText = true
			}
		}
		return sawToolUse && !sawText
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ") // collapse newlines/indentation into one table-friendly line
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
