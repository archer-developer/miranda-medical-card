# miranda-medical-card — project notes for Claude Code

Household medical-record MCP server (docs/architecture/01-overview.md) — a sibling to
[miranda](../miranda) (the "brain" — LLM orchestration, MCP *client*) and built on the shared LLM
plumbing in [miranda-llm](../miranda-llm) (see that repo's own `CLAUDE.md`). Local `go.work`
(`use . / use ../miranda-llm`) means both repos are developed together — a change here that needs
something new from `miranda-llm` is made in that sibling repo directly, not vendored.

Miranda is the orchestrator; this service is the medical expert it delegates to (docs/mcp/01-overview.md
§2 "Miranda — оркестратор. Medical Service — медицинский эксперт."). Medical/domain reasoning stays
inside this service — Miranda never analyzes medical data itself, it only routes a question here and
relays the answer back verbatim.

`docs/architecture/*.md` and `docs/mcp/*.md` are the source of truth for anything domain-specific
(processing pipeline, Knowledge Providers pattern, storage layout, each MCP tool's contract) — this
file only orients you across the codebase and states conventions; it doesn't duplicate that material.

## Architecture at a glance

```
Miranda <--Streamable HTTP (bearer token)--> httpserver
                                                  |
                                             mcpserver
                                          /   |   |    \
                                      files docs events ask/profile/timeline
                                                          |
                                                     internal/ask
                                                (Agent Loop — see below)
```

Two independent LLM-calling subsystems, both configured under `llm.providers` in `internal/config`:

- **Document Pipeline** (`internal/pipeline`, `internal/extraction`, `internal/normalization`) —
  `medical.upload_document`'s synchronous OCR → Structured Extraction → Normalization → Timeline/Profile/
  Embeddings/FTS chain (docs/architecture/02-processing-pipeline.md). One-shot `llm.Provider.Structured`/
  `Chat` calls, no open dialogue. Configured via `llm.document_provider`.
- **Agent Loop** (`internal/ask`) — `medical.ask`'s internal agent (docs/architecture/05-llm.md §3,
  docs/adr/001-internal-agent-loop-implementation.md): a single open `llm.Provider.Chat` dialogue,
  wrapped in a `*router.Router` (reliability fallback + tool-based escalation), where the model
  iteratively calls registered Knowledge Providers as LLM tools until it has enough to answer directly —
  no separate Planner/Answer Generator calls. Configured via `llm.agent_provider`. Optionally persists
  per-`sessionId` conversation history (`internal/ask/session.go`'s `SessionStore`, backed by
  `internal/storage`'s `ask_sessions`/`ask_messages` tables) so follow-up questions within the same
  Miranda conversation stay in context — a `sessionId` is entirely optional; omitting it reproduces the
  old fully stateless one-shot behavior.

## Package map

- **`cmd/miranda-medical-card`** — the only place production wiring happens (`main.go`'s `run()`):
  config → logger → SQLite + filestore → LLM providers (`buildProviders`) → `router.Router`
  (`buildAskRouter`) → Pipeline + Asker → MCP server → Streamable HTTP behind bearer auth. `logs/llm.log`
  (full LLM request/response trace) and `logs/debug.log` are both only written when `logging.level:
  debug`.
- **`cmd/medical-dev`** — scoped diagnostic CLI (docs/cli/medical_dev.md) using the same Application
  Services as the MCP server (`internal/pipeline.Pipeline`, `internal/ask.Asker`), never
  `internal/storage` directly. Shipped alongside the service binary by `scripts/deploy.sh` for one-off
  ops against the live database.
- **`internal/ask`** — the Agent Loop: `Asker`/`Ask` (agent_loop.go), the Knowledge Provider abstraction
  (`Provider`/`Registry`/`KnowledgeChunk`/`KnowledgeRequest`, provider.go) and its ~10 concrete
  implementations (providers.go, search_providers.go), per-provider `llm.ToolDef` schema construction
  (tools.go), the merged system prompt including the clinical-safety answer rules (prompt.go), and
  session persistence (session.go, on top of `internal/storage`).
- **`internal/storage`** — SQLite persistence (`modernc.org/sqlite`, pure Go). One narrow repository
  interface per entity, each in its own file (e.g. `timeline_event.go` → `TimelineRepository`) — a thin
  interface a test can fake by hand, not a mocking framework. `storage.go` owns only the shared
  connection and schema bootstrap: a single `schema` string constant (`CREATE TABLE IF NOT EXISTS ...`,
  applied unconditionally at every `Open()`) plus an append-only `schemaMigrations []string` for later
  `ALTER TABLE`s on existing tables (tolerates "duplicate column name" so replay is always safe). Adding
  a new entity means: add its table(s) to `schema`, add a new `xxx.go` with its repository
  interface/implementation, wire `storage.NewXxxRepository(store)` into `main.go`.
- **`internal/config`** — `Default()` (every field has a safe built-in value) merged with
  `config/*.yaml` files in `Load()`, then `validate()` rejects anything a hand-edited file could produce
  that `Default()` never would. `config/config.yaml.dist` is the single checked-in, documented template —
  never loaded by the running service; a real deployment's own `config/*.yaml` files are gitignored.
  Secrets are never in YAML: config stores only an environment variable *name*
  (`auth_token_env`, `api_key_envs`, ...), the value is read from the process environment at startup.
- **`internal/mcpserver`** — MCP tool registration/handlers on `github.com/modelcontextprotocol/go-sdk/mcp`.
  Each tool: typed `XxxInput`/`XxxOutput` structs with `json`+`jsonschema:"..."` tags, a handler
  registered via `mcp.AddTool`. `userGate` (users.go) is the single source of truth for `userId`/
  `subjectId` sharing rules. Every handler except `medical.ask`/`medical.download_file` leaves
  `mcp.CallToolResult.Content` nil so the SDK mirrors the full `StructuredContent` into it — see
  `server_test.go`'s `requireContentMirrorsStructured` for why that invariant matters (Miranda's relay
  only reads `Content`, never `StructuredContent`).
- **`internal/pipeline`, `internal/extraction`, `internal/normalization`, `internal/timeline`,
  `internal/profile`, `internal/search`** — the Document Pipeline's stages, each following
  docs/architecture/02-processing-pipeline.md.
- **`internal/filestore`** — binary file storage on disk, keyed by content hash; SQLite only ever
  stores a reference, never file bytes.
- **`internal/httpserver`** — bearer-auth gate in front of the MCP handler; kept free of any
  MCP-specific knowledge.

## Conventions

Same family-wide conventions as [miranda-service-skeleton](../miranda-service-skeleton) and
[miranda-llm](../miranda-llm):

- **Write explanatory comments** — doc-comments on exported symbols, comments on non-obvious logic and
  *why* a decision was made (a past incident, a rejected alternative). This is a small home-infra
  codebase maintained intermittently; future-you benefits more from carried-forward reasoning than
  terse code.
- **No Docker, no CGO, no CI.** Single static Go binary (`CGO_ENABLED=0`), deployed by hand-rolled
  `scripts/deploy.sh` over SSH to a `systemd --user` service. `go build ./... && go vet ./... && go test
  ./...` run locally before committing is the actual quality gate.
- **Error wrapping**: `fmt.Errorf("<package>: <what>: %w", err)` throughout.
- **No DI framework, no HTTP router library.** Plain `net/http`; dependencies wired by hand, top-down,
  in `main.go`'s `run()`.
- **Narrow interfaces over mocking frameworks** — every repository/provider interface a test can fake
  by hand (see `internal/storage`'s per-entity repositories, `internal/ask.Provider`,
  `internal/ask.ChatProvider`).

## Running things

- **Tests**: `go test ./...`. In-memory SQLite is the standard test fixture — `storage.Open(":memory:")`
  — see almost any `*_test.go` under `internal/storage` or `internal/ask` for the pattern. LLM-calling
  code is tested against `github.com/archer-developer/miranda-llm/llmtest`'s scriptable
  `FakeProvider`/`ChatOnlyProvider` (and, for the Agent Loop specifically, a real `*router.Router`
  wrapping one — see `internal/ask/agent_loop_test.go` — so tests exercise the actual `ChatPinned`
  pin-forwarding behavior production wiring relies on, not a simplified stand-in for it).
- **Dev server**: needs `.env` (secrets — see `internal/envfile`, real process env always wins over it)
  and `config/*.yaml` (copy `config/config.yaml.dist` as a starting point), then `go run
  ./cmd/miranda-medical-card`.
- **Diagnostics against a live/seeded database**: `go run ./cmd/medical-dev <command>` — see
  docs/cli/medical_dev.md for the supported subset (profile, timeline, document, ask, pipeline,
  backfill-titles).
