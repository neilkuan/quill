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

	"gopkg.in/yaml.v3"
)

// CopilotPicker reads sessions written by GitHub Copilot CLI.
//
// On-disk layout: ~/.copilot/session-state/<session-id>/ with
//   - workspace.yaml : session metadata
//   - events.jsonl   : full event log
//   - optional plan.md / checkpoints/ / files/
//
// The GitHub docs describe that these files exist but do not publish a
// field-level schema, so the YAML loader below accepts several common
// key names (`session_id` / `id`, `cwd` / `workdir`, `title` / `name`
// / `summary`) and falls back to the first user event in events.jsonl
// when workspace.yaml is absent or missing a title.
type CopilotPicker struct {
	// BaseDir overrides the default ~/.copilot/session-state path.
	// Mainly for tests; leave empty in production.
	BaseDir string
}

// NewCopilotPicker returns a picker that reads sessions from baseDir,
// or from the user's default Copilot CLI location when baseDir is empty.
func NewCopilotPicker(baseDir string) *CopilotPicker {
	return &CopilotPicker{BaseDir: baseDir}
}

func (c *CopilotPicker) AgentType() string { return "copilot" }

func (c *CopilotPicker) dir() (string, error) {
	if c.BaseDir != "" {
		return c.BaseDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".copilot", "session-state"), nil
}

func (c *CopilotPicker) List(cwd string, limit int) ([]Session, error) {
	baseDir, err := c.dir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", baseDir, err)
	}

	sessions := make([]Session, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sessionDir := filepath.Join(baseDir, e.Name())
		s, ok := loadCopilotSession(sessionDir, e.Name())
		if !ok {
			continue
		}
		if cwd != "" && s.CWD != cwd {
			continue
		}
		sessions = append(sessions, s)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}

// copilotWorkspace permissively captures the fields that common
// workspace.yaml layouts expose. Each field has multiple yaml aliases
// because the public docs do not pin down the exact schema.
type copilotWorkspace struct {
	SessionID  string    `yaml:"session_id"`
	ID         string    `yaml:"id"`
	SessionAlt string    `yaml:"sessionId"`
	CWD        string    `yaml:"cwd"`
	Workdir    string    `yaml:"workdir"`
	Workspace  string    `yaml:"workspace"`
	Title      string    `yaml:"title"`
	Name       string    `yaml:"name"`
	Summary    string    `yaml:"summary"`
	UpdatedAt  time.Time `yaml:"updated_at"`
	CreatedAt  time.Time `yaml:"created_at"`
}

func (w copilotWorkspace) id() string {
	for _, v := range []string{w.SessionID, w.ID, w.SessionAlt} {
		if v != "" {
			return v
		}
	}
	return ""
}

func (w copilotWorkspace) cwd() string {
	for _, v := range []string{w.CWD, w.Workdir, w.Workspace} {
		if v != "" {
			return v
		}
	}
	return ""
}

func (w copilotWorkspace) title() string {
	for _, v := range []string{w.Title, w.Name, w.Summary} {
		if v != "" {
			return v
		}
	}
	return ""
}

func loadCopilotSession(sessionDir, dirName string) (Session, bool) {
	fi, err := os.Stat(sessionDir)
	if err != nil {
		slog.Warn("copilot picker: stat failed", "path", sessionDir, "err", err)
		return Session{}, false
	}

	s := Session{
		ID:        dirName,
		UpdatedAt: fi.ModTime(),
	}

	if ws, ok := loadCopilotWorkspaceYAML(sessionDir); ok {
		if v := ws.id(); v != "" {
			s.ID = v
		}
		s.CWD = ws.cwd()
		s.Title = ws.title()
		// Prefer yaml's own timestamps over the dir mtime, which can be
		// touched by unrelated filesystem operations. updated_at wins
		// when present; otherwise fall back to created_at.
		if !ws.UpdatedAt.IsZero() {
			s.UpdatedAt = ws.UpdatedAt
		} else if !ws.CreatedAt.IsZero() {
			s.UpdatedAt = ws.CreatedAt
		}
	}

	// If workspace.yaml was missing or partial, mine events.jsonl for
	// whatever we still need (cwd from any event; title from the first
	// user message).
	if s.CWD == "" || s.Title == "" {
		fillFromCopilotEvents(sessionDir, &s)
	}

	if s.ID == "" {
		return Session{}, false
	}
	return s, true
}

func loadCopilotWorkspaceYAML(sessionDir string) (copilotWorkspace, bool) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("copilot picker: read workspace.yaml failed", "path", path, "err", err)
		}
		return copilotWorkspace{}, false
	}
	var ws copilotWorkspace
	if err := yaml.Unmarshal(raw, &ws); err != nil {
		slog.Warn("copilot picker: parse workspace.yaml failed", "path", path, "err", err)
		return copilotWorkspace{}, false
	}
	return ws, true
}

// fillFromCopilotEvents reads the first handful of events.jsonl lines
// to backfill CWD / Title when workspace.yaml did not supply them.
// Flexible about field names — different Copilot versions may label
// things differently.
func fillFromCopilotEvents(sessionDir string, s *Session) {
	path := filepath.Join(sessionDir, "events.jsonl")
	file, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("copilot picker: open events.jsonl failed", "path", path, "err", err)
		}
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for i := 0; i < titleScanLimit && scanner.Scan(); i++ {
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if s.CWD == "" {
			if v := firstNonEmpty(ev, "cwd", "workdir", "workspace"); v != "" {
				s.CWD = v
			}
		}
		if s.Title == "" {
			if isCopilotUserEvent(ev) {
				if v := extractCopilotText(ev); v != "" {
					s.Title = truncateRunes(strings.TrimSpace(stripQuillEnvelope(v)), 50)
				}
			}
		}
		if s.CWD != "" && s.Title != "" {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("copilot picker: scan error", "path", path, "err", err)
	}
}

func firstNonEmpty(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func isCopilotUserEvent(ev map[string]any) bool {
	for _, k := range []string{"type", "role", "kind", "event"} {
		if v, ok := ev[k].(string); ok && (v == "user" || v == "prompt" || v == "user_prompt") {
			return true
		}
	}
	// Some layouts nest the role under "message".
	if msg, ok := ev["message"].(map[string]any); ok {
		if r, ok := msg["role"].(string); ok && r == "user" {
			return true
		}
	}
	return false
}

func extractCopilotText(ev map[string]any) string {
	if v, ok := ev["text"].(string); ok && v != "" {
		return v
	}
	if v, ok := ev["prompt"].(string); ok && v != "" {
		return v
	}
	if v, ok := ev["content"].(string); ok && v != "" {
		return v
	}
	if msg, ok := ev["message"].(map[string]any); ok {
		if v, ok := msg["content"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
