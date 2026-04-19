package sessionpicker

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexPickerList_DedupeAndSort(t *testing.T) {
	p := NewCodexPicker("testdata/codex/history.jsonl")
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// aaaa has 3 entries, bbbb & cccc 1 each; malformed and empty-id
	// lines must be skipped — so we expect 3 unique sessions.
	if len(sessions) != 3 {
		t.Fatalf("want 3 deduped sessions, got %d: %+v", len(sessions), sessions)
	}

	// Order is by latest ts: cccc (1776592867) > bbbb (1776592667) > aaaa (1776592300).
	wantOrder := []string{"codex-cccc-0003", "codex-bbbb-0002", "codex-aaaa-0001"}
	for i, want := range wantOrder {
		if sessions[i].ID != want {
			t.Errorf("sessions[%d].ID = %q, want %q", i, sessions[i].ID, want)
		}
	}

	// aaaa's title should come from its earliest ts entry, not the
	// most recent one — picker keeps the first prompt as title.
	var aaaa Session
	for _, s := range sessions {
		if s.ID == "codex-aaaa-0001" {
			aaaa = s
			break
		}
	}
	if aaaa.Title != "first prompt of session aaaa" {
		t.Errorf("aaaa title = %q, want earliest prompt", aaaa.Title)
	}
	if aaaa.MessageCount != 3 {
		t.Errorf("aaaa MessageCount = %d, want 3", aaaa.MessageCount)
	}
	if want := time.Unix(1776592300, 0); !aaaa.UpdatedAt.Equal(want) {
		t.Errorf("aaaa UpdatedAt = %v, want %v (latest ts)", aaaa.UpdatedAt, want)
	}
}

func TestCodexPickerList_CWDFilterReturnsEmpty(t *testing.T) {
	// history.jsonl has no cwd, so any non-empty cwd filter is an
	// unsatisfiable query — the picker returns nothing rather than
	// silently showing results that were never actually matched.
	p := NewCodexPicker("testdata/codex/history.jsonl")
	sessions, err := p.List("/some/path", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want 0 sessions when cwd filter is set, got %d", len(sessions))
	}
}

func TestCodexPickerList_Limit(t *testing.T) {
	p := NewCodexPicker("testdata/codex/history.jsonl")
	sessions, err := p.List("", 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 (limit), got %d", len(sessions))
	}
	if sessions[0].ID != "codex-cccc-0003" {
		t.Errorf("limit did not return newest first: %q", sessions[0].ID)
	}
}

func TestCodexPickerList_MissingFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	p := NewCodexPicker(tmp)
	sessions, err := p.List("", 0)
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("want empty slice, got %d", len(sessions))
	}
}

func TestCodexPickerList_Title_UTF8Truncation(t *testing.T) {
	// The cccc fixture entry is a long zh-tw prompt; make sure we
	// truncate at rune boundaries (50 runes) and append "...".
	p := NewCodexPicker("testdata/codex/history.jsonl")
	sessions, _ := p.List("", 0)
	var cccc Session
	for _, s := range sessions {
		if s.ID == "codex-cccc-0003" {
			cccc = s
			break
		}
	}
	// 50 runes + "..." — total rune count should be 53.
	runes := []rune(cccc.Title)
	if len(runes) != 53 {
		t.Errorf("expected 53 runes (50 + ellipsis), got %d: %q", len(runes), cccc.Title)
	}
	if !hasEllipsis(cccc.Title) {
		t.Errorf("title should end with '...' when truncated: %q", cccc.Title)
	}
}

func hasEllipsis(s string) bool {
	return len(s) >= 3 && s[len(s)-3:] == "..."
}

// TestCodexPickerList_LocalSmoke reads the real ~/.codex/history.jsonl
// for manual verification. Skipped unless QUILL_PICKER_SMOKE=1.
//
//	QUILL_PICKER_SMOKE=1 go test ./sessionpicker/ -run CodexPickerList_LocalSmoke -v
func TestCodexPickerList_LocalSmoke(t *testing.T) {
	if os.Getenv("QUILL_PICKER_SMOKE") != "1" {
		t.Skip("set QUILL_PICKER_SMOKE=1 to run against real ~/.codex data")
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	p := NewCodexPicker("")
	sessions, err := p.List("", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("found %d session(s), showing up to 10 newest:", len(sessions))
	for i, s := range sessions {
		t.Logf("  [%d] %s | %s | msgs=%d | title=%q",
			i+1,
			s.UpdatedAt.Format("2006-01-02 15:04"),
			s.ID,
			s.MessageCount,
			s.Title,
		)
	}
}
