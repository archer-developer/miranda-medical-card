package pipeline

import (
	"context"
	"fmt"

	"github.com/archer-developer/miranda-medical-card/internal/profile"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

// This file adds the read-only Application Service methods the MCP layer
// needs (docs/mcp/02-files.md §5, docs/mcp/03-documents.md §7-8,
// docs/mcp/05-profile.md, docs/mcp/06-timeline.md) — kept separate from
// pipeline.go's write-path orchestration (UploadDocument/ReprocessDocument/
// LogEvent), which is what actually earns this package its name. Routed
// through Pipeline rather than having internal/mcpserver call
// internal/storage directly, matching docs/domain/01-overview.md §2's
// layering (MCP -> Application -> Domain -> Repository) — the same
// principle docs/cli/medical.md §3 states explicitly for the CLI ("CLI не
// обращается к Repository напрямую").

// DownloadFile implements docs/mcp/02-files.md §5's medical.download_file:
// returns a previously uploaded file's metadata and content, unmodified,
// re-checking that fileID belongs to userID on every call (via
// fileRepo.Get, not GetByID) — unlike DownloadFileByID below, this is the
// path that still enforces ownership/shared_with per request, so a
// shared_with revocation takes effect immediately instead of only at the
// moment a fileUri was minted.
func (p *Pipeline) DownloadFile(ctx context.Context, userID, fileID string) (storage.File, []byte, error) {
	file, err := p.fileRepo.Get(ctx, fileID, userID)
	if err != nil {
		return storage.File{}, nil, err
	}
	data, err := p.files.Read(file.StoragePath)
	if err != nil {
		return storage.File{}, nil, fmt.Errorf("pipeline: download file: %w", err)
	}
	return file, data, nil
}

// DownloadFileByID implements the GET /files/{fileId} endpoint (see
// internal/mcpserver.NewFileDownloadHandler): returns a previously uploaded
// file's metadata and content, unmodified, looked up by fileId alone, with
// no per-request ownership check — the URI this backs is only ever handed
// out once, inside an authenticated medical.get_document call. Callers
// that need ownership/shared_with re-checked on every fetch (e.g. after a
// share was revoked) should use DownloadFile via medical.download_file
// instead.
func (p *Pipeline) DownloadFileByID(ctx context.Context, fileID string) (storage.File, []byte, error) {
	file, err := p.fileRepo.GetByID(ctx, fileID)
	if err != nil {
		return storage.File{}, nil, err
	}
	data, err := p.files.Read(file.StoragePath)
	if err != nil {
		return storage.File{}, nil, fmt.Errorf("pipeline: download file: %w", err)
	}
	return file, data, nil
}

// ListDocuments implements docs/mcp/03-documents.md §7.
func (p *Pipeline) ListDocuments(ctx context.Context, userID string) ([]storage.MedicalDocument, error) {
	docs, err := p.documentRepo.List(ctx, userID, storage.DocumentFilter{})
	if err != nil {
		return nil, fmt.Errorf("pipeline: list documents: %w", err)
	}
	return docs, nil
}

// GetDocument implements docs/mcp/03-documents.md §8.
func (p *Pipeline) GetDocument(ctx context.Context, userID, documentID string) (storage.MedicalDocument, error) {
	return p.documentRepo.Get(ctx, documentID, userID)
}

// GetProfile implements docs/mcp/05-profile.md — returns userID's current
// MedicalProfile, or an empty one if it hasn't been built yet (see
// docs/domain/05-medical-profile.md §4: no READY documents yet is not an
// error).
func (p *Pipeline) GetProfile(ctx context.Context, userID string) (profile.Profile, error) {
	built, found, err := p.profileStore.Get(ctx, userID)
	if err != nil {
		return profile.Profile{}, fmt.Errorf("pipeline: get profile: %w", err)
	}
	if !found {
		return profile.Profile{UserID: userID}, nil
	}
	return built, nil
}

// GetTimeline implements docs/mcp/06-timeline.md.
func (p *Pipeline) GetTimeline(ctx context.Context, userID string, filter storage.TimelineFilter) ([]timeline.Event, error) {
	events, err := p.timelineRepo.List(ctx, userID, filter)
	if err != nil {
		return nil, fmt.Errorf("pipeline: get timeline: %w", err)
	}
	return events, nil
}
