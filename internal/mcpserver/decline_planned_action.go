package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

func registerDeclinePlannedActionTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.decline_planned_action",
		Description: "Cancels one of the user's own pending planned actions (see medical.planned_actions), identified by what the user said in conversation rather than an id (e.g. 'отмени прививку от бешенства'). Pass the text exactly as the user said it — the service matches it against their current pending actions itself.",
	}, declinePlannedActionHandler(pl, gate, logger))
}

type DeclinePlannedActionInput struct {
	UserID string `json:"userId" jsonschema:"User identifier."`
	Text   string `json:"text" jsonschema:"What the user said to cancel, in natural language, exactly as reported — not a plannedActionId."`
}

type DeclinePlannedActionOutput struct {
	PlannedActionID string `json:"plannedActionId"`
	Description     string `json:"description"`
	Status          string `json:"status"`
}

func declinePlannedActionHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[DeclinePlannedActionInput, DeclinePlannedActionOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in DeclinePlannedActionInput) (*mcp.CallToolResult, DeclinePlannedActionOutput, error) {
		// No subjectId — like medical.log_event/medical.delete_event, a
		// user can only manage their own planned actions, even against
		// data shared with them read-only (docs/mcp/01-overview.md §11).
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, DeclinePlannedActionOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, DeclinePlannedActionOutput{}, mcpError(codeInvalidEvent, "text must not be empty")
		}

		declined, err := pl.DeclinePlannedAction(ctx, in.UserID, in.Text)
		if err != nil {
			var notFound *pipeline.PlannedActionNotFoundError
			if errors.As(err, &notFound) {
				return nil, DeclinePlannedActionOutput{}, mcpError(codePlannedActionNotFound, "%v", notFound)
			}
			logger.Error("decline_planned_action failed", "userId", in.UserID, "error", err)
			return nil, DeclinePlannedActionOutput{}, mcpError(codeStorageError, "%v", err)
		}

		logger.Info("decline_planned_action", "userId", in.UserID, "plannedActionId", declined.ID)
		out := DeclinePlannedActionOutput{PlannedActionID: declined.ID, Description: declined.Description, Status: declined.Status}
		// Content deliberately left nil — see logEventHandler in events.go.
		return nil, out, nil
	}
}
