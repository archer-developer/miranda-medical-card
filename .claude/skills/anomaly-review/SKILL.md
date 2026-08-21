---
name: anomaly-review
description: Fetch and analyze flagged agent-loop turns from logs/anomalies/ on the production medical-card server (or a local run) — slow LLM calls, stuck tool retries, unknown tools, bad arguments, tool errors, hitting the iteration cap or a timeout. Use when the user asks to check/review anomalies, look at logs/anomalies, or investigate a flagged medical.ask turn.
---

# Reviewing medical-card's `logs/anomalies/`

## Background — what this is

Every `medical.ask` turn (`internal/ask.Asker.Ask`) is checked, as it ends, for a set of **mechanical**
anomalies (`miranda-llm/llmtrace/anomaly.Detect`, wired in via `internal/ask/anomaly.go`). This detection
is deterministic Go code — it does **not** judge whether something is a real bug, just flags the shape.
**Your job when this skill is invoked is the judgment call the detector can't make**: read the flagged
turn's actual trace, decide if it's a real problem, and (if so) diagnose the root cause using the same
three-bucket framing `CLAUDE.md`'s debugging loop already documents — missing capability, bad data, or an
over-strict prompt rule — never "just needed more iterations."

Only runs when `logging.level: debug` is set (same gate as `logs/llm.log` itself), so an anomaly file
only exists on a host where debug logging was on when the turn happened.

### The 7 anomaly kinds (`llmtrace/anomaly` package constants)

| Kind | What it means |
|---|---|
| `slow_call` | One LLM call in the turn took longer than the configured threshold (default 20s) — includes real model latency AND any tool-execution time between iterations (it's a wall-clock gap, not pure LLM time; check the surrounding calls before assuming the model itself was slow — a provider retry/rate-limit backoff shows up here too, see the example below). |
| `repeated_tool_call` | The model called the same tool with the *same* arguments more than once in one turn — usually a sign it's stuck, not making progress on what the result told it. |
| `unknown_tool` | The model called a tool name that isn't registered (`internal/ask/provider.go`'s `Registry.Get` miss) — either a hallucinated name or the tool schema wasn't offered but the model tried anyway. |
| `invalid_arguments` | The model's tool-call arguments failed to decode/parse (`internal/ask/tools.go`'s `decodeKnowledgeRequest` — bad JSON, or an unparseable date). |
| `tool_error` | A registered tool call reached its Provider but `Collect` itself failed (not the two more specific kinds above). |
| `iteration_cap` | The loop hit `askMaxToolIterations` (16) without a final reply — a real, hard stop; see `cmd/miranda-medical-card/main.go`'s doc comment on that constant for what past root causes actually were. |
| `timeout` | The turn's context deadline was exceeded (`errors.Is(err, context.DeadlineExceeded)`). |

### Where files live and what's in one

- **Server**: `archer@miranda:~/miranda-medical-card/logs/anomalies/` (SSH already configured — see
  memory `reference_miranda_server_ssh.md`).
- **Local dev run**: `logs/anomalies/` under the repo root (only appears if `config/logging.yaml` has
  `logging.level: debug` for that run).
- **Filename**: `<UTC timestamp>_<kind1-kind2-...>.log` — e.g. `20260821T124009Z_slow_call.log`. The
  kinds in the filename alone often tell you whether it's worth opening at all.
- **Content**: a `#`-prefixed header (one line per anomaly found, with a human-readable detail) followed
  by the turn's trace blocks in the **exact same format** `logs/llm.log` itself uses — so it opens
  directly with the existing `medical-dev llm-trace` tool, no special-casing needed. When a `sessionId`
  was available, the file holds the **whole conversation up to that point** (re-read from `logs/llm.log`
  at the moment the anomaly fired), not just the flagged turn — diagnosing often needs that lead-up
  context. A fully stateless call (no `sessionId`) only ever has that one turn's own blocks.

## Workflow

1. **List what's there** (server):
   ```bash
   ssh archer@miranda 'ls -la ~/miranda-medical-card/logs/anomalies/'
   ```
   The filenames alone (timestamp + kind(s)) are often enough to triage what's worth a closer look, or
   to notice a pattern (e.g. a burst of `slow_call` around the same time — a Gemini rate-limit incident,
   not a prompt bug).

2. **Fetch the ones you need** — either read in place over SSH (works well for a handful of files):
   ```bash
   ssh archer@miranda 'cat ~/miranda-medical-card/logs/anomalies/<file>'
   ```
   or copy a batch down for local analysis via the actual CLI tool (recommended once you're looking at
   more than one or two):
   ```bash
   scp archer@miranda:~/miranda-medical-card/logs/anomalies/*.log /tmp/anomalies/
   ```

3. **Render each one as a readable turn-by-turn table** with the existing CLI, exactly like debugging a
   normal `llm.log` conversation — see `CLAUDE.md`'s "Debugging a `medical.ask` prompt/tool problem" loop
   for the same pattern applied to the main log:
   ```bash
   go run ./cmd/medical-dev llm-trace -file /tmp/anomalies/<file>.log -untagged
   # or, if the header/blocks show a conversation= tag:
   go run ./cmd/medical-dev llm-trace -file /tmp/anomalies/<file>.log -latest
   ```
   This gives you: what tool got called with what arguments each iteration, what came back, and where it
   ended — the same view the header's anomaly summary is pointing you at.

4. **Diagnose, don't just report the shape** — for each anomaly, work out *why*, using the same three
   buckets `CLAUDE.md`'s debugging loop already uses for `medical.ask` problems in general:
   - **Missing capability**: no tool/parameter exists for what the model was trying to do (e.g. no
     date-range filter, no way to reference a prior result by id).
   - **Bad data**: an FTS false-positive, a too-short/misleading snippet, a genuinely empty dataset (the
     "no data uploaded yet" case is normal, not a bug — don't flag every `slow_call` on an empty-database
     question as a real problem).
   - **Prompt rule blocking valid data use**: `internal/ask/prompt.go`'s `answerRules` preventing the
     model from using something it already has.
   - For `slow_call` specifically: check the surrounding blocks' request/response for retry/rate-limit
     warnings in the accompanying app log around the same timestamp (`logs/debug.log` or the journal) —
     a `503`/key-rotation retry storm on the provider side is a transient infrastructure blip, not a code
     bug, and isn't worth chasing as one.
   - For `repeated_tool_call`/`iteration_cap`: read the full turn — is the tool result actually
     unhelpful/ambiguous (bad data), or is there a real gap in what the model can ask for (missing
     capability)?

5. **Report findings back to the user** — which files you looked at, what kind each genuinely was (a real
   bug vs. a benign transient blip vs. expected behavior on empty data), and for anything that looks like
   a real bug, a proposed fix (following the same "fix, redeploy, re-run against the fresh log" loop
   `CLAUDE.md` documents) rather than just re-describing the anomaly.

## What NOT to do

- Don't propose raising `askMaxToolIterations`/the `slow_call` threshold as a fix for a real bug — per
  `CLAUDE.md`, every root cause found so far for a cap-out was one of the three buckets above, never
  "just needed more iterations/time."
- Don't treat detection itself as verdict — a flagged file means "worth a look," not "confirmed bug."
  Plenty of `slow_call`s are just a provider having a slow moment.
- This is a **read-only, manual, on-demand** investigation — don't set up any automation, cron job, or
  recurring check as part of running this skill; that was explicitly ruled out when this feature was
  designed (detection is always-on and mechanical; the *analysis* stays human-initiated, every time).
- Don't delete anomaly files after reviewing them unless the user explicitly asks — `logs/llm.log` itself
  is a plain append-only file here (no size rotation, unlike Miranda's), but it does keep growing, and an
  anomaly file is the more targeted, permanent record of that specific turn either way.
