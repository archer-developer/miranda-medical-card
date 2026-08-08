// Package config loads miranda-medical-card's YAML configuration and merges
// it over built-in defaults, so the service runs with sane behavior even
// with no config files at all. Mirrors miranda-diary's internal/config
// exactly in structure (Default/Load/validate) — see that package's doc
// comment for the general pattern this follows.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// UserConfig describes one household member — the only source of User
// entities (docs/domain/02-user.md §3: "User не хранится в SQLite").
type UserConfig struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	// BirthDate, if set, is an ISO 8601 date (YYYY-MM-DD).
	BirthDate string `yaml:"birth_date"`
	// Sex is "male" or "female", if known — used for sex-specific reference
	// ranges (see docs/domain/02-user.md §2). Empty is valid (unknown).
	Sex string `yaml:"sex"`
	// SharedWith is a read-only allowlist: other users' ids allowed to read
	// this user's data via subjectId (see docs/domain/02-user.md §4). Empty
	// by default — visible only to the owner.
	SharedWith []string `yaml:"shared_with"`
	// Encryption enables AES-256-GCM encryption of this user's sensitive
	// text fields (see docs/architecture/06-storage.md §14). Mutually
	// exclusive with a non-empty SharedWith — see validate().
	Encryption bool `yaml:"encryption"`
}

// Config is the root of the service's configuration tree.
type Config struct {
	HTTPAddr     string          `yaml:"http_addr"`
	AuthTokenEnv string          `yaml:"auth_token_env"`
	TLS          TLSConfig       `yaml:"tls"`
	Database     DatabaseConfig  `yaml:"database"`
	Files        FilesConfig     `yaml:"files"`
	LLM          LLMConfig       `yaml:"llm"`
	Embedding    EmbeddingConfig `yaml:"embedding"`
	Search       SearchConfig    `yaml:"search"`
	Users        []UserConfig    `yaml:"users"`
	Logging      LoggingConfig   `yaml:"logging"`
}

// TLSConfig controls whether the HTTP server listens with TLS — same
// rationale as miranda-diary's TLSConfig (self-signed, loopback-only link
// that still carries per-user encryption keys).
type TLSConfig struct {
	Enabled  bool     `yaml:"enabled"`
	CertFile string   `yaml:"cert_file"`
	KeyFile  string   `yaml:"key_file"`
	Hosts    []string `yaml:"hosts"`
}

// DatabaseConfig controls the SQLite database file location.
type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// FilesConfig controls where uploaded binary content is stored (see
// internal/filestore).
type FilesConfig struct {
	Dir string `yaml:"dir"`
}

// LLMConfig configures the Gemini models used for each Pipeline/Ask stage.
// A single provider (Gemini) is used directly for now, the same way
// miranda-diary's main.go constructs embedding.NewGemini directly — every
// call site still depends only on miranda-llm's abstract interfaces
// (extraction.Provider, embedding.Embedder), so swapping providers later is
// a main.go change, not a business-logic change (see
// docs/architecture/05-llm.md §8-9).
type LLMConfig struct {
	// APIKeyEnvs names the environment variables holding Gemini API keys,
	// tried in order with automatic rotation on rate limits (see
	// gemini.RotationConfig) — e.g. ["GEMINI_API_KEY_1", "GEMINI_API_KEY_2"].
	APIKeyEnvs []string `yaml:"api_key_envs"`
	// DocumentModel is used for every Document Pipeline LLM stage: OCR
	// (Stage 1) and Structured Extraction (Stage 2a/2b, see
	// internal/extraction.Extract) and Self-Reported Event extraction (see
	// internal/events). internal/extraction.Extract takes a single Provider
	// for both OCR and Structured calls (they're still two separate LLM
	// calls, which is what mattered empirically for reliability — see
	// internal/extraction's package doc comment — just against the same
	// model/provider instance for now); splitting this into distinct
	// OCR/Extraction models would need internal/pipeline to call
	// extraction.OCR/StructuredWithRetry directly instead of
	// extraction.Extract, which is a reasonable future optimization, not a
	// correctness gap in what's implemented here.
	DocumentModel string `yaml:"document_model"`
	// PlannerModel is used by the Planner (see internal/ask).
	PlannerModel string `yaml:"planner_model"`
	// AnswerModel is used by the Answer Generator (see internal/ask).
	AnswerModel string `yaml:"answer_model"`
}

// EmbeddingConfig configures the Gemini embedding model used for semantic
// search (see docs/architecture/04-search.md §14).
type EmbeddingConfig struct {
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
}

// SearchConfig controls Knowledge Provider search limits.
type SearchConfig struct {
	DefaultLimit int `yaml:"default_limit"`
	MaxLimit     int `yaml:"max_limit"`
}

// LoggingConfig controls slog output level.
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// Default returns the built-in configuration. Every field has a safe,
// runnable value so a missing or empty config.yaml still produces a working
// service — except Users, which has no default (see validate()).
func Default() Config {
	return Config{
		HTTPAddr:     ":8791",
		AuthTokenEnv: "MEDICAL_CARD_MCP_TOKEN",
		TLS: TLSConfig{
			Enabled:  true,
			CertFile: "data/tls/cert.pem",
			KeyFile:  "data/tls/key.pem",
			Hosts:    []string{"127.0.0.1", "localhost"},
		},
		Database: DatabaseConfig{
			Path: "data/medical-card.db",
		},
		Files: FilesConfig{
			Dir: "data/files",
		},
		LLM: LLMConfig{
			APIKeyEnvs:    []string{"GEMINI_API_KEY_1"},
			DocumentModel: "gemini-3.6-flash",
			PlannerModel:  "gemini-3.5-flash-lite",
			AnswerModel:   "gemini-3.6-flash",
		},
		Embedding: EmbeddingConfig{
			APIKeyEnv: "GEMINI_API_KEY_1",
			Model:     "gemini-embedding-2",
		},
		Search: SearchConfig{
			DefaultLimit: 10,
			MaxLimit:     50,
		},
		Users: nil,
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load reads each YAML file in paths, in order, and merges it over
// Default(). A missing file is not an error. See miranda-diary's
// internal/config.Load for the full merge-semantics explanation this
// mirrors exactly.
func Load(paths ...string) (Config, error) {
	cfg := Default()

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}

		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	for i := range cfg.Users {
		cfg.Users[i].ID = strings.TrimSpace(cfg.Users[i].ID)
	}

	if err := cfg.validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func (c Config) validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: http_addr must not be empty")
	}
	if c.AuthTokenEnv == "" {
		return fmt.Errorf("config: auth_token_env must not be empty")
	}
	if c.TLS.Enabled {
		if c.TLS.CertFile == "" {
			return fmt.Errorf("config: tls.cert_file must not be empty when tls.enabled is true")
		}
		if c.TLS.KeyFile == "" {
			return fmt.Errorf("config: tls.key_file must not be empty when tls.enabled is true")
		}
		if len(c.TLS.Hosts) == 0 {
			return fmt.Errorf("config: tls.hosts must not be empty when tls.enabled is true")
		}
	}
	if c.Database.Path == "" {
		return fmt.Errorf("config: database.path must not be empty")
	}
	if c.Files.Dir == "" {
		return fmt.Errorf("config: files.dir must not be empty")
	}
	if len(c.LLM.APIKeyEnvs) == 0 {
		return fmt.Errorf("config: llm.api_key_envs must not be empty")
	}
	if c.LLM.DocumentModel == "" || c.LLM.PlannerModel == "" || c.LLM.AnswerModel == "" {
		return fmt.Errorf("config: llm.document_model, planner_model, and answer_model must all be set")
	}
	if c.Embedding.APIKeyEnv == "" {
		return fmt.Errorf("config: embedding.api_key_env must not be empty")
	}
	if c.Embedding.Model == "" {
		return fmt.Errorf("config: embedding.model must not be empty")
	}
	if c.Search.DefaultLimit < 1 {
		return fmt.Errorf("config: search.default_limit must be at least 1")
	}
	if c.Search.MaxLimit < c.Search.DefaultLimit {
		return fmt.Errorf("config: search.max_limit must be >= default_limit")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: logging.level must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}
	if len(c.Users) == 0 {
		return fmt.Errorf("config: users must not be empty — list every household member explicitly in config.yaml")
	}
	seen := make(map[string]bool, len(c.Users))
	for i, u := range c.Users {
		if u.ID == "" {
			return fmt.Errorf("config: users[%d].id must not be empty", i)
		}
		if seen[u.ID] {
			return fmt.Errorf("config: users[%d].id %q is a duplicate — entries must be unique", i, u.ID)
		}
		seen[u.ID] = true
		if u.BirthDate != "" {
			if _, err := time.Parse("2006-01-02", u.BirthDate); err != nil {
				return fmt.Errorf("config: users[%d].birth_date %q must be YYYY-MM-DD", i, u.BirthDate)
			}
		}
		if u.Sex != "" && u.Sex != "male" && u.Sex != "female" {
			return fmt.Errorf("config: users[%d].sex must be \"male\" or \"female\" if set, got %q", i, u.Sex)
		}
		// encryption and shared_with are mutually exclusive — a shared user
		// can't hold a key another reader would also need, see
		// docs/domain/02-user.md §5 and docs/architecture/06-storage.md §14.
		if u.Encryption && len(u.SharedWith) > 0 {
			return fmt.Errorf("config: users[%d] (%s): encryption and shared_with are mutually exclusive", i, u.ID)
		}
	}
	// shared_with entries must reference real, configured users — an unknown
	// id there is silently useless (never grants access to anyone) and
	// almost certainly a typo worth failing loudly on at startup rather than
	// discovering later as "why can't alex read anna's data."
	for i, u := range c.Users {
		for _, sharedID := range u.SharedWith {
			if !seen[sharedID] {
				return fmt.Errorf("config: users[%d] (%s): shared_with references unknown user %q", i, u.ID, sharedID)
			}
		}
	}
	return nil
}
