package command

import (
	"testing"
	"time"
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
		{"info", CmdInfo, true},
		{"sessions extra args", CmdSessions, true},
		{"hello world", "", false},
		{"", "", false},
		{"   sessions   ", CmdSessions, true},
		{"reset now", CmdReset, true},
		{"session", "", false},  // not "sessions"
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
