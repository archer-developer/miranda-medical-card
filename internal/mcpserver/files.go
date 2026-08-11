package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// This file implements the Files API's two ways to reach an original file
// (docs/mcp/02-files.md): the plain HTTP download handler below, and the
// medical.download_file MCP tool. There is no medical.upload_file — a File
// is only ever created as a side effect of medical.upload_document(fileUri)
// fetching the content itself (see documents.go).
//
// The two download paths exist for different trust needs, not as
// duplicates of each other:
//
//   - fileUri (medical.get_document's response, resolved by
//     NewFileDownloadHandler below) is the fast path: a plain HTTP GET, no
//     MCP round-trip, mirroring the same "hand over a URI, not the bytes"
//     pattern medical.upload_document already uses in the other direction
//     (docs/mcp/02-files.md §2). Ownership/shared_with is checked once, at
//     the moment the URI is minted inside get_document — the URI itself
//     carries no further access check and doesn't expire.
//   - medical.download_file re-checks ownership/shared_with on every call
//     (via gate.resolveOwnersToTry, same as get_document) — this is the
//     path to use when that per-call guarantee matters, e.g. a caller that
//     can't be sure a previously-obtained fileUri still reflects the
//     owner's current shared_with.

// NewFileDownloadHandler serves a previously uploaded file's raw bytes over
// plain HTTP GET — the httpserver.New route this handler is mounted under
// must be "GET /files/{fileId}" so r.PathValue("fileId") resolves and so
// the path matches what fileURI builds.
//
// Access control already happened once, when the fileUri was minted inside
// an authenticated medical.get_document call (gate.resolveOwnersToTry).
// This endpoint relies purely on the bearer token httpserver.New wraps it
// in for trust — the same full-trust model docs/mcp/01-overview.md §5
// already documents for medical.upload_document's fileUri fetch.
func NewFileDownloadHandler(pl *pipeline.Pipeline, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		fileID := strings.TrimSpace(r.PathValue("fileId"))
		if fileID == "" {
			http.Error(w, "fileId is required", http.StatusBadRequest)
			return
		}

		file, data, err := pl.DownloadFileByID(r.Context(), fileID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			logger.Error("file download failed", "fileId", fileID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", file.ContentType)
		w.Header().Set("Content-Disposition", contentDispositionAttachment(file.Filename))
		// Content-Length is derived from the bytes actually being written
		// (data), not file.Size (SQLite metadata) — the two are expected to
		// agree, but deriving it from data guarantees the header can never
		// disagree with what net/http actually sends.
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		logger.Info("file download", "fileId", fileID)
		_, _ = w.Write(data)
	}
}

// contentDispositionAttachment builds an RFC 6266 Content-Disposition
// header for filename, including both a legacy ASCII-only filename
// parameter (for clients that don't understand the extended form) and the
// filename*=UTF-8''... form (RFC 5987) carrying the exact name — needed
// because filenames here routinely contain Cyrillic (this service's
// documents are Russian-language medical records), which plain
// filename="..." cannot represent correctly per spec.
func contentDispositionAttachment(filename string) string {
	fallback := asciiFallbackFilename(filename)
	encoded := strings.ReplaceAll(url.QueryEscape(filename), "+", "%20")
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, encoded)
}

// asciiFallbackFilename replaces every non-ASCII, control, quote, or
// backslash rune in filename with "_" — a legacy fallback for HTTP clients
// that only understand the plain filename="..." parameter, not
// filename*=UTF-8''... (see contentDispositionAttachment).
func asciiFallbackFilename(filename string) string {
	var b strings.Builder
	for _, r := range filename {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}

// fileURI builds the absolute URI medical.get_document embeds for fileID,
// rooted at publicBaseURL (config.Config.PublicBaseURL). Must stay in sync
// with the "GET /files/{fileId}" route httpserver.New mounts
// NewFileDownloadHandler under.
func fileURI(publicBaseURL, fileID string) string {
	return strings.TrimRight(publicBaseURL, "/") + "/files/" + fileID
}

// --- medical.download_file ---

func registerFileTools(server *mcp.Server, pl *pipeline.Pipeline, gate *userGate, logger *slog.Logger) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "medical.download_file",
		Description: "Returns a previously uploaded file in its original, unmodified form (data is base64). Unlike the fileUri returned by medical.get_document, this re-checks the caller's ownership/shared_with access on every call — use it when that per-call guarantee matters (e.g. a fileUri obtained earlier might no longer reflect the owner's current shared_with).",
	}, downloadFileHandler(pl, gate, logger))
}

type DownloadFileInput struct {
	UserID string `json:"userId" jsonschema:"User identifier."`
	FileID string `json:"fileId" jsonschema:"File identifier."`
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
