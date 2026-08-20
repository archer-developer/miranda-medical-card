package mcpserver

import (
	"context"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

func registerPlannedActionsTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.planned_actions",
		Description: "Returns future medical actions extracted from documents' recommendations or logged directly in conversation — repeat tests, vaccinations, follow-up visits — each with an approximate due-date range (or none, if the source stated no timeframe) and a status that is set automatically once a later document's data satisfies it, never manually. Use this to build reminders for the user. Does not perform analysis — data only, unlike medical.ask.",
	}, plannedActionsHandler(pl, gate, logger))
}

type PlannedActionsInput struct {
	UserID          string `json:"userId" jsonschema:"User identifier."`
	SubjectID       string `json:"subjectId,omitempty" jsonschema:"Whose plan to fetch, if not the caller's own. Must be that household member's own user_id — the same identifier used for userId elsewhere (e.g. \"anna\"), never a display name like \"Аня\". Omit to default to the caller's own data."`
	IncludeResolved bool   `json:"includeResolved,omitempty" jsonschema:"Include already completed or declined actions. Default: only pending ones (including overdue)."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Maximum number of actions."`
}

type PlannedActionOutput struct {
	PlannedActionID   string `json:"plannedActionId"`
	Type              string `json:"type"`
	Description       string `json:"description"`
	DueDateFrom       string `json:"dueDateFrom,omitempty"`
	DueDateTo         string `json:"dueDateTo,omitempty"`
	Overdue           bool   `json:"overdue"`
	Status            string `json:"status"`
	SourceType        string `json:"sourceType"`
	SourceID          string `json:"sourceId"`
	MatchedDocumentID string `json:"matchedDocumentId,omitempty"`
	MatchedAt         string `json:"matchedAt,omitempty"`
}

type PlannedActionsOutput struct {
	Actions []PlannedActionOutput `json:"actions"`
}

func plannedActionsHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[PlannedActionsInput, PlannedActionsOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in PlannedActionsInput) (*mcp.CallToolResult, PlannedActionsOutput, error) {
		subjectID, err := gate.resolveSubject(in.UserID, in.SubjectID)
		if err != nil {
			return nil, PlannedActionsOutput{}, err
		}

		actions, err := pl.GetUpcomingPlan(ctx, subjectID, in.IncludeResolved, in.Limit)
		if err != nil {
			logger.Error("planned_actions failed", "userId", in.UserID, "subjectId", subjectID, "error", err)
			return nil, PlannedActionsOutput{}, mcpError(codeStorageError, "%v", err)
		}

		now := time.Now()
		items := make([]PlannedActionOutput, len(actions))
		for i, a := range actions {
			items[i] = PlannedActionOutput{
				PlannedActionID: a.ID, Type: a.Type, Description: a.Description,
				DueDateFrom: formatOptionalDate(a.DueDateFrom), DueDateTo: formatOptionalDate(a.DueDateTo),
				Overdue: a.Overdue(now), Status: a.Status,
				SourceType: a.SourceType, SourceID: a.SourceID,
				MatchedDocumentID: a.MatchedDocumentID, MatchedAt: formatOptionalDate(a.MatchedAt),
			}
		}

		logger.Info("planned_actions", "userId", in.UserID, "subjectId", subjectID, "count", len(items))
		// Content deliberately left nil so the MCP SDK serializes the full
		// actions array as Content — same StructuredContent-mirroring
		// invariant every other read tool relies on, see timeline.go.
		return nil, PlannedActionsOutput{Actions: items}, nil
	}
}
