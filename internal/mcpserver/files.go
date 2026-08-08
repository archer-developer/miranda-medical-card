package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func registerFileTools(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.upload_file",
		Description: "Загружает бинарный файл в файловое хранилище, без какой-либо медицинской обработки. Возвращает fileId для последующего medical.upload_document. См. docs/mcp/02-files.md §4.",
	}, uploadFileHandler(pl, gate, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.download_file",
		Description: "Возвращает ранее загруженный файл в исходном виде. См. docs/mcp/02-files.md §6.",
	}, downloadFileHandler(pl, gate, logger))
}

// --- medical.upload_file ---

type UploadFileInput struct {
	UserID      string `json:"userId" jsonschema:"Идентификатор пользователя."`
	Filename    string `json:"filename" jsonschema:"Оригинальное имя файла."`
	ContentType string `json:"contentType" jsonschema:"MIME Type."`
	Data        string `json:"data" jsonschema:"Содержимое файла, закодированное в base64."`
}

type UploadFileOutput struct {
	FileID      string `json:"fileId"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"contentType"`
}

func uploadFileHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[UploadFileInput, UploadFileOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in UploadFileInput) (*mcp.CallToolResult, UploadFileOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, UploadFileOutput{}, err
		}
		if strings.TrimSpace(in.Filename) == "" || strings.TrimSpace(in.ContentType) == "" {
			return nil, UploadFileOutput{}, mcpError(codeInvalidFile, "filename and contentType are required")
		}
		data, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil || len(data) == 0 {
			return nil, UploadFileOutput{}, mcpError(codeInvalidFile, "data must be non-empty valid base64")
		}

		file, err := pl.UploadFile(ctx, in.UserID, in.Filename, in.ContentType, data)
		if err != nil {
			logger.Error("upload_file failed", "userId", in.UserID, "error", err)
			return nil, UploadFileOutput{}, mcpError(codeStorageError, "%v", err)
		}

		logger.Info("upload_file", "userId", in.UserID, "fileId", file.ID, "size", file.Size)
		out := UploadFileOutput{FileID: file.ID, Size: file.Size, SHA256: file.SHA256, ContentType: file.ContentType}
		text := fmt.Sprintf("Uploaded. fileId: %s  size: %d bytes", out.FileID, out.Size)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- medical.download_file ---

type DownloadFileInput struct {
	UserID string `json:"userId" jsonschema:"Идентификатор пользователя."`
	FileID string `json:"fileId" jsonschema:"Идентификатор файла."`
}

type DownloadFileOutput struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	Data        string `json:"data"` // base64
}

func downloadFileHandler(pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) mcp.ToolHandlerFor[DownloadFileInput, DownloadFileOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in DownloadFileInput) (*mcp.CallToolResult, DownloadFileOutput, error) {
		if err := gate.requireUser(in.UserID); err != nil {
			return nil, DownloadFileOutput{}, err
		}
		if strings.TrimSpace(in.FileID) == "" {
			return nil, DownloadFileOutput{}, mcpError(codeFileNotFound, "fileId is required")
		}

		// Resource-scoped sharing: try the requester's own files first,
		// then anyone who has shared with them — see
		// userGate.resolveOwnersToTry's doc comment.
		var (
			file storage.File
			data []byte
			err  error
		)
		found := false
		for _, ownerID := range gate.resolveOwnersToTry(in.UserID) {
			file, data, err = pl.DownloadFile(ctx, ownerID, in.FileID)
			if err == nil {
				found = true
				break
			}
			if !errors.Is(err, storage.ErrNotFound) {
				logger.Error("download_file failed", "userId", in.UserID, "fileId", in.FileID, "error", err)
				return nil, DownloadFileOutput{}, mcpError(codeStorageError, "%v", err)
			}
		}
		if !found {
			return nil, DownloadFileOutput{}, mcpError(codeFileNotFound, "file not found")
		}

		logger.Info("download_file", "userId", in.UserID, "fileId", in.FileID)
		out := DownloadFileOutput{
			Filename: file.Filename, ContentType: file.ContentType, Size: file.Size,
			Data: base64.StdEncoding.EncodeToString(data),
		}
		text := fmt.Sprintf("%s (%s, %d bytes)", out.Filename, out.ContentType, out.Size)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}
