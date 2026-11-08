package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
)

func registerAskTool(server *mcp.Server, asker *ask.Asker, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.ask",
		InputSchema: askInputSchema(),
		Description: "Answers the user's medical question in natural language, using the full available medical history: " +
			"Timeline, medications, diagnoses, lab results, documents. The service's main tool for questions that require analysis, " +
			"root-cause search, or cross-referencing data from multiple sources. Use it whenever answering needs medical judgment — " +
			"whether a value or trend is normal, what it means, possible causes, what to do — and relay its answer to the user " +
			"verbatim: the caller must not interpret medical data itself. Merely showing, listing or counting records, or finding the " +
			"latest one, needs no judgment — medical.timeline/medical.profile return that data directly.",
	}, askHandler(asker, gate, logger))
}

// injectedKeyword marks a tool argument the calling system's backend fills in
// itself, not the model — Miranda hides such an argument from its model's
// view of the schema and injects the value (miranda's mcp.InjectedKeyword;
// miranda/docs/adr/medical-card-session-injection.md §6). Declared here, by
// the server that owns the argument, instead of in Miranda's per-server
// config.
const injectedKeyword = "x-injected"

// askInputSchema is AskInput's inferred schema with sessionId marked
// `x-injected: conversation_id`. Without the mark the calling model sees a
// required sessionId it can't know: Claude (Miranda's claude_code
// orchestrator) refused to call medical.ask at all over it (2026-11-08).
func askInputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[AskInput](nil)
	if err != nil {
		panic(fmt.Sprintf("mcpserver: infer medical.ask input schema: %v", err)) // static type: can only fail at development time
	}
	prop := schema.Properties["sessionId"]
	if prop.Extra == nil {
		prop.Extra = map[string]any{}
	}
	prop.Extra[injectedKeyword] = "conversation_id"
	return schema
}

type AskInput struct {
	UserID    string `json:"userId" jsonschema:"User identifier."`
	SubjectID string `json:"subjectId,omitempty" jsonschema:"Whose data the question is about, if not the caller's own. Must be UserID (Miranda username) of the target user — the same identifier used for userId elsewhere (e.g. \"anna\"). Omit to default to the caller's own data."`
	Question  string `json:"question" jsonschema:"The user's question in natural language."`
	// SessionID is injected by Miranda's own backend dispatch layer with its
	// resolved conversation id (the same mechanism it already uses to
	// inject miranda-diary's encryption key — see setSessionIDArg /
	// setEncryptionKeyArg in miranda/internal/httpapi/agent_loop.go; see
	// miranda/docs/medical-card-session-injection.md for the exact
	// contract). That injection unconditionally overwrites whatever the
	// calling model put here before the request ever reaches this service,
	// so requiring the field is safe — it can never force the model itself
	// to fabricate a value; Miranda's backend supplies the real one
	// regardless of what (if anything) the model wrote. No omitempty: an
	// omitted or empty sessionId on a real Miranda-routed call means
	// Miranda's own injection config for this tool has drifted (see
	// askHandler's explicit empty check below, since JSON Schema
	// "required" only guarantees presence, not non-emptiness) — that
	// should fail loudly here rather than silently degrade to a stateless
	// answer, which could go unnoticed for a long time. The only caller
	// exempt from this is medical-dev's `ask` CLI (cmd/medical-dev/main.go),
	// which calls ask.Asker.Ask directly in Go, bypassing this MCP schema
	// entirely — see that Asker.Ask's own doc comment for why an empty
	// sessionID is still a valid *Go-level* argument.
	SessionID string `json:"sessionId" jsonschema:"Session/conversation id: the calling system's own already-resolved conversation identifier, filled in by that system's backend (marked x-injected) — never by the model."`
}

type AskSourceOutput struct {
	DocumentID string `json:"documentId,omitempty"`
	EventID    string `json:"eventId,omitempty"`
	Title      string `json:"title,omitempty"`
	Excerpt    string `json:"excerpt,omitempty"`
}

type AskOutput struct {
	Answer  string            `json:"answer"`
	Sources []AskSourceOutput `json:"sources"`
}

func askHandler(asker *ask.Asker, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[AskInput, AskOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in AskInput) (*mcp.CallToolResult, AskOutput, error) {
		subjectID, err := gate.resolveSubject(in.UserID, in.SubjectID)
		if err != nil {
			return nil, AskOutput{}, err
		}
		if strings.TrimSpace(in.Question) == "" {
			return nil, AskOutput{}, mcpError(codeInvalidEvent, "question must not be empty")
		}
		// JSON Schema "required" (see AskInput.SessionID) only guarantees
		// the field is present, not non-empty — an explicit "" still needs
		// this separate check to actually catch a misconfigured caller.
		if strings.TrimSpace(in.SessionID) == "" {
			return nil, AskOutput{}, mcpError(codeInvalidEvent, "sessionId must not be empty")
		}

		result, err := asker.Ask(ctx, in.UserID, subjectID, in.SessionID, in.Question)
		if err != nil {
			logger.Error("ask failed", "userId", in.UserID, "subjectId", subjectID, "error", err)
			return nil, AskOutput{}, mcpError(codePipelineFailed, "%v", err)
		}

		sources := make([]AskSourceOutput, len(result.Sources))
		for i, s := range result.Sources {
			sources[i] = AskSourceOutput{DocumentID: s.DocumentID, EventID: s.EventID, Title: s.Title, Excerpt: s.Excerpt}
		}
		logger.Info("ask", "userId", in.UserID, "subjectId", subjectID, "sources", len(sources))
		out := AskOutput{Answer: result.Answer, Sources: sources}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.Answer}}}, out, nil
	}
}
