package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

func registerStartMedicationTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.start_medication",
		Description: "Marks one of the user's own prescribed-but-not-yet-started medications as actually started, identified by what the user said in conversation rather than an id (e.g. 'я начал принимать антибиотик', 'начал пить витамины со вчерашнего дня'). Pass the text exactly as the user said it — the service matches it against their current prescribed medications itself. Use this when the user reports having actually begun taking a drug that was only prescribed so far — see medical.profile's activeMedications, which only lists medications already confirmed started, not merely prescribed ones.",
	}, startMedicationHandler(pl, gate, logger))
}

type StartMedicationInput struct {
	UserID string `json:"userId" jsonschema:"User identifier."`
	Text   string `json:"text" jsonschema:"What the user said to confirm intake started, in natural language, exactly as reported — not a medicationId."`
}

type StartMedicationOutput struct {
	MedicationID string `json:"medicationId"`
	DrugName     string `json:"drugName"`
	Status       string `json:"status"`
	StartedAt    string `json:"startedAt,omitempty"`
}

func startMedicationHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[StartMedicationInput, StartMedicationOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in StartMedicationInput) (*mcp.CallToolResult, StartMedicationOutput, error) {
		// No subjectId — like medical.log_event/medical.complete_medication, a
		// user can only manage their own medications, even against data
		// shared with them read-only (docs/mcp/01-overview.md §11).
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, StartMedicationOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, StartMedicationOutput{}, mcpError(codeInvalidEvent, "text must not be empty")
		}

		started, err := pl.StartMedication(ctx, in.UserID, in.Text)
		if err != nil {
			var notFound *pipeline.MedicationNotFoundError
			if errors.As(err, &notFound) {
				return nil, StartMedicationOutput{}, mcpError(codeMedicationNotFound, "%v", notFound)
			}
			logger.Error("start_medication failed", "userId", in.UserID, "error", err)
			return nil, StartMedicationOutput{}, mcpError(codeStorageError, "%v", err)
		}

		logger.Info("start_medication", "userId", in.UserID, "medicationId", started.ID)
		out := StartMedicationOutput{
			MedicationID: started.ID, DrugName: started.DrugName, Status: string(started.Status),
			StartedAt: formatOptionalDate(started.StartedAt),
		}
		// Content deliberately left nil — see logEventHandler in events.go.
		return nil, out, nil
	}
}
