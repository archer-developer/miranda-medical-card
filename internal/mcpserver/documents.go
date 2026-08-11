package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// fileFetchTimeout bounds the HTTP GET medical.upload_document issues
// against a caller-supplied fileUri (see fetchFile) — a slow or hung remote
// must not hang the whole upload_document request.
const fileFetchTimeout = 30 * time.Second

var fileFetchClient = &http.Client{Timeout: fileFetchTimeout}

func registerDocumentTools(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, maxFileSizeBytes int64, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.upload_document",
		Description: "Скачивает файл по fileUri (HTTP GET, без содержимого файла в аргументах вызова) и импортирует его в медицинскую базу знаний: OCR, извлечение медицинских сущностей, Timeline, Medical Profile, поисковые индексы. Синхронный — возвращает результат только после завершения всей обработки. Ответ включает extractedCounts (число сущностей каждого типа); если оно выглядит подозрительно маленьким для содержательного документа, вызовите medical.reprocess_document.",
	}, uploadDocumentHandler(pl, gate, maxFileSizeBytes, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.reprocess_document",
		Description: "Заново прогоняет уже импортированный документ через Pipeline по тому же файлу (без повторной загрузки) — для случая, когда результат upload_document выглядит неполным (см. extractedCounts в его ответе).",
	}, reprocessDocumentHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.list_documents",
		Description: "Возвращает список медицинских документов пользователя (без содержимого), отсортированный по дате медицинского события, а не по времени загрузки.",
	}, listDocumentsHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.get_document",
		Description: "Возвращает метаданные конкретного медицинского документа (без содержимого). Для оригинального файла используйте medical.download_file, для анализа содержимого — medical.ask.",
	}, getDocumentHandler(pl, gate, logger))
}

// --- shared response shape for upload_document/reprocess_document ---

type ExtractedCountsOutput struct {
	Diagnoses            int `json:"diagnoses"`
	Medications          int `json:"medications"`
	LabResults           int `json:"labResults"`
	InstrumentalFindings int `json:"instrumentalFindings"`
	Procedures           int `json:"procedures"`
	Allergies            int `json:"allergies"`
	VitalSigns           int `json:"vitalSigns"`
	Recommendations      int `json:"recommendations"`
}

func toExtractedCountsOutput(c pipeline.ExtractedCounts) ExtractedCountsOutput {
	return ExtractedCountsOutput{
		Diagnoses: c.Diagnoses, Medications: c.Medications, LabResults: c.LabResults,
		InstrumentalFindings: c.InstrumentalFindings, Procedures: c.Procedures,
		Allergies: c.Allergies, VitalSigns: c.VitalSigns, Recommendations: c.Recommendations,
	}
}

// --- medical.upload_document ---

type UploadDocumentInput struct {
	UserID  string `json:"userId" jsonschema:"Идентификатор пользователя."`
	FileURI string `json:"fileUri" jsonschema:"URI, по которому сервис самостоятельно скачивает содержимое файла (HTTP GET)."`
}

type UploadDocumentOutput struct {
	DocumentID      string                `json:"documentId"`
	Status          string                `json:"status"`
	Summary         string                `json:"summary"`
	ExtractedCounts ExtractedCountsOutput `json:"extractedCounts"`
}

// uploadDocumentHandler never accepts a file's bytes as an MCP argument —
// it takes a URI and fetches the content itself (see fetchFile). This is
// deliberate: base64-encoding a file into a tool call's JSON is fine for
// content an LLM already holds in its own context, but unreliable once a
// document runs to hundreds of KB (a typical scanned document), since
// nothing then guarantees the LLM reproduces that string byte-for-byte —
// see docs/mcp/02-files.md §2.
func uploadDocumentHandler(pl *pipeline.Pipeline, gate *userGate, maxFileSizeBytes int64, logger *slog.Logger) mcp.ToolHandlerFor[UploadDocumentInput, UploadDocumentOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in UploadDocumentInput) (*mcp.CallToolResult, UploadDocumentOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, UploadDocumentOutput{}, err
		}
		fileURI := strings.TrimSpace(in.FileURI)
		if !strings.HasPrefix(fileURI, "http://") && !strings.HasPrefix(fileURI, "https://") {
			return nil, UploadDocumentOutput{}, mcpError(codeInvalidFile, "fileUri must be a non-empty http(s) URL")
		}

		filename, contentType, data, err := fetchFile(ctx, fileURI, maxFileSizeBytes)
		if err != nil {
			return nil, UploadDocumentOutput{}, fetchFileError(err, logger, in.UserID, fileURI)
		}

		file, err := pl.UploadFile(ctx, in.UserID, filename, contentType, data)
		if err != nil {
			logger.Error("upload_document: store fetched file failed", "userId", in.UserID, "fileUri", fileURI, "error", err)
			return nil, UploadDocumentOutput{}, mcpError(codeStorageError, "%v", err)
		}

		result, err := pl.UploadDocument(ctx, in.UserID, file.ID)
		if err != nil {
			return nil, UploadDocumentOutput{}, uploadDocumentError(err, logger, in.UserID, file.ID)
		}

		logger.Info("upload_document", "userId", in.UserID, "documentId", result.DocumentID, "status", result.Status)
		out := UploadDocumentOutput{DocumentID: result.DocumentID, Status: result.Status, Summary: result.Summary, ExtractedCounts: toExtractedCountsOutput(result.ExtractedCounts)}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.Summary}}}, out, nil
	}
}

func uploadDocumentError(err error, logger *slog.Logger, userID, fileID string) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return mcpError(codeFileNotFound, "file not found")
	case errors.Is(err, pipeline.ErrAlreadyImported):
		return mcpError(codeDocumentAlreadyImported, "file %s has already been imported", fileID)
	default:
		logger.Error("upload_document failed", "userId", userID, "fileId", fileID, "error", err)
		return mcpError(codePipelineFailed, "%v", err)
	}
}

// errFileURINotFound is fetchFile's sentinel for a 404 response — kept
// distinct from other fetch failures so fetchFileError can report it as
// FILE_NOT_FOUND (the same code medical.download_file uses for "doesn't
// exist"), rather than the more general FILE_FETCH_FAILED.
var errFileURINotFound = errors.New("mcpserver: fetch file: not found")

// fetchFile performs the HTTP GET a caller-supplied fileUri requires,
// bounding the response body to maxSizeBytes (+1, so an oversized body is
// detected rather than silently truncated) and deriving a filename/content
// type from the response the same way a browser would: Content-Disposition
// first, falling back to the URI's path, then Content-Type, falling back to
// application/octet-stream.
func fetchFile(ctx context.Context, fileURI string, maxSizeBytes int64) (filename, contentType string, data []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURI, nil)
	if err != nil {
		return "", "", nil, fmt.Errorf("mcpserver: fetch file: build request: %w", err)
	}

	resp, err := fileFetchClient.Do(req)
	if err != nil {
		return "", "", nil, fmt.Errorf("mcpserver: fetch file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return "", "", nil, errFileURINotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", nil, fmt.Errorf("mcpserver: fetch file: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSizeBytes+1))
	if err != nil {
		return "", "", nil, fmt.Errorf("mcpserver: fetch file: read body: %w", err)
	}
	if int64(len(body)) > maxSizeBytes {
		return "", "", nil, fmt.Errorf("mcpserver: fetch file: body exceeds %d-byte limit", maxSizeBytes)
	}

	contentType = resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return filenameFromResponse(resp, fileURI), contentType, body, nil
}

func filenameFromResponse(resp *http.Response, fileURI string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil && params["filename"] != "" {
			return params["filename"]
		}
	}
	if u, err := url.Parse(fileURI); err == nil {
		if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "file"
}

func fetchFileError(err error, logger *slog.Logger, userID, fileURI string) error {
	if errors.Is(err, errFileURINotFound) {
		return mcpError(codeFileNotFound, "no file found at fileUri")
	}
	logger.Error("upload_document: fetch fileUri failed", "userId", userID, "fileUri", fileURI, "error", err)
	return mcpError(codeFileFetchFailed, "failed to fetch fileUri: %v", err)
}

// --- medical.reprocess_document ---

type ReprocessDocumentInput struct {
	UserID     string `json:"userId" jsonschema:"Идентификатор пользователя."`
	DocumentID string `json:"documentId" jsonschema:"Идентификатор документа."`
}

type ReprocessDocumentOutput = UploadDocumentOutput

func reprocessDocumentHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[ReprocessDocumentInput, ReprocessDocumentOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ReprocessDocumentInput) (*mcp.CallToolResult, ReprocessDocumentOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, ReprocessDocumentOutput{}, err
		}
		if strings.TrimSpace(in.DocumentID) == "" {
			return nil, ReprocessDocumentOutput{}, mcpError(codeDocumentNotFound, "documentId is required")
		}

		result, err := pl.ReprocessDocument(ctx, in.UserID, in.DocumentID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, ReprocessDocumentOutput{}, mcpError(codeDocumentNotFound, "document not found")
			}
			logger.Error("reprocess_document failed", "userId", in.UserID, "documentId", in.DocumentID, "error", err)
			return nil, ReprocessDocumentOutput{}, mcpError(codePipelineFailed, "%v", err)
		}

		logger.Info("reprocess_document", "userId", in.UserID, "documentId", result.DocumentID, "status", result.Status)
		out := ReprocessDocumentOutput{DocumentID: result.DocumentID, Status: result.Status, Summary: result.Summary, ExtractedCounts: toExtractedCountsOutput(result.ExtractedCounts)}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.Summary}}}, out, nil
	}
}

// --- medical.list_documents ---

type ListDocumentsInput struct {
	UserID    string `json:"userId" jsonschema:"Идентификатор пользователя."`
	SubjectID string `json:"subjectId,omitempty" jsonschema:"Чьи документы получить, если не свои."`
}

type DocumentListItem struct {
	DocumentID   string `json:"documentId"`
	Title        string `json:"title"`
	DocumentType string `json:"documentType,omitempty"`
	DocumentDate string `json:"documentDate,omitempty"`
	UploadedAt   string `json:"uploadedAt"`
}

type ListDocumentsOutput struct {
	Documents []DocumentListItem `json:"documents"`
}

func listDocumentsHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[ListDocumentsInput, ListDocumentsOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ListDocumentsInput) (*mcp.CallToolResult, ListDocumentsOutput, error) {
		subjectID, err := gate.resolveSubject(in.UserID, in.SubjectID)
		if err != nil {
			return nil, ListDocumentsOutput{}, err
		}

		docs, err := pl.ListDocuments(ctx, subjectID)
		if err != nil {
			logger.Error("list_documents failed", "userId", in.UserID, "error", err)
			return nil, ListDocumentsOutput{}, mcpError(codeStorageError, "%v", err)
		}

		items := make([]DocumentListItem, len(docs))
		for i, d := range docs {
			items[i] = DocumentListItem{
				DocumentID: d.ID, Title: d.Title, DocumentType: d.DocumentType, UploadedAt: d.UploadedAt.Format("2006-01-02T15:04:05Z07:00"),
			}
			if d.DocumentDate != nil {
				items[i].DocumentDate = d.DocumentDate.Format("2006-01-02")
			}
		}
		// docs/mcp/03-documents.md §7: "по дате медицинского события, а не
		// времени загрузки" — sorted newest medical event first; items with
		// no known documentDate sort last, using UploadedAt as a fallback
		// key so they're still in a stable, sensible order relative to
		// each other rather than left in arbitrary DB order.
		sortDocumentsByDate(items)

		logger.Info("list_documents", "userId", in.UserID, "subjectId", subjectID, "count", len(items))
		text := fmt.Sprintf("%d document(s).", len(items))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, ListDocumentsOutput{Documents: items}, nil
	}
}

func sortDocumentsByDate(items []DocumentListItem) {
	key := func(it DocumentListItem) string {
		if it.DocumentDate != "" {
			return it.DocumentDate
		}
		return it.UploadedAt
	}
	sort.SliceStable(items, func(i, j int) bool { return key(items[i]) > key(items[j]) })
}

// --- medical.get_document ---

type GetDocumentInput struct {
	UserID     string `json:"userId" jsonschema:"Идентификатор пользователя."`
	DocumentID string `json:"documentId" jsonschema:"Идентификатор документа."`
}

type GetDocumentOutput struct {
	DocumentID   string `json:"documentId"`
	Title        string `json:"title"`
	DocumentType string `json:"documentType,omitempty"`
	DocumentDate string `json:"documentDate,omitempty"`
	UploadedAt   string `json:"uploadedAt"`
	FileID       string `json:"fileId"`
}

func getDocumentHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[GetDocumentInput, GetDocumentOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetDocumentInput) (*mcp.CallToolResult, GetDocumentOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, GetDocumentOutput{}, err
		}
		if strings.TrimSpace(in.DocumentID) == "" {
			return nil, GetDocumentOutput{}, mcpError(codeDocumentNotFound, "documentId is required")
		}

		var (
			doc   storage.MedicalDocument
			err   error
			found bool
		)
		for _, ownerID := range gate.resolveOwnersToTry(in.UserID) {
			doc, err = pl.GetDocument(ctx, ownerID, in.DocumentID)
			if err == nil {
				found = true
				break
			}
			if !errors.Is(err, storage.ErrNotFound) {
				logger.Error("get_document failed", "userId", in.UserID, "documentId", in.DocumentID, "error", err)
				return nil, GetDocumentOutput{}, mcpError(codeStorageError, "%v", err)
			}
		}
		if !found {
			return nil, GetDocumentOutput{}, mcpError(codeDocumentNotFound, "document not found")
		}

		out := GetDocumentOutput{DocumentID: doc.ID, Title: doc.Title, DocumentType: doc.DocumentType, UploadedAt: doc.UploadedAt.Format("2006-01-02T15:04:05Z07:00"), FileID: doc.FileID}
		if doc.DocumentDate != nil {
			out.DocumentDate = doc.DocumentDate.Format("2006-01-02")
		}
		logger.Info("get_document", "userId", in.UserID, "documentId", in.DocumentID)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.Title}}}, out, nil
	}
}
