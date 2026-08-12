package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCommandCatalog_EveryEntryIsUsable guards against a half-added
// command — the kind of thing printHelp would silently render with an
// empty/missing field rather than fail loudly.
func TestCommandCatalog_EveryEntryIsUsable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commandCatalog {
		require.NotEmpty(t, c.Name)
		require.False(t, seen[c.Name], "duplicate command name %q", c.Name)
		seen[c.Name] = true
		require.NotEmpty(t, c.Summary, "%s: missing Summary", c.Name)
		require.NotEmpty(t, c.Usage, "%s: missing Usage", c.Name)
		require.NotEmpty(t, c.Examples, "%s: missing at least one Example", c.Name)
		for _, ex := range c.Examples {
			require.NotEmpty(t, ex, "%s: empty Example string", c.Name)
		}
	}
}

// TestCommandCatalog_MatchesRunSwitch guards the other direction: every
// command run()'s switch actually dispatches must be documented here too
// — allProviderNames-style drift protection (see internal/ask/tools_test.go)
// against adding a case in main.go and forgetting the catalog.
func TestCommandCatalog_MatchesRunSwitch(t *testing.T) {
	dispatched := []string{
		"profile", "timeline", "document", "ask", "pipeline",
		"backfill-titles", "reindex-fts", "llm-trace",
	}
	for _, name := range dispatched {
		require.True(t, isKnownCommand(name), "run() dispatches %q but it's missing from commandCatalog", name)
	}
	require.Len(t, commandCatalog, len(dispatched), "commandCatalog has an entry run() never dispatches, or vice versa")
}

func TestIsKnownCommand(t *testing.T) {
	require.True(t, isKnownCommand("ask"))
	require.True(t, isKnownCommand("llm-trace"))
	require.False(t, isKnownCommand("bogus"))
	require.False(t, isKnownCommand(""))
}

func TestStyled_NoopWhenColorDisabled(t *testing.T) {
	orig := useColor
	defer func() { useColor = orig }()

	useColor = false
	require.Equal(t, "plain", styled(ansiBold, "plain"))
	require.Equal(t, "", colorStart(ansiCyan))
	require.Equal(t, "", colorEnd())

	useColor = true
	require.Equal(t, ansiBold+"plain"+ansiReset, styled(ansiBold, "plain"))
	require.Equal(t, ansiCyan, colorStart(ansiCyan))
	require.Equal(t, ansiReset, colorEnd())
}
