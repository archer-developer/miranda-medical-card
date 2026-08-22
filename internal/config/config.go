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
	HTTPAddr     string `yaml:"http_addr"`
	AuthTokenEnv string `yaml:"auth_token_env"`
	// PublicBaseURL is the externally reachable http(s) origin (scheme +
	// host + port, no trailing slash) Miranda uses to reach this service —
	// e.g. through an SSH tunnel or the local network, whatever the actual
	// deployment topology is. medical.get_document uses it to build the
	// absolute fileUri it returns for a document's original file
	// (docs/mcp/02-files.md §5), which Miranda then fetches with a plain
	// HTTP GET against GET /files/{fileId}, bearer-auth-gated the same way
	// as /mcp. Defaults to the loopback address matching HTTPAddr/TLS.Hosts'
	// own defaults — real deployments reachable over anything but localhost
	// (the common case) need to override this to whatever address/tunnel
	// Miranda actually uses to reach /mcp already.
	PublicBaseURL string          `yaml:"public_base_url"`
	TLS           TLSConfig       `yaml:"tls"`
	Database      DatabaseConfig  `yaml:"database"`
	Files         FilesConfig     `yaml:"files"`
	LLM           LLMConfig       `yaml:"llm"`
	Embedding     EmbeddingConfig `yaml:"embedding"`
	Search        SearchConfig    `yaml:"search"`
	Users         []UserConfig    `yaml:"users"`
	Logging       LoggingConfig   `yaml:"logging"`
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
// internal/filestore) and how large a file medical.upload_document will
// fetch from a caller-supplied fileUri (see internal/mcpserver/documents.go,
// docs/mcp/03-documents.md §4) — the service never accepts file bytes
// directly as an MCP argument, so this is the only per-file size bound.
type FilesConfig struct {
	Dir string `yaml:"dir"`
	// MaxSizeBytes bounds a single medical.upload_document fetch. 50MB is
	// generous for any realistic medical document/scan/photo while still
	// bounded.
	MaxSizeBytes int64 `yaml:"max_size_bytes"`
}

// ProviderConfig describes one configured LLM backend — deliberately
// mirrors miranda's own internal/config.LLMProvider field-for-field (see
// that repo's config/llm.yaml for the format this is meant to interoperate
// with at the config-authoring level). Document Pipeline stages (OCR,
// Structured Extraction — internal/extraction) still make one-shot
// Structured/Chat calls with no open dialogue, but the medical.ask agent
// loop (internal/ask) now makes genuine multi-turn Chat calls through a
// router.Router — see Escalation's doc comment for what that means for
// ToolName/Description.
type ProviderConfig struct {
	Name string `yaml:"name"`
	// Type selects the miranda-llm package: "gemini", "anthropic", or
	// "openai_compat".
	Type string `yaml:"type"`
	// BaseURL is only meaningful for Type == "openai_compat".
	BaseURL string `yaml:"base_url,omitempty"`
	Model   string `yaml:"model"`
	// APIKeyEnvs names environment variables holding API keys, never the
	// keys themselves. Only "gemini" actually rotates across more than one
	// entry (see GeminiRotation) — "anthropic" and "openai_compat" use only
	// APIKeyEnvs[0], same convention as miranda's own firstAPIKey.
	APIKeyEnvs []string `yaml:"api_key_envs"`
	// GeminiRotation tunes key-rotation behavior. Only meaningful when
	// Type == "gemini".
	GeminiRotation GeminiRotationConfig `yaml:"gemini_rotation,omitempty"`
	// Escalation names a fallback provider (by Name, elsewhere in
	// Providers) this provider can hand off to. Two independent mechanisms
	// read it, depending on which stage this provider is configured for:
	//   - LLMConfig.OCRProvider/ExtractionProvider: content-based — retried
	//     once when this provider's own OCR or Structured result comes back
	//     unusable (see internal/extraction.Extract's ocrEscalate parameter
	//     for OCR, extraction.StructuredWithRetry's escalate parameter for
	//     Structured Extraction — a suspiciously empty result, specifically).
	//     Only TargetProvider is read here; ToolName/Description are unused
	//     — a one-shot OCR/Structured call has no open dialogue for a model
	//     to reason about handing off mid-generation.
	//   - LLMConfig.AgentProvider: router.Router's own tool-based escalation
	//     (docs/architecture/05-llm.md §9.1) — when Enabled and ToolName is
	//     set, the agent model may call ToolName mid-conversation to hand a
	//     hard question to TargetProvider, and a hard provider failure
	//     mid-turn falls back to it too. This is a genuinely open multi-turn
	//     Chat loop, unlike OCRProvider/ExtractionProvider's one-shot calls,
	//     so ToolName/Description are functionally read here.
	Escalation EscalationConfig `yaml:"escalation,omitempty"`
}

// GeminiRotationConfig tunes internal/llm/gemini-equivalent key rotation —
// mirrors miranda's GeminiRotationConfig field-for-field.
type GeminiRotationConfig struct {
	CooldownSeconds int `yaml:"cooldown_seconds"`
	MaxRetryCycles  int `yaml:"max_retry_cycles"`
}

// EscalationConfig mirrors miranda's own EscalationConfig field-for-field
// (see ProviderConfig.Escalation for what's actually consulted here, and by
// which mechanism, depending on the owning provider's role).
type EscalationConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ToolName       string `yaml:"tool_name,omitempty"`
	Description    string `yaml:"description,omitempty"`
	TargetProvider string `yaml:"target_provider"`
}

// LLMConfig lists every configured LLM backend and names which one each
// stage uses, by Provider name — mirrors miranda's own
// providers/default_provider shape (see ProviderConfig's doc comment) at
// the schema level, adapted to Medical Service having two fixed,
// always-active stages (document/agent) instead of one ordered fallback
// chain.
type LLMConfig struct {
	Providers []ProviderConfig `yaml:"providers"`
	// OCRProvider runs only Stage 1 (OCR/Vision) of the Document Pipeline —
	// see internal/extraction.Extract's ocrProvider parameter. Independent
	// of ExtractionProvider (may name the same or a different configured
	// provider) so a Structured Extraction schema/prompt change, or a
	// per-model daily quota running out on one of the two stages, doesn't
	// force the other stage to move too — see internal/pipeline.Pipeline.New's
	// own doc comment for the full reasoning.
	OCRProvider string `yaml:"ocr_provider"`
	// ExtractionProvider runs every Structured-shaped call the Document
	// Pipeline makes: Structured Extraction (Stage 2a/2b, see
	// internal/extraction.Extract's structuredProvider parameter),
	// Self-Reported Event extraction (internal/events), decline matching
	// (internal/decline), and title backfill (BackfillStudyTitle).
	ExtractionProvider string `yaml:"extraction_provider"`
	// AgentProvider is the default/primary provider for the medical.ask
	// agent loop (see internal/ask), wired into a router.Router alongside
	// every other configured provider so any provider's own
	// escalation.target_provider can resolve regardless of role — replaces
	// the old, separate PlannerProvider/AnswerProvider fields (removed: the
	// agent loop is one model doing both jobs across an open conversation,
	// not two isolated one-shot calls).
	AgentProvider string `yaml:"agent_provider"`
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
		HTTPAddr:      ":8791",
		AuthTokenEnv:  "MEDICAL_CARD_MCP_TOKEN",
		PublicBaseURL: "https://127.0.0.1:8791",
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
			Dir:          "data/files",
			MaxSizeBytes: 50 * 1024 * 1024,
		},
		LLM: LLMConfig{
			Providers: []ProviderConfig{
				{
					Name: "gemini-document", Type: "gemini", Model: "gemini-3.6-flash",
					APIKeyEnvs:     []string{"GEMINI_API_KEY_1"},
					GeminiRotation: GeminiRotationConfig{CooldownSeconds: 30, MaxRetryCycles: 1},
				},
				{
					Name: "gemini-agent", Type: "gemini", Model: "gemini-3.6-flash",
					APIKeyEnvs:     []string{"GEMINI_API_KEY_1"},
					GeminiRotation: GeminiRotationConfig{CooldownSeconds: 30, MaxRetryCycles: 1},
				},
			},
			OCRProvider:        "gemini-document",
			ExtractionProvider: "gemini-document",
			AgentProvider:      "gemini-agent",
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
	if !strings.HasPrefix(c.PublicBaseURL, "http://") && !strings.HasPrefix(c.PublicBaseURL, "https://") {
		return fmt.Errorf("config: public_base_url must be a non-empty http(s) URL, got %q", c.PublicBaseURL)
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
	if c.Files.MaxSizeBytes < 1 {
		return fmt.Errorf("config: files.max_size_bytes must be at least 1")
	}
	if len(c.LLM.Providers) == 0 {
		return fmt.Errorf("config: llm.providers must not be empty")
	}
	providerNames := make(map[string]bool, len(c.LLM.Providers))
	for i, p := range c.LLM.Providers {
		if p.Name == "" {
			return fmt.Errorf("config: llm.providers[%d].name must not be empty", i)
		}
		if providerNames[p.Name] {
			return fmt.Errorf("config: llm.providers[%d].name %q is a duplicate", i, p.Name)
		}
		providerNames[p.Name] = true
		switch p.Type {
		case "gemini", "anthropic", "openai_compat":
		default:
			return fmt.Errorf("config: llm.providers[%d] (%s): type must be \"gemini\", \"anthropic\", or \"openai_compat\", got %q", i, p.Name, p.Type)
		}
		if p.Model == "" {
			return fmt.Errorf("config: llm.providers[%d] (%s): model must not be empty", i, p.Name)
		}
		if len(p.APIKeyEnvs) == 0 {
			return fmt.Errorf("config: llm.providers[%d] (%s): api_key_envs must not be empty", i, p.Name)
		}
	}
	for i, p := range c.LLM.Providers {
		if !p.Escalation.Enabled {
			continue
		}
		if p.Escalation.TargetProvider == "" {
			return fmt.Errorf("config: llm.providers[%d] (%s): escalation.target_provider must be set when escalation.enabled is true", i, p.Name)
		}
		if !providerNames[p.Escalation.TargetProvider] {
			return fmt.Errorf("config: llm.providers[%d] (%s): escalation.target_provider %q references an unknown provider", i, p.Name, p.Escalation.TargetProvider)
		}
	}
	if c.LLM.OCRProvider == "" || c.LLM.ExtractionProvider == "" || c.LLM.AgentProvider == "" {
		return fmt.Errorf("config: llm.ocr_provider, extraction_provider, and agent_provider must all be set")
	}
	if !providerNames[c.LLM.OCRProvider] {
		return fmt.Errorf("config: llm.ocr_provider %q references an unknown provider", c.LLM.OCRProvider)
	}
	if !providerNames[c.LLM.ExtractionProvider] {
		return fmt.Errorf("config: llm.extraction_provider %q references an unknown provider", c.LLM.ExtractionProvider)
	}
	if !providerNames[c.LLM.AgentProvider] {
		return fmt.Errorf("config: llm.agent_provider %q references an unknown provider", c.LLM.AgentProvider)
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
