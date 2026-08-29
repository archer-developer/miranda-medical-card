package backup

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedSQLite creates a tiny SQLite database at path with one row, so tests
// can assert the backup file is a real, independently readable snapshot
// rather than just checking it exists.
func seedSQLite(t *testing.T, path string, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec("CREATE TABLE t (v TEXT)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO t VALUES (?)", value)
	require.NoError(t, err)
}

func readSQLiteValue(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var v string
	require.NoError(t, db.QueryRow("SELECT v FROM t").Scan(&v))
	return v
}

func TestRun_SnapshotsDatabaseAndCopiesFiles(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")

	dbPath := filepath.Join(dataDir, "medical-card.db")
	seedSQLite(t, dbPath, "medical-card-data")

	filesDir := filepath.Join(dataDir, "files")
	require.NoError(t, os.MkdirAll(filepath.Join(filesDir, "alex"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(filesDir, "alex", "abc123.pdf"), []byte("doc-bytes"), 0o600))
	// A leftover .tmp from an in-flight upload must never be copied.
	require.NoError(t, os.WriteFile(filepath.Join(filesDir, "alex", "def456.pdf.tmp"), []byte("partial"), 0o600))

	backupCfg := config.BackupConfig{Dir: backupDir, RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg, dbPath, filesDir, "", "", testLogger()))

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one timestamped run directory")
	runDir := filepath.Join(backupDir, entries[0].Name())

	require.Equal(t, "medical-card-data", readSQLiteValue(t, filepath.Join(runDir, "medical-card.db")))

	got, err := os.ReadFile(filepath.Join(runDir, "files", "alex", "abc123.pdf"))
	require.NoError(t, err)
	require.Equal(t, "doc-bytes", string(got))

	require.NoFileExists(t, filepath.Join(runDir, "files", "alex", "def456.pdf.tmp"))
}

func TestRun_NoSourcesExist_NoOp(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")

	backupCfg := config.BackupConfig{Dir: backupDir, RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg,
		filepath.Join(dataDir, "medical-card.db"), filepath.Join(dataDir, "files"), "", "", testLogger()))

	_, err := os.Stat(backupDir)
	require.True(t, os.IsNotExist(err), "backup dir should never be created when nothing exists to back up")
}

func TestRun_PruneKeepsOnlyMostRecentRetentionCount(t *testing.T) {
	backupDir := t.TempDir()

	// Fabricate five already-completed run directories with distinct,
	// chronologically-ordered timestamp names, rather than calling Run
	// five times (which would need real sleeps between ticks to get
	// distinct second-resolution timestamps).
	var names []string
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		name := base.Add(time.Duration(i) * time.Hour).Format(timestampFormat)
		require.NoError(t, os.MkdirAll(filepath.Join(backupDir, name), 0o755))
		names = append(names, name)
	}

	require.NoError(t, prune(backupDir, 2))

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	var kept []string
	for _, e := range entries {
		kept = append(kept, e.Name())
	}
	require.ElementsMatch(t, names[3:], kept, "only the two newest run directories survive")
}

func TestRun_PrunesOldRunsAfterEachSuccessfulBackup(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")
	dbPath := filepath.Join(dataDir, "medical-card.db")
	seedSQLite(t, dbPath, "v")

	backupCfg := config.BackupConfig{Dir: backupDir, RetentionCount: 2}

	for i := 0; i < 3; i++ {
		require.NoError(t, Run(context.Background(), backupCfg, dbPath, "", "", "", testLogger()))
		time.Sleep(time.Second) // force a distinct second-resolution run directory name
	}

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "retention should have pruned down to 2 run directories")
}

func TestRun_CopiesConfigDirAndEnvFile(t *testing.T) {
	backupDir := filepath.Join(t.TempDir(), "backups")

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "llm.yaml"), []byte("llm: {}\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "sub", "nested.yaml"), []byte("a: 1\n"), 0o644))

	envDir := t.TempDir()
	envPath := filepath.Join(envDir, ".env")
	require.NoError(t, os.WriteFile(envPath, []byte("MEDICAL_CARD_MCP_TOKEN=secret\n"), 0o600))

	backupCfg := config.BackupConfig{Dir: backupDir, RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg, "", "", configDir, envPath, testLogger()))

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	runDir := filepath.Join(backupDir, entries[0].Name())

	got, err := os.ReadFile(filepath.Join(runDir, "config", "llm.yaml"))
	require.NoError(t, err)
	require.Equal(t, "llm: {}\n", string(got))

	gotNested, err := os.ReadFile(filepath.Join(runDir, "config", "sub", "nested.yaml"))
	require.NoError(t, err)
	require.Equal(t, "a: 1\n", string(gotNested))

	gotEnvPath := filepath.Join(runDir, ".env")
	gotEnv, err := os.ReadFile(gotEnvPath)
	require.NoError(t, err)
	require.Equal(t, "MEDICAL_CARD_MCP_TOKEN=secret\n", string(gotEnv))

	fi, err := os.Stat(gotEnvPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "restrictive .env permissions must survive the copy")
}

func TestRun_MissingInputsSkippedNotErrored(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := filepath.Join(t.TempDir(), "backups")
	dbPath := filepath.Join(dataDir, "medical-card.db")
	seedSQLite(t, dbPath, "v")

	backupCfg := config.BackupConfig{Dir: backupDir, RetentionCount: 7}
	require.NoError(t, Run(context.Background(), backupCfg, dbPath,
		filepath.Join(dataDir, "no-such-files-dir"),
		filepath.Join(dataDir, "no-such-config-dir"),
		filepath.Join(dataDir, "no-such-.env"), testLogger()))

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	runDir := filepath.Join(backupDir, entries[0].Name())

	require.NoDirExists(t, filepath.Join(runDir, "files"))
	require.NoDirExists(t, filepath.Join(runDir, "config"))
	require.NoFileExists(t, filepath.Join(runDir, ".env"))
}

func TestRun_RetentionZeroKeepsEverything(t *testing.T) {
	backupDir := t.TempDir()
	for i := 0; i < 3; i++ {
		name := time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC).Format(timestampFormat)
		require.NoError(t, os.MkdirAll(filepath.Join(backupDir, name), 0o755))
	}

	require.NoError(t, prune(backupDir, 0))

	entries, err := os.ReadDir(backupDir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
}
