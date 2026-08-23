// Package httpserver wires the MCP Streamable HTTP handler behind a single
// mux with bearer-token auth and an unauthenticated /healthz endpoint.
// Ported from miranda-diary's package of the same name.
package httpserver

import (
	"net/http"
)

// New builds the top-level HTTP handler:
//   - GET /healthz        — unauthenticated liveness check.
//   - /mcp                — MCP Streamable HTTP endpoint, bearer-auth-gated.
//   - GET /files/{fileId} — raw file download, bearer-auth-gated the same
//     way as /mcp — the URI medical.get_document embeds in its response
//     (see internal/mcpserver's fileURI/NewFileDownloadHandler) resolves
//     here, so Miranda fetches the original file with a plain HTTP GET
//     instead of a dedicated MCP tool. fileHandler must be built with
//     mcpserver.NewFileDownloadHandler.
//   - GET /links/{linkId} — short-lived ephemeral link download,
//     deliberately NOT bearer-auth-gated (docs/adr/007-short-lived-file-
//     links-for-profile-export.md): a random 128-bit id plus a few-minute
//     TTL is the access control here, not the shared token — see that
//     ADR's "Безопасность" section for why that's sufficient (this service
//     only listens on 127.0.0.1, same as /mcp and /files). linkHandler must
//     be built with mcpserver.NewLinkDownloadHandler.
//
// token is the shared secret both mcpHandler and fileHandler's callers must
// present.
func New(mcpHandler, fileHandler, linkHandler http.Handler, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/mcp", requireBearerToken(token, mcpHandler))
	mux.Handle("GET /files/{fileId}", requireBearerToken(token, fileHandler))
	mux.Handle("GET /links/{linkId}", linkHandler)
	return mux
}
