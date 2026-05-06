package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ServiceURLStore is a thread-safe, file-backed cache of Teams
// per-conversation serviceURLs.
//
// The Bot Framework only sends a conversation's serviceURL on inbound
// activities, so without persistence a process restart loses the
// destination for every cron-fired proactive message until each user
// happens to talk to the bot again. This store keeps the mapping on
// disk so cron jobs continue to fire across restarts.
//
// All methods are safe to call on a nil receiver — that path acts as
// an in-memory-only no-op store, which keeps unit tests that build
// `&Handler{...}` directly compiling without changes.
type ServiceURLStore struct {
	path string
	mu   sync.RWMutex
	urls map[string]string // conversationID -> serviceURL
}

const serviceURLStoreVersion = 1

type serviceURLFileFormat struct {
	Version int               `json:"version"`
	URLs    map[string]string `json:"urls"`
}

// OpenServiceURLStore loads the store from disk. An empty path produces
// a memory-only store (writes still update the in-memory map but never
// touch the filesystem). A missing file produces an empty store; a
// corrupt file is moved aside to "<path>.broken-<unix>" and an empty
// store is returned.
func OpenServiceURLStore(path string) (*ServiceURLStore, error) {
	s := &ServiceURLStore{path: path, urls: map[string]string{}}
	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read teams serviceURL store %q: %w", path, err)
	}

	var ff serviceURLFileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		brokenPath := fmt.Sprintf("%s.broken-%d", path, time.Now().Unix())
		if rnErr := os.Rename(path, brokenPath); rnErr != nil {
			slog.Warn("failed to move aside corrupt teams serviceURL store", "path", path, "error", rnErr)
		} else {
			slog.Warn("teams serviceURL store corrupt, moved aside and starting fresh",
				"path", path, "broken", brokenPath)
		}
		return s, nil
	}

	if ff.URLs != nil {
		for k, v := range ff.URLs {
			if k == "" || v == "" {
				continue
			}
			s.urls[k] = v
		}
	}
	return s, nil
}

// Get returns the cached serviceURL for a conversation, or "" if unknown.
// Safe to call on a nil receiver.
func (s *ServiceURLStore) Get(conversationID string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.urls[conversationID]
}

// Set updates the cached serviceURL for a conversation. Empty inputs are
// ignored. Writes to disk only when the value changes, so the steady
// state of "every inbound activity refreshes the same URL" causes no
// disk churn. Safe to call on a nil receiver (no-op).
func (s *ServiceURLStore) Set(conversationID, serviceURL string) error {
	if s == nil || conversationID == "" || serviceURL == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.urls[conversationID] == serviceURL {
		return nil
	}
	s.urls[conversationID] = serviceURL
	if s.path == "" {
		return nil
	}
	return s.saveLocked()
}

// Len returns the number of cached entries. Useful for startup logs.
// Safe to call on a nil receiver (returns 0).
func (s *ServiceURLStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.urls)
}

func (s *ServiceURLStore) saveLocked() error {
	data, err := json.MarshalIndent(serviceURLFileFormat{
		Version: serviceURLStoreVersion,
		URLs:    s.urls,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal teams serviceURL store: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
