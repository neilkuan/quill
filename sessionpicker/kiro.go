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

// KiroPicker reads sessions from ~/.kiro/sessions/cli/*.json.
//
// Each session lives as a pair on disk: <uuid>.json holds the metadata
// (session_id, cwd, title, timestamps) and <uuid>.jsonl holds the
// conversation stream. Only the .json metadata file is consulted here.
type KiroPicker struct {
	// BaseDir overrides the default ~/.kiro/sessions/cli path. Mainly
	// for tests; leave empty in production.
	BaseDir string

	// AgentName, when non-empty, restricts List results to sessions
	// whose stored `agent_name` matches. Kiro binds each session to the
	// agent that created it and refuses `session/load` across agent
	// boundaries (returns Internal error), so listing mismatched
	// sessions in the picker would only produce unloadable entries.
	// Leave empty to show every session regardless of agent.
	AgentName string
}

// NewKiroPicker returns a picker that reads sessions from baseDir, or
// from the user's default Kiro location when baseDir is empty.
func NewKiroPicker(baseDir string) *KiroPicker {
	return &KiroPicker{BaseDir: baseDir}
}

func (k *KiroPicker) AgentType() string { return "kiro-cli" }

// kiroSessionFile mirrors the fields in ~/.kiro/sessions/cli/<uuid>.json
// that the picker cares about. Extra fields in the file are ignored.
type kiroSessionFile struct {
	SessionID    string            `json:"session_id"`
	CWD          string            `json:"cwd"`
	Title        string            `json:"title"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	SessionState kiroSessionStateF `json:"session_state"`
}

type kiroSessionStateF struct {
	// AgentName is the agent binding Kiro stored for this session.
	// Empty / absent means the session was created with Kiro's default
	// agent (which Kiro itself labels `kiro_default`).
	AgentName string `json:"agent_name"`
}

func (k *KiroPicker) dir() (string, error) {
	if k.BaseDir != "" {
		return k.BaseDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".kiro", "sessions", "cli"), nil
}

func (k *KiroPicker) List(cwd string, limit int) ([]Session, error) {
	dir, err := k.dir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	sessions := make([]Session, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, meta, ok := loadKiroSession(path)
		if !ok {
			continue
		}
		if cwd != "" && s.CWD != cwd {
			continue
		}
		if k.AgentName != "" && !kiroAgentMatches(meta.AgentName, k.AgentName) {
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

// kiroMeta surfaces Kiro-specific fields that the picker wants for
// filtering but does not expose through the cross-agent Session struct.
type kiroMeta struct {
	AgentName string
}

// kiroAgentMatches decides whether a session's stored agent_name is
// acceptable for a picker configured with wantAgent. The empty /
// missing stored value is treated as `kiro_default` — Kiro's
// unparameterised default — so picker configs that explicitly target
// the default still match legacy sessions that never recorded an
// agent_name.
func kiroAgentMatches(storedAgent, wantAgent string) bool {
	if storedAgent == "" {
		storedAgent = "kiro_default"
	}
	return storedAgent == wantAgent
}

func loadKiroSession(path string) (Session, kiroMeta, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("kiro picker: read session file failed", "path", path, "err", err)
		return Session{}, kiroMeta{}, false
	}
	var f kiroSessionFile
	if err := json.Unmarshal(raw, &f); err != nil {
		slog.Warn("kiro picker: parse session file failed", "path", path, "err", err)
		return Session{}, kiroMeta{}, false
	}
	if f.SessionID == "" {
		return Session{}, kiroMeta{}, false
	}
	updated := f.UpdatedAt
	if updated.IsZero() {
		updated = f.CreatedAt
	}

	title := strings.TrimSpace(stripQuillEnvelope(f.Title))
	if looksLikeTruncatedSenderContext(f.Title) || title == "" {
		// Kiro truncates the title to the first N characters of the
		// stored prompt. When quill is the client, those first N chars
		// are the sender_context envelope opening tag — useless as a
		// title. Fall back to scanning the conversation stream.
		if recovered, ok := recoverKiroTitleFromJSONL(strings.TrimSuffix(path, ".json") + ".jsonl"); ok {
			title = recovered
		}
	}

	return Session{
		ID:        f.SessionID,
		Title:     title,
		CWD:       f.CWD,
		UpdatedAt: updated,
	}, kiroMeta{AgentName: f.SessionState.AgentName}, true
}

// looksLikeTruncatedSenderContext catches cases where Kiro only stored
// the opening tag or part of it (e.g. "<sender_con…") so the
// stripQuillEnvelope helper alone cannot recover a useful title.
func looksLikeTruncatedSenderContext(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<") {
		return false
	}
	// Literal prefix of the opening tag; anything shorter or starting
	// with the same characters is still inside the envelope.
	return strings.HasPrefix(t, "<sender_context") || strings.HasPrefix(t, "<sender_con")
}

// kiroJSONLScanLimit caps how many conversation-stream lines we read
// while hunting for a usable title. The real user prompt is typically
// the first Prompt event, so this is generous.
const kiroJSONLScanLimit = 32

// recoverKiroTitleFromJSONL reads the sibling <uuid>.jsonl file and
// returns the first real user prompt (sender_context envelope removed,
// trimmed to 50 runes). Returns ("", false) when no usable line is
// found — the caller should then leave the title blank rather than
// invent one.
func recoverKiroTitleFromJSONL(jsonlPath string) (string, bool) {
	file, err := os.Open(jsonlPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("kiro picker: open jsonl failed", "path", jsonlPath, "err", err)
		}
		return "", false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	type kiroJSONLEvent struct {
		Kind string `json:"kind"`
		Data struct {
			Content []struct {
				Kind string `json:"kind"`
				Data string `json:"data"`
			} `json:"content"`
		} `json:"data"`
	}

	for i := 0; i < kiroJSONLScanLimit && scanner.Scan(); i++ {
		var ev kiroJSONLEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Kind != "Prompt" {
			continue
		}
		for _, c := range ev.Data.Content {
			if c.Kind != "text" || c.Data == "" {
				continue
			}
			text := strings.TrimSpace(stripQuillEnvelope(c.Data))
			if text == "" {
				continue
			}
			return truncateKiroTitle(text), true
		}
	}
	return "", false
}

// truncateKiroTitle shortens text to 50 runes, appending an ellipsis
// when it was cut. Matches the convention the other pickers use.
func truncateKiroTitle(s string) string {
	const max = 50
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
