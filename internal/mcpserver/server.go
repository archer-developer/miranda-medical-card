// Package mcpserver registers Medical Service's MCP tools
// (docs/mcp/01-overview.md) and wires them to the Application Services that
// actually do the work (internal/pipeline.Pipeline for Files/Documents/
// Events, internal/ask.Asker for medical.ask). Every handler is a thin
// adapter: validate input (userId, sharing), delegate, shape the response —
// no business logic lives here (docs/mcp/01-overview.md §4: "Внутреннее
// устройство сервиса не является частью MCP API").
//
// medical.delete_document is deliberately not implemented — cascading
// deletion across every derived layer (Timeline, Domain Entities,
// Extraction, Embeddings, FTS, File) is out of scope for this pass; every
// other documented tool is registered.
package mcpserver

import (
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-medical-card/internal/ask"
	"github.com/archer-developer/miranda-medical-card/internal/config"
	"github.com/archer-developer/miranda-medical-card/internal/pipeline"
)

const (
	serverName    = "miranda-medical-card"
	serverVersion = "0.1.0"
)

// New builds the MCP server with every tool registered. maxFileSizeBytes
// bounds how much medical.upload_document (see documents.go) will read from
// a caller-supplied fileUri. A nil logger falls back to slog.Default().
func New(pl *pipeline.Pipeline, asker *ask.Asker, users []config.UserConfig, maxFileSizeBytes int64, logger *slog.Logger) *mcp.Server {
	if logger == nil {
		logger = slog.Default()
	}
	gate := newUserGate(users)

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	registerFileTools(server, pl, gate, logger)
	registerDocumentTools(server, pl, gate, maxFileSizeBytes, logger)
	registerEventTools(server, pl, gate, logger)
	registerAskTool(server, asker, gate, logger)
	registerProfileTool(server, pl, gate, logger)
	registerTimelineTool(server, pl, gate, logger)

	return server
}
