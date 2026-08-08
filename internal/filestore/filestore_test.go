package filestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/filestore"
)

func TestSaveAndRead(t *testing.T) {
	s, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	data := []byte("pdf content goes here")
	path, sha, err := s.Save("user1", data)
	require.NoError(t, err)
	require.NotEmpty(t, path)
	require.Len(t, sha, 64)

	got, err := s.Read(path)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

func TestSave_SameContentTwiceIsIdempotent(t *testing.T) {
	s, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	data := []byte("identical bytes")
	path1, sha1, err := s.Save("user1", data)
	require.NoError(t, err)
	path2, sha2, err := s.Save("user1", data)
	require.NoError(t, err)

	require.Equal(t, path1, path2)
	require.Equal(t, sha1, sha2)
}

func TestSave_SameContentDifferentUsersIsolated(t *testing.T) {
	s, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	data := []byte("shared boilerplate form")
	path1, _, err := s.Save("user1", data)
	require.NoError(t, err)
	path2, _, err := s.Save("user2", data)
	require.NoError(t, err)

	require.NotEqual(t, path1, path2, "must not collapse two different users' uploads onto the same path")
}

func TestRemove_AbsentPathIsNotAnError(t *testing.T) {
	s, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, s.Remove("user1/never-existed"))
}

func TestRemove(t *testing.T) {
	s, err := filestore.New(t.TempDir())
	require.NoError(t, err)

	path, _, err := s.Save("user1", []byte("data"))
	require.NoError(t, err)
	require.NoError(t, s.Remove(path))

	_, err = s.Read(path)
	require.Error(t, err)
}
