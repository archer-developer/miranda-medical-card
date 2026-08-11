package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

func registerEventTools(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.log_event",
		Description: "Фиксирует медицинский факт, о котором пользователь сообщил напрямую в диалоге, без исходного документа (например 'приступ головной боли, принял ибупрофен'). Передавайте текст ровно так, как его произнёс пользователь, не пытаясь самостоятельно разбирать его на составляющие — категоризация и извлечение лекарства/дозы выполняются сервисом.",
	}, logEventHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.delete_event",
		Description: "Удаляет самостоятельно зафиксированное событие (и связанные с ним записи в Timeline). Идемпотентен: повторный вызов и вызов для чужого/несуществующего eventId одинаково возвращают {\"deleted\": false}, без ошибки.",
	}, deleteEventHandler(pl, gate, logger))
}

// --- medical.log_event ---

type LogEventInput struct {
	UserID     string `json:"userId" jsonschema:"Идентификатор пользователя."`
	Text       string `json:"text" jsonschema:"Текст события на естественном языке, как его сообщил пользователь."`
	OccurredAt string `json:"occurredAt,omitempty" jsonschema:"Момент, когда событие произошло (ISO 8601). По умолчанию — момент вызова."`
}

type MedicationIntakeOutput struct {
	DrugName   string  `json:"drugName"`
	DoseAmount float64 `json:"doseAmount,omitempty"`
	DoseUnit   string  `json:"doseUnit,omitempty"`
}

type LogEventOutput struct {
	EventID          string                  `json:"eventId"`
	Status           string                  `json:"status"`
	Category         string                  `json:"category,omitempty"`
	TimelineEventIDs []string                `json:"timelineEventIds"`
	MedicationIntake *MedicationIntakeOutput `json:"medicationIntake,omitempty"`
}

func logEventHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[LogEventInput, LogEventOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in LogEventInput) (*mcp.CallToolResult, LogEventOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, LogEventOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, LogEventOutput{}, mcpError(codeInvalidEvent, "text must not be empty")
		}

		var occurredAt *time.Time
		if in.OccurredAt != "" {
			t, err := time.Parse(time.RFC3339, in.OccurredAt)
			if err != nil {
				return nil, LogEventOutput{}, mcpError(codeInvalidEvent, "occurredAt must be RFC3339, got %q", in.OccurredAt)
			}
			occurredAt = &t
		}

		result, err := pl.LogEvent(ctx, in.UserID, in.Text, occurredAt)
		if err != nil {
			logger.Error("log_event failed", "userId", in.UserID, "error", err)
			return nil, LogEventOutput{}, mcpError(codeStorageError, "%v", err)
		}

		out := LogEventOutput{EventID: result.EventID, Status: result.Status, Category: result.Category, TimelineEventIDs: result.TimelineEventIDs}
		if result.MedicationIntake != nil {
			out.MedicationIntake = &MedicationIntakeOutput{
				DrugName: result.MedicationIntake.DrugName, DoseAmount: result.MedicationIntake.DoseAmount, DoseUnit: result.MedicationIntake.DoseUnit,
			}
		}
		logger.Info("log_event", "userId", in.UserID, "eventId", out.EventID, "category", out.Category)
		text := fmt.Sprintf("Logged. eventId: %s", out.EventID)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- medical.delete_event ---

type DeleteEventInput struct {
	UserID  string `json:"userId" jsonschema:"Идентификатор пользователя."`
	EventID string `json:"eventId" jsonschema:"Идентификатор события."`
}

type DeleteEventOutput struct {
	Deleted bool `json:"deleted"`
}

func deleteEventHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[DeleteEventInput, DeleteEventOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in DeleteEventInput) (*mcp.CallToolResult, DeleteEventOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, DeleteEventOutput{}, err
		}
		if strings.TrimSpace(in.EventID) == "" {
			return nil, DeleteEventOutput{}, mcpError(codeInvalidEvent, "eventId is required")
		}

		deleted, err := pl.DeleteEvent(ctx, in.UserID, in.EventID)
		if err != nil {
			logger.Error("delete_event failed", "userId", in.UserID, "eventId", in.EventID, "error", err)
			return nil, DeleteEventOutput{}, mcpError(codeStorageError, "%v", err)
		}

		logger.Info("delete_event", "userId", in.UserID, "eventId", in.EventID, "deleted", deleted)
		out := DeleteEventOutput{Deleted: deleted}
		text := "Not found."
		if deleted {
			text = "Deleted."
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}
