package sessionpicker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CodexPicker reads sessions indexed by OpenAI Codex CLI in
// ~/.codex/history.jsonl.
//
// Unlike the other three backends, Codex keeps a flat, append-only
// index file rather than per-session directories: each line records a
// single prompt as {"session_id": ..., "ts": unix_seconds, "text": ...}.
// One session may therefore span many lines. The picker groups by
// session_id, keeping the earliest text as the title and the latest ts
// as UpdatedAt.
//
// Limitation: the history entries carry no cwd, so cwd filtering is
// impossible — passing a non-empty cwd returns no results rather than
// silently ignoring the filter, to avoid misleading a caller who
// believed they were scoping results.
type CodexPicker struct {
	// HistoryPath overrides the default ~/.codex/history.jsonl.
	// Mainly for tests; leave empty in production.
	HistoryPath string
}

// NewCodexPicker returns a picker that reads sessions from historyPath,
// or from the user's default Codex location when historyPath is empty.
func NewCodexPicker(historyPath string) *CodexPicker {
	return &CodexPicker{HistoryPath: historyPath}
}

func (c *CodexPicker) AgentType() string { return "codex-acp" }

func (c *CodexPicker) path() (string, error) {
	if c.HistoryPath != "" {
		return c.HistoryPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".codex", "history.jsonl"), nil
}

type codexHistoryEntry struct {
	SessionID string `json:"session_id"`
	TS        int64  `json:"ts"`
	Text      string `json:"text"`
}

func (c *CodexPicker) List(cwd string, limit int) ([]Session, error) {
	// Codex entries lack cwd; an explicit cwd filter can never match.
	if cwd != "" {
		return nil, nil
	}

	historyPath, err := c.path()
	if err != nil {
		return nil, err
	}

	file, err := os.Open(historyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", historyPath, err)
	}
	defer file.Close()

	type agg struct {
		firstTS   int64
		lastTS    int64
		title     string
		msgCount  int
	}
	bySession := make(map[string]*agg)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e codexHistoryEntry
		if err := json.Unmarshal(line, &e); err != nil {
			slog.Warn("codex picker: parse history line failed", "err", err)
			continue
		}
		if e.SessionID == "" {
			continue
		}
		a, ok := bySession[e.SessionID]
		if !ok {
			a = &agg{firstTS: e.TS, lastTS: e.TS, title: e.Text}
			bySession[e.SessionID] = a
		}
		a.msgCount++
		if e.TS < a.firstTS {
			a.firstTS = e.TS
			a.title = e.Text
		}
		if e.TS > a.lastTS {
			a.lastTS = e.TS
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("codex picker: scan error", "path", historyPath, "err", err)
	}

	sessions := make([]Session, 0, len(bySession))
	for id, a := range bySession {
		sessions = append(sessions, Session{
			ID:           id,
			Title:        truncateRunes(strings.TrimSpace(a.title), 50),
			UpdatedAt:    time.Unix(a.lastTS, 0),
			MessageCount: a.msgCount,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}
