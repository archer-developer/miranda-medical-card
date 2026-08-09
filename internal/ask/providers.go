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
			"(предполагаемый/действующий/хронический/снят). Используйте для вопросов об истории диагнозов, " +
			"хронических заболеваниях, дате первой постановки диагноза.",
	}
}

func (p *DiagnosisProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	diagnoses, err := p.repo.ListByUser(ctx, req.UserID)
	if err != nil {
		return nil, fmt.Errorf("ask: diagnosis provider: %w", err)
	}
	chunks := make([]KnowledgeChunk, len(diagnoses))
	for i, d := range diagnoses {
		content := fmt.Sprintf("%s. Статус: %s.", d.Name, d.Status)
		if d.Code != "" {
			content = fmt.Sprintf("%s (%s). Статус: %s.", d.Name, d.Code, d.Status)
		}
		if d.DiagnosedAt != nil {
			content += fmt.Sprintf(" Поставлен: %s.", formatDate(d.DiagnosedAt))
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
			"конкретного показателя; без него возвращаются все показатели. Используйте для вопросов о динамике " +
			"конкретных анализов, отклонениях от нормы.",
	}
}

func (p *LabProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	var results []normalization.LabResult
	var err error
	if req.IndicatorName != "" {
		results, err = p.repo.HistoryByIndicator(ctx, req.UserID, req.IndicatorName)
	} else {
		results, err = p.repo.ListByUser(ctx, req.UserID)
	}
	if err != nil {
		return nil, fmt.Errorf("ask: lab provider: %w", err)
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
