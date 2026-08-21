package ask

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/archer-developer/miranda-llm/llmtrace/analyze"
	"github.com/archer-developer/miranda-llm/llmtrace/anomaly"
)

// AnomalyConfig configures per-turn anomaly detection (see agent_loop.go's
// Ask, which attaches an anomaly.Recorder to the turn's ctx and calls
// reportAnomalies below once it ends). The zero value disables the feature
// entirely — set via SetAnomalyConfig, mirroring
// cmd/miranda-medical-card/main.go's own debug-only gating of llm.log
// itself: a Recorder would never receive any Trace call anyway when no
// tracer is installed on the router at all, so there is nothing to detect
// with logging.level below debug.
type AnomalyConfig struct {
	// LLMLogPath is llm.log's own path, re-read at anomaly time via
	// analyze.ParseAll to recover the whole conversation's blocks for the
	// anomaly file, not just this one turn's — diagnosing a bad turn often
	// needs the history that led up to it. Best-effort: if the read fails,
	// or the call has no sessionID to filter by, the anomaly file falls back
	// to just this turn's own blocks.
	LLMLogPath string
	// Dir is where an anomalous turn's blocks get written, one file per
	// anomalous turn (see llmtrace/anomaly.FileName) — created lazily on
	// first use.
	Dir string
}

func (c AnomalyConfig) enabled() bool { return c.Dir != "" }

// SetAnomalyConfig enables (or, with the zero value, disables) per-turn
// anomaly detection. Separate from NewAsker so every existing construction
// call site — production and test alike — keeps working unchanged with the
// feature off by default.
func (a *Asker) SetAnomalyConfig(cfg AnomalyConfig) {
	a.anomaly = cfg
}

// reportAnomalies runs anomaly.Detect over one turn's recorded blocks and,
// if it finds anything, writes the fuller conversation context (see
// AnomalyConfig.LLMLogPath) to a new file under AnomalyConfig.Dir and logs
// exactly one WARNING to the general app logger — never the full trace
// itself, that's what the file is for. A no-op when anomaly detection is
// disabled or recorder is nil (see Ask).
func (a *Asker) reportAnomalies(sessionID string, recorder *anomaly.Recorder, outcome anomaly.Outcome) {
	if recorder == nil {
		return
	}

	found := anomaly.Detect(recorder.Blocks(), recorder.Durations(), outcome, anomaly.Options{})
	if len(found) == 0 {
		return
	}

	blocks := recorder.Blocks()
	if sessionID != "" {
		all, err := readLLMLog(a.anomaly.LLMLogPath)
		if err != nil {
			a.logger.Warn("ask: re-reading llm.log for anomaly context failed, writing turn-only blocks", "error", err)
		} else if conv := analyze.ConversationBlocks(all, sessionID); len(conv) > 0 {
			blocks = conv
		}
	}

	path, err := writeAnomalyFile(a.anomaly.Dir, found, blocks)
	if err != nil {
		a.logger.Warn("ask: failed to write anomaly file", "error", err)
		return
	}
	a.logger.Warn("ask: turn had anomalies", "sessionId", sessionID, "kinds", anomalyKinds(found), "file", path)
}

func readLLMLog(path string) ([]analyze.Block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ask: open llm log: %w", err)
	}
	defer f.Close()
	return analyze.ParseAll(f)
}

func writeAnomalyFile(dir string, found []anomaly.Anomaly, blocks []analyze.Block) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("ask: create anomalies dir: %w", err)
	}
	path := filepath.Join(dir, anomaly.FileName(time.Now(), found))
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("ask: create anomaly file: %w", err)
	}
	defer f.Close()
	if err := anomaly.WriteFile(f, found, blocks); err != nil {
		return "", err
	}
	return path, nil
}

func anomalyKinds(found []anomaly.Anomaly) []string {
	seen := map[string]bool{}
	var kinds []string
	for _, a := range found {
		if !seen[a.Kind] {
			seen[a.Kind] = true
			kinds = append(kinds, a.Kind)
		}
	}
	return kinds
}
