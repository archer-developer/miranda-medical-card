package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

func registerCompletePlannedActionTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.complete_planned_action",
		Description: "Marks one of the user's own pending planned actions (see medical.planned_actions) as completed on their own say-so, identified by what the user said in conversation rather than an id (e.g. 'я сделал прививку от бешенства'). Pass the text exactly as the user said it — the service matches it against their current pending actions itself. Use this only when the user reports having done it themselves; a planned action tied to a document (e.g. a lab test) usually completes automatically once that document is uploaded — see medical.planned_actions.",
	}, completePlannedActionHandler(pl, gate, logger))
}

type CompletePlannedActionInput struct {
	UserID string `json:"userId" jsonschema:"User identifier."`
	Text   string `json:"text" jsonschema:"What the user said to mark completed, in natural language, exactly as reported — not a plannedActionId."`
}

type CompletePlannedActionOutput struct {
	PlannedActionID string `json:"plannedActionId"`
	Description     string `json:"description"`
	Status          string `json:"status"`
}

func completePlannedActionHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[CompletePlannedActionInput, CompletePlannedActionOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in CompletePlannedActionInput) (*mcp.CallToolResult, CompletePlannedActionOutput, error) {
		// No subjectId — like medical.decline_planned_action, a user can
		// only manage their own planned actions, even against data shared
		// with them read-only (docs/mcp/01-overview.md §11).
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, CompletePlannedActionOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, CompletePlannedActionOutput{}, mcpError(codeInvalidEvent, "text must not be empty")
		}

		completed, err := pl.CompletePlannedAction(ctx, in.UserID, in.Text)
		if err != nil {
			var notFound *pipeline.PlannedActionNotFoundError
			if errors.As(err, &notFound) {
				return nil, CompletePlannedActionOutput{}, mcpError(codePlannedActionNotFound, "%v", notFound)
			}
			logger.Error("complete_planned_action failed", "userId", in.UserID, "error", err)
			return nil, CompletePlannedActionOutput{}, mcpError(codeStorageError, "%v", err)
		}

		logger.Info("complete_planned_action", "userId", in.UserID, "plannedActionId", completed.ID)
		out := CompletePlannedActionOutput{PlannedActionID: completed.ID, Description: completed.Description, Status: completed.Status}
		// Content deliberately left nil — see logEventHandler in events.go.
		return nil, out, nil
	}
}
