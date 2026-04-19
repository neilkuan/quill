package sessionpicker

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Silence the graceful-skip warnings in test output — the behavior
	// itself is verified by the table (broken.json / no_session_id.json
	// are expected to be skipped, not counted).
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantOK  bool
		wantTyp string
	}{
		{"kiro bare binary", "kiro-cli", true, "kiro-cli"},
		{"kiro absolute path", "/usr/local/bin/kiro-cli", true, "kiro-cli"},
		{"claude", "claude-agent-acp", true, "claude-agent-acp"},
		{"copilot", "copilot", true, "copilot"},
		{"copilot absolute path", "/usr/local/bin/copilot", true, "copilot"},
		{"codex", "codex", true, "codex-acp"},
		{"codex-acp", "codex-acp", true, "codex-acp"},
		{"unknown", "some-other-agent", false, ""},
		{"empty", "", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := Detect(tc.cmd)
			if ok != tc.wantOK {
				t.Fatalf("Detect(%q) ok = %v, want %v", tc.cmd, ok, tc.wantOK)
			}
			if ok && p.AgentType() != tc.wantTyp {
				t.Fatalf("Detect(%q).AgentType() = %q, want %q", tc.cmd, p.AgentType(), tc.wantTyp)
			}
		})
	}
}

func TestKiroPickerList_SortedNewestFirst(t *testing.T) {
	p := NewKiroPicker("testdata/kiro")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Expect 3 valid sessions; broken.json, no_session_id.json, and
	// ignore_me.jsonl must be skipped.
	if len(sessions) != 3 {
		t.Fatalf("want 3 sessions, got %d: %+v", len(sessions), sessions)
	}

	wantOrder := []string{
		"cccccccc-0000-0000-0000-000000000003",
		"bbbbbbbb-0000-0000-0000-000000000002",
		"aaaaaaaa-0000-0000-0000-000000000001",
	}
	for i, want := range wantOrder {
		if sessions[i].ID != want {
			t.Errorf("sessions[%d].ID = %q, want %q", i, sessions[i].ID, want)
		}
	}
}

func TestKiroPickerList_FilterByCWD(t *testing.T) {
	p := NewKiroPicker("testdata/kiro")
	sessions, err := p.List("/home/test/proj-a", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions for proj-a, got %d", len(sessions))
	}
	for _, s := range sessions {
		if s.CWD != "/home/test/proj-a" {
			t.Errorf("unexpected cwd: %q", s.CWD)
		}
	}
}

func TestKiroPickerList_Limit(t *testing.T) {
	p := NewKiroPicker("testdata/kiro")
	sessions, err := p.List("", 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].ID != "cccccccc-0000-0000-0000-000000000003" {
		t.Errorf("limit did not return newest first: %q", sessions[0].ID)
	}
}

func TestKiroPickerList_MissingDir(t *testing.T) {
	p := NewKiroPicker("testdata/does-not-exist")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("missing dir should not be an error, got: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want empty slice for missing dir, got %d", len(sessions))
	}
}

// TestKiroPickerList_LocalSmoke lists sessions from the real ~/.kiro
// directory for manual verification. Skipped unless explicitly opted in
// via QUILL_PICKER_SMOKE=1, so `go test ./...` stays hermetic.
//
//	QUILL_PICKER_SMOKE=1 go test ./sessionpicker/ -run LocalSmoke -v
func TestKiroPickerList_LocalSmoke(t *testing.T) {
	if os.Getenv("QUILL_PICKER_SMOKE") != "1" {
		t.Skip("set QUILL_PICKER_SMOKE=1 to run against real ~/.kiro data")
	}
	// Re-enable slog output so Warn messages surface during the smoke run.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	p := NewKiroPicker("")
	sessions, err := p.List("", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("found %d session(s), showing up to 10 newest:", len(sessions))
	for i, s := range sessions {
		t.Logf("  [%d] %s | %s | cwd=%s | title=%q",
			i+1,
			s.UpdatedAt.Format("2006-01-02 15:04"),
			s.ID,
			s.CWD,
			s.Title,
		)
	}
}

func TestStripQuillEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		in, want string
	}{
		{"sender_context", "<sender_context>\n{\"schema\":\"quill.sender.v1\"}\n</sender_context>\n\nhello", "hello"},
		{"sender_context empty body", "<sender_context></sender_context>hi", "hi"},
		{"sender_context with leading whitespace", "  \n<sender_context>meta</sender_context>\n\nactual prompt", "actual prompt"},
		{"voice_transcription unwrap returns inner", "<voice_transcription>你聽到我說話嗎?</voice_transcription>", "你聽到我說話嗎?"},
		{"voice_transcription with trailing quill hint dropped", "<voice_transcription>\nhi\n</voice_transcription>\nThe above is a transcription of the user's voice message.", "hi"},
		{"sender_context then voice_transcription: returns voice inner", "<sender_context>meta</sender_context>\n\n<voice_transcription>hello?</voice_transcription>\nThe above...", "hello?"},
		{"attached_files envelope stripped", "<attached_files>meta</attached_files>\n\nreal", "real"},
		{"no envelope", "no envelope here", "no envelope here"},
		{"unrelated XML-ish", "<other_tag>content</other_tag>", "<other_tag>content</other_tag>"},
		{"envelope mid-string stays untouched", "free text then <sender_context>x</sender_context> tail", "free text then <sender_context>x</sender_context> tail"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripQuillEnvelope(tc.in); got != tc.want {
				t.Errorf("stripQuillEnvelope(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestKiroPickerList_JSONLFallbackForSenderContextTitle(t *testing.T) {
	// cccc's .json title was stored as "<sender_context>" (Kiro
	// truncated the prompt to the opening tag); the loader should
	// fall back to the sibling .jsonl and recover the real prompt,
	// minus the envelope.
	p := NewKiroPicker("testdata/kiro")
	sessions, err := p.List("/home/test/proj-a", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ccc Session
	for _, s := range sessions {
		if s.ID == "cccccccc-0000-0000-0000-000000000003" {
			ccc = s
			break
		}
	}
	if ccc.ID == "" {
		t.Fatal("cccc session should still be listed")
	}
	if ccc.Title != "幫我看一下這個 issue" {
		t.Errorf("title = %q, want real user text recovered from jsonl", ccc.Title)
	}
}

func TestKiroPickerList_AgentNameFilter(t *testing.T) {
	// Only bbbb has agent_name=openab-go in fixtures; the other two
	// have no agent_name (treated as kiro_default). The filter must
	// drop kiro_default sessions when configured for openab-go.
	p := NewKiroPicker("testdata/kiro")
	p.AgentName = "openab-go"
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("openab-go filter should keep 1 session, got %d: %+v", len(sessions), sessions)
	}
	if sessions[0].ID != "bbbbbbbb-0000-0000-0000-000000000002" {
		t.Errorf("expected bbbb (openab-go), got %q", sessions[0].ID)
	}

	// And filtering for kiro_default should pick up sessions that
	// have no agent_name recorded (aaaa, cccc).
	p.AgentName = "kiro_default"
	sessions, err = p.List("", 0)
	if err != nil {
		t.Fatalf("List (kiro_default): %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("kiro_default filter should keep 2 sessions, got %d", len(sessions))
	}
}

func TestKiroAgentMatches(t *testing.T) {
	cases := []struct {
		stored, want string
		expect       bool
	}{
		{"openab-go", "openab-go", true},
		{"", "kiro_default", true},             // empty treated as default
		{"kiro_default", "kiro_default", true}, // explicit default
		{"", "openab-go", false},               // empty session rejected by non-default filter
		{"kiro_default", "openab-go", false},
		{"foo", "bar", false},
	}
	for _, c := range cases {
		if got := kiroAgentMatches(c.stored, c.want); got != c.expect {
			t.Errorf("kiroAgentMatches(%q, %q) = %v, want %v", c.stored, c.want, got, c.expect)
		}
	}
}

func TestKiroPickerList_Metadata(t *testing.T) {
	p := NewKiroPicker("testdata/kiro")
	sessions, err := p.List("/home/test/proj-b", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1, got %d", len(sessions))
	}
	s := sessions[0]
	if s.Title != "middle session" {
		t.Errorf("Title = %q", s.Title)
	}
	if s.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be populated")
	}
	if s.CWD != "/home/test/proj-b" {
		t.Errorf("CWD = %q", s.CWD)
	}
}
