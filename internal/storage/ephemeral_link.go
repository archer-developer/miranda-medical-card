package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// EphemeralLink mirrors docs/adr/007-short-lived-file-links-for-profile-export.md
// §1 — a short-lived, random-ID link onto either inline Content (the only
// path actually used today) or, in the future, an existing File via
// FileID. Exactly one of FileID/Content is populated; see
// EphemeralLinkRepository.Get's doc comment for why the FileID path isn't
// resolved yet.
type EphemeralLink struct {
	ID          string
	FileID      string // empty when this link carries inline Content instead.
	Content     []byte // nil when this link resolves through FileID instead.
	ContentType string
	Filename    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// EphemeralLinkRepository is the storage side of internal/linkstore — it
// only persists/reads rows and deletes expired ones; TTL semantics
// (what "expired" means right now, minting new random IDs) live in
// internal/linkstore, same split as storage.FileRepository vs
// internal/filestore for on-disk bytes.
type EphemeralLinkRepository interface {
	// Add inserts l. If l.ID is empty, a new id is generated and returned
	// on the result.
	Add(ctx context.Context, l EphemeralLink) (EphemeralLink, error)
	// Get returns the EphemeralLink with the given id, regardless of
	// whether it has expired — expiry is internal/linkstore's concern, not
	// this repository's. Returns ErrNotFound if no such id exists.
	//
	// A row whose FileID is set (Content nil) can be returned here, but
	// nothing resolves it into bytes yet — internal/linkstore.Store.Resolve
	// errors on that shape rather than pretending to support it (see
	// docs/adr/007 §2, "не реализован сейчас: разрешение через file_id").
	Get(ctx context.Context, id string) (EphemeralLink, error)
	// DeleteExpired removes every row whose expires_at is before cutoff,
	// returning how many rows were removed — the periodic cleanup
	// docs/adr/007 §3 describes.
	DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error)
}

type sqliteEphemeralLinkRepository struct {
	db *sql.DB
}

// NewEphemeralLinkRepository builds an EphemeralLinkRepository backed by s.
func NewEphemeralLinkRepository(s *Store) EphemeralLinkRepository {
	return &sqliteEphemeralLinkRepository{db: s.db}
}

func (r *sqliteEphemeralLinkRepository) Add(ctx context.Context, l EphemeralLink) (EphemeralLink, error) {
	if l.ID == "" {
		l.ID = uuid.New().String()
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now().UTC()
	}

	var fileID any
	if l.FileID != "" {
		fileID = l.FileID
	}
	var content any
	if l.Content != nil {
		content = l.Content
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO ephemeral_links (id, file_id, content, content_type, filename, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		l.ID, fileID, content, l.ContentType, l.Filename, l.CreatedAt.Unix(), l.ExpiresAt.Unix(),
	)
	if err != nil {
		return EphemeralLink{}, fmt.Errorf("storage: add ephemeral link: %w", err)
	}
	return l, nil
}

func (r *sqliteEphemeralLinkRepository) Get(ctx context.Context, id string) (EphemeralLink, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, file_id, content, content_type, filename, created_at, expires_at
		FROM ephemeral_links WHERE id = ?`, id)

	var (
		l                    EphemeralLink
		fileID               sql.NullString
		createdAt, expiresAt int64
	)
	err := row.Scan(&l.ID, &fileID, &l.Content, &l.ContentType, &l.Filename, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EphemeralLink{}, ErrNotFound
	}
	if err != nil {
		return EphemeralLink{}, fmt.Errorf("storage: scan ephemeral link: %w", err)
	}
	l.FileID = fileID.String
	l.CreatedAt = time.Unix(createdAt, 0).UTC()
	l.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return l, nil
}

func (r *sqliteEphemeralLinkRepository) DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, `DELETE FROM ephemeral_links WHERE expires_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, fmt.Errorf("storage: delete expired ephemeral links: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("storage: delete expired ephemeral links: rows affected: %w", err)
	}
	return n, nil
}
