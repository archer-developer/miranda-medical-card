// This file implements medical-dev's own --help/no-args output: a
// hand-maintained catalog of every command (commandCatalog below) rendered
// as one table plus a per-command usage/example block, colorized when
// stdout looks like a real terminal. Hand-maintained rather than derived
// from each command's flag.FlagSet — those live scattered across main.go
// and llm_trace.go with no shared registration point, and flag.FlagSet's
// own -h output is already available per-command (every fs.Parse below
// uses flag.ExitOnError, which prints flag usage on a bad flag); this is
// the higher-level "what commands exist and how do I use them" view that's
// missing, not a replacement for per-flag usage.
package main

import "fmt"

// commandHelp is one entry in commandCatalog.
type commandHelp struct {
	Name     string
	Summary  string
	Usage    string   // full invocation pattern, e.g. "timeline --user <id> [--from YYYY-MM-DD]"
	Examples []string // full command lines, without the "medical-dev " prefix
}

// commandCatalog lists every medical-dev command in the same order
// run()'s switch does — kept in sync with it and with each runXxx
// function's flag.NewFlagSet by hand, same as this repo's other small
// pieces of intentional duplication (see e.g. cmd/medical-dev/main.go's
// llmLogPath comment) rather than building a registration abstraction for
// eight entries that changes maybe once a month.
var commandCatalog = []commandHelp{
	{
		Name:    "profile",
		Summary: "Show a user's Medical Profile (JSON)",
		Usage:   "profile --user <id>",
		Examples: []string{
			`profile --user alex`,
		},
	},
	{
		Name:    "timeline",
		Summary: "Show a user's Timeline, optionally filtered by date range or event type (JSON)",
		Usage:   "timeline --user <id> [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--type TYPE]",
		Examples: []string{
			`timeline --user alex`,
			`timeline --user alex --from 2026-01-01 --to 2026-06-30`,
			`timeline --user alex --type lab_result`,
		},
	},
	{
		Name:    "planned-actions",
		Summary: "Show a user's Planned Actions — future medical actions extracted from documents or logged directly in conversation, with due-date range and status (JSON)",
		Usage:   "planned-actions --user <id> [--include-resolved]",
		Examples: []string{
			`planned-actions --user alex`,
			`planned-actions --user alex --include-resolved`,
		},
	},
	{
		Name:    "document",
		Summary: "Show full internal details of one document — metadata, recognized text, Summary, extracted entities (JSON)",
		Usage:   "document <documentId> --user <id>",
		Examples: []string{
			`document doc_01J8EXAMPLE --user alex`,
		},
	},
	{
		Name:    "ask",
		Summary: "Run the medical.ask agent loop against a question; prints Question/Answer/Sources, traces to logs/llm.log when logging.level: debug",
		Usage:   `ask --user <id> "question"`,
		Examples: []string{
			`ask --user alex "Какие лекарства я сейчас принимаю?"`,
			`ask --user alex "Дай рекомендации по питанию на основании анализов"`,
		},
	},
	{
		Name:    "pipeline",
		Summary: "Re-run the Processing Pipeline on an already-imported document (OCR + Extraction + Normalization + ...), with full Debug-level tracing to stderr",
		Usage:   "pipeline <documentId> --user <id>",
		Examples: []string{
			`pipeline doc_01J8EXAMPLE --user alex`,
		},
	},
	{
		Name:    "backfill-titles",
		Summary: "One-off migration: backfill Document.Title from extraction.Schema's studyTitle for documents imported before it existed",
		Usage:   "backfill-titles --user <id> [--provider NAME]",
		Examples: []string{
			`backfill-titles --user alex`,
			`backfill-titles --user alex --provider gemini-agent`,
		},
	},
	{
		Name:    "reindex-fts",
		Summary: "One-off migration: rebuild a user's document FTS index from already-stored data (Summary/RecognizedText) — no LLM calls",
		Usage:   "reindex-fts --user <id>",
		Examples: []string{
			`reindex-fts --user alex`,
		},
	},
	{
		Name:    "llm-trace",
		Summary: "Analyze logs/llm.log as a per-turn table of what the model actually did — which tool, what args, what came back",
		Usage:   "llm-trace [--file PATH] [--conversation ID] [--latest] [--untagged]",
		Examples: []string{
			`llm-trace`,
			`llm-trace --latest`,
			`llm-trace --conversation session_2026_08_12_03`,
			`llm-trace --untagged`,
		},
	},
}

// isKnownCommand reports whether name is one of commandCatalog's — used by
// run() to reject a typo'd command before paying for config load and
// storage.Open, rather than failing on an unrelated-looking database error.
func isKnownCommand(name string) bool {
	for _, c := range commandCatalog {
		if c.Name == name {
			return true
		}
	}
	return false
}

// printHelp renders commandCatalog — a summary table first (for "what
// commands exist"), then one usage/examples block per command (for "how do
// I actually call this one") — colorized via styled/useColor (see color.go)
// when stdout looks like a real terminal.
func printHelp() {
	fmt.Println(styled(ansiBold, "medical-dev") + " — developer diagnostics for miranda-medical-card (docs/cli/medical_dev.md)")
	fmt.Println()
	fmt.Println(styled(ansiBold, "Usage:"))
	fmt.Println("  medical-dev <command> [flags]")
	fmt.Println()
	fmt.Println(styled(ansiBold, "Commands:"))

	nameWidth := 0
	for _, c := range commandCatalog {
		if len(c.Name) > nameWidth {
			nameWidth = len(c.Name)
		}
	}
	for _, c := range commandCatalog {
		// Color codes go outside the %-*s width directive — they take zero
		// visual columns but do count towards Go's padding width if left
		// inside it, which would misalign every row after the first.
		fmt.Printf("  %s%-*s%s  %s\n", colorStart(ansiCyan), nameWidth, c.Name, colorEnd(), c.Summary)
	}

	fmt.Println()
	fmt.Println("Run any command with no flags (or a wrong one) to see its own flag errors; full usage and examples below.")

	for _, c := range commandCatalog {
		fmt.Println()
		fmt.Println(styled(ansiBold+ansiCyan, c.Name))
		fmt.Println("  " + styled(ansiDim, "medical-dev ") + c.Usage)
		if len(c.Examples) > 0 {
			fmt.Println()
			for _, ex := range c.Examples {
				fmt.Println("    " + styled(ansiGreen, "medical-dev "+ex))
			}
		}
	}
}
