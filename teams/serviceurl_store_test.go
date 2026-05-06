package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestServiceURLStore_NilReceiverIsSafe(t *testing.T) {
	var s *ServiceURLStore
	if got := s.Get("conv-1"); got != "" {
		t.Errorf("nil Get = %q, want empty", got)
	}
	if err := s.Set("conv-1", "https://service"); err != nil {
		t.Errorf("nil Set returned error: %v", err)
	}
	if got := s.Len(); got != 0 {
		t.Errorf("nil Len = %d, want 0", got)
	}
}

func TestServiceURLStore_InMemoryOnly(t *testing.T) {
	s, err := OpenServiceURLStore("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set("conv-a", "https://service-a"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := s.Get("conv-a"); got != "https://service-a" {
		t.Errorf("Get = %q, want service-a", got)
	}
}

func TestServiceURLStore_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "serviceurls.json")

	s1, err := OpenServiceURLStore(path)
	if err != nil {
		t.Fatalf("Open#1: %v", err)
	}
	if err := s1.Set("conv-a", "https://service-a"); err != nil {
		t.Fatalf("Set conv-a: %v", err)
	}
	if err := s1.Set("conv-b", "https://service-b"); err != nil {
		t.Fatalf("Set conv-b: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected store file to exist: %v", err)
	}

	s2, err := OpenServiceURLStore(path)
	if err != nil {
		t.Fatalf("Open#2: %v", err)
	}
	if got := s2.Get("conv-a"); got != "https://service-a" {
		t.Errorf("reloaded Get(conv-a) = %q", got)
	}
	if got := s2.Get("conv-b"); got != "https://service-b" {
		t.Errorf("reloaded Get(conv-b) = %q", got)
	}
	if s2.Len() != 2 {
		t.Errorf("Len = %d, want 2", s2.Len())
	}
}

func TestServiceURLStore_NoWriteOnUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serviceurls.json")

	s, err := OpenServiceURLStore(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Set("conv-a", "https://service-a"); err != nil {
		t.Fatalf("Set#1: %v", err)
	}
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat#1: %v", err)
	}

	// Same value — should NOT touch the file.
	if err := s.Set("conv-a", "https://service-a"); err != nil {
		t.Fatalf("Set#2: %v", err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat#2: %v", err)
	}
	if !info2.ModTime().Equal(info1.ModTime()) {
		t.Errorf("identical Set caused write: mtime changed")
	}
}

func TestServiceURLStore_EmptyArgsIgnored(t *testing.T) {
	s, _ := OpenServiceURLStore("")
	if err := s.Set("", "https://x"); err != nil {
		t.Fatalf("Set empty conv: %v", err)
	}
	if err := s.Set("conv-a", ""); err != nil {
		t.Fatalf("Set empty url: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("empty inputs should not be stored, Len = %d", s.Len())
	}
}

func TestServiceURLStore_CorruptFileMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serviceurls.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	s, err := OpenServiceURLStore(path)
	if err != nil {
		t.Fatalf("Open should not error on corrupt file: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0 (started fresh)", s.Len())
	}

	matches, _ := filepath.Glob(path + ".broken-*")
	if len(matches) == 0 {
		t.Errorf("expected a *.broken-<ts> sibling, got none")
	}
}

func TestServiceURLStore_FileFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serviceurls.json")
	s, _ := OpenServiceURLStore(path)
	if err := s.Set("conv-a", "https://service-a"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var ff serviceURLFileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if ff.Version != serviceURLStoreVersion {
		t.Errorf("version = %d, want %d", ff.Version, serviceURLStoreVersion)
	}
	if ff.URLs["conv-a"] != "https://service-a" {
		t.Errorf("urls[conv-a] = %q", ff.URLs["conv-a"])
	}
}
