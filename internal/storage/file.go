package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// File mirrors docs/domain/03-files-and-documents.md §2 — a binary object
// with no medical semantics. StoragePath is deliberately not exposed
// through MCP (see that doc's field table); this package doesn't enforce
// that itself, it's a caller-side concern for whatever serializes File to
// an MCP response.
type File struct {
	ID          string
	UserID      string
	Filename    string
	ContentType string
	Size        int64
	SHA256      string
	StoragePath string
	UploadedAt  time.Time
}

// ErrNotFound is returned by a repository's Get when no row matches (or
// matches a different userID — see FileRepository.Get's doc comment for why
// the two cases are indistinguishable by design).
var ErrNotFound = errors.New("storage: not found")

// FileRepository mirrors docs/domain/03-files-and-documents.md §2
// "Repository". Deduplication itself (deciding whether to reuse an existing
// File instead of calling Add) is the caller's job, done by calling
// FindBySHA256 first — see that doc's invariant "(userId, sha256) не
// строгий уникальный ключ": Add here is a plain insert with no dedup logic
// of its own, and the schema has no UNIQUE constraint on (user_id, sha256)
// for exactly that reason.
type FileRepository interface {
	// Add inserts f. If f.ID is empty, a new id is generated (prefixed
	// file_) and returned on the result — callers building a File from
	// scratch don't need to invent an id themselves, mirroring
	// miranda-diary's Store.Add.
	Add(ctx context.Context, f File) (File, error)
	// Get returns the File with the given id, if it belongs to userID.
	// Returns ErrNotFound both when no such id exists and when it exists
	// but belongs to a different user — the two cases are intentionally
	// indistinguishable to the caller, so this can't be used to enumerate
	// other users' file ids.
	Get(ctx context.Context, id, userID string) (File, error)
	// FindBySHA256 looks up an existing File for (userID, sha256) — the
	// dedup check callers run before Add. Returns ErrNotFound if none
	// exists yet.
	FindBySHA256(ctx context.Context, userID, sha256 string) (File, error)
	Remove(ctx context.Context, id, userID string) error
}

type sqliteFileRepository struct {
	db *sql.DB
}

// NewFileRepository builds a FileRepository backed by s.
func NewFileRepository(s *Store) FileRepository {
	return &sqliteFileRepository{db: s.db}
}

const fileSelectColumns = `SELECT id, user_id, filename, content_type, size, sha256, storage_path, uploaded_at`

func (r *sqliteFileRepository) Add(ctx context.Context, f File) (File, error) {
	if f.ID == "" {
		f.ID = "file_" + uuid.New().String()
	}
	if f.UploadedAt.IsZero() {
		f.UploadedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO files (id, user_id, filename, content_type, size, sha256, storage_path, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.UserID, f.Filename, f.ContentType, f.Size, f.SHA256, f.StoragePath, f.UploadedAt.Unix(),
	)
	if err != nil {
		return File{}, fmt.Errorf("storage: add file: %w", err)
	}
	return f, nil
}

func (r *sqliteFileRepository) Get(ctx context.Context, id, userID string) (File, error) {
	row := r.db.QueryRowContext(ctx, fileSelectColumns+` FROM files WHERE id = ? AND user_id = ?`, id, userID)
	return scanFile(row)
}

func (r *sqliteFileRepository) FindBySHA256(ctx context.Context, userID, sha256 string) (File, error) {
	row := r.db.QueryRowContext(ctx, fileSelectColumns+` FROM files WHERE user_id = ? AND sha256 = ? LIMIT 1`, userID, sha256)
	return scanFile(row)
}

func (r *sqliteFileRepository) Remove(ctx context.Context, id, userID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM files WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("storage: remove file: %w", err)
	}
	return nil
}

func scanFile(row *sql.Row) (File, error) {
	var f File
	var uploadedAt int64
	err := row.Scan(&f.ID, &f.UserID, &f.Filename, &f.ContentType, &f.Size, &f.SHA256, &f.StoragePath, &uploadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("storage: scan file: %w", err)
	}
	f.UploadedAt = time.Unix(uploadedAt, 0).UTC()
	return f, nil
}
