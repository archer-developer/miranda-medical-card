package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/events"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

// LogEventResult mirrors docs/mcp/07-events.md §3's medical.log_event
// response shape.
type LogEventResult struct {
	EventID          string
	Status           string
	Category         string
	TimelineEventIDs []string
	MedicationIntake *LoggedMedicationIntake // nil unless a medication was recognized
	PlannedAction    *LoggedPlannedAction    // nil unless a future action was recognized
}

// LoggedMedicationIntake mirrors the medicationIntake object in
// docs/mcp/07-events.md §3's example response.
type LoggedMedicationIntake struct {
	DrugName   string
	DoseAmount float64
	DoseUnit   string
}

// LoggedPlannedAction mirrors docs/mcp/07-events.md §3's plannedAction
// response object — see docs/adr/004-planned-actions.md §3 for why this
// goes through medical.log_event rather than a dedicated write tool.
type LoggedPlannedAction struct {
	PlannedActionID string
	Type            string
	Description     string
	DueDateFrom     *time.Time
	DueDateTo       *time.Time
}

// LogEvent implements docs/mcp/07-events.md §3 / docs/architecture/02-processing-pipeline.md
// §17 — the short, text-only Pipeline for a fact the user reports directly,
// without a source document.
//
// Unlike UploadDocument, a Structured Extraction failure here is never
// fatal: docs/mcp/07-events.md §3 is explicit that the user's rawText must
// never be lost just because automatic structuring didn't work (there's no
// fileUri equivalent to fall back to for a self-reported note, unlike
// a document). So the event always reaches status READY; category/
// description simply stay empty when extraction fails.
func (p *Pipeline) LogEvent(ctx context.Context, userID, text string, occurredAt *time.Time) (LogEventResult, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: text must not be empty")
	}

	occurred := time.Now().UTC()
	if occurredAt != nil {
		occurred = *occurredAt
	}

	event, err := p.selfReportedEvents.Add(ctx, storage.SelfReportedEvent{
		UserID: userID, RawText: text, OccurredAt: occurred,
	})
	if err != nil {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: create event: %w", err)
	}
	if err := p.selfReportedEvents.UpdateStatus(ctx, event.ID, userID, storage.DocumentStatusRunning); err != nil {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: %w", err)
	}

	extracted, extractErr := events.Extract(ctx, p.provider, text)
	// extractErr is deliberately not returned — see this function's doc
	// comment. extracted stays its zero value (empty category/description,
	// nil MedicationIntake) when extraction failed, which is exactly the
	// "nothing structured, text preserved" outcome docs/mcp/07-events.md §3
	// requires.
	_ = extractErr

	var medicationIntakeID string
	var loggedIntake *LoggedMedicationIntake
	if extracted.MedicationIntake != nil && strings.TrimSpace(extracted.MedicationIntake.DrugName) != "" {
		// drugName canonicalization mirrors normalization's own (currently
		// whitespace-only, see normalization's canonicalize doc comment for
		// the still-open gap) — duplicated here as a single TrimSpace
		// rather than exporting normalization's helper for one call site.
		drugName := strings.TrimSpace(extracted.MedicationIntake.DrugName)
		intake, err := p.medicationIntakes.Add(ctx, storage.MedicationIntake{
			UserID: userID, SourceType: "self_reported", SourceID: event.ID,
			DrugName: drugName, DoseAmount: extracted.MedicationIntake.DoseAmount,
			DoseUnit: extracted.MedicationIntake.DoseUnit, TakenAt: occurred,
			Reason: extracted.MedicationIntake.Reason,
		})
		if err != nil {
			return LogEventResult{}, fmt.Errorf("pipeline: log event: store medication intake: %w", err)
		}
		medicationIntakeID = intake.ID
		loggedIntake = &LoggedMedicationIntake{DrugName: drugName, DoseAmount: intake.DoseAmount, DoseUnit: intake.DoseUnit}
	}

	var loggedPlan *LoggedPlannedAction
	if extracted.PlannedAction != nil && strings.TrimSpace(extracted.PlannedAction.Description) != "" {
		// anchorDate is occurred (the moment reported, or a backdated
		// occurredAt), not time.Now() — see normalization.BuildPlannedAction's
		// doc comment: the same anchor a document-derived PlannedAction uses
		// is its own document's date, so a self-reported one should anchor
		// on the moment it describes, not the moment this call happens to
		// run.
		built, buildErr := normalization.BuildPlannedAction(ctx, "", userID,
			normalization.PlannedActionSourceSelfReported, event.ID, occurred, *extracted.PlannedAction, p.indicatorAliases)
		if buildErr != nil {
			p.logger.Warn("pipeline: log event: build planned action failed", "eventId", event.ID, "error", buildErr)
		}
		stored, err := p.plannedActions.Add(ctx, built)
		if err != nil {
			return LogEventResult{}, fmt.Errorf("pipeline: log event: store planned action: %w", err)
		}
		loggedPlan = &LoggedPlannedAction{
			PlannedActionID: stored.ID, Type: stored.Type, Description: stored.Description,
			DueDateFrom: stored.DueDateFrom, DueDateTo: stored.DueDateTo,
		}
	}

	if err := p.selfReportedEvents.UpdateExtracted(ctx, event.ID, userID, extracted.Category, extracted.Description, medicationIntakeID); err != nil {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: update extracted: %w", err)
	}

	timelineEventIDs, err := p.buildEventTimeline(ctx, userID, event.ID, text, occurred, extracted, loggedIntake)
	if err != nil {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: timeline: %w", err)
	}

	ftsContent := strings.TrimSpace(text + "\n" + extracted.Description)
	if err := p.fts.IndexEvent(ctx, userID, event.ID, ftsContent); err != nil {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: index fts: %w", err)
	}
	// Best-effort, same reasoning as generateDocumentEmbedding.
	p.generateEventEmbedding(ctx, userID, event.ID, ftsContent)

	if err := p.selfReportedEvents.UpdateStatus(ctx, event.ID, userID, storage.DocumentStatusReady); err != nil {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: %w", err)
	}
	if err := p.rebuildProfile(ctx, userID); err != nil {
		return LogEventResult{}, fmt.Errorf("pipeline: log event: rebuild profile: %w", err)
	}

	return LogEventResult{
		EventID: event.ID, Status: storage.DocumentStatusReady, Category: extracted.Category,
		TimelineEventIDs: timelineEventIDs, MedicationIntake: loggedIntake, PlannedAction: loggedPlan,
	}, nil
}

// buildEventTimeline creates one or two TimelineEvents for a logged event —
// see docs/domain/12-self-reported-events.md §5. A "symptom"-type event
// captures the note itself, made whenever the note isn't purely a
// medication intake report (category != "medication_intake", which also
// covers extraction having failed entirely — the raw note must still be
// visible on the Timeline, not silently dropped). A "medication_taken"
// event is added whenever a medication was recognized, regardless of
// category — the two are independent, matching the headache+ibuprofen
// example in that doc producing both at once.
func (p *Pipeline) buildEventTimeline(ctx context.Context, userID, eventID, rawText string, occurred time.Time, extracted events.Result, intake *LoggedMedicationIntake) ([]string, error) {
	var ids []string

	if extracted.Category != "medication_intake" {
		summary := extracted.Description
		if summary == "" {
			summary = rawText
		}
		title := "Запись пользователя"
		switch extracted.Category {
		case "symptom":
			title = "Симптом"
		case "observation":
			title = "Наблюдение"
		}
		id := "evt_" + eventID + "_symptom"
		if err := p.timelineRepo.Add(ctx, timeline.Event{
			ID: id, UserID: userID, Date: occurred, Type: "symptom",
			Title: title, Summary: summary,
			SourceEntityType: "self_reported_event", SourceEntityID: eventID,
		}); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	if intake != nil {
		summary := intake.DrugName
		if intake.DoseAmount > 0 {
			summary = fmt.Sprintf("%s %g %s", intake.DrugName, intake.DoseAmount, intake.DoseUnit)
		}
		id := "evt_" + eventID + "_intake"
		if err := p.timelineRepo.Add(ctx, timeline.Event{
			ID: id, UserID: userID, Date: occurred, Type: "medication_taken",
			Title: intake.DrugName, Summary: summary,
			SourceEntityType: "medication_intake", SourceEntityID: eventID,
		}); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	return ids, nil
}

// generateEventEmbedding is LogEvent's counterpart to
// generateDocumentEmbedding — see that method's doc comment for the
// best-effort reasoning, which applies identically here.
func (p *Pipeline) generateEventEmbedding(ctx context.Context, userID, eventID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	vector, err := p.embedder.Embed(ctx, text)
	if err != nil {
		p.logger.Warn("pipeline: generate event embedding failed", "eventId", eventID, "error", err)
		return
	}
	err = p.embed.Add(ctx, storage.Embedding{
		UserID: userID, SourceType: "self_reported_event", SourceID: eventID,
		Provider: p.embeddingProvider, ModelVersion: p.embeddingModel, Vector: vector,
	})
	if err != nil {
		p.logger.Warn("pipeline: store event embedding failed", "eventId", eventID, "error", err)
	}
}

// DeleteEvent implements docs/mcp/07-events.md §4: idempotent, and an
// ownership mismatch is indistinguishable from the event not existing (see
// docs/mcp/01-overview.md §11's "не раскрывать существование чужих
// данных").
func (p *Pipeline) DeleteEvent(ctx context.Context, userID, eventID string) (bool, error) {
	event, err := p.selfReportedEvents.Get(ctx, eventID, userID)
	if err != nil {
		if err == storage.ErrNotFound {
			return false, nil
		}
		return false, fmt.Errorf("pipeline: delete event: %w", err)
	}

	if err := p.timelineRepo.RemoveBySourceEntity(ctx, "self_reported_event", eventID); err != nil {
		return false, fmt.Errorf("pipeline: delete event: remove timeline events: %w", err)
	}
	if err := p.timelineRepo.RemoveBySourceEntity(ctx, "medication_intake", eventID); err != nil {
		return false, fmt.Errorf("pipeline: delete event: remove timeline events: %w", err)
	}
	if event.MedicationIntakeID != "" {
		if err := p.medicationIntakes.RemoveBySource(ctx, "self_reported", eventID); err != nil {
			return false, fmt.Errorf("pipeline: delete event: remove medication intake: %w", err)
		}
	}
	// Unconditional (unlike MedicationIntakeID's guard above) — there's no
	// equivalent "did this event produce a plan?" flag stored on
	// SelfReportedEvent itself, and RemoveBySource is a no-op delete when
	// nothing matches, so it's cheap to just always attempt it.
	if err := p.plannedActions.RemoveBySource(ctx, normalization.PlannedActionSourceSelfReported, eventID); err != nil {
		return false, fmt.Errorf("pipeline: delete event: remove planned action: %w", err)
	}
	if err := p.fts.RemoveEvent(ctx, eventID); err != nil {
		return false, fmt.Errorf("pipeline: delete event: remove fts: %w", err)
	}
	if err := p.embed.RemoveBySource(ctx, "self_reported_event", eventID); err != nil {
		return false, fmt.Errorf("pipeline: delete event: remove embedding: %w", err)
	}
	if err := p.selfReportedEvents.Remove(ctx, eventID, userID); err != nil {
		return false, fmt.Errorf("pipeline: delete event: %w", err)
	}
	if err := p.rebuildProfile(ctx, userID); err != nil {
		return false, fmt.Errorf("pipeline: delete event: rebuild profile: %w", err)
	}
	return true, nil
}
