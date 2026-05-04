package cronjob

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const storeVersion = 1

// ErrLimitReached is returned by Add when max_per_thread is exceeded.
var ErrLimitReached = errors.New("cronjob: max jobs per thread reached")

type fileFormat struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

// Store is a thread-safe JSON-backed catalogue of cron jobs.
//
// Reads use an internal RWMutex; writes serialise the entire store to
// a temp file and rename it over the original path for atomic on-disk
// updates. Sub-millisecond write latency is fine because cron jobs are
// modified rarely (a handful of writes per day in a typical bot).
type Store struct {
	path string
	mu   sync.RWMutex
	jobs map[string]*Job // keyed by ID
}

// Open loads the store from disk. A missing file produces an empty
// store; a corrupt file is renamed to "<path>.broken-<unix>" and an
// empty store is returned.
func Open(path string) (*Store, error) {
	s := &Store{path: path, jobs: map[string]*Job{}}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cronjob store %q: %w", path, err)
	}

	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		brokenPath := fmt.Sprintf("%s.broken-%d", path, time.Now().Unix())
		if rnErr := os.Rename(path, brokenPath); rnErr != nil {
			slog.Warn("failed to move aside corrupt cronjob store", "path", path, "error", rnErr)
		} else {
			slog.Warn("cronjob store corrupt, moved aside and starting fresh",
				"path", path, "broken", brokenPath)
		}
		return s, nil
	}

	for i := range ff.Jobs {
		j := ff.Jobs[i]
		// Best-effort re-parse so the scheduler can use j.Parsed().
		if sch, _, err := ParseSchedule(j.Schedule, time.UTC, time.Minute, time.Now().UTC()); err == nil {
			j.parsed = sch
		} else {
			slog.Warn("cronjob: failed to re-parse stored schedule, job will be skipped",
				"id", j.ID, "schedule", j.Schedule, "error", err)
		}
		s.jobs[j.ID] = &j
	}
	return s, nil
}

// List returns a snapshot of jobs. If threadKey is non-empty, only jobs
// whose ThreadKey matches are returned.
func (s *Store) List(threadKey string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if threadKey != "" && j.ThreadKey != threadKey {
			continue
		}
		out = append(out, *j)
	}
	return out
}

// Add assigns an ID, persists the job, and returns the inserted copy.
// If the per-thread count would exceed maxPerThread, returns
// ErrLimitReached and leaves the store unchanged.
func (s *Store) Add(job Job, maxPerThread int) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if maxPerThread > 0 {
		count := 0
		for _, j := range s.jobs {
			if j.ThreadKey == job.ThreadKey {
				count++
			}
		}
		if count >= maxPerThread {
			return Job{}, ErrLimitReached
		}
	}

	if job.ID == "" {
		job.ID = randomID()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	stored := job
	s.jobs[stored.ID] = &stored
	if err := s.saveLocked(); err != nil {
		delete(s.jobs, stored.ID)
		return Job{}, err
	}
	return stored, nil
}

// Update replaces an existing job (matched by ID + ThreadKey).
func (s *Store) Update(job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.jobs[job.ID]
	if !ok || existing.ThreadKey != job.ThreadKey {
		return fmt.Errorf("cronjob: job %q not found in thread %q", job.ID, job.ThreadKey)
	}
	updated := job
	s.jobs[job.ID] = &updated
	return s.saveLocked()
}

// Remove deletes a job by id; ThreadKey is checked to prevent
// cross-thread deletion.
func (s *Store) Remove(threadKey, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.ThreadKey != threadKey {
		return fmt.Errorf("cronjob: job %q not found in thread %q", id, threadKey)
	}
	delete(s.jobs, id)
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	jobs := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, *j)
	}
	data, err := json.MarshalIndent(fileFormat{Version: storeVersion, Jobs: jobs}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cronjob store: %w", err)
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

func randomID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is system-level; fall back to time-based
		// id rather than panic.
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	}
	return hex.EncodeToString(b[:])
}
