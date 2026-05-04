package cronjob

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStoreInTmp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cronjobs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

func TestStoreEmpty(t *testing.T) {
	s, _ := newStoreInTmp(t)
	if got := s.List(""); len(got) != 0 {
		t.Errorf("expected empty store, got %d", len(got))
	}
}

func TestStoreAddListRemove(t *testing.T) {
	s, _ := newStoreInTmp(t)

	now := mustTime(t, "2026-05-04T10:00:00Z")
	job := Job{
		ThreadKey:  "tg:1",
		SenderID:   "100",
		SenderName: "neil",
		Schedule:   "every 5m",
		Kind:       KindInterval,
		Prompt:     "ping",
		NextFire:   now.Add(5 * time.Minute),
		CreatedAt:  now,
		parsed:     mustInterval(t, 5*time.Minute),
	}
	added, err := s.Add(job, 100)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.ID == "" {
		t.Error("Add did not assign an ID")
	}

	all := s.List("")
	if len(all) != 1 {
		t.Fatalf("List len=%d want 1", len(all))
	}
	thread := s.List("tg:1")
	if len(thread) != 1 {
		t.Fatalf("thread filter len=%d want 1", len(thread))
	}
	other := s.List("tg:2")
	if len(other) != 0 {
		t.Fatalf("other thread filter len=%d want 0", len(other))
	}

	if err := s.Remove("tg:1", added.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := s.List(""); len(got) != 0 {
		t.Errorf("after Remove len=%d want 0", len(got))
	}
}

func TestStoreMaxPerThread(t *testing.T) {
	s, _ := newStoreInTmp(t)
	for i := 0; i < 3; i++ {
		_, err := s.Add(Job{ThreadKey: "tg:1", Schedule: "every 5m", Kind: KindInterval, parsed: mustInterval(t, 5*time.Minute)}, 3)
		if err != nil {
			t.Fatalf("Add #%d: %v", i, err)
		}
	}
	if _, err := s.Add(Job{ThreadKey: "tg:1", Schedule: "every 5m", Kind: KindInterval, parsed: mustInterval(t, 5*time.Minute)}, 3); err == nil {
		t.Error("expected error when exceeding max_per_thread")
	}
}

func TestStorePersistAndReload(t *testing.T) {
	s, path := newStoreInTmp(t)
	j := Job{ThreadKey: "tg:1", Schedule: "every 5m", Kind: KindInterval, parsed: mustInterval(t, 5*time.Minute)}
	if _, err := s.Add(j, 100); err != nil {
		t.Fatalf("Add: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open reload: %v", err)
	}
	got := s2.List("")
	if len(got) != 1 {
		t.Errorf("after reload len=%d want 1", len(got))
	}
	if got[0].Parsed() == nil {
		t.Error("reloaded job has nil parsed schedule")
	}
}

func TestStoreCorruptFileMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cronjobs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open should recover from corrupt file, got %v", err)
	}
	if got := s.List(""); len(got) != 0 {
		t.Errorf("recovered store should be empty, got %d", len(got))
	}
	// The corrupt file should be renamed aside.
	matches, _ := filepath.Glob(filepath.Join(dir, "cronjobs.json.broken-*"))
	if len(matches) != 1 {
		t.Errorf("expected one .broken-* file, got %d", len(matches))
	}
}

// helper
func mustInterval(t *testing.T, d time.Duration) Schedule {
	t.Helper()
	s, err := newIntervalSchedule(d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
