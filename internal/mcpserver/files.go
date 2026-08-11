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

// registerFileTools registers the read side of the Files API only.
// medical.upload_file (which used to accept a file's bytes as a base64
// `data` argument) has been removed: a File is now created only as a side
// effect of medical.upload_document(fileUri) fetching the content itself
// (see documents.go, docs/mcp/03-documents.md §4) — no MCP tool in this
// service ever accepts raw file bytes as an argument, see docs/mcp/02-files.md §2.
func registerFileTools(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.download_file",
		Description: "Возвращает ранее загруженный файл в исходном, неизменённом виде (данные — base64).",
	}, downloadFileHandler(pl, gate, logger))
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
