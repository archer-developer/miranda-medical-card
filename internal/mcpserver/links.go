package mcpserver

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/archer-developer/miranda-medical-card/internal/linkstore"
)

// NewLinkDownloadHandler serves a previously minted ephemeral link's bytes
// over plain HTTP GET — docs/adr/007-short-lived-file-links-for-profile-export.md.
// The httpserver.New route this is mounted under must be "GET
// /links/{linkId}" so r.PathValue("linkId") resolves.
//
// Unlike NewFileDownloadHandler, this route carries no bearer token (see
// httpserver.New's doc comment) — the random id plus links.Resolve's TTL
// check is the entire access control here, by design.
func NewLinkDownloadHandler(links *linkstore.Store, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		linkID := strings.TrimSpace(r.PathValue("linkId"))
		if linkID == "" {
			http.Error(w, "linkId is required", http.StatusBadRequest)
			return
		}

		content, contentType, filename, err := links.Resolve(r.Context(), linkID)
		if err != nil {
			if errors.Is(err, linkstore.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			logger.Error("link download failed", "linkId", linkID, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", contentDispositionAttachment(filename))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		logger.Info("link download", "linkId", linkID)
		_, _ = w.Write(content)
	}
}

// linkURI builds the absolute URI medical.profile's format=file_uri
// response embeds for linkID, rooted at publicBaseURL. Must stay in sync
// with the "GET /links/{linkId}" route httpserver.New mounts
// NewLinkDownloadHandler under.
func linkURI(publicBaseURL, linkID string) string {
	return strings.TrimRight(publicBaseURL, "/") + "/links/" + linkID
}
