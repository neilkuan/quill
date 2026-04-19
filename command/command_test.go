package command

import (
	"strings"
	"testing"
	"time"

	"github.com/neilkuan/quill/sessionpicker"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input   string
		wantCmd string
		wantOk  bool
	}{
		{"sessions", CmdSessions, true},
		{"Sessions", CmdSessions, true},
		{"SESSIONS", CmdSessions, true},
		{"reset", CmdReset, true},
		{"resume", CmdResume, true},
		{"Resume", CmdResume, true},
		{"RESUME", CmdResume, true},
		{"info", CmdInfo, true},
		{"stop", CmdStop, true},
		{"Stop", CmdStop, true},
		{"STOP", CmdStop, true},
		{"cancel", CmdStop, true}, // alias → stop
		{"CANCEL", CmdStop, true},
		{"session-picker", CmdPicker, true},
		{"Session-Picker", CmdPicker, true},
		{"history", CmdPicker, true},         // alias → session-picker
		{"pick", CmdPicker, true},            // alias → session-picker
		{"session_picker", CmdPicker, true},  // Telegram-friendly spelling (no hyphen)
		{"Session_Picker", CmdPicker, true},
		{"pick 3", CmdPicker, true},       // alias with numeric arg
		{"history load abc", CmdPicker, true}, // alias with load subcommand
		{"sessions extra args", CmdSessions, true},
		{"hello world", "", false},
		{"", "", false},
		{"   sessions   ", CmdSessions, true},
		{"reset now", CmdReset, true},
		{"session", "", false}, // not "sessions"
		// Telegram msg.Command() returns bare name without /
		// Discord slash commands also use bare names
		// Both pass through ParseCommand correctly
	}

	for _, tt := range tests {
		cmd, ok := ParseCommand(tt.input)
		if ok != tt.wantOk {
			t.Errorf("ParseCommand(%q): got ok=%v, want %v", tt.input, ok, tt.wantOk)
			continue
		}
		if ok && cmd.Name != tt.wantCmd {
			t.Errorf("ParseCommand(%q): got name=%q, want %q", tt.input, cmd.Name, tt.wantCmd)
		}
	}
}

func TestParseCommand_Args(t *testing.T) {
	cmd, ok := ParseCommand("sessions foo bar")
	if !ok {
		t.Fatal("expected ok")
	}
	if cmd.Args != "foo bar" {
		t.Errorf("got args=%q, want %q", cmd.Args, "foo bar")
	}
}

// fakePicker is a stand-in sessionpicker.Picker for unit tests — no
// filesystem access, deterministic results.
type fakePicker struct {
	agentType string
	sessions  []sessionpicker.Session
	err       error
}

func (f *fakePicker) AgentType() string { return f.agentType }

func (f *fakePicker) List(cwd string, limit int) ([]sessionpicker.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := f.sessions
	if cwd != "" {
		filtered := out[:0:0]
		for _, s := range out {
			if s.CWD == cwd {
				filtered = append(filtered, s)
			}
		}
		out = filtered
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func TestExecutePicker_NilPickerGivesHelpfulMessage(t *testing.T) {
	msg := ExecutePicker(nil, nil, "discord:chan-1", "", "")
	if !strings.Contains(strings.ToLower(msg), "not supported") {
		t.Errorf("expected friendly 'not supported' message, got: %q", msg)
	}
}

func TestExecutePicker_ListsCachesAndLoadsByIndex(t *testing.T) {
	fake := &fakePicker{
		agentType: "kiro-cli",
		sessions: []sessionpicker.Session{
			{ID: "sess-1", Title: "first", CWD: "/work/a", UpdatedAt: time.Now().Add(-time.Hour)},
			{ID: "sess-2", Title: "second", CWD: "/work/a", UpdatedAt: time.Now().Add(-30 * time.Minute)},
		},
	}
	thread := "discord:chan-cache"

	out := ExecutePicker(nil, fake, thread, "", "/work/a")
	if !strings.Contains(out, "sess-1") || !strings.Contains(out, "sess-2") {
		t.Fatalf("listing should contain both session IDs (truncated): %s", out)
	}

	// After the listing call, the cache should remember the sessions
	// so an index lookup resolves.
	cached, ok := getPickerListing(thread)
	if !ok {
		t.Fatal("expected cached listing to be present after ExecutePicker")
	}
	if len(cached) != 2 {
		t.Errorf("cached length = %d, want 2", len(cached))
	}
}

func TestExecutePicker_OutOfRangeIndex(t *testing.T) {
	fake := &fakePicker{agentType: "kiro-cli"}
	thread := "discord:chan-oor"
	// Prime cache with one entry.
	cachePickerListing(thread, []sessionpicker.Session{
		{ID: "only", Title: "t", UpdatedAt: time.Now()},
	})

	msg := ExecutePicker(nil, fake, thread, "5", "")
	if !strings.Contains(strings.ToLower(msg), "out of range") {
		t.Errorf("expected out-of-range message, got: %q", msg)
	}
}

func TestExecutePicker_NoCacheForIndex(t *testing.T) {
	fake := &fakePicker{agentType: "kiro-cli"}
	// Unique thread key so no prior cache applies.
	msg := ExecutePicker(nil, fake, "thread-no-cache", "2", "")
	if !strings.Contains(strings.ToLower(msg), "no recent listing") {
		t.Errorf("expected 'no recent listing' when cache is empty, got: %q", msg)
	}
}

func TestExecutePicker_UnknownSubcommand(t *testing.T) {
	fake := &fakePicker{agentType: "kiro-cli"}
	msg := ExecutePicker(nil, fake, "discord:chan-usage", "???", "")
	if !strings.Contains(strings.ToLower(msg), "usage") {
		t.Errorf("expected usage hint, got: %q", msg)
	}
}

func TestExecutePicker_EmptyListSuggestsAll(t *testing.T) {
	// Picker has sessions, but none match the cwdFilter — output
	// should hint that `all` exists.
	fake := &fakePicker{
		agentType: "codex-acp",
		sessions:  []sessionpicker.Session{{ID: "x", Title: "y", CWD: "/elsewhere", UpdatedAt: time.Now()}},
	}
	msg := ExecutePicker(nil, fake, "discord:chan-empty", "", "/no/match")
	if !strings.Contains(msg, "/session-picker all") {
		t.Errorf("expected hint about `/session-picker all`, got: %q", msg)
	}
}

func TestPickerCache_TTLExpiry(t *testing.T) {
	thread := "thread-ttl"
	cachePickerListing(thread, []sessionpicker.Session{{ID: "a"}})
	// Manually age the entry past the TTL to avoid a real sleep.
	pickerCacheMu.Lock()
	e := pickerCache[thread]
	e.expiresAt = time.Now().Add(-time.Second)
	pickerCache[thread] = e
	pickerCacheMu.Unlock()

	if _, ok := getPickerListing(thread); ok {
		t.Error("expected expired cache entry to be absent")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d"},
		{72 * time.Hour, "3d"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
