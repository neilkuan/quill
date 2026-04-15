package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// mustSave is a test helper that calls Save and fails the test on error.
func mustSave(t *testing.T, store *SessionStore, threadKey, sessionID string) {
	t.Helper()
	if err := store.Save(threadKey, sessionID); err != nil {
		t.Fatalf("Save(%q, %q) failed: %v", threadKey, sessionID, err)
	}
}

func TestNewSessionStore_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}

	// .quill directory should exist
	info, err := os.Stat(filepath.Join(dir, ".quill"))
	if err != nil {
		t.Fatalf("expected .quill dir to exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected .quill to be a directory")
	}
}

func TestSessionStore_SaveAndLookup(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Lookup on empty store
	if got := store.Lookup("discord:123"); got != "" {
		t.Errorf("expected empty string for missing key, got %q", got)
	}

	// Save and lookup
	mustSave(t, store, "discord:123", "sess_abc")
	if got := store.Lookup("discord:123"); got != "sess_abc" {
		t.Errorf("expected 'sess_abc', got %q", got)
	}

	// Different key
	mustSave(t, store, "tg:-100:42", "sess_def")
	if got := store.Lookup("tg:-100:42"); got != "sess_def" {
		t.Errorf("expected 'sess_def', got %q", got)
	}

	// Original key still there
	if got := store.Lookup("discord:123"); got != "sess_abc" {
		t.Errorf("expected 'sess_abc' still present, got %q", got)
	}
}

func TestSessionStore_SaveReturnsError(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Normal save should succeed
	if err := store.Save("discord:123", "sess_abc"); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestSessionStore_Overwrite(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mustSave(t, store, "discord:123", "sess_old")
	mustSave(t, store, "discord:123", "sess_new")

	if got := store.Lookup("discord:123"); got != "sess_new" {
		t.Errorf("expected 'sess_new' after overwrite, got %q", got)
	}
}

func TestSessionStore_Remove(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mustSave(t, store, "discord:123", "sess_abc")
	store.Remove("discord:123")

	if got := store.Lookup("discord:123"); got != "" {
		t.Errorf("expected empty after remove, got %q", got)
	}
}

func TestSessionStore_RemoveNonExistent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not panic
	store.Remove("nonexistent")
}

func TestSessionStore_Touch(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mustSave(t, store, "discord:123", "sess_abc")

	store.mu.RLock()
	before := store.records["discord:123"].LastActive
	store.mu.RUnlock()

	store.Touch("discord:123")

	store.mu.RLock()
	after := store.records["discord:123"].LastActive
	store.mu.RUnlock()

	if !after.After(before) {
		t.Errorf("expected LastActive to be updated, before=%v after=%v", before, after)
	}
}

func TestSessionStore_TouchNonExistent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not panic
	store.Touch("nonexistent")
}

func TestSessionStore_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Create store and save data
	store1, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustSave(t, store1, "discord:123", "sess_abc")
	mustSave(t, store1, "tg:-100:42", "sess_def")

	// Create new store from same directory — should load persisted data
	store2, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := store2.Lookup("discord:123"); got != "sess_abc" {
		t.Errorf("expected 'sess_abc' from persisted store, got %q", got)
	}
	if got := store2.Lookup("tg:-100:42"); got != "sess_def" {
		t.Errorf("expected 'sess_def' from persisted store, got %q", got)
	}
}

func TestSessionStore_PersistenceAfterRemove(t *testing.T) {
	dir := t.TempDir()

	store1, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustSave(t, store1, "discord:123", "sess_abc")
	mustSave(t, store1, "tg:-100:42", "sess_def")
	store1.Remove("discord:123")

	// Reload
	store2, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := store2.Lookup("discord:123"); got != "" {
		t.Errorf("expected empty after persisted remove, got %q", got)
	}
	if got := store2.Lookup("tg:-100:42"); got != "sess_def" {
		t.Errorf("expected 'sess_def' still present, got %q", got)
	}
}

func TestSessionStore_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	storeDir := filepath.Join(dir, ".quill")
	os.MkdirAll(storeDir, 0700)

	// Write corrupt JSON
	os.WriteFile(filepath.Join(storeDir, "sessions.json"), []byte("not json{{{"), 0600)

	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should start fresh, not crash
	if got := store.Lookup("anything"); got != "" {
		t.Errorf("expected empty store after corrupt file, got %q", got)
	}

	// Should be able to save new data
	mustSave(t, store, "discord:123", "sess_new")
	if got := store.Lookup("discord:123"); got != "sess_new" {
		t.Errorf("expected 'sess_new', got %q", got)
	}
}

func TestSessionStore_FileFormat(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mustSave(t, store, "discord:123", "sess_abc")

	// Read the file and verify it's valid JSON
	data, err := os.ReadFile(filepath.Join(dir, ".quill", "sessions.json"))
	if err != nil {
		t.Fatalf("failed to read store file: %v", err)
	}

	var records map[string]*SessionRecord
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("store file is not valid JSON: %v", err)
	}

	rec, ok := records["discord:123"]
	if !ok {
		t.Fatal("expected 'discord:123' key in store file")
	}
	if rec.SessionID != "sess_abc" {
		t.Errorf("expected session_id 'sess_abc', got %q", rec.SessionID)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("expected non-zero created_at")
	}
	if rec.LastActive.IsZero() {
		t.Error("expected non-zero last_active")
	}
}
