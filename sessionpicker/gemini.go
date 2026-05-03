package sessionpicker

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GeminiPicker reads sessions written by gemini-cli's ACP mode.
//
// On-disk layout: <baseDir>/<project-tmp-id>/chats/session-<TS>-<id8>.jsonl,
// where <baseDir> is ~/.gemini/tmp by default. Each chats/ directory belongs
// to one project; gemini-cli derives the directory name either via a project
// registry (newer versions) or via sha256(cwd) (legacy). Because that mapping
// is opaque from quill's side, the picker walks every project directory and
// filters by the per-session `projectHash` field stored inside the file —
// `projectHash` is always sha256(cwd) regardless of how the parent directory
// was named.
//
// Each .jsonl file is an event stream:
//   - Optional initial metadata line (Partial<ConversationRecord>):
//     {"sessionId":"...","projectHash":"...","startTime":"...","lastUpdated":"..."}
//   - Followed by message records (`{"id":..,"timestamp":..,"type":"user"|"gemini",..}`),
//     `$set` metadata updates, and `$rewindTo` markers (ignored here).
//
// Subagent sessions (`kind:"subagent"`) are implementation details of tool
// calls and are skipped, mirroring the upstream session browser.
type GeminiPicker struct {
	// BaseDir overrides the default ~/.gemini/tmp path. Mainly for tests.
	BaseDir string
}

// NewGeminiPicker returns a picker that reads sessions from baseDir, or
// from the user's default ~/.gemini/tmp location when baseDir is empty.
func NewGeminiPicker(baseDir string) *GeminiPicker {
	return &GeminiPicker{BaseDir: baseDir}
}

func (g *GeminiPicker) AgentType() string { return "gemini" }

func (g *GeminiPicker) dir() (string, error) {
	if g.BaseDir != "" {
		return g.BaseDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".gemini", "tmp"), nil
}

// geminiProjectHash mirrors gemini-cli's getProjectHash: sha256 of the
// absolute project root, hex-encoded. Used to match a session file's
// embedded `projectHash` against the caller's cwd filter.
func geminiProjectHash(cwd string) string {
	if cwd == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:])
}

func (g *GeminiPicker) List(cwd string, limit int) ([]Session, error) {
	baseDir, err := g.dir()
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

	wantedHash := geminiProjectHash(cwd)

	sessions := make([]Session, 0)
	for _, d := range projectDirs {
		if !d.IsDir() {
			continue
		}
		chatsDir := filepath.Join(baseDir, d.Name(), "chats")
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("gemini picker: read chats dir failed", "path", chatsDir, "err", err)
			}
			continue
		}
		for _, f := range entries {
			if f.IsDir() || !isGeminiSessionFile(f.Name()) {
				continue
			}
			path := filepath.Join(chatsDir, f.Name())
			loaded, ok := loadGeminiSession(path, f)
			if !ok {
				continue
			}
			if cwd != "" && !geminiMatchesCWD(loaded, cwd, wantedHash) {
				continue
			}
			sessions = append(sessions, loaded.toSession(cwd))
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

// isGeminiSessionFile reports whether name matches gemini-cli's
// `session-<timestamp>-<id8>.json{,l}` naming. Subagent files live under
// a separate `chats/<parent-id>/` subdirectory and are filtered out by
// the IsDir() check upstream.
func isGeminiSessionFile(name string) bool {
	if !strings.HasPrefix(name, "session-") {
		return false
	}
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json")
}

// geminiSessionMeta holds the fields the picker needs to render a row.
// Mirrors the subset of ConversationRecord we read from the JSONL stream.
type geminiSessionMeta struct {
	SessionID   string   `json:"sessionId"`
	ProjectHash string   `json:"projectHash"`
	StartTime   string   `json:"startTime"`
	LastUpdated string   `json:"lastUpdated"`
	Summary     string   `json:"summary"`
	Directories []string `json:"directories"`
	Kind        string   `json:"kind"`
}

// geminiMessage captures only the fields we need from a per-line message
// record so we can extract the first user prompt as a title fallback.
type geminiMessage struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Content   json.RawMessage `json:"content"`
}

// geminiMetadataUpdate is the `{"$set":{...}}` envelope gemini-cli writes
// when it amends earlier metadata mid-stream. We merge $set into our
// running meta so the latest sessionId/projectHash/lastUpdated wins.
type geminiMetadataUpdate struct {
	Set *geminiSessionMeta `json:"$set"`
}

// loadedGeminiSession bundles the parsed metadata + the picker-display
// fields. Kept internal so List can do its cwd filter against the rich
// metadata before collapsing into a sessionpicker.Session.
type loadedGeminiSession struct {
	Meta             geminiSessionMeta
	UpdatedAt        time.Time
	FirstUserMessage string
}

func (l loadedGeminiSession) toSession(filterCWD string) Session {
	cwd := ""
	if len(l.Meta.Directories) > 0 {
		// First entry is the initial workspace root; later entries are
		// from `/dir add`. Use [0] so the displayed cwd matches what
		// the session was started against.
		cwd = l.Meta.Directories[0]
	}
	// When the caller filtered by cwd and the session has no
	// `directories` field but matched via projectHash, surface the
	// requested cwd so the picker row shows where the session ran.
	if cwd == "" && filterCWD != "" {
		cwd = filterCWD
	}
	title := strings.TrimSpace(geminiTitle(l.Meta, l.FirstUserMessage))
	return Session{
		ID:        l.Meta.SessionID,
		Title:     truncateRunes(title, 50),
		CWD:       cwd,
		UpdatedAt: l.UpdatedAt,
	}
}

func loadGeminiSession(path string, f os.DirEntry) (loadedGeminiSession, bool) {
	fi, err := f.Info()
	if err != nil {
		slog.Warn("gemini picker: stat failed", "path", path, "err", err)
		return loadedGeminiSession{}, false
	}

	file, err := os.Open(path)
	if err != nil {
		slog.Warn("gemini picker: open failed", "path", path, "err", err)
		return loadedGeminiSession{}, false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Sessions can include large tool outputs; bump the buffer so
	// Scanner does not choke on long lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var meta geminiSessionMeta
	var firstUserMessage string

	for i := 0; i < geminiScanLimit && scanner.Scan(); i++ {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}

		// Try metadata-update envelope first so $set wins over the
		// initial metadata line if both appear early.
		var upd geminiMetadataUpdate
		if err := json.Unmarshal(raw, &upd); err == nil && upd.Set != nil {
			mergeGeminiMeta(&meta, upd.Set)
			continue
		}

		// $rewindTo entries are noise for the picker — skip explicitly
		// to avoid being mistaken for partial metadata.
		if isGeminiRewind(raw) {
			continue
		}

		// Partial metadata line (the initial header gemini writes).
		var partial geminiSessionMeta
		if err := json.Unmarshal(raw, &partial); err == nil && hasGeminiMetaFields(partial) {
			mergeGeminiMeta(&meta, &partial)
			continue
		}

		// Otherwise treat as a message record and harvest the first
		// user prompt as a title fallback.
		if firstUserMessage == "" {
			var m geminiMessage
			if err := json.Unmarshal(raw, &m); err == nil && m.Type == "user" {
				if text := decodeGeminiContent(m.Content); text != "" {
					firstUserMessage = stripQuillEnvelope(text)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("gemini picker: scan error", "path", path, "err", err)
	}

	if meta.Kind == "subagent" {
		// Subagent transcripts belong to a parent session and should
		// not be resumable from the picker.
		return loadedGeminiSession{}, false
	}
	if meta.SessionID == "" {
		return loadedGeminiSession{}, false
	}

	updated := parseGeminiTime(meta.LastUpdated)
	if updated.IsZero() {
		updated = parseGeminiTime(meta.StartTime)
	}
	if updated.IsZero() {
		updated = fi.ModTime()
	}

	return loadedGeminiSession{
		Meta:             meta,
		UpdatedAt:        updated,
		FirstUserMessage: firstUserMessage,
	}, true
}

// geminiScanLimit caps how many JSONL lines we read while hunting for
// metadata + the first user message. Initial metadata + a handful of
// $set lines + the first user prompt all land in the first few hundred
// records of even very long sessions; capping protects against giant
// tool-output messages.
const geminiScanLimit = 256

// hasGeminiMetaFields reports whether m carries at least one field that
// only appears on metadata lines. Stops us from misclassifying random
// JSON objects as metadata when they unmarshal cleanly into the struct
// (e.g. a `{"id":"..."}` message would otherwise produce an empty meta).
func hasGeminiMetaFields(m geminiSessionMeta) bool {
	return m.SessionID != "" || m.ProjectHash != "" || m.StartTime != "" ||
		m.LastUpdated != "" || m.Summary != "" || m.Kind != "" ||
		len(m.Directories) > 0
}

// mergeGeminiMeta merges non-empty fields from src into dst. Treats
// empty strings / empty slices as "absent" so a $set line that only
// updates lastUpdated does not blank out sessionId.
func mergeGeminiMeta(dst, src *geminiSessionMeta) {
	if src == nil {
		return
	}
	if src.SessionID != "" {
		dst.SessionID = src.SessionID
	}
	if src.ProjectHash != "" {
		dst.ProjectHash = src.ProjectHash
	}
	if src.StartTime != "" {
		dst.StartTime = src.StartTime
	}
	if src.LastUpdated != "" {
		dst.LastUpdated = src.LastUpdated
	}
	if src.Summary != "" {
		dst.Summary = src.Summary
	}
	if len(src.Directories) > 0 {
		dst.Directories = src.Directories
	}
	if src.Kind != "" {
		dst.Kind = src.Kind
	}
}

// geminiTitle picks the best display title: AI-generated summary first,
// then the first user message. The caller truncates.
func geminiTitle(meta geminiSessionMeta, firstUserMessage string) string {
	if s := strings.TrimSpace(meta.Summary); s != "" {
		return s
	}
	return firstUserMessage
}

// decodeGeminiContent handles the `PartListUnion` shape gemini-cli writes:
// either a plain string, a single `{type:"text", text:"..."}` object, or
// an array of such parts. Returns the first text span.
func decodeGeminiContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	type part struct {
		Text string `json:"text"`
	}
	var single part
	if err := json.Unmarshal(raw, &single); err == nil && single.Text != "" {
		return single.Text
	}
	var arr []part
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, p := range arr {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// parseGeminiTime accepts the ISO-8601 timestamps gemini writes
// ("2025-08-31T09:58:17.629Z"). Returns the zero time on parse failure.
func parseGeminiTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}

// isGeminiRewind cheaply detects `{"$rewindTo":...}` records without a
// full unmarshal. The picker discards them — they roll back the message
// stream but never carry session metadata.
func isGeminiRewind(raw []byte) bool {
	return strings.Contains(string(raw), `"$rewindTo"`)
}

// geminiMatchesCWD returns true when the session's stored projectHash or
// directories prove it ran in cwd. Strict on projectHash (sha256(cwd)),
// permissive on directories (any entry equal to cwd is enough — covers
// sessions that added cwd via `/dir add` partway through).
func geminiMatchesCWD(l loadedGeminiSession, cwd, wantedHash string) bool {
	if wantedHash != "" && l.Meta.ProjectHash == wantedHash {
		return true
	}
	for _, d := range l.Meta.Directories {
		if d == cwd {
			return true
		}
	}
	return false
}
