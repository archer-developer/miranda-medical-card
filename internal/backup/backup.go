// Package backup periodically snapshots everything under data/ that holds
// irreplaceable state — the SQLite database (config.DatabaseConfig.Path)
// and the filestore (config.FilesConfig.Dir) — plus config.yaml's *.yaml
// files and .env, into timestamped subdirectories on disk. The SQLite
// database is copied via SQLite's own VACUUM INTO rather than a raw file
// copy — see Run for why. It owns no ticker or scheduling logic of its
// own: cmd/miranda-medical-card drives it on a ticker the same way
// internal/linkstore's expired-link sweep is driven from run().
//
// Mirrors miranda's own internal/backup field-for-field, adapted to this
// service having exactly one SQLite store and one filestore directory
// instead of miranda's several independent SQLite stores.
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/archer-developer/miranda-medical-card/internal/config"
)

// configSubdir and filesSubdir name the directories inside a run's
// timestamped backup directory that configDir's *.yaml files and the
// filestore's contents (respectively) are copied into.
const (
	configSubdir = "config"
	filesSubdir  = "files"
)

// vacuumBusyTimeoutMS bounds how long a backup connection waits for the
// application's own connection to release its lock, mirroring the
// busy_timeout every other store in this codebase opens with (see
// internal/storage.Open) rather than failing immediately on "database is
// locked".
const vacuumBusyTimeoutMS = 5000

// timestampFormat names each run's subdirectory so lexicographic sort order
// equals chronological order — prune relies on this to find the oldest runs
// without parsing each name back into a time.Time.
const timestampFormat = "20060102-150405"

// Run performs one full backup cycle into a new
// backupCfg.Dir/<UTC timestamp>/ subdirectory:
//
//   - VACUUM INTO a fresh copy of dbPath (config.DatabaseConfig.Path), if it
//     currently exists, as medical-card.db (the constant filename, not
//     dbPath's own basename, so a restore doesn't need to know what the
//     source deployment happened to name it).
//   - Recursively copy filesDir (config.FilesConfig.Dir — the filestore's
//     content-addressed binary uploads, internal/filestore) into a files/
//     subdirectory, if it exists. In-flight uploads are written to a
//     "*.tmp" sibling before an atomic rename (see filestore.Store.Save) —
//     copyDir skips any "*.tmp" entry it finds so a backup racing a
//     concurrent upload never captures a half-written file.
//   - Copy configDir's *.yaml files (config.yaml's own source files —
//     without them a restored backup can't even start) into a config/
//     subdirectory, if configDir exists.
//   - Copy envPath (the .env file holding API keys/the auth token) alongside
//     them, if it exists — deliberately included despite being sensitive,
//     since restoring a working deployment from a backup alone (without
//     also having preserved .env some other way) would otherwise be
//     impossible.
//
// Old run subdirectories beyond backupCfg.RetentionCount are pruned after a
// successful run.
//
// VACUUM INTO (rather than copying the SQLite file bytes directly) is what
// makes backing up the database safe to run while the application's own
// connection is open and possibly mid-write: SQLite computes a
// transactionally consistent snapshot as it streams pages into the
// destination file, the same guarantee a raw `cp` of a live database file
// does not have — a copy caught mid-write can capture a torn page or, since
// this database runs in WAL mode, miss data still sitting in the -wal file
// (see internal/storage.Open). filestore's files, config.yaml's *.yaml
// files, and .env don't need this treatment: filestore files are immutable
// and written atomically once (see filestore.Store.Save's doc comment),
// and config/.env are edited by hand, not written to by the running
// process. data/tls's self-signed certificate/key are deliberately *not*
// backed up — cmd/miranda-medical-card's run() regenerates them from
// scratch via tlscert.EnsureSelfSigned on any startup that finds them
// missing, so they carry no state worth preserving.
//
// Any input that doesn't exist (dbPath, filesDir, configDir, or envPath) is
// skipped rather than treated as an error. If nothing exists at all, Run
// returns nil without creating an (empty) timestamped subdirectory.
func Run(ctx context.Context, backupCfg config.BackupConfig, dbPath, filesDir, configDir, envPath string, logger *slog.Logger) error {
	haveDB := isFile(dbPath)
	haveFiles := isDir(filesDir)
	haveConfigDir := isDir(configDir)
	haveEnv := isFile(envPath)

	if !haveDB && !haveFiles && !haveConfigDir && !haveEnv {
		return nil
	}

	runDir := filepath.Join(backupCfg.Dir, time.Now().UTC().Format(timestampFormat))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("backup: create %s: %w", runDir, err)
	}

	if haveDB {
		dst := filepath.Join(runDir, "medical-card.db")
		if err := snapshot(ctx, dbPath, dst); err != nil {
			// A partial backup set is worse than none: retention counts
			// runDir as one of the kept snapshots, and a restore built from
			// an incomplete set silently loses whichever piece failed. Tear
			// it down rather than leave it to be counted as a good backup.
			_ = os.RemoveAll(runDir)
			return fmt.Errorf("backup: snapshot database: %w", err)
		}
	}
	if haveFiles {
		if err := copyDir(filesDir, filepath.Join(runDir, filesSubdir)); err != nil {
			_ = os.RemoveAll(runDir)
			return fmt.Errorf("backup: copy files dir %s: %w", filesDir, err)
		}
	}
	if haveConfigDir {
		if err := copyDir(configDir, filepath.Join(runDir, configSubdir)); err != nil {
			_ = os.RemoveAll(runDir)
			return fmt.Errorf("backup: copy config dir %s: %w", configDir, err)
		}
	}
	if haveEnv {
		if err := copyFile(envPath, filepath.Join(runDir, filepath.Base(envPath))); err != nil {
			_ = os.RemoveAll(runDir)
			return fmt.Errorf("backup: copy %s: %w", envPath, err)
		}
	}
	logger.Info("backup complete", "dir", runDir, "database", haveDB, "files", haveFiles, "config", haveConfigDir, "env", haveEnv)

	if err := prune(backupCfg.Dir, backupCfg.RetentionCount); err != nil {
		return fmt.Errorf("backup: prune %s: %w", backupCfg.Dir, err)
	}
	return nil
}

func isDir(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func isFile(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// snapshot opens its own short-lived connection to src (independent of any
// connection the running application already holds open on the same file)
// and runs VACUUM INTO dst on it.
func snapshot(ctx context.Context, src, dst string) error {
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)", src, vacuumBusyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("vacuum %s into %s: %w", src, dst, err)
	}
	return nil
}

// copyFile copies src to dst, preserving src's file mode — notably so a
// restrictively-permissioned .env (holding API keys/the auth token) or a
// filestore entry (written 0o600, see filestore.Store.Save) doesn't end up
// world-readable at its backup destination.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return out.Close()
}

// copyDir recursively copies every file under srcDir into dstDir, preserving
// the relative directory structure and each file's mode (see copyFile). Any
// entry whose name ends in ".tmp" is skipped — filestore.Store.Save writes
// new content to such a sibling before an atomic rename, so a backup racing
// a concurrent upload could otherwise observe and copy a half-written file.
func copyDir(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

// prune deletes the oldest timestamped run subdirectories of dir beyond the
// most recent keep. keep == 0 means keep all of them (same convention as
// config.LoggingConfig.MaxBackups).
func prune(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	if len(runs) <= keep {
		return nil
	}

	sort.Strings(runs) // timestampFormat sorts lexicographically == chronologically
	for _, name := range runs[:len(runs)-keep] {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return nil
}
