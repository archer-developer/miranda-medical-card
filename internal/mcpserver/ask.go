package mcpserver

import (
	"context"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
)

func registerAskTool(server *mcp.Server, asker *ask.Asker, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.ask",
		Description: "Answers the user's medical question in natural language, using the full available medical history: Timeline, medications, diagnoses, lab results, documents. The service's main tool for questions that require analysis, root-cause search, or cross-referencing data from multiple sources — use medical.profile for a plain snapshot of current state, medical.timeline for a chronology of events.",
	}, askHandler(asker, gate, logger))
}

type AskInput struct {
	UserID    string `json:"userId" jsonschema:"User identifier."`
	SubjectID string `json:"subjectId,omitempty" jsonschema:"Whose data the question is about, if not the caller's own."`
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
	SessionID string `json:"sessionId" jsonschema:"Session/conversation id: the calling system's own already-resolved conversation identifier, supplied by that system's backend integration — never invented or guessed by the model itself. Required."`
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
