package sessionpicker

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestGeminiPickerList_NoFilter(t *testing.T) {
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Expected: aaaa (proj-a), bbbb (proj-b), dddd (proj-c via $set).
	// Skipped: cccc (subagent), eeeeeeee (no metadata header),
	// not-a-session-file (filename mismatch), parent-session-id-xxx
	// (it's a directory, not a file).
	if len(sessions) != 3 {
		t.Fatalf("want 3 valid sessions, got %d: %+v", len(sessions), sessions)
	}
	wantOrder := []string{
		"dddddddd-0000-0000-0000-000000000004", // 12:10 (latest)
		"bbbbbbbb-0000-0000-0000-000000000002", // 11:30
		"aaaaaaaa-0000-0000-0000-000000000001", // 10:05
	}
	for i, want := range wantOrder {
		if sessions[i].ID != want {
			t.Errorf("sessions[%d].ID = %q, want %q", i, sessions[i].ID, want)
		}
	}
}

func TestGeminiPickerList_FilterByProjectHash(t *testing.T) {
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("/home/test/proj-a", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Only aaaa matches /home/test/proj-a — cccc has matching hash
	// but is a subagent (skipped).
	if len(sessions) != 1 {
		t.Fatalf("want 1 session for proj-a, got %d: %+v", len(sessions), sessions)
	}
	if sessions[0].ID != "aaaaaaaa-0000-0000-0000-000000000001" {
		t.Errorf("ID = %q, want aaaaaaaa", sessions[0].ID)
	}
	if sessions[0].CWD != "/home/test/proj-a" {
		t.Errorf("CWD = %q, want /home/test/proj-a", sessions[0].CWD)
	}
}

func TestGeminiPickerList_FilterByDirectories(t *testing.T) {
	// dddd's parent dir is "registry-id-xyz" (not the sha256 of cwd),
	// and its initial metadata had no directories. The directories
	// field is added later via a $set update. The picker must accept
	// it because either the projectHash or any entry in directories
	// satisfies the cwd filter.
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("/home/test/proj-c", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session for proj-c, got %d", len(sessions))
	}
	if sessions[0].ID != "dddddddd-0000-0000-0000-000000000004" {
		t.Errorf("ID = %q, want dddddddd", sessions[0].ID)
	}
	if sessions[0].CWD != "/home/test/proj-c" {
		t.Errorf("CWD = %q, want /home/test/proj-c", sessions[0].CWD)
	}
}

func TestGeminiPickerList_TitlePreference(t *testing.T) {
	// aaaa has summary "refactor request for proj-a" — preferred over
	// the first user message.
	// bbbb has no summary — falls back to "plain string body for proj-b".
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	titles := map[string]string{}
	for _, s := range sessions {
		titles[s.ID] = s.Title
	}
	if got := titles["aaaaaaaa-0000-0000-0000-000000000001"]; got != "refactor request for proj-a" {
		t.Errorf("aaaa title = %q, want summary preference", got)
	}
	if got := titles["bbbbbbbb-0000-0000-0000-000000000002"]; got != "plain string body for proj-b" {
		t.Errorf("bbbb title = %q, want first user message", got)
	}
}

func TestGeminiPickerList_StripsSenderContext(t *testing.T) {
	// aaaa's first user message is wrapped in <sender_context>...
	// </sender_context> followed by the real prompt. Even though the
	// summary wins as the displayed title, ensure the user-message
	// extraction strips the envelope so dddd-style cases (no summary)
	// behave correctly. dddd has no envelope; we exercise the strip
	// logic separately via TestStripQuillEnvelope.
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("/home/test/proj-c", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].Title != "work in proj-c, parent dir is registry-named" {
		t.Errorf("title = %q", sessions[0].Title)
	}
}

func TestGeminiPickerList_Limit(t *testing.T) {
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("", 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 (limit), got %d", len(sessions))
	}
	if sessions[0].ID != "dddddddd-0000-0000-0000-000000000004" {
		t.Errorf("limit did not return newest first: %q", sessions[0].ID)
	}
}

func TestGeminiPickerList_MissingDir(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "no-such-dir")
	p := NewGeminiPicker(tmp)
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("missing dir should not be an error, got: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want 0 sessions, got %d", len(sessions))
	}
}

func TestGeminiPickerList_SkipsSubagent(t *testing.T) {
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range sessions {
		if s.ID == "cccccccc-0000-0000-0000-000000000003" {
			t.Errorf("subagent session must not appear in picker list")
		}
	}
}

func TestGeminiPickerList_NoMatchForUnknownCWD(t *testing.T) {
	p := NewGeminiPicker("testdata/gemini")
	sessions, err := p.List("/nowhere/in/fixtures", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want 0 sessions for unmatched cwd, got %d: %+v", len(sessions), sessions)
	}
}

func TestGeminiProjectHash(t *testing.T) {
	// Spot-check against the known sha256 of "/home/test/proj-a".
	got := geminiProjectHash("/home/test/proj-a")
	want := "d6629e6149339910972284b6fc170407938d2470535eb4dbae3921a01d014b2b"
	if got != want {
		t.Errorf("hash mismatch:\n got  %s\n want %s", got, want)
	}
	if geminiProjectHash("") != "" {
		t.Error("empty cwd should produce empty hash")
	}
}

func TestIsGeminiSessionFile(t *testing.T) {
	cases := map[string]bool{
		"session-2026-01-01T00-00-aaaaaaaa.jsonl": true,
		"session-2026-01-01T00-00-aaaaaaaa.json":  true,
		"session-foo.jsonl":                       true,
		"session-foo.txt":                         false,
		"not-a-session.jsonl":                     false,
		"":                                        false,
		"sessions-foo.jsonl":                      false,
	}
	for name, want := range cases {
		if got := isGeminiSessionFile(name); got != want {
			t.Errorf("isGeminiSessionFile(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestGeminiPickerList_LocalSmoke walks the real ~/.gemini/tmp directory
// for manual verification. Skipped unless QUILL_PICKER_SMOKE=1.
//
//	QUILL_PICKER_SMOKE=1 go test ./sessionpicker/ -run GeminiPickerList_LocalSmoke -v
func TestGeminiPickerList_LocalSmoke(t *testing.T) {
	if os.Getenv("QUILL_PICKER_SMOKE") != "1" {
		t.Skip("set QUILL_PICKER_SMOKE=1 to run against real ~/.gemini data")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	p := NewGeminiPicker("")
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
