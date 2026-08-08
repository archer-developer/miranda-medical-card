// Package timeline implements TimelineBuilder (docs/domain/04-timeline.md
// §6, docs/domain/01-overview.md §7) — a pure Domain Service, no I/O, no
// LLM: given the entities Normalization just produced for one document, it
// returns the TimelineEvent rows that should replace whatever previously
// existed for that document (see docs/architecture/02-processing-pipeline.md
// §7).
package timeline

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
)

// Event mirrors docs/domain/04-timeline.md §2.
type Event struct {
	ID               string
	UserID           string
	Date             time.Time
	Type             string
	Title            string
	Summary          string
	DocumentID       string
	SourceEntityType string
	SourceEntityID   string
}

// procedureTimelineType maps Procedure.Type to TimelineEvent.Type per
// docs/domain/08-procedure.md §4.
var procedureTimelineType = map[string]string{
	"surgery":         "surgery",
	"hospitalization": "hospitalization",
	"vaccination":     "vaccination",
	"consultation":    "consultation",
	"examination":     "procedure",
	"other":           "procedure",
}

var vitalSignLabels = map[string]string{
	"blood_pressure": "Давление",
	"weight":         "Вес",
	"height":         "Рост",
	"pulse":          "Пульс",
	"temperature":    "Температура",
}

// Build returns the TimelineEvent set for one document's just-normalized
// entities — the full replacement set a caller should pass to
// TimelineRepository after first calling RemoveByDocument, mirroring the
// document-scoped replace semantics used throughout Normalization/Storage
// (see docs/architecture/02-processing-pipeline.md §6).
//
// documentTitle/documentDate come from the MedicalDocument the caller has
// already built (see pipeline.buildTitle) — reused here rather than
// recomputed, and used as the fallback date for any entity missing its own
// (e.g. a Diagnosis with no diagnosedAt still needs *a* date, since
// TimelineEvent.date is required).
//
// LabResult is handled differently from every other entity type: instead of
// one event per LabResult (which would flood the timeline with 20-40
// entries for a single blood draw), every LabResult in the document is
// grouped into one TimelineEvent — this matches
// docs/domain/04-timeline.md §2's own example ("Общий анализ крови...
// Выявлено небольшое повышение ALT"), which describes a panel-level event,
// not a per-indicator one. Every other entity type has low per-document
// cardinality (a handful of diagnoses/medications/procedures/vitals at
// most), so one event per entity, as the architecture doc's literal wording
// says, doesn't have the same flooding problem.
func Build(userID, documentID, documentTitle string, documentDate *time.Time, normalized normalization.Result) []Event {
	var events []Event
	seq := 0
	nextID := func() string {
		seq++
		return fmt.Sprintf("evt_%s_%d", documentID, seq)
	}
	resolveDate := func(d *time.Time) (time.Time, bool) {
		if d != nil {
			return *d, true
		}
		if documentDate != nil {
			return *documentDate, true
		}
		return time.Time{}, false
	}

	for _, d := range normalized.Diagnoses {
		date, ok := resolveDate(d.DiagnosedAt)
		if !ok {
			continue
		}
		summary := d.Name
		if d.Notes != "" {
			summary = d.Name + ". " + d.Notes
		}
		events = append(events, Event{
			ID: nextID(), UserID: userID, Date: date, Type: "diagnosis",
			Title: d.Name, Summary: summary, DocumentID: documentID,
			SourceEntityType: "diagnosis", SourceEntityID: d.ID,
		})
	}

	for _, m := range normalized.Medications {
		eventType := "medication_started"
		date, ok := resolveDate(m.StartedAt)
		if m.Status == "discontinued" || m.Status == "completed" {
			eventType = "medication_stopped"
			date, ok = resolveDate(m.EndedAt)
			if !ok {
				date, ok = resolveDate(m.StartedAt)
			}
		}
		// A within-document dose change (medication_changed, see
		// docs/domain/06-medication.md §4) would require pairing an "old"
		// and "new" Medication entry for the same drug in one document —
		// extraction.Result's flat medication list gives no such pairing
		// signal, so this case is deliberately not detected here; every
		// Medication becomes started/stopped only.
		if !ok {
			continue
		}
		title := m.DrugName
		summary := m.DrugName
		if m.DoseAmount > 0 {
			summary = fmt.Sprintf("%s %g %s", m.DrugName, m.DoseAmount, m.DoseUnit)
		}
		if m.Frequency != "" {
			summary += ", " + m.Frequency
		}
		events = append(events, Event{
			ID: nextID(), UserID: userID, Date: date, Type: eventType,
			Title: title, Summary: summary, DocumentID: documentID,
			SourceEntityType: "medication", SourceEntityID: m.ID,
		})
	}

	for _, p := range normalized.Procedures {
		date, ok := resolveDate(p.PerformedAt)
		if !ok {
			continue
		}
		eventType, known := procedureTimelineType[p.Type]
		if !known {
			eventType = "procedure"
		}
		summary := p.Name
		if p.Notes != "" {
			summary = p.Name + ". " + p.Notes
		}
		events = append(events, Event{
			ID: nextID(), UserID: userID, Date: date, Type: eventType,
			Title: p.Name, Summary: summary, DocumentID: documentID,
			SourceEntityType: "procedure", SourceEntityID: p.ID,
		})
	}

	for _, v := range normalized.VitalSigns {
		date, ok := resolveDate(v.MeasuredAt)
		if !ok {
			continue
		}
		title := vitalSignLabels[v.Type]
		if title == "" {
			title = v.Type
		}
		var summary string
		if v.Type == "blood_pressure" {
			summary = fmt.Sprintf("%s: %g/%g mmHg", title, v.Systolic, v.Diastolic)
		} else {
			summary = fmt.Sprintf("%s: %g %s", title, v.Value, v.Unit)
		}
		events = append(events, Event{
			ID: nextID(), UserID: userID, Date: date, Type: "vital_sign",
			Title: title, Summary: summary, DocumentID: documentID,
			SourceEntityType: "vital_sign", SourceEntityID: v.ID,
		})
	}

	if len(normalized.LabResults) > 0 {
		if event, ok := buildLabResultsEvent(userID, documentID, documentTitle, documentDate, normalized.LabResults, nextID); ok {
			events = append(events, event)
		}
	}

	// Allergy generates a generic "document" event rather than a dedicated
	// type — see docs/domain/07-diagnosis-and-allergy.md §3 "Timeline". One
	// event per document (not per allergy), consistent with the same
	// no-flooding reasoning as LabResults; a document rarely lists more
	// than one or two allergies anyway, but the summary lists all of them
	// together for a single, readable event.
	if len(normalized.Allergies) > 0 {
		date, ok := resolveDate(nil)
		if ok {
			names := make([]string, len(normalized.Allergies))
			for i, a := range normalized.Allergies {
				names[i] = a.Substance
			}
			events = append(events, Event{
				ID: nextID(), UserID: userID, Date: date, Type: "document",
				Title: "Аллергии", Summary: "Зафиксированы аллергии: " + strings.Join(names, ", "),
				DocumentID: documentID,
			})
		}
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].Date.Before(events[j].Date) })
	return events
}

// buildLabResultsEvent groups every LabResult in the document into one
// TimelineEvent — see Build's doc comment. Prefers the most common TakenAt
// among the results (falls back to documentDate) since a panel drawn on one
// visit occasionally has one or two results dated slightly differently
// (e.g. a same-day repeat test) without that being a reason to split the
// whole panel into separate events.
func buildLabResultsEvent(userID, documentID, documentTitle string, documentDate *time.Time, results []normalization.LabResult, nextID func() string) (Event, bool) {
	dateCounts := map[time.Time]int{}
	for _, r := range results {
		if r.TakenAt != nil {
			dateCounts[*r.TakenAt]++
		}
	}
	var date time.Time
	best := 0
	for d, count := range dateCounts {
		if count > best {
			best, date = count, d
		}
	}
	if best == 0 {
		if documentDate == nil {
			return Event{}, false
		}
		date = *documentDate
	}

	var notable []string
	for _, r := range results {
		flag := labFlag(r)
		if flag == "" {
			continue
		}
		notable = append(notable, fmt.Sprintf("%s %g %s (%s)", r.IndicatorName, r.Value, r.Unit, flag))
		if len(notable) == 3 {
			break
		}
	}

	summary := fmt.Sprintf("%d показателей.", len(results))
	if len(notable) > 0 {
		summary += " " + strings.Join(notable, "; ") + "."
	}

	title := documentTitle
	if title == "" {
		title = "Лабораторное исследование"
	}
	return Event{
		ID: nextID(), UserID: userID, Date: date, Type: "lab_result",
		Title: title, Summary: summary, DocumentID: documentID,
	}, true
}

// labFlag reports whether r.Value falls outside r.ReferenceLow/ReferenceHigh
// — computed here for the Timeline summary text only (mirroring
// docs/domain/11-value-objects.md §3's LabValue.flag, "вычисляется
// относительно referenceLow/referenceHigh, если оба заданы; иначе не
// заполняется"), not persisted anywhere, since it's a cheap derived
// presentation detail rather than a fact worth storing.
func labFlag(r normalization.LabResult) string {
	if r.ReferenceLow == 0 && r.ReferenceHigh == 0 {
		return ""
	}
	if r.ReferenceHigh != 0 && r.Value > r.ReferenceHigh {
		return "повышен"
	}
	if r.ReferenceLow != 0 && r.Value < r.ReferenceLow {
		return "понижен"
	}
	return ""
}
