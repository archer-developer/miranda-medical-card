// Minimal, dependency-free ANSI color support for printHelp (help.go) —
// no new module dependency for what's a handful of escape codes around
// plain-text output; a real terminal-capabilities library would be
// overkill for a CLI whose only colorized output is its own help text.
package main

import "os"

const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"
	ansiDim   = "\033[2m"
	ansiCyan  = "\033[36m"
	ansiGreen = "\033[32m"
)

// useColor is decided once at startup rather than per-call: NO_COLOR
// (https://no-color.org — respected the same way regardless of value, per
// that spec) or TERM=dumb disables it outright; otherwise it's on only
// when stdout is an actual terminal, not a pipe/redirect/file — piping
// medical-dev's output to a file or another command (e.g. grep) must not
// embed escape codes a reader wouldn't expect.
var useColor = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && isTerminal(os.Stdout)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// styled wraps s in codes (e.g. ansiBold+ansiCyan) when useColor, or
// returns s unchanged otherwise — for coloring a whole string in place.
func styled(codes, s string) string {
	if !useColor {
		return s
	}
	return codes + s + ansiReset
}

// colorStart/colorEnd bracket a fmt.Printf field that also needs Go's own
// %-*s width padding (see printHelp's command table) — codes must sit
// outside the width directive, since they'd otherwise count towards the
// padded width despite taking zero visual columns.
func colorStart(codes string) string {
	if !useColor {
		return ""
	}
	return codes
}

func colorEnd() string {
	if !useColor {
		return ""
	}
	return ansiReset
}
