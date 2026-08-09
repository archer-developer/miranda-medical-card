package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	llm "github.com/archer-developer/miranda-llm"
)

// structuredWithEscalation calls provider.Structured(ctx, req); if that
// returns a hard error and escalate is non-nil, retries once against
// escalate instead of failing the whole request — the same "ask a
// different model/provider once" posture as
// extraction.StructuredWithRetry's own escalation, but simpler: Plan and
// GenerateAnswer's schemas have no natural "successful but suspiciously
// empty" shape the way LabResult categories do (an empty providers array
// from Plan is a legitimate answer — "no medical history lookup needed" —
// not a failure signal), so this only escalates on a genuine error, never
// on content. escalate may be nil to disable this entirely, e.g. when the
// corresponding llm.yaml provider has no escalation configured (see
// cmd/miranda-medical-card/main.go's resolveEscalationProvider). A nil
// logger falls back to slog.Default().
func structuredWithEscalation(ctx context.Context, provider, escalate StructuredProvider, req llm.StructuredRequest, stage string, logger *slog.Logger) (json.RawMessage, error) {
	if logger == nil {
		logger = slog.Default()
	}
	raw, err := provider.Structured(ctx, req)
	if err == nil {
		return raw, nil
	}
	if escalate == nil {
		return nil, err
	}
	logger.Warn("ask: provider failed, escalating to a different provider", "stage", stage, "error", err)
	raw, escErr := escalate.Structured(ctx, req)
	if escErr != nil {
		return nil, fmt.Errorf("%s: primary failed: %w; escalation also failed: %w", stage, err, escErr)
	}
	return raw, nil
}
