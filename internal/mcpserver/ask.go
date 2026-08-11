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
		Description: "Отвечает на медицинский вопрос пользователя на естественном языке, используя всю доступную медицинскую историю: Timeline, лекарства, диагнозы, анализы, документы. Главный инструмент сервиса для вопросов, требующих анализа, поиска причин или сопоставления данных из нескольких источников — для простого снимка текущего состояния используйте medical.profile, для хронологии событий — medical.timeline.",
	}, askHandler(asker, gate, logger))
}

type AskInput struct {
	UserID    string `json:"userId" jsonschema:"Идентификатор пользователя."`
	SubjectID string `json:"subjectId,omitempty" jsonschema:"О чьих данных вопрос, если не о своих."`
	Question  string `json:"question" jsonschema:"Вопрос пользователя на естественном языке."`
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

		result, err := asker.Ask(ctx, subjectID, in.Question)
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
