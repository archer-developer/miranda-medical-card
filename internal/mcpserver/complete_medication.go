package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

func registerCompleteMedicationTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.complete_medication",
		Description: "Marks one of the user's own currently active medications (see medical.profile's activeMedications) as finished, identified by what the user said in conversation rather than an id (e.g. 'я закончил принимать курс антибиотиков', 'закончил пить витамины'). Pass the text exactly as the user said it — the service matches it against their current active medications itself.",
	}, completeMedicationHandler(pl, gate, logger))
}

type CompleteMedicationInput struct {
	UserID string `json:"userId" jsonschema:"User identifier."`
	Text   string `json:"text" jsonschema:"What the user said to mark the medication finished, in natural language, exactly as reported — not a medicationId."`
}

type CompleteMedicationOutput struct {
	MedicationID string `json:"medicationId"`
	DrugName     string `json:"drugName"`
	Status       string `json:"status"`
	EndedAt      string `json:"endedAt,omitempty"`
}

func completeMedicationHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[CompleteMedicationInput, CompleteMedicationOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in CompleteMedicationInput) (*mcp.CallToolResult, CompleteMedicationOutput, error) {
		// No subjectId — like medical.log_event/medical.resolve_diagnosis, a
		// user can only manage their own medications, even against data
		// shared with them read-only (docs/mcp/01-overview.md §11).
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, CompleteMedicationOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, CompleteMedicationOutput{}, mcpError(codeInvalidEvent, "text must not be empty")
		}

		completed, err := pl.CompleteMedication(ctx, in.UserID, in.Text)
		if err != nil {
			var notFound *pipeline.MedicationNotFoundError
			if errors.As(err, &notFound) {
				return nil, CompleteMedicationOutput{}, mcpError(codeMedicationNotFound, "%v", notFound)
			}
			logger.Error("complete_medication failed", "userId", in.UserID, "error", err)
			return nil, CompleteMedicationOutput{}, mcpError(codeStorageError, "%v", err)
		}

		logger.Info("complete_medication", "userId", in.UserID, "medicationId", completed.ID)
		out := CompleteMedicationOutput{
			MedicationID: completed.ID, DrugName: completed.DrugName, Status: completed.Status,
			EndedAt: formatOptionalDate(completed.EndedAt),
		}
		// Content deliberately left nil — see logEventHandler in events.go.
		return nil, out, nil
	}
}
