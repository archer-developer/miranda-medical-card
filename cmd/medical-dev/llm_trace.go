// This file implements `medical-dev llm-trace` — a reader for logs/llm.log
// (see internal/llmtrace's block format: one "=== <time> provider=<name>
// [conversation=<id>] ===" section per Provider.Chat/Structured call, with
// the exact request/response each provider sent, added by
// cmd/miranda-medical-card/main.go's buildLLMTraceWriter when
// logging.level is debug). Originally a standing command built around
// manually reconstructing this same step-by-step table by hand (scp the
// log, write a throwaway parser) while debugging a medical.ask "exceeded N
// tool-call iterations" failure; the actual parsing/analysis logic has
// since moved to miranda-llm/llmtrace/analyze so Miranda's own web UI and
// CLI can reuse it instead of maintaining a second implementation — this
// file is now just flag parsing plus a thin call into that package.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/archer-developer/miranda-llm/llmtrace/analyze"
)

func runLLMTrace(args []string) error {
	fs := flag.NewFlagSet("llm-trace", flag.ExitOnError)
	file := fs.String("file", llmLogPath, "path to the LLM trace log")
	conversation := fs.String("conversation", "", "show every turn of this conversation id")
	latest := fs.Bool("latest", false, "show every turn of the most recently started conversation")
	untagged := fs.Bool("untagged", false, "show the most recent untagged (stateless-session) turns — see analyze.RenderUntagged")
	if err := fs.Parse(args); err != nil {
		return err
	}

	f, err := os.Open(*file)
	if err != nil {
		return fmt.Errorf("open trace log %s: %w", *file, err)
	}
	defer f.Close()

	blocks, err := analyze.ParseAll(f)
	if err != nil {
		return fmt.Errorf("parse trace log %s: %w", *file, err)
	}

	if *untagged {
		const untaggedTail = 20
		return analyze.RenderUntagged(os.Stdout, blocks, untaggedTail)
	}

	if *latest {
		id := analyze.LatestConversation(blocks)
		if id == "" {
			return fmt.Errorf("no conversation-tagged blocks found in %s", *file)
		}
		*conversation = id
	}

	if *conversation == "" {
		analyze.RenderConversationSummary(os.Stdout, blocks)
		return nil
	}
	return analyze.RenderConversationDetail(os.Stdout, blocks, *conversation)
}
