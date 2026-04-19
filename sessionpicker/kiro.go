package sessionpicker

import (
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
	SessionID string    `json:"session_id"`
	CWD       string    `json:"cwd"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
		s, ok := loadKiroSession(path)
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

func loadKiroSession(path string) (Session, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("kiro picker: read session file failed", "path", path, "err", err)
		return Session{}, false
	}
	var f kiroSessionFile
	if err := json.Unmarshal(raw, &f); err != nil {
		slog.Warn("kiro picker: parse session file failed", "path", path, "err", err)
		return Session{}, false
	}
	if f.SessionID == "" {
		return Session{}, false
	}
	updated := f.UpdatedAt
	if updated.IsZero() {
		updated = f.CreatedAt
	}
	return Session{
		ID:        f.SessionID,
		Title:     f.Title,
		CWD:       f.CWD,
		UpdatedAt: updated,
	}, true
}
