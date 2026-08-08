// Package filestore is the "Local Files" half of
// docs/architecture/06-storage.md §5 ("SQLite + Local Files") — storage.File
// keeps only a StoragePath referencing what this package writes; binary
// content itself never goes into SQLite (see that doc §7, "Не рекомендуется
// хранить бинарные данные в BLOB-полях").
package filestore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Store writes uploaded file content under baseDir, one subdirectory per
// user so a directory listing never mixes different users' files, and
// filenames content-addressed by SHA256 — the same hash storage.File
// already records for dedup, so the on-disk path is trivially derivable
// from data alone and two identical uploads for the same user always land
// on the same path (an idempotent write, not an error, if content already
// exists — see Save).
type Store struct {
	baseDir string
}

// New returns a Store rooted at baseDir, creating it if necessary.
func New(baseDir string) (*Store, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: create base dir: %w", err)
	}
	return &Store{baseDir: baseDir}, nil
}

// Save writes data under userID's subdirectory, content-addressed by its
// own SHA256, and returns the path (relative to baseDir — see
// storage.File.StoragePath's doc comment: this value is what gets
// persisted there) and hex-encoded hash. Writing the same (userID, data)
// twice is a safe no-op, not an error — matches how storage.FileRepository
// treats (userID, sha256) as a dedup key, not a hard uniqueness
// constraint: whichever caller uploads first "wins" the path, and every
// later identical upload just confirms the same bytes are already there.
func (s *Store) Save(userID string, data []byte) (relativePath, sha256Hex string, err error) {
	sum := sha256.Sum256(data)
	sha256Hex = hex.EncodeToString(sum[:])
	relativePath = filepath.Join(userID, sha256Hex)

	fullPath := filepath.Join(s.baseDir, relativePath)
	if _, statErr := os.Stat(fullPath); statErr == nil {
		return relativePath, sha256Hex, nil
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		return "", "", fmt.Errorf("filestore: create user dir: %w", err)
	}
	// Write to a temp file then rename, so a crash mid-write can never
	// leave a partially-written file at fullPath — File is documented as
	// immutable once it exists (docs/domain/03-files-and-documents.md §2),
	// which only holds if a reader never observes a half-written state.
	tmp := fullPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", "", fmt.Errorf("filestore: write: %w", err)
	}
	if err := os.Rename(tmp, fullPath); err != nil {
		_ = os.Remove(tmp)
		return "", "", fmt.Errorf("filestore: finalize write: %w", err)
	}
	return relativePath, sha256Hex, nil
}

// Read returns the content previously written at relativePath (as returned
// by Save).
func (s *Store) Read(relativePath string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.baseDir, relativePath))
	if err != nil {
		return nil, fmt.Errorf("filestore: read: %w", err)
	}
	return data, nil
}

// Remove deletes the content at relativePath. Removing an already-absent
// path is not an error — mirrors storage repositories' Remove semantics
// (deleting what's already gone is treated as success, see e.g.
// DocumentRepository.Remove).
func (s *Store) Remove(relativePath string) error {
	if err := os.Remove(filepath.Join(s.baseDir, relativePath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("filestore: remove: %w", err)
	}
	return nil
}
