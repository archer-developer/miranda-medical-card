package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func registerDocumentTools(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.upload_document",
		Description: "Импортирует ранее загруженный файл (fileId из medical.upload_file) в медицинскую базу знаний: OCR, извлечение медицинских сущностей, Timeline, Medical Profile, поисковые индексы. См. docs/mcp/03-documents.md §4.",
	}, uploadDocumentHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.reprocess_document",
		Description: "Заново прогоняет уже импортированный документ через Pipeline по тому же файлу — для случая, когда результат upload_document выглядит неполным. См. docs/mcp/03-documents.md §6.",
	}, reprocessDocumentHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.list_documents",
		Description: "Возвращает список медицинских документов пользователя (без содержимого). См. docs/mcp/03-documents.md §7.",
	}, listDocumentsHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.get_document",
		Description: "Возвращает метаданные конкретного медицинского документа. См. docs/mcp/03-documents.md §8.",
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
	UserID string `json:"userId" jsonschema:"Идентификатор пользователя."`
	FileID string `json:"fileId" jsonschema:"Идентификатор ранее загруженного файла."`
}

type UploadDocumentOutput struct {
	DocumentID      string                `json:"documentId"`
	Status          string                `json:"status"`
	Summary         string                `json:"summary"`
	ExtractedCounts ExtractedCountsOutput `json:"extractedCounts"`
}

func uploadDocumentHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[UploadDocumentInput, UploadDocumentOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in UploadDocumentInput) (*mcp.CallToolResult, UploadDocumentOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, UploadDocumentOutput{}, err
		}
		if strings.TrimSpace(in.FileID) == "" {
			return nil, UploadDocumentOutput{}, mcpError(codeFileNotFound, "fileId is required")
		}

		result, err := pl.UploadDocument(ctx, in.UserID, in.FileID)
		if err != nil {
			return nil, UploadDocumentOutput{}, uploadDocumentError(err, logger, in.UserID, in.FileID)
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
