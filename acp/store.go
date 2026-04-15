package acp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionRecord is a single persisted session mapping.
type SessionRecord struct {
	SessionID  string    `json:"session_id"`
	CreatedAt  time.Time `json:"created_at"`
	LastActive time.Time `json:"last_active"`
}

// SessionStore persists threadKey → sessionId mappings to disk
// so sessions can be resumed after agent process restarts.
type SessionStore struct {
	path    string
	records map[string]*SessionRecord
	mu      sync.RWMutex
}

// NewSessionStore creates or loads a session store from the given directory.
// The store file is {dir}/.quill/sessions.json.
func NewSessionStore(dir string) (*SessionStore, error) {
	storeDir := filepath.Join(dir, ".quill")
	if err := os.MkdirAll(storeDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create store dir: %w", err)
	}

	storePath := filepath.Join(storeDir, "sessions.json")
	s := &SessionStore{
		path:    storePath,
		records: make(map[string]*SessionRecord),
	}

	// Load existing records if file exists
	data, err := os.ReadFile(storePath)
	if err == nil {
		if err := json.Unmarshal(data, &s.records); err != nil {
			slog.Warn("corrupt session store, starting fresh", "path", storePath, "error", err)
			s.records = make(map[string]*SessionRecord)
		}
	}

	return s, nil
}

// Save persists a threadKey → sessionId mapping.
func (s *SessionStore) Save(threadKey, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.records[threadKey] = &SessionRecord{
		SessionID:  sessionID,
		CreatedAt:  now,
		LastActive: now,
	}
	return s.flush()
}

// Touch updates the last_active timestamp for a thread key.
func (s *SessionStore) Touch(threadKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.records[threadKey]; ok {
		rec.LastActive = time.Now()
		s.flush() // best-effort, Touch failure is non-critical
	}
}

// Lookup returns the stored session ID for a thread key, or empty string if not found.
func (s *SessionStore) Lookup(threadKey string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if rec, ok := s.records[threadKey]; ok {
		return rec.SessionID
	}
	return ""
}

// Remove deletes a thread key from the store.
func (s *SessionStore) Remove(threadKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.records, threadKey)
	s.flush() // best-effort
}

// flush persists the in-memory records to disk using atomic write (temp file + rename).
// Note: no file locking is performed. If multiple Quill processes share the same
// workingDir, concurrent writes may cause lost updates. This is acceptable for the
// expected single-process deployment model.
func (s *SessionStore) flush() error {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		slog.Error("failed to marshal session store", "error", err)
		return err
	}
	// Atomic write: write to temp file then rename to avoid corruption
	// if the process is killed mid-write.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		slog.Error("failed to write session store tmp", "path", tmp, "error", err)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Error("failed to rename session store", "from", tmp, "to", s.path, "error", err)
		os.Remove(tmp)
		return err
	}
	return nil
}
