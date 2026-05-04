package cronjob

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSchedulerFiresDueOneshotAndDeletes(t *testing.T) {
	s, _ := newStoreInTmp(t)
	now := time.Now().UTC()
	job := Job{
		ThreadKey: "tg:1",
		Schedule:  "in 1ms",
		Kind:      KindOneshot,
		NextFire:  now.Add(1 * time.Millisecond),
		parsed:    newOneshotSchedule(now.Add(1 * time.Millisecond)),
	}
	added, err := s.Add(job, 100)
	if err != nil {
		t.Fatal(err)
	}

	d := &countingDispatcher{}
	r := NewRegistry()
	r.Register("tg", d)
	gates := NewGates(50)
	defer gates.Close()

	sch := NewScheduler(s, r, gates, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sch.Run(ctx)

	// Wait up to 500ms for at least one fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if d.count() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d.count() != 1 {
		t.Errorf("Fire count=%d want 1", d.count())
	}

	// Oneshot should have been removed from the store.
	if got := s.List("tg:1"); len(got) != 0 {
		t.Errorf("oneshot job not deleted: %+v", got)
	}
	_ = added
}

func TestSchedulerSkipsDisabled(t *testing.T) {
	s, _ := newStoreInTmp(t)
	now := time.Now().UTC()
	job := Job{
		ThreadKey: "tg:1",
		Schedule:  "every 1ms",
		Kind:      KindInterval,
		NextFire:  now.Add(1 * time.Millisecond),
		Disabled:  true,
		parsed:    mustInterval(t, time.Minute),
	}
	if _, err := s.Add(job, 100); err != nil {
		t.Fatal(err)
	}

	d := &countingDispatcher{}
	r := NewRegistry()
	r.Register("tg", d)
	gates := NewGates(50)
	defer gates.Close()

	sch := NewScheduler(s, r, gates, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sch.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	if got := d.count(); got != 0 {
		t.Errorf("disabled job fired %d times", got)
	}
}

type countingDispatcher struct {
	mu sync.Mutex
	n  int
}

func (c *countingDispatcher) Fire(ctx context.Context, job Job) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func (c *countingDispatcher) NotifyDropped(ctx context.Context, job Job) {}

func (c *countingDispatcher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
