package mcpserver

import "time"

// formatOptionalDate formats t as YYYY-MM-DD, or "" if t is nil — shared by
// every handler that surfaces an optional domain date through MCP JSON.
func formatOptionalDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}
