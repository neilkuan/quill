package sessionpicker

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setCopilotFixtureMtimes stamps session directories so List's mtime
// fallback is deterministic.
func setCopilotFixtureMtimes(t *testing.T, base string, dirs map[string]time.Time) {
	t.Helper()
	for rel, when := range dirs {
		path := filepath.Join(base, rel)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

func TestCopilotPickerList_YAMLOverridesEvents(t *testing.T) {
	base := "testdata/copilot"
	setCopilotFixtureMtimes(t, base, map[string]time.Time{
		"session-aaaa-0001": time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC),
		"session-bbbb-0002": time.Date(2026, 4, 17, 9, 0, 0, 0, time.UTC),
		"session-cccc-0003": time.Date(2026, 4, 16, 8, 0, 0, 0, time.UTC),
	})

	p := NewCopilotPicker(base)
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("want 3 sessions, got %d: %+v", len(sessions), sessions)
	}

	var aaaa Session
	for _, s := range sessions {
		if s.ID == "session-aaaa-0001" {
			aaaa = s
			break
		}
	}
	if aaaa.Title != "yaml-sourced title" {
		t.Errorf("title from aaaa = %q, want yaml title (yaml must take precedence over events)", aaaa.Title)
	}
	if aaaa.CWD != "/home/test/proj-copilot" {
		t.Errorf("cwd from aaaa = %q", aaaa.CWD)
	}
	if aaaa.UpdatedAt.Year() != 2026 || aaaa.UpdatedAt.Month() != 4 || aaaa.UpdatedAt.Day() != 18 {
		t.Errorf("updated_at from aaaa should come from yaml: got %v", aaaa.UpdatedAt)
	}
}

func TestCopilotPickerList_FallbackToEvents(t *testing.T) {
	p := NewCopilotPicker("testdata/copilot")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var bbbb Session
	for _, s := range sessions {
		if s.ID == "session-bbbb-0002" {
			bbbb = s
			break
		}
	}
	if bbbb.ID == "" {
		t.Fatal("bbbb session should be listed even without workspace.yaml")
	}
	if bbbb.Title != "第一筆 user prompt 從 events.jsonl 讀出" {
		t.Errorf("title from bbbb = %q, want first user event text", bbbb.Title)
	}
	if bbbb.CWD != "/home/test/proj-copilot" {
		t.Errorf("cwd from bbbb = %q", bbbb.CWD)
	}
}

func TestCopilotPickerList_AlternateYAMLKeys(t *testing.T) {
	// session-cccc uses id / workspace / name instead of
	// session_id / cwd / title. The picker should accept both.
	p := NewCopilotPicker("testdata/copilot")
	sessions, err := p.List("/home/test/proj-other", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("want 1 session for proj-other, got %d", len(sessions))
	}
	s := sessions[0]
	if s.ID != "session-cccc-0003" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.Title != "alt-keys session" {
		t.Errorf("Title = %q (expected to come from yaml 'name')", s.Title)
	}
}

func TestCopilotPickerList_FilterAndLimit(t *testing.T) {
	p := NewCopilotPicker("testdata/copilot")

	all, _ := p.List("", 2)
	if len(all) != 2 {
		t.Fatalf("limit=2 should cap to 2, got %d", len(all))
	}

	filtered, _ := p.List("/home/test/proj-copilot", 0)
	if len(filtered) != 2 {
		t.Fatalf("want 2 for proj-copilot (aaaa + bbbb), got %d", len(filtered))
	}
}

// TestCopilotPickerList_LocalSmoke reads the real ~/.copilot/session-state
// directory for manual verification. Skipped unless QUILL_PICKER_SMOKE=1.
//
//	QUILL_PICKER_SMOKE=1 go test ./sessionpicker/ -run CopilotPickerList_LocalSmoke -v
func TestCopilotPickerList_LocalSmoke(t *testing.T) {
	if os.Getenv("QUILL_PICKER_SMOKE") != "1" {
		t.Skip("set QUILL_PICKER_SMOKE=1 to run against real ~/.copilot data")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	p := NewCopilotPicker("")
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

func TestCopilotPickerList_MissingDir(t *testing.T) {
	p := NewCopilotPicker("testdata/copilot-does-not-exist")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("missing dir should not be an error, got: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want empty slice, got %d", len(sessions))
	}
}
