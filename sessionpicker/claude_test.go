package sessionpicker

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setFixtureMtimes forces deterministic modification times on Claude
// test fixtures so List's mtime-based sort order is predictable
// regardless of when the repo was checked out.
func setFixtureMtimes(t *testing.T, base string, files map[string]time.Time) {
	t.Helper()
	for rel, when := range files {
		path := filepath.Join(base, rel)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

func TestClaudePickerList_SortAndTitleHeuristic(t *testing.T) {
	base := "testdata/claude"
	setFixtureMtimes(t, base, map[string]time.Time{
		"-home-test-proj-a/aaaaaaaa-1111-0000-0000-000000000001.jsonl": time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC),
		"-home-test-proj-b/bbbbbbbb-2222-0000-0000-000000000002.jsonl": time.Date(2026, 4, 12, 9, 0, 0, 0, time.UTC),
		"-home-test-proj-a/cccccccc-3333-0000-0000-000000000003.jsonl": time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC),
	})

	p := NewClaudePicker(base)
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("want 3 sessions, got %d: %+v", len(sessions), sessions)
	}

	wantOrder := []string{
		"cccccccc-3333-0000-0000-000000000003",
		"bbbbbbbb-2222-0000-0000-000000000002",
		"aaaaaaaa-1111-0000-0000-000000000001",
	}
	for i, want := range wantOrder {
		if sessions[i].ID != want {
			t.Errorf("sessions[%d].ID = %q, want %q", i, sessions[i].ID, want)
		}
	}

	// aaaa session has two ignored user messages (caveat + slash command)
	// before the real prompt; the heuristic should surface the real one.
	var aaaa Session
	for _, s := range sessions {
		if s.ID == "aaaaaaaa-1111-0000-0000-000000000001" {
			aaaa = s
			break
		}
	}
	if aaaa.Title != "幫我重構這個 function" {
		t.Errorf("title from aaaa = %q, want real prompt (slash-command wrapper should be skipped)", aaaa.Title)
	}
	if aaaa.CWD != "/home/test/proj-a" {
		t.Errorf("cwd from aaaa = %q", aaaa.CWD)
	}

	// bbbb session uses the list-of-blocks content form.
	var bbbb Session
	for _, s := range sessions {
		if s.ID == "bbbbbbbb-2222-0000-0000-000000000002" {
			bbbb = s
			break
		}
	}
	if bbbb.Title != "list block form prompt" {
		t.Errorf("title from bbbb = %q (block-form content should be readable)", bbbb.Title)
	}
}

func TestClaudePickerList_FilterByCWD(t *testing.T) {
	p := NewClaudePicker("testdata/claude")
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

func TestClaudePickerList_BadLinesDoNotFailSession(t *testing.T) {
	// The cccc fixture has a garbage first line followed by a valid
	// user event — the scanner must shrug off the bad line and still
	// extract cwd/title/id from the second line.
	p := NewClaudePicker("testdata/claude")
	sessions, err := p.List("/home/test/proj-a", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ccc Session
	for _, s := range sessions {
		if s.ID == "cccccccc-3333-0000-0000-000000000003" {
			ccc = s
			break
		}
	}
	if ccc.ID == "" {
		t.Fatal("cccc session should still be listed despite bad first line")
	}
	if ccc.Title == "" {
		t.Error("title should have been recovered from the second line")
	}
}

func TestClaudePickerList_MissingDir(t *testing.T) {
	p := NewClaudePicker("testdata/claude-does-not-exist")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("missing dir should not be an error, got: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want empty slice for missing dir, got %d", len(sessions))
	}
}

// TestClaudePickerList_LocalSmoke lists sessions from the real
// ~/.claude/projects directory for manual verification. Skipped unless
// explicitly opted in via QUILL_PICKER_SMOKE=1.
//
//	QUILL_PICKER_SMOKE=1 go test ./sessionpicker/ -run ClaudePickerList_LocalSmoke -v
func TestClaudePickerList_LocalSmoke(t *testing.T) {
	if os.Getenv("QUILL_PICKER_SMOKE") != "1" {
		t.Skip("set QUILL_PICKER_SMOKE=1 to run against real ~/.claude data")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	p := NewClaudePicker("")
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

func TestEncodeClaudeCWD(t *testing.T) {
	cases := map[string]string{
		"/Users/me/proj":       "-Users-me-proj",
		"/Users/me/.dotfile":   "-Users-me--dotfile",
		"/home/test/proj-a":    "-home-test-proj-a",
		"/path/with_underscore": "-path-with-underscore",
	}
	for in, want := range cases {
		if got := encodeClaudeCWD(in); got != want {
			t.Errorf("encodeClaudeCWD(%q) = %q, want %q", in, got, want)
		}
	}
}
