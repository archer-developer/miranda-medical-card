package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestLoad_NoFilesFailsBecauseUsersHasNoDefault(t *testing.T) {
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "users")
}

func TestLoad_MinimalValidConfig(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: alex
`)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "alex", cfg.Users[0].ID)
	require.Equal(t, ":8791", cfg.HTTPAddr, "unset fields keep Default()'s value")
}

func TestLoad_DuplicateUserIDRejected(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: alex
  - id: alex
`)
	_, err := config.Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

func TestLoad_EncryptionAndSharedWithMutuallyExclusive(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: alex
  - id: kid
    shared_with: [alex]
    encryption: true
`)
	_, err := config.Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")
}

func TestLoad_SharedWithUnknownUserRejected(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: alex
    shared_with: [nobody]
`)
	_, err := config.Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown user")
}

func TestLoad_SharedWithValidUserAccepted(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: alex
  - id: kid
    shared_with: [alex]
`)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, []string{"alex"}, cfg.Users[1].SharedWith)
}

func TestLoad_InvalidBirthDateRejected(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: alex
    birth_date: "not-a-date"
`)
	_, err := config.Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "birth_date")
}

func TestLoad_InvalidSexRejected(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: alex
    sex: robot
`)
	_, err := config.Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sex")
}

func TestLoad_TrimsWhitespaceFromUserID(t *testing.T) {
	path := writeYAML(t, `
users:
  - id: "  alex  "
`)
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "alex", cfg.Users[0].ID)
}

func TestLoad_LaterFileOverridesEarlierFieldByField(t *testing.T) {
	first := writeYAML(t, `
http_addr: ":9999"
users:
  - id: alex
`)
	second := writeYAML(t, `
logging:
  level: debug
`)
	cfg, err := config.Load(first, second)
	require.NoError(t, err)
	require.Equal(t, ":9999", cfg.HTTPAddr, "second file doesn't mention http_addr, so first file's value should survive")
	require.Equal(t, "debug", cfg.Logging.Level)
}

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err, "still fails, but because users is empty, not because the file is missing")
	require.Contains(t, err.Error(), "users")
}
