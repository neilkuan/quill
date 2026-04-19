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
		{"bare binary", "kiro-cli", true, "kiro-cli"},
		{"absolute path", "/usr/local/bin/kiro-cli", true, "kiro-cli"},
		{"unknown", "some-other-agent", false, ""},
		{"claude not yet implemented", "claude-agent-acp", false, ""},
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
