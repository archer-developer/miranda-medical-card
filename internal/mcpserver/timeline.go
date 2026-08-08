package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func registerTimelineTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.timeline",
		Description: "Возвращает хронологическую последовательность медицинских событий: анализы, консультации, диагнозы, назначения, операции, вакцинации, самостоятельно зафиксированные события. Не выполняет анализ. См. docs/mcp/06-timeline.md.",
	}, timelineHandler(pl, gate, logger))
}

type TimelineInput struct {
	UserID    string   `json:"userId" jsonschema:"Идентификатор пользователя."`
	SubjectID string   `json:"subjectId,omitempty" jsonschema:"Чью историю получить, если не свою."`
	From      string   `json:"from,omitempty" jsonschema:"Начало периода (YYYY-MM-DD)."`
	To        string   `json:"to,omitempty" jsonschema:"Конец периода (YYYY-MM-DD)."`
	Types     []string `json:"types,omitempty" jsonschema:"Фильтр по типам событий."`
	Limit     int      `json:"limit,omitempty" jsonschema:"Максимальное количество событий."`
}

type TimelineEventOutput struct {
	EventID    string `json:"eventId"`
	Date       string `json:"date"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	DocumentID string `json:"documentId,omitempty"`
}

type TimelineOutput struct {
	Events []TimelineEventOutput `json:"events"`
}

func timelineHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[TimelineInput, TimelineOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in TimelineInput) (*mcp.CallToolResult, TimelineOutput, error) {
		subjectID, err := gate.resolveSubject(in.UserID, in.SubjectID)
		if err != nil {
			return nil, TimelineOutput{}, err
		}

		filter := storage.TimelineFilter{Types: in.Types, Limit: in.Limit}
		if in.From != "" {
			t, err := time.Parse("2006-01-02", in.From)
			if err != nil {
				return nil, TimelineOutput{}, mcpError(codeInvalidEvent, "from must be YYYY-MM-DD, got %q", in.From)
			}
			filter.From = &t
		}
		if in.To != "" {
			t, err := time.Parse("2006-01-02", in.To)
			if err != nil {
				return nil, TimelineOutput{}, mcpError(codeInvalidEvent, "to must be YYYY-MM-DD, got %q", in.To)
			}
			filter.To = &t
		}

		events, err := pl.GetTimeline(ctx, subjectID, filter)
		if err != nil {
			logger.Error("timeline failed", "userId", in.UserID, "subjectId", subjectID, "error", err)
			return nil, TimelineOutput{}, mcpError(codeStorageError, "%v", err)
		}

		items := make([]TimelineEventOutput, len(events))
		for i, e := range events {
			items[i] = TimelineEventOutput{
				EventID: e.ID, Date: e.Date.Format("2006-01-02"), Type: e.Type,
				Title: e.Title, Summary: e.Summary, DocumentID: e.DocumentID,
			}
		}

		logger.Info("timeline", "userId", in.UserID, "subjectId", subjectID, "count", len(items))
		text := fmt.Sprintf("%d event(s).", len(items))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, TimelineOutput{Events: items}, nil
	}
}
