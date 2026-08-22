package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

func registerResolveDiagnosisTool(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.resolve_diagnosis",
		Description: "Marks one of user's current diagnoses (see medical.profile's activeDiagnoses/chronicConditions) as resolved, identified by what the user said in conversation rather than an id (e.g. 'да, ОРВИ прошла, можешь отметить', 'простуда прошла'). Pass the text exactly as the user said it — the service matches it against their current non-resolved diagnoses itself.",
	}, resolveDiagnosisHandler(pl, gate, logger))
}

type ResolveDiagnosisInput struct {
	UserID string `json:"userId" jsonschema:"User identifier."`
	Text   string `json:"text" jsonschema:"What the user said to confirm resolution, in natural language, exactly as reported — not a diagnosisId."`
}

type ResolveDiagnosisOutput struct {
	DiagnosisID        string `json:"diagnosisId"`
	Name               string `json:"name"`
	Status             string `json:"status"`
	ActualResolutionAt string `json:"actualResolutionAt,omitempty"`
}

func resolveDiagnosisHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[ResolveDiagnosisInput, ResolveDiagnosisOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ResolveDiagnosisInput) (*mcp.CallToolResult, ResolveDiagnosisOutput, error) {
		// No subjectId — like medical.log_event/medical.decline_planned_action,
		// a user can only manage their own diagnoses, even against data
		// shared with them read-only (docs/mcp/01-overview.md §11).
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, ResolveDiagnosisOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, ResolveDiagnosisOutput{}, mcpError(codeInvalidEvent, "text must not be empty")
		}

		resolved, err := pl.ResolveDiagnosis(ctx, in.UserID, in.Text)
		if err != nil {
			var notFound *pipeline.DiagnosisNotFoundError
			if errors.As(err, &notFound) {
				return nil, ResolveDiagnosisOutput{}, mcpError(codeDiagnosisNotFound, "%v", notFound)
			}
			logger.Error("resolve_diagnosis failed", "userId", in.UserID, "error", err)
			return nil, ResolveDiagnosisOutput{}, mcpError(codeStorageError, "%v", err)
		}

		logger.Info("resolve_diagnosis", "userId", in.UserID, "diagnosisId", resolved.ID)
		out := ResolveDiagnosisOutput{
			DiagnosisID: resolved.ID, Name: resolved.Name, Status: resolved.Status,
			ActualResolutionAt: formatOptionalDate(resolved.ActualResolutionAt),
		}
		// Content deliberately left nil — see logEventHandler in events.go.
		return nil, out, nil
	}
}
