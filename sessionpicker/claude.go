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

// ClaudePicker reads sessions written by @agentclientprotocol/claude-agent-acp.
//
// On-disk layout: ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl,
// where <encoded-cwd> replaces every non-alphanumeric rune in the
// absolute cwd with '-'. Each JSONL file is an event stream; the first
// line is typically a file-history-snapshot followed by user/assistant
// messages, each carrying its own cwd / sessionId / timestamp.
type ClaudePicker struct {
	// BaseDir overrides the default ~/.claude/projects path. Mainly
	// for tests; leave empty in production.
	BaseDir string
}

// NewClaudePicker returns a picker that reads sessions from baseDir, or
// from the user's default Claude projects location when baseDir is empty.
func NewClaudePicker(baseDir string) *ClaudePicker {
	return &ClaudePicker{BaseDir: baseDir}
}

func (c *ClaudePicker) AgentType() string { return "claude-agent-acp" }

func (c *ClaudePicker) dir() (string, error) {
	if c.BaseDir != "" {
		return c.BaseDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// encodeClaudeCWD mirrors the directory-naming scheme claude-agent-acp
// uses under ~/.claude/projects: every non-ASCII-alphanumeric rune
// (including '/', '.', '_') becomes '-'. So both "/Users/me/proj" and
// "/Users/me/.proj" collapse into "-Users-me--proj" style names.
func encodeClaudeCWD(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (c *ClaudePicker) List(cwd string, limit int) ([]Session, error) {
	baseDir, err := c.dir()
	if err != nil {
		return nil, err
	}

	projectDirs, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", baseDir, err)
	}

	var wantedDir string
	if cwd != "" {
		wantedDir = encodeClaudeCWD(cwd)
	}

	sessions := make([]Session, 0)
	for _, d := range projectDirs {
		if !d.IsDir() {
			continue
		}
		if wantedDir != "" && d.Name() != wantedDir {
			continue
		}
		projectPath := filepath.Join(baseDir, d.Name())
		files, err := os.ReadDir(projectPath)
		if err != nil {
			slog.Warn("claude picker: read project dir failed", "path", projectPath, "err", err)
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(projectPath, f.Name())
			s, ok := loadClaudeSession(path, f)
			if !ok {
				continue
			}
			// When filtering by cwd, prefer the value embedded in the
			// session itself if available (more authoritative than the
			// lossy directory name).
			if cwd != "" && s.CWD != "" && s.CWD != cwd {
				continue
			}
			sessions = append(sessions, s)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}

// claudeEvent picks up just the fields we need from each JSONL line;
// everything else is ignored.
type claudeEvent struct {
	Type      string          `json:"type"`
	IsMeta    bool            `json:"isMeta"`
	CWD       string          `json:"cwd"`
	SessionID string          `json:"sessionId"`
	Timestamp time.Time       `json:"timestamp"`
	Message   claudeMessage   `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// titleScanLimit caps how many JSONL lines we read while hunting for a
// usable title — real sessions expose a good first prompt well before
// this, and it keeps us from slurping multi-megabyte files.
const titleScanLimit = 64

func loadClaudeSession(path string, f os.DirEntry) (Session, bool) {
	fi, err := f.Info()
	if err != nil {
		slog.Warn("claude picker: stat failed", "path", path, "err", err)
		return Session{}, false
	}

	file, err := os.Open(path)
	if err != nil {
		slog.Warn("claude picker: open failed", "path", path, "err", err)
		return Session{}, false
	}
	defer file.Close()

	s := Session{
		ID:        strings.TrimSuffix(f.Name(), ".jsonl"),
		UpdatedAt: fi.ModTime(),
	}

	scanner := bufio.NewScanner(file)
	// Claude prompts can include large tool outputs; bump the buffer so
	// Scanner does not choke on long lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for i := 0; i < titleScanLimit && scanner.Scan(); i++ {
		var ev claudeEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if s.CWD == "" && ev.CWD != "" {
			s.CWD = ev.CWD
		}
		if ev.SessionID != "" && s.ID == "" {
			// Should not happen (filename is authoritative) but keep
			// the embedded id as a fallback.
			s.ID = ev.SessionID
		}
		if s.Title == "" {
			if t := extractClaudeTitle(ev); t != "" {
				s.Title = t
			}
		}
		if s.Title != "" && s.CWD != "" {
			break
		}
	}
	// Read errors mid-file are non-fatal — we already have whatever we
	// could parse; just log so they do not disappear silently.
	if err := scanner.Err(); err != nil {
		slog.Warn("claude picker: scan error", "path", path, "err", err)
	}

	if s.ID == "" {
		return Session{}, false
	}
	return s, true
}

// extractClaudeTitle returns the first user-facing prompt text in ev,
// or "" if ev is not a usable title source. Slash-command wrappers and
// meta messages are deliberately skipped so the picker surfaces what a
// human would recognise.
func extractClaudeTitle(ev claudeEvent) string {
	if ev.Type != "user" || ev.IsMeta {
		return ""
	}
	if ev.Message.Role != "" && ev.Message.Role != "user" {
		return ""
	}

	text := decodeClaudeContent(ev.Message.Content)
	if text == "" {
		return ""
	}
	if isClaudeCommandWrapper(text) {
		return ""
	}
	// Peel off quill's `<sender_context>...</sender_context>` preamble
	// so titles reflect what the user typed, not the metadata envelope
	// quill prepends to every prompt.
	text = stripQuillEnvelope(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return truncateRunes(text, 50)
}

// decodeClaudeContent handles both shapes Claude writes: a plain string,
// or an array of content blocks where we pluck the first "text" block.
func decodeClaudeContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// String form.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	// List-of-blocks form.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// claudeCommandWrapperPrefixes lists the opening tags Claude Code emits
// around user content when a slash command or local command was used.
// These envelopes are not meaningful titles and are skipped by the
// picker. New wrappers can be added here without touching call sites.
var claudeCommandWrapperPrefixes = []string{
	"<command-name>",
	"<command-message>",
	"<command-args>",
	"<local-command-caveat>",
	"<local-command-stdout>",
	"<local-command-stderr>",
}

// isClaudeCommandWrapper reports whether text is a slash/local command
// envelope rather than a real user prompt. Detection is by exact
// opening tag at the start of the trimmed content — a free-form prompt
// that merely mentions "command-name" somewhere inside is not a wrapper.
func isClaudeCommandWrapper(text string) bool {
	t := strings.TrimSpace(text)
	for _, prefix := range claudeCommandWrapperPrefixes {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// truncateRunes trims s to at most n runes without cutting a multi-byte
// character in half, appending "..." when truncation happened.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
