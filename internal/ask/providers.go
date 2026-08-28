// This file implements every structured-data Provider (docs/architecture/
// 03-knowledge-providers.md §8's Registry, minus Documents/Embeddings —
// see search_providers.go for those). Each one is a thin adapter: read
// the relevant storage.*Repository, map to []KnowledgeChunk. Confidence
// values follow docs/architecture/04-search.md §18's scale — 1.00 for
// document-derived structured facts, 0.90 for self-reported ones (only
// TimelineProvider distinguishes the two, since it's the only provider
// whose rows can come from either source).
package ask

import (
	"context"
	"fmt"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

const defaultProviderLimit = 30

func limitOrDefault(n int) int {
	if n <= 0 {
		return defaultProviderLimit
	}
	return n
}

func formatDate(t *time.Time) string {
	if t == nil {
		return "дата неизвестна"
	}
	return t.Format("2006-01-02")
}

// --- Timeline ---

type TimelineProvider struct{ repo storage.TimelineRepository }

func NewTimelineProvider(repo storage.TimelineRepository) *TimelineProvider {
	return &TimelineProvider{repo: repo}
}

func (p *TimelineProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "timeline",
		Description: "Хронология медицинских событий: анализы, консультации, диагнозы, назначения и отмены лекарств, " +
			"операции, госпитализации, вакцинации, самостоятельно зафиксированные симптомы и приёмы лекарств. " +
			"Используйте для вопросов о последовательности событий, периодах времени ('прошлой зимой', 'после операции') " +
			"и для определения, когда что-то произошло впервые.",
	}
}

func (p *TimelineProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	events, err := p.repo.List(ctx, req.UserID, storage.TimelineFilter{From: req.From, To: req.To, Limit: limitOrDefault(req.Limit)})
	if err != nil {
		return nil, fmt.Errorf("ask: timeline provider: %w", err)
	}
	chunks := make([]KnowledgeChunk, len(events))
	for i, e := range events {
		confidence := 1.0
		var eventID string
		if e.DocumentID == "" {
			// self-reported (symptom/medication_taken) — see
			// docs/domain/12-self-reported-events.md §6.
			confidence = 0.90
			eventID = e.SourceEntityID
		}
		chunks[i] = KnowledgeChunk{
			Source: "timeline", Title: e.Title, Confidence: confidence,
			Content:    fmt.Sprintf("%s: %s. %s", e.Date.Format("2006-01-02"), e.Title, e.Summary),
			DocumentID: e.DocumentID, EventID: eventID,
		}
	}
	return chunks, nil
}

// --- Self-Reported Events ---

// selfReportedEventTypes are the timeline.Event.Type values that originate
// from medical.log_event rather than a document — see
// docs/domain/12-self-reported-events.md §5's TimelineEvent.type mapping
// (symptom/observation -> "symptom", self-reported medication intake ->
// "medication_taken"). TimelineProvider itself surfaces every event type
// mixed together with no way to filter to just these; this provider exists
// so a question specifically about the user's own reported symptoms/intake
// doesn't have to wade through document-derived events to find them.
var selfReportedEventTypes = []string{"symptom", "medication_taken"}

type SelfReportedEventProvider struct{ repo storage.TimelineRepository }

func NewSelfReportedEventProvider(repo storage.TimelineRepository) *SelfReportedEventProvider {
	return &SelfReportedEventProvider{repo: repo}
}

func (p *SelfReportedEventProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "self_reported_events",
		Description: "Самостоятельно зафиксированные пользователем события (medical.log_event): симптомы, " +
			"самочувствие, наблюдения, а также самостоятельно отмеченные приёмы лекарств. Это неверифицируемые " +
			"записи со слов пользователя, в отличие от документально подтверждённых фактов. Используйте для " +
			"вопросов о жалобах и самочувствии пользователя ('когда болела голова', 'как часто бывает изжога'), " +
			"а не для назначений врача (см. medications) или лабораторных анализов.",
	}
}

func (p *SelfReportedEventProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	events, err := p.repo.List(ctx, req.UserID, storage.TimelineFilter{
		From: req.From, To: req.To, Limit: limitOrDefault(req.Limit), Types: selfReportedEventTypes,
	})
	if err != nil {
		return nil, fmt.Errorf("ask: self-reported event provider: %w", err)
	}
	chunks := make([]KnowledgeChunk, len(events))
	for i, e := range events {
		// Always self-reported by construction (Types filter above), unlike
		// TimelineProvider which mixes sources and branches on DocumentID —
		// see docs/domain/12-self-reported-events.md §6 for the 0.90 scale.
		chunks[i] = KnowledgeChunk{
			Source: "self_reported_events", Title: e.Title, Confidence: 0.90,
			Content: fmt.Sprintf("%s: %s. %s", e.Date.Format("2006-01-02"), e.Title, e.Summary),
			EventID: e.SourceEntityID,
		}
	}
	return chunks, nil
}

// --- Medication ---

type MedicationProvider struct{ repo storage.MedicationRepository }

func NewMedicationProvider(repo storage.MedicationRepository) *MedicationProvider {
	return &MedicationProvider{repo: repo}
}

func (p *MedicationProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "medications",
		Description: "Информация о назначенных лекарствах: дозировки, курсы лечения, даты начала/отмены, " +
			"статус (действующий/отменённый/завершённый), причина назначения. Возвращает сырые записи по всем " +
			"документам — используйте для вопросов о конкретных препаратах, истории назначений, пересечении курсов.",
	}
}

func (p *MedicationProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	meds, err := p.repo.ListByUser(ctx, req.UserID, storage.MedicationFilter{})
	if err != nil {
		return nil, fmt.Errorf("ask: medication provider: %w", err)
	}
	chunks := make([]KnowledgeChunk, 0, len(meds))
	for _, m := range meds {
		content := fmt.Sprintf("%s. Статус: %s.", m.DrugName, m.Status)
		if m.DoseAmount > 0 {
			content = fmt.Sprintf("%s %g %s. Статус: %s.", m.DrugName, m.DoseAmount, m.DoseUnit, m.Status)
		}
		if m.StartedAt != nil {
			content += fmt.Sprintf(" Начат: %s.", formatDate(m.StartedAt))
		}
		if m.EndedAt != nil {
			content += fmt.Sprintf(" Окончен/отменён: %s.", formatDate(m.EndedAt))
		}
		if m.Reason != "" {
			content += " Причина: " + m.Reason + "."
		}
		chunks = append(chunks, KnowledgeChunk{
			Source: "medications", Title: m.DrugName, Content: content,
			Confidence: 1.0, DocumentID: m.DocumentID,
		})
	}
	return chunks, nil
}

// --- Diagnosis ---

type DiagnosisProvider struct{ repo storage.DiagnosisRepository }

func NewDiagnosisProvider(repo storage.DiagnosisRepository) *DiagnosisProvider {
	return &DiagnosisProvider{repo: repo}
}

func (p *DiagnosisProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "diagnoses",
		Description: "Диагнозы: название, код (МКБ-10, если указан), дата постановки, статус " +
			"(предполагаемый/действующий/хронический/снят/заменён более точным диагнозом), и для некоторых активных диагнозов — оценочная дата " +
			"разрешения (не из документа, общая медицинская оценка по типу диагноза, не факт) вместе с пометкой, " +
			"если этот срок уже прошёл. Используйте для вопросов об истории диагнозов, хронических заболеваниях, " +
			"дате первой постановки диагноза, и о том, не пора ли перепроверить диагноз, который должен был пройти.",
	}
}

func (p *DiagnosisProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	diagnoses, err := p.repo.ListByUser(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("ask: diagnosis provider: %w", err)
	}
	now := time.Now()
	chunks := make([]KnowledgeChunk, len(diagnoses))
	for i, d := range diagnoses {
		content := fmt.Sprintf("%s. Статус: %s.", d.Name, d.Status)
		if d.Code != "" {
			content = fmt.Sprintf("%s (%s). Статус: %s.", d.Name, d.Code, d.Status)
		}
		if d.DiagnosedAt != nil {
			content += fmt.Sprintf(" Поставлен: %s.", formatDate(d.DiagnosedAt))
		}
		// ExpectedResolutionTo is only meaningful while the diagnosis is
		// still active — medical.resolve_diagnosis deliberately leaves it
		// set after resolving (see storage.DiagnosisRepository.MarkResolved's
		// doc comment: it's a historical estimate, not overwritten by the
		// actual outcome), so a resolved diagnosis can still carry a stale
		// one that must not be presented as still pending.
		if d.Status == "active" && d.ExpectedResolutionTo != nil {
			// Explicitly labeled as an estimate, not a documented fact — this
			// comes from the extraction model's general medical knowledge of
			// the condition's typical course, not anything the source
			// document itself states (see extraction.Schema's
			// expectedResolutionAmount* description).
			content += fmt.Sprintf(" Ожидаемое разрешение (оценочно, не из документа): к %s.", formatDate(d.ExpectedResolutionTo))
			// d.Overdue never flips Status itself (see that method's doc
			// comment) — surfaced here only as a prompt for the model to
			// suggest the patient re-check, not as a claim the diagnosis
			// resolved or didn't.
			if d.Overdue(now) {
				content += " Ожидаемый срок уже прошёл, а диагноз всё ещё числится действующим — возможно, стоит уточнить у врача."
			}
		}
		if d.ActualResolutionAt != nil {
			content += fmt.Sprintf(" Пациент подтвердил разрешение: %s.", formatDate(d.ActualResolutionAt))
		}
		chunks[i] = KnowledgeChunk{Source: "diagnoses", Title: d.Name, Content: content, Confidence: 1.0, DocumentID: d.DocumentID}
	}
	return chunks, nil
}

// --- Procedure ---

type ProcedureProvider struct{ repo storage.ProcedureRepository }

func NewProcedureProvider(repo storage.ProcedureRepository) *ProcedureProvider {
	return &ProcedureProvider{repo: repo}
}

func (p *ProcedureProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "procedures",
		Description: "Медицинские процедуры: операции, обследования, госпитализации, вакцинации, консультации — " +
			"название, дата, врач/клиника, заключение. Используйте для вопросов об операциях, обследованиях, вакцинациях.",
	}
}

func (p *ProcedureProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	procedures, err := p.repo.ListByUser(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("ask: procedure provider: %w", err)
	}
	chunks := make([]KnowledgeChunk, len(procedures))
	for i, pr := range procedures {
		content := fmt.Sprintf("%s (%s).", pr.Name, formatDate(pr.PerformedAt))
		if pr.PerformedBy != "" {
			content += " " + pr.PerformedBy + "."
		}
		if pr.Notes != "" {
			content += " " + pr.Notes
		}
		chunks[i] = KnowledgeChunk{Source: "procedures", Title: pr.Name, Content: content, Confidence: 1.0, DocumentID: pr.DocumentID}
	}
	return chunks, nil
}

// --- Planned Actions ---

type PlannedActionProvider struct {
	repo storage.PlannedActionRepository
}

func NewPlannedActionProvider(repo storage.PlannedActionRepository) *PlannedActionProvider {
	return &PlannedActionProvider{repo: repo}
}

func (p *PlannedActionProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "planned_actions",
		Description: "Запланированные будущие медицинские действия: контрольные анализы, прививки, обследования, " +
			"повторные визиты — извлечённые из рекомендаций в документах или сказанные пользователем напрямую в " +
			"диалоге, с ожидаемым сроком (диапазон дат, если он был указан) и статусом pending/completed/declined " +
			"(completed выставляется автоматически, когда подходящий результат появляется в новом документе — " +
			"никогда вручную). Используйте для вопросов 'что мне нужно сделать', 'какие анализы предстоят', " +
			"'что просрочено', 'я уже это делал?'.",
	}
}

func (p *PlannedActionProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	actions, err := p.repo.ListByUser(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("ask: planned action provider: %w", err)
	}
	now := time.Now()
	chunks := make([]KnowledgeChunk, len(actions))
	for i, a := range actions {
		content := a.Description
		switch {
		case a.DueDateFrom != nil && a.DueDateTo != nil && a.DueDateFrom.Equal(*a.DueDateTo):
			content += fmt.Sprintf(" Срок: %s.", formatDate(a.DueDateTo))
		case a.DueDateFrom != nil && a.DueDateTo != nil:
			content += fmt.Sprintf(" Срок: %s — %s.", formatDate(a.DueDateFrom), formatDate(a.DueDateTo))
		case a.DueDateTo != nil:
			content += fmt.Sprintf(" Срок: до %s.", formatDate(a.DueDateTo))
		}
		switch a.Status {
		case normalization.PlannedActionStatusCompleted:
			content += " Статус: выполнено"
			if a.MatchedAt != nil {
				content += fmt.Sprintf(" (%s)", formatDate(a.MatchedAt))
			}
			content += "."
		case normalization.PlannedActionStatusDeclined:
			content += " Статус: отменено пользователем."
		default:
			content += " Статус: ожидает."
			if a.Overdue(now) {
				content += " Просрочено."
			}
		}

		// Confidence/source-linking mirrors TimelineProvider's own
		// document-vs-self-reported split (see this file's doc comment) —
		// the second provider in this package whose rows can come from
		// either.
		confidence := 1.0
		chunk := KnowledgeChunk{Source: "planned_actions", Title: a.Description, Content: content}
		if a.SourceType == normalization.PlannedActionSourceDocument {
			chunk.DocumentID = a.SourceID
		} else {
			confidence = 0.9
			chunk.EventID = a.SourceID
		}
		chunk.Confidence = confidence
		chunks[i] = chunk
	}
	return chunks, nil
}

// --- Lab Results ---

type LabProvider struct{ repo storage.LabResultRepository }

func NewLabProvider(repo storage.LabResultRepository) *LabProvider {
	return &LabProvider{repo: repo}
}

func (p *LabProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "lab_results",
		Description: "История результатов лабораторных анализов (кровь, моча и т.п.): значение, единица измерения, " +
			"референсный диапазон, дата. Укажите indicatorName (например ALT, LDL), чтобы получить историю " +
			"конкретного показателя; from/to, чтобы получить все показатели за период (например все показатели " +
			"одного анализа по его дате); documentId — id, который уже вернул более ранний вызов timeline или " +
			"documents для этого документа/события, — чтобы получить все его показатели одним вызовом. Без " +
			"параметров возвращаются все показатели, сначала самые новые.",
	}
}

func (p *LabProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	// IndicatorName is the field the lab_results tool's own schema exposes
	// (see tools.go's toolParameters — it deliberately has no searchQuery
	// property, unlike documents/embeddings). Query is consulted only as a
	// defensive fallback for a tool call that didn't conform to that
	// schema (e.g. a model that included an extra field the schema didn't
	// declare) — both name what to search for, so both drive the same
	// alias/substring-aware lookup; only an empty search term falls back
	// to every indicator (see KnowledgeRequest.IndicatorName's doc
	// comment).
	term := req.IndicatorName
	if term == "" {
		term = req.Query
	}

	var results []normalization.LabResult
	var err error
	switch {
	case req.DocumentID != "":
		results, err = p.repo.ListByDocument(ctx, req.DocumentID)
		if err == nil {
			// ListByDocument, unlike ListByUser/HistoryByIndicator, isn't
			// itself scoped to a user — documentId is model-supplied (a
			// value it read back from an earlier tool result), so a
			// hallucinated or cross-household id must never leak another
			// user's results here.
			results = filterByOwner(results, req.UserID)
		}
	case term != "":
		results, err = p.repo.HistoryByIndicator(ctx, req.UserID, term)
	default:
		results, err = p.repo.ListByUser(ctx, req.UserID)
	}
	if err != nil {
		return nil, fmt.Errorf("ask: lab provider: %w", err)
	}

	results = filterByDateRange(results, req.From, req.To)
	// Every branch above already returns most-recent-first (see
	// labResultOrderBy), so truncating here keeps the newest Limit results
	// — matching the lab_results tool schema's own "most recent first"
	// description (tools.go's limitProperty). Unlike the other providers'
	// own Collect (which all call limitOrDefault before hitting storage),
	// this used to truncate only "if req.Limit > 0" — so a bare
	// lab_results call with no indicatorName/limit (exactly what "show me
	// my lab results" produces) fell through to ListByUser's entire
	// multi-year, unbounded history. limitOrDefault closes that gap the
	// same way every sibling provider already does.
	limit := limitOrDefault(req.Limit)
	if len(results) > limit {
		results = results[:limit]
	}

	chunks := make([]KnowledgeChunk, len(results))
	for i, r := range results {
		var content string
		if r.QualitativeValue != "" {
			content = fmt.Sprintf("%s: %s (%s).", r.IndicatorName, r.QualitativeValue, formatDate(r.TakenAt))
		} else {
			value, unit := r.Value, r.Unit
			if r.NormalizedUnit != "" {
				value, unit = r.NormalizedValue, r.NormalizedUnit
			}
			content = fmt.Sprintf("%s: %g %s (%s).", r.IndicatorName, value, unit, formatDate(r.TakenAt))
			if r.ReferenceLow != 0 || r.ReferenceHigh != 0 {
				content += fmt.Sprintf(" Норма: %g–%g.", r.ReferenceLow, r.ReferenceHigh)
			}
		}
		chunks[i] = KnowledgeChunk{Source: "lab_results", Title: r.IndicatorName, Content: content, Confidence: 1.0, DocumentID: r.DocumentID}
	}
	return chunks, nil
}

// filterByOwner drops any result whose UserID isn't userID — see its call
// site in LabProvider.Collect for why ListByDocument specifically needs
// this and every other lookup here doesn't.
func filterByOwner(results []normalization.LabResult, userID string) []normalization.LabResult {
	kept := results[:0]
	for _, r := range results {
		if r.UserID == userID {
			kept = append(kept, r)
		}
	}
	return kept
}

// filterByDateRange keeps only results taken within [from, to] (either
// bound optional). A result with no TakenAt is dropped as soon as either
// bound is set — an undated row can't be known to fall inside a requested
// range, so silently including it would defeat the point of asking for one.
func filterByDateRange(results []normalization.LabResult, from, to *time.Time) []normalization.LabResult {
	if from == nil && to == nil {
		return results
	}
	kept := results[:0]
	for _, r := range results {
		if r.TakenAt == nil {
			continue
		}
		if from != nil && r.TakenAt.Before(*from) {
			continue
		}
		if to != nil && r.TakenAt.After(*to) {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// --- Instrumental Findings ---

type InstrumentalFindingProvider struct {
	repo storage.InstrumentalFindingRepository
}

func NewInstrumentalFindingProvider(repo storage.InstrumentalFindingRepository) *InstrumentalFindingProvider {
	return &InstrumentalFindingProvider{repo: repo}
}

func (p *InstrumentalFindingProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "instrumental_findings",
		Description: "История находок инструментальных исследований (УЗИ, МРТ, КТ, ЭКГ): размеры органов, " +
			"эхогенность, описательные находки. Требует и structure (например 'Печень'), и parameter (например " +
			"'правая доля КВР') — без обоих ничего не возвращается, это не общий поиск по всем находкам. " +
			"Используйте для вопросов о динамике конкретного параметра конкретной структуры.",
	}
}

func (p *InstrumentalFindingProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	if req.Structure == "" || req.Parameter == "" {
		return nil, nil
	}
	findings, err := p.repo.HistoryByStructureParameter(ctx, req.UserID, req.Structure, req.Parameter)
	if err != nil {
		return nil, fmt.Errorf("ask: instrumental finding provider: %w", err)
	}
	chunks := make([]KnowledgeChunk, len(findings))
	for i, f := range findings {
		var content string
		if f.QualitativeValue != "" {
			content = fmt.Sprintf("%s, %s: %s (%s).", f.Structure, f.Parameter, f.QualitativeValue, formatDate(f.MeasuredAt))
		} else {
			value, unit := f.Value, f.Unit
			if f.NormalizedUnit != "" {
				value, unit = f.NormalizedValue, f.NormalizedUnit
			}
			content = fmt.Sprintf("%s, %s: %g %s (%s).", f.Structure, f.Parameter, value, unit, formatDate(f.MeasuredAt))
		}
		chunks[i] = KnowledgeChunk{
			Source: "instrumental_findings", Title: f.Structure + ", " + f.Parameter,
			Content: content, Confidence: 1.0, DocumentID: f.DocumentID,
		}
	}
	return chunks, nil
}

// --- Medical Profile ---

type ProfileProvider struct{ store *profile.Store }

func NewProfileProvider(store *profile.Store) *ProfileProvider {
	return &ProfileProvider{store: store}
}

func (p *ProfileProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "profile",
		Description: "Текущее состояние здоровья одним снимком: действующие диагнозы, хронические заболевания, " +
			"принимаемые сейчас лекарства, аллергии, прививки, последние результаты анализов и жизненных " +
			"показателей. Используйте для вопросов о текущем статусе ('что я принимаю сейчас', 'какие у меня " +
			"хронические болезни') — не для истории или динамики.",
	}
}

func (p *ProfileProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	built, found, err := p.store.Get(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("ask: profile provider: %w", err)
	}
	if !found {
		return nil, nil
	}

	var chunks []KnowledgeChunk
	if len(built.ActiveMedications) > 0 {
		var lines []string
		for _, m := range built.ActiveMedications {
			line := m.DrugName
			if m.DoseAmount > 0 {
				line = fmt.Sprintf("%s %g %s", m.DrugName, m.DoseAmount, m.DoseUnit)
			}
			lines = append(lines, line)
		}
		chunks = append(chunks, KnowledgeChunk{Source: "profile", Title: "Текущие лекарства", Content: joinLines("Принимает сейчас:", lines), Confidence: 1.0})
	}
	if len(built.ActiveDiagnoses) > 0 {
		var lines []string
		for _, d := range built.ActiveDiagnoses {
			lines = append(lines, d.Name)
		}
		chunks = append(chunks, KnowledgeChunk{Source: "profile", Title: "Действующие диагнозы", Content: joinLines("Действующие диагнозы:", lines), Confidence: 1.0})
	}
	if len(built.Allergies) > 0 {
		var lines []string
		for _, a := range built.Allergies {
			lines = append(lines, a.Substance)
		}
		chunks = append(chunks, KnowledgeChunk{Source: "profile", Title: "Аллергии", Content: joinLines("Известные аллергии:", lines), Confidence: 1.0})
	}
	if len(built.LatestLabResults) > 0 {
		var lines []string
		for _, l := range built.LatestLabResults {
			line := l.IndicatorName + ": "
			if l.QualitativeValue != "" {
				line += l.QualitativeValue
			} else {
				line += fmt.Sprintf("%g %s", l.Value, l.Unit)
			}
			line += fmt.Sprintf(" (%s)", formatDate(l.TakenAt))
			if l.DocumentTitle != "" {
				line += fmt.Sprintf(" [%s]", l.DocumentTitle)
			}
			lines = append(lines, line)
		}
		chunks = append(chunks, KnowledgeChunk{Source: "profile", Title: "Последние результаты анализов", Content: joinLines("Последние результаты анализов:", lines), Confidence: 1.0})
	}
	if len(built.LatestVitalSigns) > 0 {
		var lines []string
		for _, v := range built.LatestVitalSigns {
			label := vitalSignLabels[v.Type]
			if label == "" {
				label = v.Type
			}
			var line string
			if v.Type == "blood_pressure" {
				line = fmt.Sprintf("%s: %g/%g мм рт.ст.", label, v.Systolic, v.Diastolic)
			} else {
				line = fmt.Sprintf("%s: %g %s", label, v.Value, v.Unit)
			}
			line += fmt.Sprintf(" (%s)", formatDate(v.MeasuredAt))
			if v.DocumentTitle != "" {
				line += fmt.Sprintf(" [%s]", v.DocumentTitle)
			}
			lines = append(lines, line)
		}
		chunks = append(chunks, KnowledgeChunk{Source: "profile", Title: "Последние жизненные показатели", Content: joinLines("Последние жизненные показатели:", lines), Confidence: 1.0})
	}
	return chunks, nil
}

// vitalSignLabels maps VitalSign.Type to a human-readable label — mirrors
// internal/timeline's own vitalSignLabels (unexported there, so duplicated
// here rather than adding a cross-package dependency for a 5-entry map).
var vitalSignLabels = map[string]string{
	"blood_pressure": "Давление",
	"weight":         "Вес",
	"height":         "Рост",
	"pulse":          "Пульс",
	"temperature":    "Температура",
}

func joinLines(header string, lines []string) string {
	content := header
	for _, l := range lines {
		content += "\n- " + l
	}
	return content
}
