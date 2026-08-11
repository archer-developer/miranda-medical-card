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
		Description: "Records a medical fact the user reported directly in conversation, with no source document (e.g. 'headache, took ibuprofen'). Pass the text exactly as the user said it, without trying to parse it into parts yourself — categorization and medication/dose extraction are done by the service.",
	}, logEventHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.delete_event",
		Description: "Deletes a self-reported event (and its associated Timeline entries). Idempotent: calling it again, or for someone else's/a nonexistent eventId, both return {\"deleted\": false} rather than an error.",
	}, deleteEventHandler(pl, gate, logger))
}

// --- medical.log_event ---

type LogEventInput struct {
	UserID     string `json:"userId" jsonschema:"User identifier."`
	Text       string `json:"text" jsonschema:"The event's text in natural language, exactly as the user reported it."`
	OccurredAt string `json:"occurredAt,omitempty" jsonschema:"When the event happened (ISO 8601). Defaults to the time of the call."`
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
	UserID  string `json:"userId" jsonschema:"User identifier."`
	EventID string `json:"eventId" jsonschema:"Event identifier."`
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
