package command

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/sessionpicker"
)

const (
	CmdSessions = "sessions"
	CmdReset    = "reset"
	CmdResume   = "resume"
	CmdInfo     = "info"
	CmdStop     = "stop"
	CmdPicker   = "session-picker"
)

type Command struct {
	Name string
	Args string
}

// ParseCommand checks if text is a known bot command.
func ParseCommand(text string) (*Command, bool) {
	text = strings.TrimSpace(text)
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return nil, false
	}

	name := strings.ToLower(parts[0])
	// "cancel" is an alias for "stop" — same ACP intent (session/cancel).
	if name == "cancel" {
		name = CmdStop
	}
	// "history" / "pick" are aliases for "session-picker", for people
	// who prefer a shorter phrase at the chat prompt.
	if name == "history" || name == "pick" {
		name = CmdPicker
	}
	known := map[string]bool{
		CmdSessions: true, CmdReset: true, CmdResume: true, CmdInfo: true, CmdStop: true, CmdPicker: true,
	}
	if !known[name] {
		return nil, false
	}

	args := ""
	if len(parts) > 1 {
		args = strings.Join(parts[1:], " ")
	}
	return &Command{Name: name, Args: args}, true
}

// ExecuteSessions returns a formatted list of all active sessions.
func ExecuteSessions(pool *acp.SessionPool) string {
	sessions := pool.ListSessions()
	active, max := pool.Stats()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Active Sessions: %d/%d**\n\n", active, max))

	if len(sessions) == 0 {
		sb.WriteString("No active sessions.")
		return sb.String()
	}

	for i, s := range sessions {
		status := "alive"
		if !s.Alive {
			status = "dead"
		}
		sb.WriteString(fmt.Sprintf(
			"`%s` [%s]\n"+
				"  created %s ago | active %s ago | msgs: %d",
			s.ThreadKey,
			status,
			formatDuration(time.Since(s.CreatedAt)),
			formatDuration(time.Since(s.LastActive)),
			s.MessageCount,
		))
		if i < len(sessions)-1 {
			sb.WriteString("\n\n")
		} else {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// VoiceInfo holds voice feature status for display.
type VoiceInfo struct {
	STTEnabled  bool
	STTProvider string
	STTModel    string
	TTSEnabled  bool
	TTSModel    string
	TTSVoice    string
}

// ExecuteInfo returns detailed info for a specific session.
func ExecuteInfo(pool *acp.SessionPool, threadKey string, voice *VoiceInfo) string {
	info, err := pool.GetSessionInfo(threadKey)
	if err != nil {
		return "No active session for this thread."
	}

	status := "Running"
	if !info.Alive {
		status = "Dead"
	}
	if info.Resumed {
		status += " (restored)"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"**Session Info**\n"+
			"Thread: `%s`\n"+
			"Session ID: `%s`\n"+
			"Status: %s\n"+
			"Created: %s ago\n"+
			"Last Active: %s ago\n"+
			"Messages: %d",
		info.ThreadKey,
		info.SessionID,
		status,
		formatDuration(time.Since(info.CreatedAt)),
		formatDuration(time.Since(info.LastActive)),
		info.MessageCount,
	))

	if voice != nil {
		sb.WriteString("\n\n**Voice**\n")
		if voice.STTEnabled {
			sb.WriteString(fmt.Sprintf("STT: `%s` / `%s`\n", voice.STTProvider, voice.STTModel))
		} else {
			sb.WriteString("STT: disabled\n")
		}
		if voice.TTSEnabled {
			sb.WriteString(fmt.Sprintf("TTS: `%s` / %s", voice.TTSModel, voice.TTSVoice))
		} else {
			sb.WriteString("TTS: disabled")
		}
	}

	return sb.String()
}

// ExecuteReset kills the current session and returns a status message.
func ExecuteReset(pool *acp.SessionPool, threadKey string) string {
	if err := pool.ResetSession(threadKey); err != nil {
		return "No active session to reset."
	}
	return "Session reset. A new session will be created on the next message."
}

// ExecuteResume attempts to restore a previous session for this thread.
func ExecuteResume(pool *acp.SessionPool, threadKey string) string {
	_, msg := pool.ResumeSession(threadKey)
	return msg
}

// ExecuteStop sends session/cancel to the agent for this thread. The
// current prompt stops producing output but the session (and its
// conversation history) is preserved — the next message keeps the
// context.
func ExecuteStop(pool *acp.SessionPool, threadKey string) string {
	if err := pool.CancelSession(threadKey); err != nil {
		return "No active session to stop."
	}
	return "🛑 Stop signal sent — the agent should wrap up shortly."
}

// pickerListLimit is the default number of sessions to show when a
// user asks /session-picker without an argument. Keeps the chat
// response short and picks a comfortable index range for /session-picker <N>.
const pickerListLimit = 10

// pickerListMaxAll raises the cap when the user explicitly passes
// `all` to bypass the cwd filter.
const pickerListMaxAll = 20

// pickerCacheTTL controls how long the last-listing results stay
// addressable by index. Long enough to cover a user pausing to read
// the list and then picking, short enough that stale picker output
// from yesterday does not resolve to the wrong session today.
const pickerCacheTTL = 5 * time.Minute

type pickerCacheEntry struct {
	sessions  []sessionpicker.Session
	expiresAt time.Time
}

var (
	pickerCacheMu sync.Mutex
	pickerCache   = make(map[string]pickerCacheEntry)
)

// cachePickerListing stores the most recent listing for a thread so a
// follow-up /session-picker <N> can resolve the numeric index.
func cachePickerListing(threadKey string, sessions []sessionpicker.Session) {
	pickerCacheMu.Lock()
	defer pickerCacheMu.Unlock()
	pickerCache[threadKey] = pickerCacheEntry{
		sessions:  sessions,
		expiresAt: time.Now().Add(pickerCacheTTL),
	}
}

func getPickerListing(threadKey string) ([]sessionpicker.Session, bool) {
	pickerCacheMu.Lock()
	defer pickerCacheMu.Unlock()
	e, ok := pickerCache[threadKey]
	if !ok || time.Now().After(e.expiresAt) {
		delete(pickerCache, threadKey)
		return nil, false
	}
	return e.sessions, true
}

// ExecutePicker handles /session-picker for a chat thread. args is
// the trimmed argument string after the command name; cwdFilter is
// usually the agent pool's working_dir so the default listing stays
// scoped to the current project.
//
// Supported shapes:
//
//	/session-picker              → list up to 10 sessions matching cwdFilter
//	/session-picker all          → list up to 20 sessions across all cwds
//	/session-picker <N>          → load session at index N from the last listing
//	/session-picker load <id>    → load the session with that exact ID
//
// When picker is nil (agent backend unsupported), a friendly message
// is returned without touching the pool.
func ExecutePicker(pool *acp.SessionPool, picker sessionpicker.Picker, threadKey, args, cwdFilter string) string {
	if picker == nil {
		return "Session picker is not supported for the current agent backend."
	}
	args = strings.TrimSpace(args)

	switch {
	case args == "":
		return pickerListResponse(picker, threadKey, cwdFilter, pickerListLimit, false)
	case strings.EqualFold(args, "all"):
		return pickerListResponse(picker, threadKey, "", pickerListMaxAll, true)
	case strings.HasPrefix(strings.ToLower(args), "load "):
		id := strings.TrimSpace(args[len("load "):])
		return pickerLoadByID(pool, picker, threadKey, id)
	default:
		if n, err := strconv.Atoi(args); err == nil {
			return pickerLoadByIndex(pool, threadKey, n)
		}
		return "Usage: `/session-picker` | `/session-picker <N>` | `/session-picker load <session-id>` | `/session-picker all`"
	}
}

func pickerListResponse(picker sessionpicker.Picker, threadKey, cwd string, limit int, bypassCWD bool) string {
	sessions, err := picker.List(cwd, limit)
	if err != nil {
		return fmt.Sprintf("Failed to list sessions: `%v`", err)
	}

	var sb strings.Builder
	header := fmt.Sprintf("**%s sessions**", picker.AgentType())
	if bypassCWD {
		header += " _(all cwds)_"
	} else if cwd != "" {
		header += fmt.Sprintf(" _(cwd: `%s`)_", cwd)
	}
	sb.WriteString(header)
	sb.WriteByte('\n')

	if len(sessions) == 0 {
		if !bypassCWD && cwd != "" {
			sb.WriteString("\nNo sessions match this cwd. Try `/session-picker all` — some agents (e.g. Codex) do not record cwd.")
		} else {
			sb.WriteString("\nNo sessions found.")
		}
		cachePickerListing(threadKey, nil)
		return sb.String()
	}

	cachePickerListing(threadKey, sessions)

	for i, s := range sessions {
		sb.WriteByte('\n')
		sb.WriteString(formatPickerRow(i+1, s))
	}
	sb.WriteString("\n\nPick with `/session-picker <N>` or `/session-picker load <session-id>`.")
	return sb.String()
}

// formatPickerRow renders one listing line. Session IDs are shown
// truncated to the first chunk so the line stays readable; the full
// ID remains available via `/session-picker load <full-id>`.
func formatPickerRow(n int, s sessionpicker.Session) string {
	id := s.ID
	if len(id) > 13 {
		id = id[:13] + "…"
	}
	title := s.Title
	if title == "" {
		title = "(untitled)"
	}
	when := formatDuration(time.Since(s.UpdatedAt))
	cwd := s.CWD
	if cwd == "" {
		cwd = "(no cwd)"
	}
	return fmt.Sprintf("`%d.` `%s` %s ago\n   %s\n   `%s`", n, id, when, title, cwd)
}

func pickerLoadByIndex(pool *acp.SessionPool, threadKey string, n int) string {
	if n < 1 {
		return "Index must be 1 or higher."
	}
	sessions, ok := getPickerListing(threadKey)
	if !ok {
		return "No recent listing in this chat. Run `/session-picker` first."
	}
	if n > len(sessions) {
		return fmt.Sprintf("Index %d is out of range — the last listing had %d entr(y|ies).", n, len(sessions))
	}
	sel := sessions[n-1]
	_, msg := pool.LoadSessionForThread(threadKey, sel.ID, sel.CWD)
	return msg
}

func pickerLoadByID(pool *acp.SessionPool, picker sessionpicker.Picker, threadKey, id string) string {
	if id == "" {
		return "Usage: `/session-picker load <session-id>`"
	}
	// Look up the session's cwd when possible so the new agent process
	// loads with the original working directory. If we cannot find it
	// (rare: id not in any listing), pass empty and let the pool fall
	// back to its own working_dir.
	cwd := ""
	if sessions, err := picker.List("", pickerListMaxAll*5); err == nil {
		for _, s := range sessions {
			if s.ID == id {
				cwd = s.CWD
				break
			}
		}
	}
	_, msg := pool.LoadSessionForThread(threadKey, id, cwd)
	return msg
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
