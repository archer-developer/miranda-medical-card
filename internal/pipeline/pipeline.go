// Package pipeline is the Processing Pipeline orchestrator
// (docs/architecture/02-processing-pipeline.md) — the actual implementation
// behind medical.upload_document and medical.reprocess_document
// (docs/mcp/03-documents.md §4, §6). It is the only thing in this codebase
// that calls internal/extraction, internal/normalization, and
// internal/storage together in one place; each of those packages stays
// unaware of the other two, exactly as docs/architecture/06-storage.md §2
// ("Независимость уровней") requires.
package pipeline

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/archer-developer/miranda-llm/embedding"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/filestore"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

// ErrAlreadyImported is returned by UploadDocument when fileID has already
// been imported as a MedicalDocument for this user — docs/mcp/03-documents.md
// §4's DOCUMENT_ALREADY_IMPORTED.
var ErrAlreadyImported = errors.New("pipeline: file already imported")

// ExtractedCounts mirrors docs/mcp/03-documents.md §4's extractedCounts —
// how many entities of each type this run produced, so a caller (Miranda,
// via the eventual MCP layer) can flag a suspiciously-empty result to the
// user without this package needing to know anything about MCP responses.
type ExtractedCounts struct {
	Diagnoses            int
	Medications          int
	LabResults           int
	InstrumentalFindings int
	Procedures           int
	Allergies            int
	VitalSigns           int
	Recommendations      int
}

// Result mirrors docs/mcp/03-documents.md §4's upload_document response
// shape (also §6's reprocess_document, which returns "the same shape").
type Result struct {
	DocumentID      string
	Status          string // storage.DocumentStatusReady or storage.DocumentStatusFailed
	Summary         string
	ExtractedCounts ExtractedCounts
}

// Pipeline wires internal/extraction (LLM calls), internal/filestore
// (binary content), and internal/storage (SQLite) together. Construct with
// New; all fields are unexported so callers can't reach into individual
// repositories and bypass the orchestration this type exists to enforce
// (e.g. "an Extraction row is never active except via Activate").
type Pipeline struct {
	provider           extraction.Provider
	escalationProvider extraction.Provider
	files              *filestore.Store

	fileRepo         storage.FileRepository
	documentRepo     storage.DocumentRepository
	extractionRepo   storage.ExtractionRepository
	canonicalUnits   storage.CanonicalUnitRepository
	indicatorAliases storage.IndicatorAliasRepository

	diagnoses            storage.DiagnosisRepository
	medications          storage.MedicationRepository
	labResults           storage.LabResultRepository
	instrumentalFindings storage.InstrumentalFindingRepository
	procedures           storage.ProcedureRepository
	allergies            storage.AllergyRepository
	vitalSigns           storage.VitalSignRepository

	timelineRepo   storage.TimelineRepository
	profileStore   *profile.Store
	profileBuilder *profile.Builder

	selfReportedEvents storage.SelfReportedEventRepository
	medicationIntakes  storage.MedicationIntakeRepository

	fts   storage.FTSRepository
	embed storage.EmbeddingRepository

	embedder          embedding.Embedder
	embeddingProvider string
	embeddingModel    string
	logger            *slog.Logger
}

// New builds a Pipeline. provider supplies both OCR (Chat) and Structured
// Extraction calls (see extraction.Provider); escalationProvider, if
// non-nil, is tried once for OCR (on any hard error) and once for
// Structured Extraction (see extraction.StructuredWithRetry, when
// provider's own attempts are exhausted and still suspiciously empty) —
// nil disables both entirely (see config.LLMConfig.EscalationModel). Typed
// as the full extraction.Provider, not just StructuredProvider, precisely
// because it needs to run OCR too. embedder generates Embedding Search
// vectors (see docs/architecture/04-search.md §14), tagged with
// embeddingProvider/embeddingModel on every stored row (see
// storage.Embedding.Provider/ModelVersion — the latter is what
// EmbeddingRepository.ListByUser scopes a search to, so it must stay
// consistent for vectors to remain comparable). files and s are opened once
// at process startup and shared across every request. A nil logger falls
// back to slog.Default().
func New(provider extraction.Provider, escalationProvider extraction.Provider, embedder embedding.Embedder, embeddingProvider, embeddingModel string, files *filestore.Store, s *storage.Store, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	diagnoses := storage.NewDiagnosisRepository(s)
	medications := storage.NewMedicationRepository(s)
	labResults := storage.NewLabResultRepository(s)
	procedures := storage.NewProcedureRepository(s)
	allergies := storage.NewAllergyRepository(s)
	vitalSigns := storage.NewVitalSignRepository(s)

	return &Pipeline{
		provider:           provider,
		escalationProvider: escalationProvider,
		files:              files,

		fileRepo:         storage.NewFileRepository(s),
		documentRepo:     storage.NewDocumentRepository(s),
		extractionRepo:   storage.NewExtractionRepository(s),
		canonicalUnits:   storage.NewCanonicalUnitRepository(s),
		indicatorAliases: storage.NewIndicatorAliasRepository(s),

		diagnoses:            diagnoses,
		medications:          medications,
		labResults:           labResults,
		instrumentalFindings: storage.NewInstrumentalFindingRepository(s),
		procedures:           procedures,
		allergies:            allergies,
		vitalSigns:           vitalSigns,

		timelineRepo:   storage.NewTimelineRepository(s),
		profileStore:   profile.NewStore(storage.NewProfileRepository(s)),
		profileBuilder: profile.NewBuilder(medications, diagnoses, procedures, allergies, labResults, vitalSigns),

		selfReportedEvents: storage.NewSelfReportedEventRepository(s),
		medicationIntakes:  storage.NewMedicationIntakeRepository(s),

		fts:   storage.NewFTSRepository(s),
		embed: storage.NewEmbeddingRepository(s),

		embedder:          embedder,
		embeddingProvider: embeddingProvider,
		embeddingModel:    embeddingModel,
		logger:            logger,
	}
}

// UploadFile is the storage half of File creation (docs/mcp/02-files.md §2):
// write data to disk, dedup by (userID, sha256) per that doc's §4, and
// record a File row. It has no MCP tool of its own — the only caller is
// medical.upload_document's handler (internal/mcpserver/documents.go),
// after it has fetched the bytes from a caller-supplied fileUri
// (docs/mcp/03-documents.md §4).
func (p *Pipeline) UploadFile(ctx context.Context, userID, filename, contentType string, data []byte) (storage.File, error) {
	path, sha, err := p.files.Save(userID, data)
	if err != nil {
		return storage.File{}, fmt.Errorf("pipeline: upload file: %w", err)
	}

	existing, err := p.fileRepo.FindBySHA256(ctx, userID, sha)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.File{}, fmt.Errorf("pipeline: upload file: dedup check: %w", err)
	}

	return p.fileRepo.Add(ctx, storage.File{
		UserID:      userID,
		Filename:    filename,
		ContentType: contentType,
		Size:        int64(len(data)),
		SHA256:      sha,
		StoragePath: path,
	})
}

// UploadDocument implements docs/mcp/03-documents.md §4: creates a
// MedicalDocument for fileID and runs it through the full Pipeline once.
func (p *Pipeline) UploadDocument(ctx context.Context, userID, fileID string) (Result, error) {
	file, err := p.fileRepo.Get(ctx, fileID, userID)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: upload document: %w", err)
	}

	already, err := p.documentRepo.List(ctx, userID, storage.DocumentFilter{FileID: fileID})
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: upload document: check already imported: %w", err)
	}
	if len(already) > 0 {
		return Result{}, ErrAlreadyImported
	}

	doc, err := p.documentRepo.Add(ctx, storage.MedicalDocument{UserID: userID, FileID: fileID})
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: upload document: create document: %w", err)
	}

	return p.run(ctx, userID, doc.ID, file, 1)
}

// ReprocessDocument implements docs/mcp/03-documents.md §6: re-runs the
// Pipeline for an already-imported document against the same File, adding a
// new Extraction version rather than reusing the old one (see
// docs/domain/03-files-and-documents.md §4 "Инварианты" —
// Extraction is immutable once created).
func (p *Pipeline) ReprocessDocument(ctx context.Context, userID, documentID string) (Result, error) {
	doc, err := p.documentRepo.Get(ctx, documentID, userID)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: reprocess document: %w", err)
	}
	file, err := p.fileRepo.Get(ctx, doc.FileID, userID)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: reprocess document: load file: %w", err)
	}
	versions, err := p.extractionRepo.ListVersions(ctx, documentID)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: reprocess document: list extraction versions: %w", err)
	}

	return p.run(ctx, userID, documentID, file, len(versions)+1)
}

// run is the shared core of UploadDocument and ReprocessDocument — from
// "a MedicalDocument row and its File exist" through OCR, Structured
// Extraction, Normalization, and persisting every derived entity. On any
// failure it best-effort marks the document FAILED (see docs/domain/11-value-objects.md
// §7's status semantics) before returning the real error — the status
// update's own failure is deliberately not what gets returned to the
// caller, since the original error is more actionable.
func (p *Pipeline) run(ctx context.Context, userID, documentID string, file storage.File, version int) (Result, error) {
	fail := func(err error) (Result, error) {
		_ = p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusFailed)
		return Result{}, err
	}

	p.logger.Debug("pipeline: run start", "documentId", documentID, "userId", userID, "version", version, "fileId", file.ID, "size", file.Size)

	if err := p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusRunning); err != nil {
		return Result{}, fmt.Errorf("pipeline: run: %w", err)
	}

	data, err := p.files.Read(file.StoragePath)
	if err != nil {
		return fail(fmt.Errorf("pipeline: run: read file: %w", err))
	}

	extracted, raw, stillSuspicious, err := extraction.Extract(ctx, p.provider, p.escalationProvider, base64.StdEncoding.EncodeToString(data), file.ContentType, p.logger)
	if err != nil {
		return fail(fmt.Errorf("pipeline: run: extract: %w", err))
	}
	// stillSuspicious means every attempt (primary retries + escalation,
	// see extraction.Extract's doc comment) came back with the categories
	// expected for this documentType empty despite substantial recognized
	// text — extraction.StructuredWithRetry deliberately doesn't turn this
	// into an error itself (it can't tell "the model failed" from "this
	// document genuinely has nothing structured to extract"), but Pipeline
	// can be less permissive: silently marking such a document READY with
	// zero entities looks identical to a real, empty-but-successful
	// result, and nothing prompts the user to notice and request
	// medical.reprocess_document. Treated the same as any other hard
	// failure here — FAILED, not a new status value (see
	// docs/domain/11-value-objects.md §7's PENDING -> RUNNING -> (READY |
	// FAILED) state machine) — nothing downstream (Normalize, Timeline,
	// Profile, FTS) runs against a result already known to have nothing
	// useful in it.
	if stillSuspicious {
		return fail(fmt.Errorf("pipeline: run: extract: structured result still suspiciously empty after every attempt (including escalation, if configured) for documentType %q", extracted.DocumentType))
	}

	record, err := p.extractionRepo.Add(ctx, storage.ExtractionRecord{DocumentID: documentID, Version: version, Raw: raw})
	if err != nil {
		return fail(fmt.Errorf("pipeline: run: store extraction: %w", err))
	}
	if err := p.extractionRepo.Activate(ctx, record.ID); err != nil {
		return fail(fmt.Errorf("pipeline: run: activate extraction: %w", err))
	}

	normalized, normErrs := normalization.Normalize(ctx, userID, documentID, extracted, p.canonicalUnits, p.indicatorAliases)
	// A bad date or unresolvable unit on one entity (see Normalize's own
	// doc comment) is not fatal to the whole document — every
	// successfully-normalized entity is still persisted below. Each error
	// is logged (Warn, since it means one entity's date/unit was dropped
	// silently from that field) rather than discarded.
	for _, normErr := range normErrs {
		p.logger.Warn("pipeline: normalize: entity field dropped", "documentId", documentID, "error", normErr)
	}
	p.logger.Debug("pipeline: normalized",
		"documentId", documentID, "diagnoses", len(normalized.Diagnoses), "medications", len(normalized.Medications),
		"labResults", len(normalized.LabResults), "instrumentalFindings", len(normalized.InstrumentalFindings),
		"procedures", len(normalized.Procedures), "allergies", len(normalized.Allergies),
		"vitalSigns", len(normalized.VitalSigns), "normalizeErrors", len(normErrs))

	if err := p.persistCanonicalUnits(ctx, userID, normalized); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist canonical units: %w", err))
	}

	if err := p.diagnoses.ReplaceForDocument(ctx, documentID, normalized.Diagnoses); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist diagnoses: %w", err))
	}
	if err := p.medications.ReplaceForDocument(ctx, documentID, normalized.Medications); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist medications: %w", err))
	}
	if err := p.labResults.ReplaceForDocument(ctx, documentID, normalized.LabResults); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist lab results: %w", err))
	}
	if err := p.instrumentalFindings.ReplaceForDocument(ctx, documentID, normalized.InstrumentalFindings); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist instrumental findings: %w", err))
	}
	if err := p.procedures.ReplaceForDocument(ctx, documentID, normalized.Procedures); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist procedures: %w", err))
	}
	if err := p.allergies.ReplaceForDocument(ctx, documentID, normalized.Allergies); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist allergies: %w", err))
	}
	if err := p.vitalSigns.ReplaceForDocument(ctx, documentID, normalized.VitalSigns); err != nil {
		return fail(fmt.Errorf("pipeline: run: persist vital signs: %w", err))
	}

	summary := buildSummary(extracted)
	title := buildTitle(extracted)
	documentDate := parseOptionalDate(extracted.DocumentDate)
	update := storage.DocumentExtractedUpdate{
		DocumentType:   extracted.DocumentType,
		DocumentDate:   documentDate,
		Title:          title,
		Organization:   extracted.Organization,
		Doctor:         extracted.Doctor,
		RecognizedText: extracted.FullText,
		Summary:        summary,
	}
	if err := p.documentRepo.UpdateExtracted(ctx, documentID, userID, update); err != nil {
		return fail(fmt.Errorf("pipeline: run: update document: %w", err))
	}

	if err := p.rebuildTimeline(ctx, userID, documentID, title, documentDate, normalized); err != nil {
		return fail(fmt.Errorf("pipeline: run: rebuild timeline: %w", err))
	}
	if err := p.rebuildProfile(ctx, userID); err != nil {
		return fail(fmt.Errorf("pipeline: run: rebuild profile: %w", err))
	}

	// FTS is a pure, local SQLite operation — treated as required, same as
	// every other persistence step above (see docs/architecture/04-search.md
	// §13's recommended index content: full text + Summary + recommendations).
	ftsContent := strings.Join(append([]string{extracted.FullText, summary}, extracted.Recommendations...), "\n")
	if err := p.fts.IndexDocument(ctx, userID, documentID, title, ftsContent); err != nil {
		return fail(fmt.Errorf("pipeline: run: index fts: %w", err))
	}

	// Embeddings depend on an external LLM call — per
	// docs/architecture/02-processing-pipeline.md §14 ("Границы транзакций"),
	// a failure here must never make the document unavailable for
	// Timeline/Profile/FTS, so it's best-effort: logged, not propagated.
	p.generateDocumentEmbedding(ctx, userID, documentID, summary)

	if err := p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusReady); err != nil {
		return Result{}, fmt.Errorf("pipeline: run: mark ready: %w", err)
	}

	p.logger.Debug("pipeline: run done", "documentId", documentID, "userId", userID, "status", storage.DocumentStatusReady)

	return Result{
		DocumentID: documentID,
		Status:     storage.DocumentStatusReady,
		Summary:    summary,
		ExtractedCounts: ExtractedCounts{
			Diagnoses:            len(normalized.Diagnoses),
			Medications:          len(normalized.Medications),
			LabResults:           len(normalized.LabResults),
			InstrumentalFindings: len(normalized.InstrumentalFindings),
			Procedures:           len(normalized.Procedures),
			Allergies:            len(normalized.Allergies),
			VitalSigns:           len(normalized.VitalSigns),
			Recommendations:      len(extracted.Recommendations),
		},
	}, nil
}

// generateDocumentEmbedding replaces documentID's "summary"-type Embedding
// (see storage.Embedding's doc comment for the scope decision) with a fresh
// vector of text. Best-effort: any error is logged, never returned — see
// this method's call site for why. Skipped entirely (not even logged) when
// text is empty, e.g. a document Extraction produced no diagnoses/
// recommendations to build a summary from — embedding an empty string is a
// wasted call, not a real failure.
func (p *Pipeline) generateDocumentEmbedding(ctx context.Context, userID, documentID, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := p.embed.RemoveByDocument(ctx, documentID); err != nil {
		p.logger.Warn("pipeline: remove previous document embedding failed", "documentId", documentID, "error", err)
	}
	vector, err := p.embedder.Embed(ctx, text)
	if err != nil {
		p.logger.Warn("pipeline: generate document embedding failed", "documentId", documentID, "error", err)
		return
	}
	err = p.embed.Add(ctx, storage.Embedding{
		UserID: userID, SourceType: "summary", SourceID: documentID,
		Provider: p.embeddingProvider, ModelVersion: p.embeddingModel, Vector: vector,
	})
	if err != nil {
		p.logger.Warn("pipeline: store document embedding failed", "documentId", documentID, "error", err)
	}
}

// rebuildTimeline replaces every TimelineEvent for documentID with the set
// timeline.Build derives from normalized — the document-scoped replace
// pattern used throughout (see docs/domain/04-timeline.md §4).
func (p *Pipeline) rebuildTimeline(ctx context.Context, userID, documentID, documentTitle string, documentDate *time.Time, normalized normalization.Result) error {
	if err := p.timelineRepo.RemoveByDocument(ctx, documentID); err != nil {
		return err
	}
	for _, e := range timeline.Build(userID, documentID, documentTitle, documentDate, normalized) {
		if err := p.timelineRepo.Add(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// rebuildProfile fully rebuilds and replaces userID's MedicalProfile — run
// after every successful document import, per
// docs/domain/05-medical-profile.md §4 ("пересобирается полностью после
// каждого успешного upload_document").
func (p *Pipeline) rebuildProfile(ctx context.Context, userID string) error {
	built, err := p.profileBuilder.Build(ctx, userID)
	if err != nil {
		return err
	}
	return p.profileStore.Replace(ctx, built)
}

// persistCanonicalUnits records the canonical unit for any (userID,
// indicator) Normalize established for the first time on this run — see
// storage.CanonicalUnitRepository's doc comment for why this write step
// lives in the caller rather than inside CanonicalUnitResolver itself.
// SetIfAbsent is a no-op for indicators that already had a canonical unit
// (including ones this very call just normalized against), so calling it
// unconditionally for every normalized entity with a NormalizedUnit is
// safe and doesn't need its own "is this actually new" check.
func (p *Pipeline) persistCanonicalUnits(ctx context.Context, userID string, normalized normalization.Result) error {
	for _, l := range normalized.LabResults {
		if l.NormalizedUnit == "" {
			continue
		}
		if err := p.canonicalUnits.SetIfAbsent(ctx, userID, l.IndicatorName, l.NormalizedUnit); err != nil {
			return err
		}
	}
	for _, f := range normalized.InstrumentalFindings {
		if f.NormalizedUnit == "" {
			continue
		}
		// Same compound key normalization.go's own lookup uses — see
		// normalization.go's InstrumentalFindings loop.
		key := f.Structure + "/" + f.Parameter
		if err := p.canonicalUnits.SetIfAbsent(ctx, userID, key, f.NormalizedUnit); err != nil {
			return err
		}
	}
	return nil
}

// documentTypeLabels gives buildTitle/buildSummary a short, fixed
// human-readable label per extraction.Result.DocumentType — the schema
// itself has no free-text "title" field (see extraction.Schema), so this is
// the mechanical fallback used whenever a real Summary-generation LLM stage
// doesn't exist yet (see this function's callers' doc comments).
var documentTypeLabels = map[string]string{
	"lab_report":        "Лабораторное исследование",
	"consultation":      "Консультация",
	"discharge_summary": "Выписка",
	"prescription":      "Рецепт",
	"imaging_report":    "Инструментальное исследование",
	"referral":          "Направление",
	"other":             "Медицинский документ",
}

func documentTypeLabel(documentType string) string {
	if label, ok := documentTypeLabels[documentType]; ok {
		return label
	}
	return "Медицинский документ"
}

// buildTitle produces MedicalDocument.Title (docs/domain/03-files-and-documents.md
// §3 — "короткое человекочитаемое название, для списков"). Prefers
// extracted.StudyTitle — the document's own printed title/heading (e.g.
// "МРТ пояснично-крестцового отдела позвоночника без контрастного
// усиления"), transcribed by Extraction itself (see Schema's studyTitle
// field) — since that's the specific label a user would actually recognize
// and reference in a question, unlike the fixed 7-value documentType label
// ("Инструментальное исследование" for every imaging study regardless of
// modality or body part). Falls back to the mechanical documentType label +
// organization, exactly as before, when StudyTitle wasn't printed/
// transcribed — same "don't fabricate, leave it to the fallback" posture as
// Organization/Doctor already have.
func buildTitle(extracted extraction.Result) string {
	label := documentTypeLabel(extracted.DocumentType)
	if extracted.StudyTitle != "" {
		label = extracted.StudyTitle
	}
	if extracted.Organization == "" {
		return label
	}
	return fmt.Sprintf("%s — %s", label, extracted.Organization)
}

// buildSummary produces MedicalDocument.Summary (docs/domain/03-files-and-documents.md
// §3 — "Только факты, без выводов модели"). Assembled mechanically from
// already-extracted structured fields (diagnoses, recommendations) rather
// than a separate LLM summarization call — docs/architecture/06-storage.md §4
// describes Summary as its own storage layer without mandating how it's
// produced, and a plain concatenation of facts Extraction already verified
// trivially satisfies "no model conclusions" without the cost, latency, or
// new failure mode of a fourth LLM call per document. Revisit if a real
// free-text summarization stage is added later.
func buildSummary(extracted extraction.Result) string {
	var parts []string
	parts = append(parts, documentTypeLabel(extracted.DocumentType)+".")

	if len(extracted.Diagnoses) > 0 {
		names := make([]string, 0, len(extracted.Diagnoses))
		for _, d := range extracted.Diagnoses {
			if d.Code != "" {
				names = append(names, fmt.Sprintf("%s (%s)", d.Name, d.Code))
			} else {
				names = append(names, d.Name)
			}
		}
		parts = append(parts, "Диагнозы: "+strings.Join(names, ", ")+".")
	}

	if len(extracted.Recommendations) > 0 {
		parts = append(parts, "Рекомендации: "+strings.Join(extracted.Recommendations, "; ")+".")
	}

	return strings.Join(parts, " ")
}

const dateLayout = "2006-01-02"

func parseOptionalDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return nil
	}
	return &t
}
