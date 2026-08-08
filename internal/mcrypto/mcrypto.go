// Package mcrypto implements the AES-256-GCM encryption-at-rest pattern
// documented in docs/architecture/06-storage.md §14 — the same pattern
// already proven in miranda-diary's internal/diary/crypto.go, generalized
// here so more than one storage repository can reuse it (medical-card has
// several sensitive text stores — MedicalDocument.RecognizedText/Summary,
// SelfReportedEvent.RawText/Description — where diary only had one).
//
// Per-user key-check sentinel: verifyOrInitKeyCheck lets a repository
// validate a submitted key against a stored per-user sentinel before doing
// any real read/write, so a wrong key fails fast and explicitly rather than
// silently corrupting or skipping records. First call for a user
// initializes the sentinel; every later call validates against it.
//
// Not yet wired into any repository's read/write path — see this package's
// use from a repository (e.g. a future DocumentRepository.Add taking an
// encryptionKey []byte) as the next integration step. Encrypting
// MedicalDocument.RecognizedText/Summary specifically also means those
// repositories must skip FTS indexing and Embedding generation for
// encrypted users (see internal/pipeline's generateDocumentEmbedding and
// FTS indexing calls) — indexing plaintext derived from encrypted content
// into an unencrypted FTS5/Embeddings store would defeat the purpose of
// encrypting the source column in the first place. That interaction needs
// to be handled at the same time encryption is wired in, not as an
// afterthought.
package mcrypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
)

// keyCheckSentinel is the plaintext encrypted and stored once per user in
// the key-check table. Its only purpose is to let a caller validate a key
// before touching any real record.
const keyCheckSentinel = "medical-card-key-check-v1"

// GCMNonceSize is the standard AES-GCM nonce length in bytes. Callers
// storing a nonce alongside ciphertext must check its length against this
// before calling Decrypt — cipher.AEAD.Open panics (rather than erroring)
// on a wrong-length nonce.
const GCMNonceSize = 12

// Encrypt encrypts plaintext using AES-256-GCM with a random 12-byte nonce.
// Returns the ciphertext (already including the GCM authentication tag)
// and the nonce separately, since both need to be stored.
func Encrypt(key []byte, plaintext string) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("mcrypto: create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("mcrypto: create GCM: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("mcrypto: generate nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return ciphertext, nonce, nil
}

// Decrypt decrypts a ciphertext produced by Encrypt. Returns an error
// (wrapping the underlying cipher error) when the key is wrong or the data
// has been tampered with.
func Decrypt(key, nonce, ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("mcrypto: create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("mcrypto: create GCM: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("mcrypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}

// VerifyOrInitKeyCheck validates key against a per-user sentinel stored in
// tableName (a table with columns (user_id TEXT PRIMARY KEY, check_nonce
// BLOB, check_cipher BLOB) — every repository that supports encryption
// shares one such table rather than each repository keeping its own, since
// the sentinel isn't specific to any one repository's data), creating the
// row on first use.
//
// The SELECT and first-use INSERT run inside one transaction so they hold
// db's connection for the whole check-then-act sequence — see
// miranda-diary/internal/diary/crypto.go's verifyOrInitKeyCheck for the
// full correctness argument (depends on the caller's *sql.DB being capped
// to one connection, as storage.Store already is).
func VerifyOrInitKeyCheck(ctx context.Context, db *sql.DB, tableName, userID string, key []byte) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcrypto: begin key check: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	var checkNonce, checkCipher []byte
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT check_nonce, check_cipher FROM %s WHERE user_id = ?`, tableName),
		userID,
	).Scan(&checkNonce, &checkCipher)

	if errors.Is(err, sql.ErrNoRows) {
		ciphertext, nonce, err := Encrypt(key, keyCheckSentinel)
		if err != nil {
			return fmt.Errorf("mcrypto: init key check: %w", err)
		}
		_, err = tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (user_id, check_nonce, check_cipher) VALUES (?, ?, ?)`, tableName),
			userID, nonce, ciphertext,
		)
		if err != nil {
			return fmt.Errorf("mcrypto: store key check: %w", err)
		}
		return tx.Commit()
	}
	if err != nil {
		return fmt.Errorf("mcrypto: load key check: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mcrypto: commit key check: %w", err)
	}

	if len(checkNonce) != GCMNonceSize {
		return fmt.Errorf("mcrypto: wrong encryption key for user %q", userID)
	}
	plaintext, err := Decrypt(key, checkNonce, checkCipher)
	if err != nil || plaintext != keyCheckSentinel {
		return fmt.Errorf("mcrypto: wrong encryption key for user %q", userID)
	}
	return nil
}
