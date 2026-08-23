// Package linkstore mints and resolves short-lived, random-ID links for
// ephemeral, derived data — docs/adr/007-short-lived-file-links-for-profile-export.md.
// The motivating case is medical.profile's format=file_uri response
// (internal/mcpserver/profile.go): a caller whose real goal is handing the
// profile JSON to another tool (e.g. code-execution-sandbox's upload_file,
// for PDF generation) gets back a tiny fileUri instead of the full JSON
// blob, so the data can travel server-to-server without ever passing back
// through a model's own context (where a large JSON blob routinely gets
// summarized/truncated rather than copied verbatim — see that ADR's
// "Проблема" section).
//
// Distinct from internal/filestore (permanent, on-disk documents, keyed by
// content hash): a link here is random-ID keyed, lives for a few minutes,
// and its bytes sit in SQLite via storage.EphemeralLinkRepository, not on
// disk — the same TTL+random-UUID access-control pattern already proven
// for miranda's internal/attachments and miranda-code-execution-sandbox's
// internal/filestage.Stager (see that ADR's "Безопасность" section).
//
// Unlike the free-function Mint/Resolve(ctx, db *sql.DB, ...) sketch in
// that ADR, this package wraps a storage.EphemeralLinkRepository rather
// than a raw *sql.DB — the same "one narrow repository interface per
// entity" boundary internal/storage keeps for every other entity (see this
// repo's CLAUDE.md), and the only way to get an injectable clock seam for
// TestResolve_Expired without sleeping in a test (mirrors
// internal/ask.SessionStore's own now field for the same reason).
package linkstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// ErrNotFound is returned by Resolve when linkID never existed or has
// expired — the two cases are deliberately indistinguishable (see
// docs/adr/007 §2: no point giving a caller a way to tell "never existed"
// from "expired", and no reason to hand a timing oracle to whoever's
// probing a 128-bit random id).
var ErrNotFound = errors.New("linkstore: not found")

// Store mints and resolves ephemeral links backed by repo.
type Store struct {
	repo storage.EphemeralLinkRepository
	now  func() time.Time // test seam, defaults to time.Now
}

// New builds a Store backed by repo.
func New(repo storage.EphemeralLinkRepository) *Store {
	return &Store{repo: repo, now: time.Now}
}

// Mint stages content behind a new random-ID link that expires after ttl,
// and returns that ID plus the exact expiry it was minted with (so a
// caller reporting expiresAt back to a user/model reflects this Store's own
// clock, not a second, separately-read one). Building the externally
// reachable URI (e.g. rooted at config.Config.PublicBaseURL) is the
// caller's job, mirroring how internal/mcpserver's fileURI builds a URI
// from a storage.File's bare ID rather than that package owning URL
// construction itself.
func (s *Store) Mint(ctx context.Context, content []byte, contentType, filename string, ttl time.Duration) (linkID string, expiresAt time.Time, err error) {
	now := s.now()
	expiresAt = now.Add(ttl)
	added, err := s.repo.Add(ctx, storage.EphemeralLink{
		Content: content, ContentType: contentType, Filename: filename,
		CreatedAt: now, ExpiresAt: expiresAt,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("linkstore: mint: %w", err)
	}
	return added.ID, expiresAt, nil
}

// Resolve returns the content behind a previously minted linkID, or
// ErrNotFound if it never existed or has expired.
func (s *Store) Resolve(ctx context.Context, linkID string) (content []byte, contentType, filename string, err error) {
	link, err := s.repo.Get(ctx, linkID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, "", "", ErrNotFound
		}
		return nil, "", "", fmt.Errorf("linkstore: resolve: %w", err)
	}
	if !s.now().Before(link.ExpiresAt) {
		return nil, "", "", ErrNotFound
	}
	// FileID resolution isn't implemented yet (docs/adr/007 §2) — every
	// link Mint creates today carries Content, so this only guards against
	// a row this package never wrote itself.
	if link.Content == nil {
		return nil, "", "", fmt.Errorf("linkstore: resolve: file_id-backed links are not implemented")
	}
	return link.Content, link.ContentType, link.Filename, nil
}

// DeleteExpired removes every link that has already expired, returning how
// many rows were removed — see docs/adr/007 §3's periodic cleanup, wired up
// on a ticker in cmd/miranda-medical-card/main.go.
func (s *Store) DeleteExpired(ctx context.Context) (int64, error) {
	n, err := s.repo.DeleteExpired(ctx, s.now())
	if err != nil {
		return 0, fmt.Errorf("linkstore: delete expired: %w", err)
	}
	return n, nil
}
