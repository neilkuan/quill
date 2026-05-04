package cronjob

import (
	"context"
	"log/slog"
	"time"
)

// Scheduler periodically scans the Store for jobs whose NextFire has
// elapsed, submits them through the Gates, and either reschedules
// (cron / interval) or deletes (oneshot) them.
type Scheduler struct {
	store    *Store
	registry *Registry
	gates    *Gates
	tick     time.Duration
}

func NewScheduler(store *Store, registry *Registry, gates *Gates, tick time.Duration) *Scheduler {
	if tick <= 0 {
		tick = 1 * time.Second
	}
	return &Scheduler{store: store, registry: registry, gates: gates, tick: tick}
}

// Run blocks until ctx is cancelled. Intended to be called in its own
// goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			s.scanAndDispatch(ctx, t.UTC())
		}
	}
}

func (s *Scheduler) scanAndDispatch(ctx context.Context, now time.Time) {
	for _, job := range s.store.List("") {
		if job.Disabled {
			continue
		}
		if job.NextFire.IsZero() || job.NextFire.After(now) {
			continue
		}
		if job.Parsed() == nil {
			slog.Warn("cron job has nil parsed schedule, skipping",
				"id", job.ID, "schedule", job.Schedule)
			continue
		}

		prefix := PrefixOf(job.ThreadKey)
		dispatcher := s.registry.Get(prefix)
		if dispatcher == nil {
			slog.Warn("cron job: no dispatcher registered for prefix",
				"id", job.ID, "thread_key", job.ThreadKey, "prefix", prefix)
			continue
		}

		s.gates.Submit(ctx, dispatcher, job)

		// Compute next fire and persist.
		updated := job
		updated.LastFire = now
		next := job.Parsed().Next(now)
		if next.IsZero() {
			if err := s.store.Remove(updated.ThreadKey, updated.ID); err != nil {
				slog.Warn("cron remove after oneshot fire", "id", updated.ID, "error", err)
			}
			continue
		}
		updated.NextFire = next
		if err := s.store.Update(updated); err != nil {
			slog.Warn("cron update after fire", "id", updated.ID, "error", err)
		}
	}
}
