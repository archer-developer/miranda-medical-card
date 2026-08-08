package storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

func TestFileRepository_AddAssignsIDWhenEmpty(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFileRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.File{
		UserID: "user1", Filename: "lab.pdf", ContentType: "application/pdf",
		Size: 1234, SHA256: "abc123", StoragePath: "/data/user1/lab.pdf",
	})
	require.NoError(t, err)
	require.NotEmpty(t, added.ID)
	require.False(t, added.UploadedAt.IsZero())

	got, err := repo.Get(ctx, added.ID, "user1")
	require.NoError(t, err)
	require.Equal(t, "lab.pdf", got.Filename)
	require.Equal(t, "abc123", got.SHA256)
}

func TestFileRepository_Get_WrongUserReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFileRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.File{UserID: "user1", Filename: "a.pdf", SHA256: "x"})
	require.NoError(t, err)

	_, err = repo.Get(ctx, added.ID, "user2")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestFileRepository_FindBySHA256(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFileRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.File{UserID: "user1", Filename: "a.pdf", SHA256: "deadbeef"})
	require.NoError(t, err)

	found, err := repo.FindBySHA256(ctx, "user1", "deadbeef")
	require.NoError(t, err)
	require.Equal(t, added.ID, found.ID)

	_, err = repo.FindBySHA256(ctx, "user1", "not-uploaded-yet")
	require.ErrorIs(t, err, storage.ErrNotFound)

	// Same hash, different user — deliberately not deduplicated across
	// users, see File's doc comment.
	_, err = repo.FindBySHA256(ctx, "user2", "deadbeef")
	require.ErrorIs(t, err, storage.ErrNotFound)
}

func TestFileRepository_Remove(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewFileRepository(newTestStore(t))

	added, err := repo.Add(ctx, storage.File{UserID: "user1", Filename: "a.pdf", SHA256: "x"})
	require.NoError(t, err)

	require.NoError(t, repo.Remove(ctx, added.ID, "user1"))

	_, err = repo.Get(ctx, added.ID, "user1")
	require.True(t, errors.Is(err, storage.ErrNotFound))
}
