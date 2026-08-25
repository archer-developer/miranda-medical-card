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

`docs/architecture/*.md`, `docs/domain/*.md`, and `docs/mcp/*.md` are the source of truth for anything
domain-specific (processing pipeline, Knowledge Providers pattern, storage layout, each medical entity's
business model, each MCP tool's contract) — this file only orients you across the codebase and states
conventions; it doesn't duplicate that material.

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
  `Chat` calls, no open dialogue. OCR (Stage 1) and Structured Extraction (Stage 2a/2b, plus
  Self-Reported Event extraction and decline matching) are independently configured via
  `llm.ocr_provider`/`llm.extraction_provider` — they may name the same or different providers.
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
- **`cmd/extract-test`, `cmd/renormalize-labs`** — throwaway/one-off tools, not part of production
  wiring and not shipped by `scripts/deploy.sh` — each says so in its own doc comment. `extract-test` is
  a harness for validating the Structured Extraction prompt against real documents outside the real
  Pipeline; `renormalize-labs` is a standalone migration that re-derives `LabResult.IndicatorName` for
  rows persisted before `internal/normalization/indicator_aliases.go` existed.
- **`internal/ask`** — the Agent Loop: `Asker`/`Ask` (agent_loop.go), the Knowledge Provider abstraction
  (`Provider`/`Registry`/`KnowledgeChunk`/`KnowledgeRequest`, provider.go) and its ~10 concrete
  implementations (providers.go, search_providers.go), per-provider `llm.ToolDef` schema construction
  (tools.go), the merged system prompt including the clinical-safety answer rules (prompt.go), session
  persistence (session.go, on top of `internal/storage`), and per-turn anomaly detection (anomaly.go,
  `SetAnomalyConfig` — see "Reviewing `logs/anomalies/`" below).
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
- **`internal/events`** — Structured Extraction for Self-Reported Events (docs/mcp/07-events.md §3), the
  text-only counterpart to `internal/extraction`'s document-based extraction — powers `medical.log_event`.
- **`internal/planning`, `internal/planmatch`, `internal/decline`** — Planned Actions
  (docs/adr/004-planned-actions.md): `planning` defines the raw, LLM-facing shape of a future medical
  action recommended in a document or self-reported note; `planmatch` is the auto-completion half —
  given the entities Normalization just produced, decide which still-pending PlannedActions it satisfies
  (docs/adr/005-planned-action-cross-source-dedup.md); `decline` is a small reusable "which of the user's
  few current `<thing>`s does this free-text message refer to" matcher, built for
  `medical.decline_planned_action`'s matching step but reused wherever free text needs to resolve to one
  of a short list of candidates.
- **`internal/nutrition`** — the Nutrition Advisor Domain Service (docs/adr/006-nutrition-guidance.md): a
  small, one-shot Structured LLM call turning a household member's active diagnoses/allergies/age/sex
  into dietary guidance; consumes the `users[].language` config field for its prompt.
- **`internal/mcrypto`** — the AES-256-GCM encryption-at-rest pattern (docs/architecture/06-storage.md
  §14, the same pattern proven in `miranda-diary`'s `internal/diary/crypto.go`), opted into per-user via
  `encryption: true` in that user's config — mutually exclusive with `shared_with` (a shared user can't
  hold someone else's key). Miranda injects the per-user key on each MCP call touching encrypted data;
  Medical Service never stores or logs it itself.
- **`internal/filestore`** — binary file storage on disk, keyed by content hash; SQLite only ever
  stores a reference, never file bytes.
- **`internal/linkstore`** — short-lived, random-ID links for ephemeral/derived data (as opposed to
  `internal/filestore`'s permanent, on-disk documents) — `Store.Mint`/`Resolve`, backed by SQLite via
  `storage.EphemeralLinkRepository`, TTL-checked against an injectable clock seam (mirrors
  `internal/ask.SessionStore`'s own `now` field). Currently only used by `medical.profile`'s
  `format=file_uri` (docs/adr/007-short-lived-file-links-for-profile-export.md) — resolved by the
  unauthenticated `GET /links/{linkId}` route, deliberately separate from `/files/{fileId}` (see that
  ADR's "Почему это НЕ то же самое" section for why).
- **`internal/httpserver`** — bearer-auth gate in front of the MCP handler and `/files/{fileId}`; kept
  free of any MCP-specific knowledge. `/links/{linkId}` (`internal/linkstore`) is the one route mounted
  here without that gate — its random id + TTL is the access control, not the shared token.

## Conventions

Same family-wide conventions as [miranda-service-skeleton](../miranda-service-skeleton) and
[miranda-llm](../miranda-llm):

- **Write explanatory comments** — doc-comments on exported symbols, comments on non-obvious logic and
  *why* a decision was made (a past incident, a rejected alternative). This is a small home-infra
  codebase maintained intermittently; future-you benefits more from carried-forward reasoning than
  terse code.
- **Keep docs in the same change, not a follow-up** — if a change alters behavior that
  `docs/architecture/*.md`, `docs/domain/*.md`, `docs/mcp/*.md`, `docs/cli/*.md`, or this file describes,
  update that doc as
  part of the same change. Cheap now, expensive later: this file's own opening line calls those docs
  "the source of truth," and a stale one actively misleads the next person (or the next Claude Code
  session) who trusts it instead of the code. Periodic on-demand doc-vs-code audits are a backstop for
  drift that slips through anyway (changes made outside a session, a doc nobody's revisited) — worth
  asking for occasionally — not a substitute for this.
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
- **Diagnostics against a live/seeded database**: `go run ./cmd/medical-dev <command>` (or bare
  `medical-dev`/`medical-dev help` for a full command list with examples) — see docs/cli/medical_dev.md
  for the documented subset (profile, timeline, planned-actions, document, ask, pipeline) plus `backfill-titles`,
  `reindex-fts`, and `llm-trace`, three commands not in that doc because they're one-off
  migrations/log-analysis rather than Application-Service-shaped reads (see each one's own doc comment
  in `cmd/medical-dev/`). `scripts/deploy.sh` ships the `medical-dev` binary to the server alongside the
  service itself, so every command works identically against the live database over SSH. `llm-trace`'s
  own parsing/analysis logic (block reassembly, provider-shape decoding, conversation grouping) lives in
  `miranda-llm/llmtrace/analyze` — `cmd/medical-dev/llm_trace.go` is just a thin flag-parsing wrapper
  around it, shared with Miranda's own equivalent CLI command and web UI so neither service maintains
  its own copy.

### Claude Code: use medical-dev directly, don't just reason about the code

When investigating a bug report, testing a fix, or deciding whether a change actually works, run
`medical-dev` yourself — locally (`go run ./cmd/medical-dev ...`, needs `.env` + `config/*.yaml`, see
above) or against the live server (`ssh archer@miranda 'cd ~/miranda-medical-card && ./medical-dev
...'`) — rather than only reading source and predicting behavior. This is expected, not something to
ask permission for each time; treat it the same as running `go test`. The one thing that *does* warrant
checking with the user first is anything that mutates the live database beyond a diagnostic read —
`backfill-titles`/`reindex-fts` are safe (idempotent, no LLM calls, easy to reason about), a live
`deploy.sh` redeploy or restarting the running service mid-question is not something to do silently.

**Debugging a `medical.ask` prompt/tool problem (a bad answer, a loop that exhausts its iteration cap,
a tool the model misuses) — the loop that found and fixed several real bugs this way:**

1. **Reproduce** with `logging.level: debug` set (`config/logging.yaml` on whichever host — local or
   server) and run the same question through `medical-dev ask --user <id> "question"`. This builds the
   identical `Registry`/`Asker` a real MCP `medical.ask` call does (see `runAsk` in
   `cmd/medical-dev/main.go`) and traces into `logs/llm.log` exactly like the live service would (see
   `openLLMTraceWriter`, mirroring `cmd/miranda-medical-card/main.go`'s own `buildLLMTraceWriter`) — so
   a question behaves and traces identically whichever entry point it came through.
2. **Inspect** with `medical-dev llm-trace`: bare, it lists every `sessionId`-tagged conversation found
   in the log; `--conversation <id>` or `--latest` prints one as a turn-by-turn table — what tool got
   called with what arguments, what came back, and the final answer (or a note that the conversation
   was cut off on a tool call with no answer). A `medical-dev ask` call, and any real stateless
   `medical.ask` call with no `sessionId` (the default for a one-off question), never gets a
   conversation id at all — use `--untagged` for those; it groups the log's untagged tail back into
   separate questions.
3. **Diagnose from the turn-by-turn table**, not from guessing: is the model missing a capability (no
   tool/parameter exists for what it's trying to do — e.g. no date-range filter on a provider), is it
   being misled by bad data (an FTS false-positive, a too-short snippet), or is a system-prompt rule
   (`internal/ask/prompt.go`'s `answerRules`) blocking it from using data it already has? Every root
   cause found so far was one of these three, never "just needed more iterations" — resist raising
   `askMaxToolIterations` as the first move.
4. **Fix, redeploy** (`./scripts/deploy.sh` — restarts the live service; say so before running it
   against the production server), **and re-run step 1-2 against the fresh log** to confirm the same
   question now behaves differently, not just that the code compiles.

**Reviewing `logs/anomalies/`:** every `Ask()` turn is checked, as it ends, for a set of mechanical
anomalies — a slow LLM call, the model retrying a tool with identical arguments, a call to a tool that
doesn't exist, malformed tool arguments, a tool execution error, or hitting the iteration cap/a timeout
(`miranda-llm/llmtrace/anomaly.Detect`, wired in via `internal/ask/anomaly.go`'s `reportAnomalies`). A
flagged turn gets its own file under `logs/anomalies/` (the whole conversation so far when a `sessionId`
is available, re-read from `logs/llm.log` — just that turn's blocks otherwise) plus exactly one `WARN`
line in the normal app log/journal — never a full trace dump there, that's what the file is for. This
only runs when `logging.level: debug` is set, same as `logs/llm.log` itself (a Recorder attached to a
turn's `ctx` would never see a call to detect anything from otherwise). Detection is mechanical/
deterministic — it does not judge whether an anomaly is a real bug or a benign edge case. **Review is
manual and on-demand, not automated**: periodically (or when investigating a report), open a session and
ask it to look through `logs/anomalies/` — each file is already in `llm.log`'s own block format, so the
same `medical-dev llm-trace` tooling and the debugging loop above apply directly; the file's leading
`#`-prefixed header lines out anomaly kind(s) even before opening the trace itself.
