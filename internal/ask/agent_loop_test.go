package ask_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	llm "github.com/archer-developer/miranda-llm"
	"github.com/archer-developer/miranda-llm/llmtest"
	"github.com/archer-developer/miranda-llm/router"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

const testProviderTimeout = 5 * time.Second

// newRouterWithFake wraps fake in a real *router.Router (rather than a
// hand-rolled ask.ChatProvider double) so these tests exercise the actual
// ChatPinned pin-forwarding behavior production wiring relies on, not a
// simplified stand-in for it.
func newRouterWithFake(t *testing.T, fake *llmtest.FakeProvider) *router.Router {
	t.Helper()
	r, err := router.New([]llm.Provider{fake}, nil, "fake")
	require.NoError(t, err)
	return r
}

// scriptedProvider is a hand-written ask.Provider recording every Collect
// call it receives and returning a fixed chunk set or error.
type scriptedProvider struct {
	name   string
	chunks []ask.KnowledgeChunk
	err    error
	calls  []ask.KnowledgeRequest
}

func (p *scriptedProvider) Metadata() ask.ProviderMetadata {
	return ask.ProviderMetadata{Name: p.name, Description: "test provider " + p.name}
}

func (p *scriptedProvider) Collect(_ context.Context, req ask.KnowledgeRequest) ([]ask.KnowledgeChunk, error) {
	p.calls = append(p.calls, req)
	if p.err != nil {
		return nil, p.err
	}
	return p.chunks, nil
}

// recordingChatProvider is a minimal ask.ChatProvider that always replies
// with a fixed text-only response (no tool calls) and records the last
// ChatRequest it received — used by tools_test.go, which only cares what
// tools get offered to the model, not multi-turn tool-calling behavior.
type recordingChatProvider struct {
	text    string
	lastReq llm.ChatRequest
}

func (r *recordingChatProvider) ChatPinned(_ context.Context, req llm.ChatRequest, _ string, onProviderUsed func(string)) (<-chan llm.StreamChunk, error) {
	r.lastReq = req
	ch := make(chan llm.StreamChunk, 2)
	ch <- llm.StreamChunk{TextDelta: r.text}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	if onProviderUsed != nil {
		onProviderUsed("fake")
	}
	return ch, nil
}

func toolByName(t *testing.T, tools []llm.ToolDef, name string) llm.ToolDef {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found among %d tools", name, len(tools))
	return llm.ToolDef{}
}

func TestAsk_NoToolCallReturnsImmediateAnswer(t *testing.T) {
	fake := llmtest.New("fake", llmtest.Response{Text: "Просто ответ без поиска."})
	registry := ask.NewRegistry(&scriptedProvider{name: "timeline"})
	asker := ask.NewAsker(newRouterWithFake(t, fake), registry, ask.NewSessionStore(nil), testProviderTimeout, 20, 8, nil)

	result, err := asker.Ask(context.Background(), "alex", "alex", "", "Общий вопрос")
	require.NoError(t, err)
	require.Equal(t, "Просто ответ без поиска.", result.Answer)
	require.Empty(t, result.Sources)
}

func TestAsk_OneToolCallThenReply(t *testing.T) {
	provider := &scriptedProvider{name: "lab_results", chunks: []ask.KnowledgeChunk{
		{Source: "lab_results", Content: "ALT 54.7 (2025-03-14)", Confidence: 1.0, DocumentID: "doc1"},
	}}
	fake := llmtest.New("fake",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_1", Name: "lab_results", Arguments: `{"indicatorName":"ALT"}`}},
		llmtest.Response{Text: "ALT впервые повышен 14 марта 2025 (54.7)."},
	)
	registry := ask.NewRegistry(provider)
	asker := ask.NewAsker(newRouterWithFake(t, fake), registry, ask.NewSessionStore(nil), testProviderTimeout, 20, 8, nil)

	result, err := asker.Ask(context.Background(), "alex", "alex", "", "Когда впервые повысился ALT?")
	require.NoError(t, err)
	require.Equal(t, "ALT впервые повышен 14 марта 2025 (54.7).", result.Answer)
	require.Len(t, result.Sources, 1)
	require.Equal(t, "doc1", result.Sources[0].DocumentID)

	require.Len(t, provider.calls, 1)
	require.Equal(t, "ALT", provider.calls[0].IndicatorName)
	require.Equal(t, "alex", provider.calls[0].UserID, "UserID must be injected from subjectID, not model-supplied arguments")

	require.Len(t, fake.Requests, 2, "one Chat call for the tool-call turn, one for the final reply")
	second := fake.Requests[1]
	// [0]=system, [1]=user question, [2]=assistant tool-call, [3]=tool result
	require.Len(t, second.Messages, 4)
	require.Equal(t, llm.RoleAssistant, second.Messages[2].Role)
	require.Len(t, second.Messages[2].ToolCalls, 1)
	require.Equal(t, "call_1", second.Messages[2].ToolCalls[0].ID)
	require.Equal(t, llm.RoleTool, second.Messages[3].Role)
	require.Equal(t, "call_1", second.Messages[3].ToolCallID)
	require.Contains(t, second.Messages[3].Content, "ALT 54.7")
}

func TestAsk_SequentialMultiIterationToolUse(t *testing.T) {
	labProvider := &scriptedProvider{name: "lab_results", chunks: []ask.KnowledgeChunk{{Source: "lab_results", Content: "ALT 54.7"}}}
	timelineProvider := &scriptedProvider{name: "timeline", chunks: []ask.KnowledgeChunk{{Source: "timeline", Content: "Операция 2025-02-01"}}}
	fake := llmtest.New("fake",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_1", Name: "lab_results", Arguments: `{}`}},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_2", Name: "timeline", Arguments: `{}`}},
		llmtest.Response{Text: "Собрано из двух источников."},
	)
	registry := ask.NewRegistry(labProvider, timelineProvider)
	asker := ask.NewAsker(newRouterWithFake(t, fake), registry, ask.NewSessionStore(nil), testProviderTimeout, 20, 8, nil)

	result, err := asker.Ask(context.Background(), "alex", "alex", "", "Вопрос, требующий двух источников")
	require.NoError(t, err)
	require.Equal(t, "Собрано из двух источников.", result.Answer)
	require.Len(t, labProvider.calls, 1)
	require.Len(t, timelineProvider.calls, 1)
	require.Len(t, fake.Requests, 3, "one Chat call per tool-call iteration, plus the final reply")
}

// TestAsk_SourcesNeverOmitChunkTheModelWasShown guards against a review
// finding: maxChunks used to cap the FINAL merged pool of chunks across the
// whole conversation, so a chunk fully shown to the model in one tool
// call's own (uncapped) result could still be silently dropped from the
// final `sources` list by an unrelated later call's contribution pushing
// the total over the cap. The fix caps each call's own chunks up front
// (before the model ever sees them) and never re-caps at the end — so with
// maxChunks=2 and two separate 2-chunk tool calls (4 total, over the cap),
// every chunk from BOTH calls must still appear in Sources, since none of
// them were ever trimmed after being shown.
func TestAsk_SourcesNeverOmitChunkTheModelWasShown(t *testing.T) {
	labProvider := &scriptedProvider{name: "lab_results", chunks: []ask.KnowledgeChunk{
		{Source: "lab_results", Content: "ALT 54.7", DocumentID: "docA1"},
		{Source: "lab_results", Content: "AST 41.2", DocumentID: "docA2"},
	}}
	timelineProvider := &scriptedProvider{name: "timeline", chunks: []ask.KnowledgeChunk{
		{Source: "timeline", Content: "Операция 2025-02-01", DocumentID: "docB1"},
		{Source: "timeline", Content: "Выписка 2025-02-05", DocumentID: "docB2"},
	}}
	fake := llmtest.New("fake",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_1", Name: "lab_results", Arguments: `{}`}},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_2", Name: "timeline", Arguments: `{}`}},
		llmtest.Response{Text: "Ответ на основе обоих источников."},
	)
	registry := ask.NewRegistry(labProvider, timelineProvider)
	// maxChunks=2: smaller than either call's own chunk count individually
	// equal to it (no per-call truncation happens here), but smaller than
	// the 4-chunk combined total — exactly the case that used to lose data
	// at the final aggregation step.
	asker := ask.NewAsker(newRouterWithFake(t, fake), registry, ask.NewSessionStore(nil), testProviderTimeout, 2, 8, nil)

	result, err := asker.Ask(context.Background(), "alex", "alex", "", "Вопрос про оба источника")
	require.NoError(t, err)
	require.Len(t, result.Sources, 4, "every chunk from both tool calls must survive into sources, none dropped by the final aggregation")

	var docIDs []string
	for _, s := range result.Sources {
		docIDs = append(docIDs, s.DocumentID)
	}
	require.ElementsMatch(t, []string{"docA1", "docA2", "docB1", "docB2"}, docIDs)
}

func TestAsk_ProviderErrorBecomesToolMessageNotAbort(t *testing.T) {
	failing := &scriptedProvider{name: "lab_results", err: errors.New("db unavailable")}
	fake := llmtest.New("fake",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_1", Name: "lab_results", Arguments: `{}`}},
		llmtest.Response{Text: "Не удалось получить данные, но вот что я знаю."},
	)
	registry := ask.NewRegistry(failing)
	asker := ask.NewAsker(newRouterWithFake(t, fake), registry, ask.NewSessionStore(nil), testProviderTimeout, 20, 8, nil)

	result, err := asker.Ask(context.Background(), "alex", "alex", "", "Вопрос")
	require.NoError(t, err, "a Provider.Collect error must not abort the whole Ask call")
	require.Equal(t, "Не удалось получить данные, но вот что я знаю.", result.Answer)

	toolMsg := fake.Requests[1].Messages[len(fake.Requests[1].Messages)-1]
	require.Equal(t, llm.RoleTool, toolMsg.Role)
	require.Contains(t, toolMsg.Content, "error:")
	require.Contains(t, toolMsg.Content, "db unavailable")
}

func TestAsk_UnknownToolNameHandledGracefully(t *testing.T) {
	fake := llmtest.New("fake",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_1", Name: "not_a_real_tool", Arguments: `{}`}},
		llmtest.Response{Text: "Отвечаю без этого инструмента."},
	)
	registry := ask.NewRegistry(&scriptedProvider{name: "timeline"})
	asker := ask.NewAsker(newRouterWithFake(t, fake), registry, ask.NewSessionStore(nil), testProviderTimeout, 20, 8, nil)

	result, err := asker.Ask(context.Background(), "alex", "alex", "", "Вопрос")
	require.NoError(t, err, "a hallucinated tool name must not panic or abort the loop")
	require.Equal(t, "Отвечаю без этого инструмента.", result.Answer)

	toolMsg := fake.Requests[1].Messages[len(fake.Requests[1].Messages)-1]
	require.Contains(t, toolMsg.Content, "unknown tool")
}

func TestAsk_ExceedingMaxIterationsReturnsError(t *testing.T) {
	provider := &scriptedProvider{name: "timeline", chunks: []ask.KnowledgeChunk{{Source: "timeline", Content: "x"}}}
	fake := llmtest.New("fake",
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_1", Name: "timeline", Arguments: `{}`}},
		llmtest.Response{ToolCall: &llm.ToolCall{ID: "call_2", Name: "timeline", Arguments: `{}`}},
	)
	registry := ask.NewRegistry(provider)
	asker := ask.NewAsker(newRouterWithFake(t, fake), registry, ask.NewSessionStore(nil), testProviderTimeout, 20, 2, nil)

	_, err := asker.Ask(context.Background(), "alex", "alex", "", "Вопрос")
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeded")
}

func TestAsk_SessionContinuity_ReplaysHistoryIntoSecondCall(t *testing.T) {
	store := newTestStore(t)
	sessions := ask.NewSessionStore(storage.NewAskSessionRepository(store))
	registry := ask.NewRegistry(&scriptedProvider{name: "timeline"})

	fake1 := llmtest.New("fake", llmtest.Response{Text: "Первый ответ."})
	asker1 := ask.NewAsker(newRouterWithFake(t, fake1), registry, sessions, testProviderTimeout, 20, 8, nil)
	_, err := asker1.Ask(context.Background(), "alex", "alex", "sess1", "Первый вопрос")
	require.NoError(t, err)

	fake2 := llmtest.New("fake", llmtest.Response{Text: "Второй ответ с учётом контекста."})
	asker2 := ask.NewAsker(newRouterWithFake(t, fake2), registry, sessions, testProviderTimeout, 20, 8, nil)
	result, err := asker2.Ask(context.Background(), "alex", "alex", "sess1", "Второй вопрос")
	require.NoError(t, err)
	require.Equal(t, "Второй ответ с учётом контекста.", result.Answer)

	require.Len(t, fake2.Requests, 1)
	msgs := fake2.Requests[0].Messages
	// [0]=system, [1]=first user, [2]=first assistant final, [3]=new user
	require.Len(t, msgs, 4)
	require.Equal(t, "Первый вопрос", msgs[1].Content)
	require.Equal(t, "Первый ответ.", msgs[2].Content)
	require.Equal(t, "Второй вопрос", msgs[3].Content)
}
