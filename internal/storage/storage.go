// Package storage is the SQLite-backed persistence layer covering
// docs/architecture/06-storage.md §4's File/Document/Extraction/Domain
// layers: File and MedicalDocument (see file.go, document.go —
// docs/domain/03-files-and-documents.md §2-3), the raw, versioned
// Extraction record (extraction_record.go — same doc §4, not to be
// confused with internal/extraction's LLM-calling code), and the Domain
// Entities internal/normalization produces (Diagnosis, Medication,
// LabResult, InstrumentalFinding, Procedure, Allergy, VitalSign).
//
// Follows the same conventions as miranda-diary's internal/diary/store.go:
// modernc.org/sqlite (pure Go, no cgo), a single shared *sql.DB capped to
// one connection (SQLite has one writer regardless; queuing in
// database/sql avoids SQLITE_BUSY instead of surfacing it), WAL journal
// mode, and an append-only schema + migrations list that's safe to replay
// against an existing database.
//
// Each entity gets its own narrow repository interface (see e.g.
// diagnosis.go) rather than one large Store with every method, per this
// family's "narrow interfaces over mocking frameworks" convention (see
// miranda-service-skeleton/CLAUDE.md) — every repository is a thin
// interface a test can fake by hand, and Store is only the shared
// connection + schema bootstrap.
package storage

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// schema creates every Domain Layer table if it doesn't already exist.
// Column names are snake_case; Go-side field names are documented on each
// entity's repository file, not repeated here.
//
// Nullable date columns (e.g. diagnosed_at) are stored as INTEGER unix
// seconds with no NOT NULL constraint — nil in Go maps to SQL NULL, exactly
// mirroring normalization.Diagnosis.DiagnosedAt's own *time.Time
// optionality rather than inventing a sentinel value.
const schema = `
CREATE TABLE IF NOT EXISTS files (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    filename     TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    uploaded_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_files_user_id        ON files(user_id);
CREATE INDEX IF NOT EXISTS idx_files_user_id_sha256 ON files(user_id, sha256);

CREATE TABLE IF NOT EXISTS medical_documents (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL,
    file_id         TEXT NOT NULL,
    status          TEXT NOT NULL,
    document_type   TEXT NOT NULL DEFAULT '',
    document_date   INTEGER,
    title           TEXT NOT NULL DEFAULT '',
    organization    TEXT NOT NULL DEFAULT '',
    doctor          TEXT NOT NULL DEFAULT '',
    recognized_text TEXT NOT NULL DEFAULT '',
    summary         TEXT NOT NULL DEFAULT '',
    uploaded_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_medical_documents_user_id ON medical_documents(user_id);
CREATE INDEX IF NOT EXISTS idx_medical_documents_file_id ON medical_documents(file_id);

CREATE TABLE IF NOT EXISTS canonical_units (
    user_id        TEXT NOT NULL,
    indicator_name TEXT NOT NULL,
    unit           TEXT NOT NULL,
    PRIMARY KEY (user_id, indicator_name)
);

CREATE TABLE IF NOT EXISTS extraction_records (
    id             TEXT PRIMARY KEY,
    document_id    TEXT NOT NULL,
    version        INTEGER NOT NULL,
    active         INTEGER NOT NULL DEFAULT 0,
    prompt_version TEXT NOT NULL DEFAULT '',
    model_used     TEXT NOT NULL DEFAULT '',
    raw            TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_extraction_records_document_id        ON extraction_records(document_id);
CREATE INDEX IF NOT EXISTS idx_extraction_records_document_id_active ON extraction_records(document_id, active);

CREATE TABLE IF NOT EXISTS diagnoses (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    document_id  TEXT NOT NULL,
    name         TEXT NOT NULL,
    code         TEXT NOT NULL DEFAULT '',
    code_system  TEXT NOT NULL DEFAULT '',
    diagnosed_at INTEGER,
    status       TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_diagnoses_user_id     ON diagnoses(user_id);
CREATE INDEX IF NOT EXISTS idx_diagnoses_document_id ON diagnoses(document_id);

CREATE TABLE IF NOT EXISTS medications (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    document_id   TEXT NOT NULL,
    drug_name     TEXT NOT NULL,
    dose_amount   REAL NOT NULL DEFAULT 0,
    dose_unit     TEXT NOT NULL DEFAULT '',
    frequency     TEXT NOT NULL DEFAULT '',
    route         TEXT NOT NULL DEFAULT '',
    started_at    INTEGER,
    ended_at      INTEGER,
    status        TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    prescribed_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_medications_user_id     ON medications(user_id);
CREATE INDEX IF NOT EXISTS idx_medications_document_id ON medications(document_id);

CREATE TABLE IF NOT EXISTS lab_results (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL,
    document_id       TEXT NOT NULL,
    indicator_name    TEXT NOT NULL,
    code              TEXT NOT NULL DEFAULT '',
    code_system       TEXT NOT NULL DEFAULT '',
    value             REAL NOT NULL DEFAULT 0,
    qualitative_value TEXT NOT NULL DEFAULT '',
    unit              TEXT NOT NULL DEFAULT '',
    normalized_value  REAL NOT NULL DEFAULT 0,
    normalized_unit   TEXT NOT NULL DEFAULT '',
    reference_low     REAL NOT NULL DEFAULT 0,
    reference_high    REAL NOT NULL DEFAULT 0,
    taken_at          INTEGER
);
CREATE INDEX IF NOT EXISTS idx_lab_results_user_id            ON lab_results(user_id);
CREATE INDEX IF NOT EXISTS idx_lab_results_document_id        ON lab_results(document_id);
CREATE INDEX IF NOT EXISTS idx_lab_results_user_id_indicator   ON lab_results(user_id, indicator_name);

CREATE TABLE IF NOT EXISTS instrumental_findings (
    id                TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL,
    document_id       TEXT NOT NULL,
    procedure_id      TEXT NOT NULL DEFAULT '',
    structure         TEXT NOT NULL,
    parameter         TEXT NOT NULL,
    value             REAL NOT NULL DEFAULT 0,
    unit              TEXT NOT NULL DEFAULT '',
    normalized_value  REAL NOT NULL DEFAULT 0,
    normalized_unit   TEXT NOT NULL DEFAULT '',
    qualitative_value TEXT NOT NULL DEFAULT '',
    reference_low     REAL NOT NULL DEFAULT 0,
    reference_high    REAL NOT NULL DEFAULT 0,
    measured_at       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_instrumental_findings_document_id ON instrumental_findings(document_id);
CREATE INDEX IF NOT EXISTS idx_instrumental_findings_user_structure_parameter
    ON instrumental_findings(user_id, structure, parameter);

CREATE TABLE IF NOT EXISTS procedures (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    document_id  TEXT NOT NULL,
    type         TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL,
    performed_at INTEGER,
    performed_by TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_procedures_user_id     ON procedures(user_id);
CREATE INDEX IF NOT EXISTS idx_procedures_document_id ON procedures(document_id);

CREATE TABLE IF NOT EXISTS allergies (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    document_id TEXT NOT NULL,
    substance   TEXT NOT NULL,
    reaction    TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_allergies_user_id     ON allergies(user_id);
CREATE INDEX IF NOT EXISTS idx_allergies_document_id ON allergies(document_id);

-- user_key_checks backs internal/mcrypto.VerifyOrInitKeyCheck for every
-- repository that supports per-user encryption (see
-- docs/architecture/06-storage.md §14) — shared across repositories rather
-- than one table per repository, since the sentinel validates a user's key
-- generally, not any one repository's data specifically. Not yet written
-- to by any repository — see internal/mcrypto's package doc comment for
-- the integration work still needed before this table is actually used.
CREATE TABLE IF NOT EXISTS user_key_checks (
    user_id      TEXT PRIMARY KEY,
    check_nonce  BLOB NOT NULL,
    check_cipher BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS embeddings (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    source_type   TEXT NOT NULL,
    source_id     TEXT NOT NULL,
    provider      TEXT NOT NULL,
    model_version TEXT NOT NULL,
    vector        BLOB NOT NULL,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_embeddings_user_id_model ON embeddings(user_id, model_version);
CREATE INDEX IF NOT EXISTS idx_embeddings_source         ON embeddings(source_type, source_id);

-- tokenize='trigram': the default unicode61 tokenizer only matches whole
-- tokens after tokenization, with no stemming — "бессонница" would never
-- match text containing "бессонницу"/"бессонницей" (Russian case endings),
-- which makes it close to useless for this service's primary language.
-- SQLite has no Russian-aware stemmer (unlike 'porter', English-only), so
-- trigram (substring matching via 3-grams) is used instead — see
-- internal/storage's fts.go doc comment for the query-side implication
-- (quoted terms, 3+ characters).
CREATE VIRTUAL TABLE IF NOT EXISTS document_fts USING fts5(
    document_id UNINDEXED,
    user_id UNINDEXED,
    title,
    content,
    tokenize='trigram'
);

CREATE VIRTUAL TABLE IF NOT EXISTS event_fts USING fts5(
    event_id UNINDEXED,
    user_id UNINDEXED,
    content,
    tokenize='trigram'
);

CREATE TABLE IF NOT EXISTS self_reported_events (
    id                    TEXT PRIMARY KEY,
    user_id               TEXT NOT NULL,
    raw_text              TEXT NOT NULL,
    occurred_at           INTEGER NOT NULL,
    logged_at             INTEGER NOT NULL,
    status                TEXT NOT NULL,
    category              TEXT NOT NULL DEFAULT '',
    description           TEXT NOT NULL DEFAULT '',
    medication_intake_id  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_self_reported_events_user_id ON self_reported_events(user_id);

CREATE TABLE IF NOT EXISTS medication_intakes (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    source_type TEXT NOT NULL,
    source_id   TEXT NOT NULL,
    drug_name   TEXT NOT NULL,
    dose_amount REAL NOT NULL DEFAULT 0,
    dose_unit   TEXT NOT NULL DEFAULT '',
    taken_at    INTEGER NOT NULL,
    reason      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_medication_intakes_user_id ON medication_intakes(user_id);
CREATE INDEX IF NOT EXISTS idx_medication_intakes_source  ON medication_intakes(source_type, source_id);

CREATE TABLE IF NOT EXISTS medical_profiles (
    user_id     TEXT PRIMARY KEY,
    data        TEXT NOT NULL,
    rebuilt_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS timeline_events (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL,
    date               INTEGER NOT NULL,
    type               TEXT NOT NULL,
    title              TEXT NOT NULL,
    summary            TEXT NOT NULL DEFAULT '',
    document_id        TEXT NOT NULL DEFAULT '',
    source_entity_type TEXT NOT NULL DEFAULT '',
    source_entity_id   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_timeline_events_user_id_date ON timeline_events(user_id, date);
CREATE INDEX IF NOT EXISTS idx_timeline_events_document_id  ON timeline_events(document_id);
CREATE INDEX IF NOT EXISTS idx_timeline_events_source_entity ON timeline_events(source_entity_type, source_entity_id);

CREATE TABLE IF NOT EXISTS vital_signs (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL,
    document_id TEXT NOT NULL,
    type        TEXT NOT NULL,
    systolic    REAL NOT NULL DEFAULT 0,
    diastolic   REAL NOT NULL DEFAULT 0,
    value       REAL NOT NULL DEFAULT 0,
    unit        TEXT NOT NULL DEFAULT '',
    measured_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_vital_signs_user_id     ON vital_signs(user_id);
CREATE INDEX IF NOT EXISTS idx_vital_signs_document_id ON vital_signs(document_id);
CREATE INDEX IF NOT EXISTS idx_vital_signs_user_id_type ON vital_signs(user_id, type);
`

// schemaMigrations holds changes applied after schema's initial CREATE
// TABLE statements (see miranda-diary/internal/diary/store.go for the
// precedent this mirrors) — an already-existing table (as opposed to a
// brand-new database, where the column is already present via schema
// above) needs an explicit ALTER TABLE to pick up a new column; Open
// tolerates "duplicate column name" so the same migration is safe to
// replay against a database that already has it (including a fresh one).
var schemaMigrations = []string{
	`ALTER TABLE lab_results ADD COLUMN qualitative_value TEXT NOT NULL DEFAULT ''`,
}

// Store is the shared SQLite connection every entity's repository is built
// on (see e.g. diagnosis.go's sqliteDiagnosisRepository). Open once per
// process and share across repositories — SQLite has exactly one writer
// regardless of how many *sql.DB handles point at the same file, so
// sharing one avoids needless lock contention between them.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, applies schema and
// any pending migrations, and returns a ready Store. The caller must call
// Close when done.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}

	// Single writer: cap the pool to one connection so concurrent writes
	// queue inside database/sql rather than hitting SQLite's
	// busy_timeout=0 default and returning SQLITE_BUSY immediately.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: enable WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: enable foreign keys: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: apply schema: %w", err)
	}

	for _, m := range schemaMigrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			_ = db.Close()
			return nil, fmt.Errorf("storage: apply migration: %w", err)
		}
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
