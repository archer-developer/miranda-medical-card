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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/archer-developer/miranda-llm/embedding"

	"github.com/archer-developer/miranda-medical-card/internal/extraction"
	"github.com/archer-developer/miranda-medical-card/internal/filestore"
	"github.com/archer-developer/miranda-medical-card/internal/normalization"
	"github.com/archer-developer/miranda-medical-card/internal/nutrition"
	"github.com/archer-developer/miranda-medical-card/internal/planmatch"
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
	PlannedActions       int
}

// Result mirrors docs/mcp/03-documents.md §4's upload_document response
// shape (also §6's reprocess_document, which returns "the same shape").
type Result struct {
	DocumentID      string
	Status          string // storage.DocumentStatusReady or storage.DocumentStatusFailed
	Summary         string
	ExtractedCounts ExtractedCounts
	// PlannedActions is this document's own current PlannedActions — see
	// ExtractedCounts.PlannedActions for just their count, this is the same
	// set with a full description/type/due date each, so a caller (Miranda)
	// can tell the user what was actually added to their plan without a
	// separate medical.planned_actions round trip. Read back from storage
	// after matchPlannedActions (see its call site), not built directly from
	// normalized.PlannedActions — ReplaceForSource's reconciliation may keep
	// an existing row's id/status on a reprocess (see its own doc comment),
	// so only a fresh read reflects what's actually persisted.
	PlannedActions []normalization.PlannedAction
}

// Pipeline wires internal/extraction (LLM calls), internal/filestore
// (binary content), and internal/storage (SQLite) together. Construct with
// New; all fields are unexported so callers can't reach into individual
// repositories and bypass the orchestration this type exists to enforce
// (e.g. "an Extraction row is never active except via Activate").
type Pipeline struct {
	// ocrProvider/ocrEscalation and extractionProvider/extractionEscalation
	// are independently configured (config.LLMConfig's ocr_provider and
	// extraction_provider) — see New's doc comment for why they're split
	// rather than one shared pair. Each field is typed as the full
	// extraction.Provider (not the narrower ChatProvider/StructuredProvider
	// each call site actually needs) purely so both pairs can be built the
	// same way in main.go/cmd/medical-dev — every concrete provider
	// (gemini/anthropic/openai_compat) implements both capabilities anyway,
	// and Go satisfies the narrower interface implicitly at each call site
	// (extraction.Extract's ocrProvider param, decline.Match/events.Extract's
	// StructuredProvider param, etc.) with no explicit cast needed.
	ocrProvider          extraction.Provider
	ocrEscalation        extraction.Provider
	extractionProvider   extraction.Provider
	extractionEscalation extraction.Provider
	files                *filestore.Store

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
	plannedActions       storage.PlannedActionRepository

	timelineRepo   storage.TimelineRepository
	profileStore   *profile.Store
	profileBuilder *profile.Builder
	// users is only consulted by rebuildProfile, for Nutrition Advisor's
	// age/sex input (docs/adr/006-nutrition-guidance.md §7) — nil is a
	// valid value (see rebuildProfile), so callers that don't care about
	// Nutrition Guidance (most tests) can leave it unset.
	users UserRepository

	selfReportedEvents storage.SelfReportedEventRepository
	medicationIntakes  storage.MedicationIntakeRepository

	fts   storage.FTSRepository
	embed storage.EmbeddingRepository

	embedder          embedding.Embedder
	embeddingProvider string
	embeddingModel    string
	logger            *slog.Logger
}

// New builds a Pipeline. ocrProvider runs Stage 1 (OCR/Vision) calls;
// extractionProvider runs every Structured-shaped call the Document
// Pipeline makes — Stage 2a/2b (Structured Extraction, Instrumental
// Structured), Self-Reported Event extraction (internal/events), decline
// matching (internal/decline), and title backfill
// (BackfillStudyTitle). They're independently configured
// (config.LLMConfig's ocr_provider/extraction_provider) rather than one
// shared provider — see extraction.Extract's own doc comment for why: OCR
// and Structured Extraction can each need a different model (a different
// quota budget, a schema/prompt change that only touches Structured
// Extraction) without the other stage moving too. ocrEscalation/
// extractionEscalation, if non-nil, are each tried once for their own stage
// on a hard error or (for Structured Extraction) a suspiciously empty
// result (see extraction.StructuredWithRetry) — either may independently be
// nil to disable escalation for that stage alone. embedder generates
// Embedding Search vectors (see docs/architecture/04-search.md §14), tagged
// with embeddingProvider/embeddingModel on every stored row (see
// storage.Embedding.Provider/ModelVersion — the latter is what
// EmbeddingRepository.ListByUser scopes a search to, so it must stay
// consistent for vectors to remain comparable). files and s are opened once
// at process startup and shared across every request. A nil logger falls
// back to slog.Default(). users, if non-nil, is consulted by rebuildProfile
// for Nutrition Advisor's age/sex input (docs/adr/006-nutrition-guidance.md
// §7) — a nil users is valid and just means every rebuilt Profile's
// Nutrition Guidance is generated without age/sex context, the same as an
// unset User.BirthDate/Sex would.
func New(ocrProvider, ocrEscalation, extractionProvider, extractionEscalation extraction.Provider, embedder embedding.Embedder, embeddingProvider, embeddingModel string, files *filestore.Store, s *storage.Store, logger *slog.Logger, users UserRepository) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	diagnoses := storage.NewDiagnosisRepository(s)
	medications := storage.NewMedicationRepository(s)
	labResults := storage.NewLabResultRepository(s)
	procedures := storage.NewProcedureRepository(s)
	allergies := storage.NewAllergyRepository(s)
	vitalSigns := storage.NewVitalSignRepository(s)
	documentRepo := storage.NewDocumentRepository(s)

	return &Pipeline{
		ocrProvider:          ocrProvider,
		ocrEscalation:        ocrEscalation,
		extractionProvider:   extractionProvider,
		extractionEscalation: extractionEscalation,
		files:                files,

		fileRepo:         storage.NewFileRepository(s),
		documentRepo:     documentRepo,
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
		plannedActions:       storage.NewPlannedActionRepository(s),

		timelineRepo:   storage.NewTimelineRepository(s),
		profileStore:   profile.NewStore(storage.NewProfileRepository(s)),
		profileBuilder: profile.NewBuilder(medications, diagnoses, procedures, allergies, labResults, vitalSigns, documentRepo),
		users:          users,

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
	path, sha, err := p.files.Save(userID, filename, data)
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
	// A FAILED document for this exact file (e.g. OCR/Structured Extraction
	// kept coming back suspiciously empty, see extraction.Extract) is not
	// "already imported" in the sense docs/mcp/03-documents.md §4's
	// DOCUMENT_ALREADY_IMPORTED describes — that code means the file was
	// *successfully* imported. Bouncing a retry off that stale failure with
	// the same misleading error, forever, was the actual bug: it gave the
	// caller no way to tell "already have this" apart from "processing
	// broke last time," and no path to retry short of separately
	// discovering the document id and calling medical.reprocess_document.
	// Any other status (READY, or RUNNING/PENDING mid-flight) still blocks
	// as before — only a genuinely failed previous attempt gets retried
	// here, reusing that same document row (same as reprocess_document, and
	// for the same reason: avoid piling up a fresh orphan FAILED row on
	// every retry of a persistently-failing file).
	var failed *storage.MedicalDocument
	for i := range already {
		if already[i].Status != storage.DocumentStatusFailed {
			return Result{}, ErrAlreadyImported
		}
		failed = &already[i]
	}
	if failed != nil {
		versions, err := p.extractionRepo.ListVersions(ctx, failed.ID)
		if err != nil {
			return Result{}, fmt.Errorf("pipeline: upload document: list extraction versions: %w", err)
		}
		return p.run(ctx, userID, failed.ID, file, len(versions)+1)
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

// run is UploadDocument/ReprocessDocument's extractFunc for process: OCR the
// File's bytes, then run Structured Extraction against the transcription
// (extraction.Extract does both).
func (p *Pipeline) run(ctx context.Context, userID, documentID string, file storage.File, version int) (Result, error) {
	return p.process(ctx, userID, documentID, version, func() (extraction.Result, json.RawMessage, bool, error) {
		data, err := p.files.Read(file.StoragePath)
		if err != nil {
			return extraction.Result{}, nil, false, fmt.Errorf("read file: %w", err)
		}
		return extraction.Extract(ctx, p.ocrProvider, p.ocrEscalation, p.extractionProvider, p.extractionEscalation, base64.StdEncoding.EncodeToString(data), file.ContentType, p.logger)
	})
}

// ReextractDocument re-runs Structured Extraction (Stage 2a/2b) and every
// downstream stage — Normalization, Timeline, Medical Profile, Embeddings,
// FTS — against an already-imported document's already-stored
// RecognizedText (MedicalDocument.RecognizedText, persisted by an earlier
// full run/ReprocessDocument), skipping OCR entirely.
//
// This is docs/architecture/02-processing-pipeline.md §2's "Независимость
// этапов" ("можно ... повторно выполнить Extraction ... без повторного
// выполнения остальных этапов") applied to the one stage boundary that
// actually matters in practice: OCR is a flat per-document image-transcription
// cost a Structured Extraction schema/prompt change never needs to redo,
// while every stage from Structured Extraction onward can change behavior
// against the exact same recognized text (e.g. Diagnosis.status/
// expectedResolution/statusReasoning are a Structured Extraction judgment
// call, see extraction.Schema — a schema fix there needs a fresh Structured
// Extraction call per document, but never a fresh OCR pass). Adds a new
// Extraction version, same as ReprocessDocument (Extraction is immutable
// once created, see docs/domain/03-files-and-documents.md §4). Beyond how it
// obtains its Structured Extraction result (extraction.ExtractFromText
// against stored text, instead of run's extraction.Extract against a
// freshly-OCR'd file), it is process's extractFunc exactly like run — no
// separate persistence/downstream path.
//
// Returns a storage.ErrNotFound-wrapping error if the document has no stored
// RecognizedText yet (e.g. it predates this method and never went through a
// full run) — ReprocessDocument is the only way to populate RecognizedText
// for the first time.
func (p *Pipeline) ReextractDocument(ctx context.Context, userID, documentID string) (Result, error) {
	doc, err := p.documentRepo.Get(ctx, documentID, userID)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: reextract document: %w", err)
	}
	if strings.TrimSpace(doc.RecognizedText) == "" {
		return Result{}, fmt.Errorf("pipeline: reextract document: no stored recognized text (never went through a full run): %w", storage.ErrNotFound)
	}
	versions, err := p.extractionRepo.ListVersions(ctx, documentID)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: reextract document: list extraction versions: %w", err)
	}

	return p.process(ctx, userID, documentID, len(versions)+1, func() (extraction.Result, json.RawMessage, bool, error) {
		return extraction.ExtractFromText(ctx, p.extractionProvider, p.extractionEscalation, doc.RecognizedText, p.logger)
	})
}

// process is the shared core of run and ReextractDocument — the actual
// Pipeline orchestration from "a MedicalDocument row exists" through
// Structured Extraction (however extractFunc chooses to produce it: fresh
// OCR + Structured for run, stored text + Structured for ReextractDocument),
// Normalization, and persisting every derived entity. On any failure it
// best-effort marks the document FAILED (see docs/domain/11-value-objects.md
// §7's status semantics) before returning the real error — the status
// update's own failure is deliberately not what gets returned to the
// caller, since the original error is more actionable.
func (p *Pipeline) process(ctx context.Context, userID, documentID string, version int, extractFunc func() (extraction.Result, json.RawMessage, bool, error)) (Result, error) {
	fail := func(err error) (Result, error) {
		_ = p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusFailed)
		return Result{}, err
	}

	p.logger.Debug("pipeline: process start", "documentId", documentID, "userId", userID, "version", version)

	if err := p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusRunning); err != nil {
		return fail(fmt.Errorf("pipeline: process: %w", err))
	}

	extracted, raw, stillSuspicious, err := extractFunc()
	if err != nil {
		return fail(fmt.Errorf("pipeline: process: extract: %w", err))
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
		return fail(fmt.Errorf("pipeline: process: extract: structured result still suspiciously empty after every attempt (including escalation, if configured) for documentType %q", extracted.DocumentType))
	}

	record, err := p.extractionRepo.Add(ctx, storage.ExtractionRecord{DocumentID: documentID, Version: version, Raw: raw})
	if err != nil {
		return fail(fmt.Errorf("pipeline: process: store extraction: %w", err))
	}
	if err := p.extractionRepo.Activate(ctx, record.ID); err != nil {
		return fail(fmt.Errorf("pipeline: process: activate extraction: %w", err))
	}

	return p.normalizeAndPersist(ctx, userID, documentID, extracted)
}

// RenormalizeDocument re-runs Normalization and every downstream stage —
// domain entities, Timeline, Medical Profile, FTS, Embeddings — against an
// already-imported document's current active Extraction, without calling
// any LLM at all: no OCR, no Structured Extraction, just a fresh pass of
// internal/normalization.Normalize over the same already-stored
// Extraction.Raw JSON, unmarshaled back into extraction.Result (round-trips
// cleanly — Raw is exactly what an earlier Extract/ExtractFromText call
// itself marshaled, see finalizeStage2b).
//
// This is docs/architecture/02-processing-pipeline.md §2's "Независимость
// этапов" one step further than ReextractDocument: useful when only
// Normalization's own logic changed (a unit-conversion fix, a date-parsing
// fix) and neither the OCR transcription nor the Structured Extraction
// judgment call (diagnosis status, etc.) needs to change at all. Unlike run
// and ReextractDocument, this never adds a new Extraction version — see
// docs/architecture/02-processing-pipeline.md §2 "Идемпотентность"
// ("повторный запуск Normalize() должен заменить существующие сущности, а
// не создать новые"): the Extraction itself didn't change, only what was
// derived from it, so there is nothing new to version.
func (p *Pipeline) RenormalizeDocument(ctx context.Context, userID, documentID string) (Result, error) {
	record, err := p.extractionRepo.GetActive(ctx, documentID)
	if err != nil {
		return Result{}, fmt.Errorf("pipeline: renormalize document: get active extraction: %w", err)
	}
	var extracted extraction.Result
	if err := json.Unmarshal(record.Raw, &extracted); err != nil {
		return Result{}, fmt.Errorf("pipeline: renormalize document: unmarshal stored extraction: %w", err)
	}

	// fail mirrors process's own closure — symmetry matters here since,
	// past this point, RenormalizeDocument shares normalizeAndPersist's
	// failure handling with process/ReextractDocument, and every one of
	// those failure paths marks the document FAILED rather than leaving it
	// stuck in RUNNING.
	fail := func(err error) (Result, error) {
		_ = p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusFailed)
		return Result{}, err
	}

	if err := p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusRunning); err != nil {
		return fail(fmt.Errorf("pipeline: renormalize document: %w", err))
	}

	return p.normalizeAndPersist(ctx, userID, documentID, extracted)
}

// normalizeAndPersist is the shared tail of process and RenormalizeDocument:
// from a Structured Extraction result (freshly produced or replayed from
// storage) through Normalization and persisting every derived entity to
// marking the document READY. On any failure it best-effort marks the
// document FAILED before returning the real error, same contract as
// process's own doc comment describes.
func (p *Pipeline) normalizeAndPersist(ctx context.Context, userID, documentID string, extracted extraction.Result) (Result, error) {
	fail := func(err error) (Result, error) {
		_ = p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusFailed)
		return Result{}, err
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
		"vitalSigns", len(normalized.VitalSigns), "plannedActions", len(normalized.PlannedActions),
		"normalizeErrors", len(normErrs))

	if err := p.persistCanonicalUnits(ctx, userID, normalized); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist canonical units: %w", err))
	}

	if err := p.diagnoses.ReplaceForDocument(ctx, documentID, normalized.Diagnoses); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist diagnoses: %w", err))
	}
	if err := p.medications.ReplaceForDocument(ctx, documentID, normalized.Medications); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist medications: %w", err))
	}
	if err := p.labResults.ReplaceForDocument(ctx, documentID, normalized.LabResults); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist lab results: %w", err))
	}
	if err := p.instrumentalFindings.ReplaceForDocument(ctx, documentID, normalized.InstrumentalFindings); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist instrumental findings: %w", err))
	}
	if err := p.procedures.ReplaceForDocument(ctx, documentID, normalized.Procedures); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist procedures: %w", err))
	}
	if err := p.allergies.ReplaceForDocument(ctx, documentID, normalized.Allergies); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist allergies: %w", err))
	}
	if err := p.vitalSigns.ReplaceForDocument(ctx, documentID, normalized.VitalSigns); err != nil {
		return fail(fmt.Errorf("pipeline: process: persist vital signs: %w", err))
	}
	if err := p.matchPlannedActions(ctx, userID, documentID, normalized); err != nil {
		return fail(fmt.Errorf("pipeline: process: match planned actions: %w", err))
	}
	documentPlannedActions, err := p.plannedActionsForDocument(ctx, userID, documentID)
	if err != nil {
		return fail(fmt.Errorf("pipeline: process: list planned actions: %w", err))
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
		return fail(fmt.Errorf("pipeline: process: update document: %w", err))
	}

	if err := p.rebuildTimeline(ctx, userID, documentID, title, documentDate, normalized); err != nil {
		return fail(fmt.Errorf("pipeline: process: rebuild timeline: %w", err))
	}
	if err := p.rebuildProfile(ctx, userID); err != nil {
		return fail(fmt.Errorf("pipeline: process: rebuild profile: %w", err))
	}

	// FTS is a pure, local SQLite operation — treated as required, same as
	// every other persistence step above (see docs/architecture/04-search.md
	// §13's recommended index content: full text + Summary + recommendations —
	// except FullText itself for documentTypesWithoutFreeTextContent, see its
	// doc comment).
	ftsParts := []string{summary}
	if !documentTypesWithoutFreeTextContent[extracted.DocumentType] {
		ftsParts = append([]string{extracted.FullText}, ftsParts...)
	}
	ftsParts = append(ftsParts, extracted.Recommendations...)
	ftsContent := strings.Join(ftsParts, "\n")
	if err := p.fts.IndexDocument(ctx, userID, documentID, title, ftsContent); err != nil {
		return fail(fmt.Errorf("pipeline: process: index fts: %w", err))
	}

	// Embeddings depend on an external LLM call — per
	// docs/architecture/02-processing-pipeline.md §14 ("Границы транзакций"),
	// a failure here must never make the document unavailable for
	// Timeline/Profile/FTS, so it's best-effort: logged, not propagated.
	p.generateDocumentEmbedding(ctx, userID, documentID, summary)

	if err := p.documentRepo.UpdateStatus(ctx, documentID, userID, storage.DocumentStatusReady); err != nil {
		return Result{}, fmt.Errorf("pipeline: process: mark ready: %w", err)
	}

	p.logger.Debug("pipeline: process done", "documentId", documentID, "userId", userID, "status", storage.DocumentStatusReady)

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
			PlannedActions:       len(normalized.PlannedActions),
		},
		PlannedActions: documentPlannedActions,
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

// matchPlannedActions implements docs/adr/004-planned-actions.md §4's
// auto-completion step. Runs for every document (upload and reprocess —
// same code path, no branching): first reconciles documentID's own
// document-sourced PlannedActions (normalization.PlannedActionSourceDocument)
// against normalized.PlannedActions via ReplaceForSource's non-destructive
// reconciliation, then unconditionally reverts any PlannedAction previously
// completed *by* documentID back to pending (ClearMatchesFromDocument) —
// necessary before rematching so a reprocess whose new extraction no longer
// contains the previously-matching result doesn't leave a stale completion
// behind — and finally rematches every user's still-pending action against
// this document's freshly normalized entities (planmatch.Match), marking
// whatever matches as completed with a backlink to documentID/the matched
// entity.
//
// A pure, local SQLite operation with no external LLM call, so — like
// Diagnoses/Timeline/Profile above — this is required, not best-effort like
// Embeddings.
func (p *Pipeline) matchPlannedActions(ctx context.Context, userID, documentID string, normalized normalization.Result) error {
	if err := p.plannedActions.ReplaceForSource(ctx, normalization.PlannedActionSourceDocument, documentID, normalized.PlannedActions); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	if err := p.plannedActions.ClearMatchesFromDocument(ctx, documentID); err != nil {
		return fmt.Errorf("clear matches: %w", err)
	}
	pending, err := p.plannedActions.ListPending(ctx, userID)
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}
	for _, c := range planmatch.Match(normalized, pending) {
		if err := p.plannedActions.MarkCompleted(ctx, c.PlannedActionID, documentID, c.MatchedEntityID, time.Now()); err != nil {
			return fmt.Errorf("mark completed: %w", err)
		}
	}
	return nil
}

// plannedActionsForDocument reads back documentID's own current
// PlannedActions (sourceType document, sourceId documentID) after
// matchPlannedActions has persisted them — see Result.PlannedActions' doc
// comment for why this can't just reuse the in-memory normalized.PlannedActions
// passed into matchPlannedActions. ListByUser is already the narrowest
// repository method that can answer this; a document-scoped variant isn't
// worth adding for what's a low-volume, household-scale table.
func (p *Pipeline) plannedActionsForDocument(ctx context.Context, userID, documentID string) ([]normalization.PlannedAction, error) {
	all, err := p.plannedActions.ListByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list by user: %w", err)
	}
	var forDocument []normalization.PlannedAction
	for _, a := range all {
		if a.SourceType == normalization.PlannedActionSourceDocument && a.SourceID == documentID {
			forDocument = append(forDocument, a)
		}
	}
	return forDocument, nil
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

// RebuildProfile re-runs Medical Profile aggregation for userID against
// already-persisted data and replaces the stored MedicalProfile — exactly
// the same step every document upload/reprocess and self-reported event
// already triggers automatically via rebuildProfile below. Exposed as its
// own public method purely so medical-dev's "profile --rebuild" flag
// (docs/cli/medical_dev.md §9) can trigger it by hand — e.g. after a
// Profile-shaping change ships (new aggregation logic, a new field), to
// refresh existing users' stored snapshots without re-running OCR/
// Structured Extraction on every one of their documents. Not otherwise
// called from within this package — internal call sites use rebuildProfile
// directly since they don't need the freshly rebuilt Profile value back.
func (p *Pipeline) RebuildProfile(ctx context.Context, userID string) (profile.Profile, error) {
	if err := p.rebuildProfile(ctx, userID); err != nil {
		return profile.Profile{}, err
	}
	return p.GetProfile(ctx, userID)
}

// rebuildProfile fully rebuilds and replaces userID's MedicalProfile — run
// after every successful document import, per
// docs/domain/05-medical-profile.md §4 ("пересобирается полностью после
// каждого успешного upload_document"). Nutrition Guidance
// (docs/adr/006-nutrition-guidance.md) is generated as a separate step
// after profileBuilder.Build, not inside Build itself — see that ADR's §2
// for why profile.Builder stays LLM-free and this orchestration lives here
// instead, the same way indexEmbedding below is a separate stage rather
// than something Build does.
func (p *Pipeline) rebuildProfile(ctx context.Context, userID string) error {
	built, err := p.profileBuilder.Build(ctx, userID)
	if err != nil {
		return err
	}
	built.NutritionGuidance = p.nutritionGuidance(ctx, userID, built)
	return p.profileStore.Replace(ctx, built)
}

// nutritionGuidance produces the NutritionGuidance to store on this rebuild
// of userID's Profile — built is the just-computed Profile (not yet
// persisted), used for its ActiveDiagnoses (already a superset of
// ChronicConditions, docs/domain/05-medical-profile.md §2) and Allergies.
// Never returns an error: every failure mode (no dietary-relevant input,
// provider error, malformed result) is handled here and logged, per
// docs/adr/006-nutrition-guidance.md §4-5 — Nutrition Guidance is an
// enrichment, not a required part of a successful rebuild, the same
// posture indexEmbedding's own failure handling already takes for
// Embedding Search.
func (p *Pipeline) nutritionGuidance(ctx context.Context, userID string, built profile.Profile) profile.NutritionGuidance {
	now := time.Now().UTC()

	input := nutrition.Input{
		Diagnoses:     diagnosisNames(built.ActiveDiagnoses),
		Allergies:     allergyDescriptions(built.Allergies),
		Medications:   medicationNames(built.ActiveMedications),
		PastSurgeries: procedureNames(built.Surgeries),
	}
	if symptoms, err := p.recentSymptoms(ctx, userID, now); err != nil {
		p.logger.Warn("pipeline: list recent self-reported symptoms for nutrition guidance failed", "userId", userID, "error", err)
	} else {
		input.RecentSymptoms = symptoms
	}
	if p.users != nil {
		if u, err := p.users.FindByID(ctx, userID); err != nil {
			p.logger.Warn("pipeline: find user for nutrition guidance failed", "userId", userID, "error", err)
		} else {
			input.AgeYears = ageYears(u.BirthDate, now)
			input.Sex = u.Sex
		}
	}

	// §4: nothing dietary-relevant to reason over — skip the call
	// entirely rather than pay for a generic, non-medical answer. This is
	// not an error, so no carry-forward of a previous value below: an
	// empty Profile really does mean "no known restrictions right now."
	if input.Empty() {
		return profile.NutritionGuidance{}
	}

	guidance, err := nutrition.Generate(ctx, p.extractionProvider, input, now)
	if err == nil {
		return guidance
	}
	p.logger.Warn("pipeline: generate nutrition guidance failed", "userId", userID, "error", err)

	// §5: MedicalProfile only supports a full Replace, so doing nothing
	// here would silently wipe out yesterday's successfully generated
	// guidance on every transient LLM failure. Carry the previous stored
	// value forward instead — a stale-but-correct guidance beats losing it
	// for one failed call. No previous Profile (first-ever build, or a
	// user who's never had one generated) just yields the zero value,
	// which is already valid (docs/domain/05-medical-profile.md §4).
	previous, found, err := p.profileStore.Get(ctx, userID)
	if err != nil {
		p.logger.Warn("pipeline: read previous profile for nutrition guidance carry-forward failed", "userId", userID, "error", err)
		return profile.NutritionGuidance{}
	}
	if !found {
		return profile.NutritionGuidance{}
	}
	return previous.NutritionGuidance
}

// recentSymptoms returns the descriptions of userID's "symptom"-category
// self-reported events with OccurredAt in the last month, oldest first —
// docs/adr/006-nutrition-guidance.md §6's "не старше месяца". Filters
// Category/Status in Go rather than in SQL (only the date bound is pushed
// down via storage.DateRange) — the same split resolveActiveDiagnoses
// already uses for status filtering in internal/profile, reasonable here
// too since one user's one-month event list is never large.
func (p *Pipeline) recentSymptoms(ctx context.Context, userID string, now time.Time) ([]string, error) {
	from := now.AddDate(0, -1, 0)
	events, err := p.selfReportedEvents.ListByUser(ctx, userID, storage.DateRange{From: &from})
	if err != nil {
		return nil, fmt.Errorf("pipeline: list self-reported events: %w", err)
	}
	var symptoms []string
	for _, e := range events {
		if e.Category != "symptom" || e.Status != storage.DocumentStatusReady {
			continue
		}
		text := e.Description
		if text == "" {
			// Structuring found nothing (docs/domain/12-self-reported-events.md
			// §3's invariant: still saved READY) — fall back to the raw
			// text rather than dropping the symptom from input entirely.
			text = e.RawText
		}
		if text != "" {
			symptoms = append(symptoms, text)
		}
	}
	return symptoms, nil
}

// procedureNames maps Profile.Vaccinations/Surgeries down to plain names —
// used for nutrition.Input.PastSurgeries (built.Surgeries already has no
// date bound applied, see that field's own doc comment in internal/profile,
// so this needs no filtering of its own beyond the name projection).
func procedureNames(procedures []profile.ProcedureSummary) []string {
	if len(procedures) == 0 {
		return nil
	}
	names := make([]string, len(procedures))
	for i, p := range procedures {
		names[i] = p.Name
	}
	return names
}

func medicationNames(medications []profile.MedicationSummary) []string {
	if len(medications) == 0 {
		return nil
	}
	names := make([]string, len(medications))
	for i, m := range medications {
		names[i] = m.DrugName
	}
	return names
}

func diagnosisNames(diagnoses []profile.DiagnosisSummary) []string {
	if len(diagnoses) == 0 {
		return nil
	}
	names := make([]string, len(diagnoses))
	for i, d := range diagnoses {
		names[i] = d.Name
	}
	return names
}

func allergyDescriptions(allergies []profile.AllergySummary) []string {
	if len(allergies) == 0 {
		return nil
	}
	descriptions := make([]string, len(allergies))
	for i, a := range allergies {
		if a.Reaction == "" {
			descriptions[i] = a.Substance
		} else {
			descriptions[i] = fmt.Sprintf("%s (%s)", a.Substance, a.Reaction)
		}
	}
	return descriptions
}

// ageYears computes a whole-years age from birthDate as of now — nil if
// birthDate is unknown. Deliberately a plain number, not a bucketed
// "child"/"elderly" label — see docs/adr/006-nutrition-guidance.md §7:
// where age-band reasoning kicks in is left to the prompt/model, not
// hardcoded here.
func ageYears(birthDate *time.Time, now time.Time) *int {
	if birthDate == nil {
		return nil
	}
	years := now.Year() - birthDate.Year()
	if now.YearDay() < birthDate.YearDay() {
		years--
	}
	if years < 0 {
		years = 0
	}
	return &years
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

// documentTypesWithoutFreeTextContent are extraction.Result.DocumentType
// values whose entire expected content is already fully captured as
// structured entities — lab_report as LabResult rows, prescription as
// Medication rows (mirrors extraction.expectedCategoriesByDocumentType,
// which lists exactly these two as having a single, fully-structured
// expected category and no "recommendations"/free-text category). Their
// raw OCR'd FullText carries no genuine free-text content beyond that —
// only boilerplate (reference ranges, lab-methodology footnotes,
// accreditation text) FTS has no way to tell apart from something real,
// which is exactly docs/architecture/04-search.md §2's "Использовать
// наиболее структурированный источник" and §13's "Не рекомендуется
// индексировать структурированные сущности — они уже представлены в
// SQLite". Traced back to a real medical.ask failure where an FTS hit on
// "рекомендации" turned out to be a lab report's WHO reference-range
// citation, not a doctor's actual advice — the agent had no way to tell
// the difference either, and burned its remaining tool calls chasing it.
// Summary and Recommendations (both already-extracted, not raw OCR) stay
// indexed for these types regardless, in the rare case Extraction did
// find real free text worth surfacing.
var documentTypesWithoutFreeTextContent = map[string]bool{
	"lab_report":   true,
	"prescription": true,
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
