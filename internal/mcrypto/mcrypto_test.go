package mcrypto_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/archer-developer/miranda-medical-card/internal/mcrypto"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func TestEncryptDecrypt_RoundTrips(t *testing.T) {
	key := randomKey(t)
	ciphertext, nonce, err := mcrypto.Encrypt(key, "Пациент жалуется на бессонницу")
	require.NoError(t, err)
	require.Len(t, nonce, mcrypto.GCMNonceSize)

	plaintext, err := mcrypto.Decrypt(key, nonce, ciphertext)
	require.NoError(t, err)
	require.Equal(t, "Пациент жалуется на бессонницу", plaintext)
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	ciphertext, nonce, err := mcrypto.Encrypt(randomKey(t), "secret")
	require.NoError(t, err)

	_, err = mcrypto.Decrypt(randomKey(t), nonce, ciphertext)
	require.Error(t, err)
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE user_key_checks (user_id TEXT PRIMARY KEY, check_nonce BLOB NOT NULL, check_cipher BLOB NOT NULL)`)
	require.NoError(t, err)
	return db
}

func TestVerifyOrInitKeyCheck_FirstCallInitializesSentinel(t *testing.T) {
	db := newTestDB(t)
	key := randomKey(t)

	require.NoError(t, mcrypto.VerifyOrInitKeyCheck(context.Background(), db, "user_key_checks", "user1", key))

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM user_key_checks WHERE user_id = ?`, "user1").Scan(&count))
	require.Equal(t, 1, count)
}

func TestVerifyOrInitKeyCheck_SameKeySucceedsOnSubsequentCalls(t *testing.T) {
	db := newTestDB(t)
	key := randomKey(t)
	ctx := context.Background()

	require.NoError(t, mcrypto.VerifyOrInitKeyCheck(ctx, db, "user_key_checks", "user1", key))
	require.NoError(t, mcrypto.VerifyOrInitKeyCheck(ctx, db, "user_key_checks", "user1", key))
}

func TestVerifyOrInitKeyCheck_WrongKeyOnSubsequentCallFails(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	require.NoError(t, mcrypto.VerifyOrInitKeyCheck(ctx, db, "user_key_checks", "user1", randomKey(t)))

	err := mcrypto.VerifyOrInitKeyCheck(ctx, db, "user_key_checks", "user1", randomKey(t))
	require.Error(t, err)
}

func TestVerifyOrInitKeyCheck_ScopedPerUser(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	keyA, keyB := randomKey(t), randomKey(t)
	require.NoError(t, mcrypto.VerifyOrInitKeyCheck(ctx, db, "user_key_checks", "user1", keyA))
	// A different user with a different key must not collide with user1's
	// sentinel — this call must succeed as user2's *first* use, not be
	// checked against user1's key.
	require.NoError(t, mcrypto.VerifyOrInitKeyCheck(ctx, db, "user_key_checks", "user2", keyB))
}
