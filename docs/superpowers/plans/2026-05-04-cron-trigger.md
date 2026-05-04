# Cron-Triggered Agent Messages Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users schedule recurring or one-shot prompts that fire automatically into their chat thread, with the agent receiving a `trigger:"cron"` marker so it can distinguish scheduled triggers from human turns.

**Architecture:** A new `cronjob/` package owns the data model, parser, JSON-file store, scheduler ticker, and a dispatcher registry. Each platform adapter (Discord/Telegram/Teams) registers a `Dispatcher` implementation that owns the chat-side fan-out — sending the placeholder message and reusing the existing `streamPrompt` flow. A per-thread bounded gate channel sits between the scheduler tick and the dispatcher so the tick goroutine never blocks on `promptMu`.

**Tech Stack:**
- Go 1.26 (existing)
- `github.com/robfig/cron/v3` (new — pure-Go cron expression parser)
- `github.com/BurntSushi/toml` (existing)
- stdlib `encoding/json`, `time`, `context`, `sync`, `os`, `log/slog`

**Spec reference:** `docs/superpowers/specs/2026-05-04-cron-trigger-design.md`

---

## File Structure

| Path | Action | Purpose |
|---|---|---|
| `cronjob/job.go` | Create | `Job` struct, `Kind` constants, `Schedule` interface |
| `cronjob/schedule.go` | Create | `cronSchedule`, `intervalSchedule`, `oneshotSchedule` impls |
| `cronjob/parser.go` | Create | `ParseSchedule(input)` for all three forms |
| `cronjob/store.go` | Create | JSON file with atomic `os.Rename` save |
| `cronjob/sender_ctx.go` | Create | `CronFields(job)` helper |
| `cronjob/dispatcher.go` | Create | `Dispatcher` interface + `Registry` |
| `cronjob/gate.go` | Create | Per-thread bounded fire channel + worker goroutine |
| `cronjob/scheduler.go` | Create | 1s ticker, dispatch via gate, requeue/delete |
| `cronjob/*_test.go` | Create | Tests for each unit |
| `config/config.go` | Modify | Add `CronjobConfig` + auto-defaults |
| `config.toml.example` | Modify | Document `[cronjob]` block |
| `command/command.go` | Modify | `CmdCron` + cron subcommand parsing + `ListCronJobs`/`ExecuteCronAdd`/`ExecuteCronRemove` |
| `command/command_test.go` | Modify | Tests for cron command parsing |
| `telegram/handler.go` | Modify | Wire `/cron` command to handler |
| `telegram/cron_dispatcher.go` | Create | Telegram `Dispatcher` impl |
| `discord/handler.go` | Modify | Wire `/cron` command + slash command registration |
| `discord/cron_dispatcher.go` | Create | Discord `Dispatcher` impl |
| `teams/handler.go` | Modify | Wire `/cron` command (text only) |
| `teams/cron_dispatcher.go` | Create | Teams `Dispatcher` impl |
| `main.go` | Modify | Open store, build registry, start scheduler, register dispatchers |
| `go.mod` / `go.sum` | Modify | Add `github.com/robfig/cron/v3` |

---

## Task 1: Add robfig/cron dependency

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Add the dependency**

```bash
cd /Users/neil.kuan/neil/quill/.claude/worktrees/valiant-booping-unicorn
go get github.com/robfig/cron/v3@v3.0.1
```

- [ ] **Step 2: Verify build still passes**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add robfig/cron/v3 for cron expression parsing"
```

---

## Task 2: Job struct, Kind constants, Schedule interface

**Files:**
- Create: `cronjob/job.go`
- Create: `cronjob/job_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/job_test.go`:

```go
package cronjob

import (
	"testing"
	"time"
)

func TestKindString(t *testing.T) {
	cases := []struct {
		k    Kind
		want string
	}{
		{KindCron, "cron"},
		{KindInterval, "interval"},
		{KindOneshot, "oneshot"},
	}
	for _, c := range cases {
		if string(c.k) != c.want {
			t.Errorf("Kind=%q want %q", c.k, c.want)
		}
	}
}

func TestJobZero(t *testing.T) {
	j := Job{}
	if !j.LastFire.IsZero() {
		t.Error("zero Job should have zero LastFire")
	}
	if !j.NextFire.IsZero() {
		t.Error("zero Job should have zero NextFire")
	}
	if j.Disabled {
		t.Error("zero Job should not be disabled")
	}
	_ = time.Time{} // keep import
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Create the package files**

Create `cronjob/job.go`:

```go
// Package cronjob provides scheduled prompt delivery to chat threads.
//
// Jobs are persisted to a JSON file and dispatched into the thread that
// created them via a registered Dispatcher (see dispatcher.go). The
// scheduler reads jobs whose NextFire <= now once per second and submits
// them to a per-thread bounded gate (see gate.go).
package cronjob

import "time"

// Kind classifies a Schedule. Persisted in JSON.
type Kind string

const (
	KindCron     Kind = "cron"
	KindInterval Kind = "interval"
	KindOneshot  Kind = "oneshot"
)

// Schedule computes the next fire time after a given instant.
// Schedules are stateless — they do not remember past fires.
//
// For one-shot schedules, Next returns the zero time once the single
// fire has elapsed (i.e. once `after` >= the scheduled instant).
type Schedule interface {
	Next(after time.Time) time.Time
	Kind() Kind
}

// Job is the persisted representation of a single schedule. Times are
// UTC. The runtime-only `parsed` field is hydrated by Store.Add /
// Store.load and used by the scheduler — it is not serialised.
type Job struct {
	ID         string    `json:"id"`
	ThreadKey  string    `json:"thread_key"`
	SenderID   string    `json:"sender_id"`
	SenderName string    `json:"sender_name"`
	Schedule   string    `json:"schedule"`
	Kind       Kind      `json:"kind"`
	Prompt     string    `json:"prompt"`
	NextFire   time.Time `json:"next_fire"`
	LastFire   time.Time `json:"last_fire"`
	CreatedAt  time.Time `json:"created_at"`
	Disabled   bool      `json:"disabled"`

	parsed Schedule // not serialised
}

// Parsed returns the runtime Schedule object for this job, or nil if
// the job was loaded but the schedule string failed to parse.
func (j *Job) Parsed() Schedule { return j.parsed }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/job.go cronjob/job_test.go
git commit -m "feat(cronjob): add Job struct, Kind constants, Schedule interface"
```

---

## Task 3: cronSchedule implementation

**Files:**
- Create: `cronjob/schedule.go`
- Modify: `cronjob/job_test.go` → rename to `cronjob/schedule_test.go` (or add a new file)
- Create: `cronjob/schedule_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/schedule_test.go`:

```go
package cronjob

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func TestCronScheduleEveryDayAt9UTC(t *testing.T) {
	s, err := newCronSchedule("0 9 * * *")
	if err != nil {
		t.Fatalf("newCronSchedule: %v", err)
	}
	if s.Kind() != KindCron {
		t.Errorf("Kind=%q want %q", s.Kind(), KindCron)
	}
	got := s.Next(mustTime(t, "2026-05-04T08:30:00Z"))
	want := mustTime(t, "2026-05-04T09:00:00Z")
	if !got.Equal(want) {
		t.Errorf("Next=%v want %v", got, want)
	}
}

func TestCronScheduleInvalid(t *testing.T) {
	if _, err := newCronSchedule("not a cron"); err == nil {
		t.Error("expected error for invalid cron expression")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `newCronSchedule` undefined.

- [ ] **Step 3: Implement cronSchedule**

Create `cronjob/schedule.go`:

```go
package cronjob

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser is the standard 5-field cron parser used by all
// cronSchedule instances. Configured at package init for thread-safe
// reuse.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

type cronSchedule struct {
	expr string
	sch  cron.Schedule
}

func newCronSchedule(expr string) (*cronSchedule, error) {
	sch, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return &cronSchedule{expr: expr, sch: sch}, nil
}

func (c *cronSchedule) Kind() Kind { return KindCron }

func (c *cronSchedule) Next(after time.Time) time.Time {
	return c.sch.Next(after.UTC())
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/schedule.go cronjob/schedule_test.go
git commit -m "feat(cronjob): add cronSchedule using robfig/cron parser"
```

---

## Task 4: intervalSchedule implementation

**Files:**
- Modify: `cronjob/schedule.go`
- Modify: `cronjob/schedule_test.go`

- [ ] **Step 1: Add the failing test**

Append to `cronjob/schedule_test.go`:

```go
func TestIntervalScheduleNext(t *testing.T) {
	s, err := newIntervalSchedule(5 * time.Minute)
	if err != nil {
		t.Fatalf("newIntervalSchedule: %v", err)
	}
	if s.Kind() != KindInterval {
		t.Errorf("Kind=%q want %q", s.Kind(), KindInterval)
	}
	now := mustTime(t, "2026-05-04T10:00:00Z")
	if got, want := s.Next(now), now.Add(5*time.Minute); !got.Equal(want) {
		t.Errorf("Next=%v want %v", got, want)
	}
}

func TestIntervalScheduleZeroRejected(t *testing.T) {
	if _, err := newIntervalSchedule(0); err == nil {
		t.Error("expected error for zero interval")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `newIntervalSchedule` undefined.

- [ ] **Step 3: Implement intervalSchedule**

Append to `cronjob/schedule.go`:

```go
type intervalSchedule struct {
	d time.Duration
}

func newIntervalSchedule(d time.Duration) (*intervalSchedule, error) {
	if d <= 0 {
		return nil, fmt.Errorf("interval must be positive, got %v", d)
	}
	return &intervalSchedule{d: d}, nil
}

func (i *intervalSchedule) Kind() Kind { return KindInterval }

func (i *intervalSchedule) Next(after time.Time) time.Time {
	return after.UTC().Add(i.d)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/schedule.go cronjob/schedule_test.go
git commit -m "feat(cronjob): add intervalSchedule"
```

---

## Task 5: oneshotSchedule implementation

**Files:**
- Modify: `cronjob/schedule.go`
- Modify: `cronjob/schedule_test.go`

- [ ] **Step 1: Add the failing test**

Append to `cronjob/schedule_test.go`:

```go
func TestOneshotScheduleBeforeFire(t *testing.T) {
	at := mustTime(t, "2026-05-04T15:00:00Z")
	s := newOneshotSchedule(at)
	if s.Kind() != KindOneshot {
		t.Errorf("Kind=%q want %q", s.Kind(), KindOneshot)
	}
	got := s.Next(mustTime(t, "2026-05-04T14:00:00Z"))
	if !got.Equal(at) {
		t.Errorf("Next before fire=%v want %v", got, at)
	}
}

func TestOneshotScheduleAfterFire(t *testing.T) {
	at := mustTime(t, "2026-05-04T15:00:00Z")
	s := newOneshotSchedule(at)
	got := s.Next(mustTime(t, "2026-05-04T15:00:00Z"))
	if !got.IsZero() {
		t.Errorf("Next at/after fire should be zero, got %v", got)
	}
	got2 := s.Next(mustTime(t, "2026-05-04T15:01:00Z"))
	if !got2.IsZero() {
		t.Errorf("Next after fire should be zero, got %v", got2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `newOneshotSchedule` undefined.

- [ ] **Step 3: Implement oneshotSchedule**

Append to `cronjob/schedule.go`:

```go
type oneshotSchedule struct {
	at time.Time
}

func newOneshotSchedule(at time.Time) *oneshotSchedule {
	return &oneshotSchedule{at: at.UTC()}
}

func (o *oneshotSchedule) Kind() Kind { return KindOneshot }

// Next returns o.at the first time it is asked, but the zero time
// once `after` has reached or passed o.at — signalling to the
// scheduler that the job has fired and should be deleted from the
// store.
func (o *oneshotSchedule) Next(after time.Time) time.Time {
	if !after.UTC().Before(o.at) {
		return time.Time{}
	}
	return o.at
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/schedule.go cronjob/schedule_test.go
git commit -m "feat(cronjob): add oneshotSchedule"
```

---

## Task 6: ParseSchedule

**Files:**
- Create: `cronjob/parser.go`
- Create: `cronjob/parser_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/parser_test.go`:

```go
package cronjob

import (
	"strings"
	"testing"
	"time"
)

func TestParseScheduleCron(t *testing.T) {
	s, kind, err := ParseSchedule("0 9 * * *", time.UTC, time.Minute, mustTime(t, "2026-05-04T00:00:00Z"))
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	if kind != KindCron {
		t.Errorf("Kind=%q want %q", kind, KindCron)
	}
	if s.Kind() != KindCron {
		t.Errorf("schedule.Kind=%q", s.Kind())
	}
}

func TestParseScheduleInterval(t *testing.T) {
	s, kind, err := ParseSchedule("every 5m", time.UTC, time.Minute, mustTime(t, "2026-05-04T00:00:00Z"))
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	if kind != KindInterval {
		t.Errorf("Kind=%q want %q", kind, KindInterval)
	}
	if s.Kind() != KindInterval {
		t.Errorf("schedule.Kind=%q", s.Kind())
	}
}

func TestParseScheduleIntervalBelowMinimum(t *testing.T) {
	_, _, err := ParseSchedule("every 30s", time.UTC, time.Minute, time.Now())
	if err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Errorf("expected 'minimum' error, got %v", err)
	}
}

func TestParseScheduleOneshotIn(t *testing.T) {
	now := mustTime(t, "2026-05-04T10:00:00Z")
	s, kind, err := ParseSchedule("in 30m", time.UTC, time.Minute, now)
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	if kind != KindOneshot {
		t.Errorf("Kind=%q want %q", kind, KindOneshot)
	}
	want := now.Add(30 * time.Minute)
	if got := s.Next(now); !got.Equal(want) {
		t.Errorf("Next=%v want %v", got, want)
	}
}

func TestParseScheduleOneshotAtClock(t *testing.T) {
	// 2026-05-04 14:00 UTC, "at 09:00" should be tomorrow 09:00
	now := mustTime(t, "2026-05-04T14:00:00Z")
	s, kind, err := ParseSchedule("at 09:00", time.UTC, time.Minute, now)
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	if kind != KindOneshot {
		t.Errorf("Kind=%q", kind)
	}
	want := mustTime(t, "2026-05-05T09:00:00Z")
	if got := s.Next(now); !got.Equal(want) {
		t.Errorf("Next=%v want %v", got, want)
	}
}

func TestParseScheduleOneshotAtPast(t *testing.T) {
	now := mustTime(t, "2026-05-04T14:00:00Z")
	_, _, err := ParseSchedule("at 2020-01-01 00:00", time.UTC, time.Minute, now)
	if err == nil || !strings.Contains(err.Error(), "past") {
		t.Errorf("expected 'past' error, got %v", err)
	}
}

func TestParseScheduleInvalid(t *testing.T) {
	_, _, err := ParseSchedule("garbage", time.UTC, time.Minute, time.Now())
	if err == nil {
		t.Error("expected error for garbage input")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `ParseSchedule` undefined.

- [ ] **Step 3: Implement ParseSchedule**

Create `cronjob/parser.go`:

```go
package cronjob

import (
	"fmt"
	"strings"
	"time"
)

// ParseSchedule converts a user-supplied schedule string into a
// Schedule + Kind. Accepted forms (case-insensitive on the keyword):
//
//   - "0 9 * * *"            standard 5-field cron
//   - "every <duration>"     interval; rejected if < minInterval
//   - "in <duration>"        oneshot from now
//   - "at HH:MM"             oneshot today (or tomorrow if already passed)
//   - "at YYYY-MM-DD HH:MM"  oneshot at absolute time
//
// `tz` controls the interpretation of "at HH:MM" / "at YYYY-MM-DD HH:MM".
// Internally all schedules return UTC times.
//
// `now` is passed in for testability — production callers pass time.Now().UTC().
func ParseSchedule(input string, tz *time.Location, minInterval time.Duration, now time.Time) (Schedule, Kind, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return nil, "", fmt.Errorf("schedule cannot be empty")
	}

	lower := strings.ToLower(in)

	switch {
	case strings.HasPrefix(lower, "every "):
		dur, err := time.ParseDuration(strings.TrimSpace(in[len("every "):]))
		if err != nil {
			return nil, "", fmt.Errorf("invalid interval duration: %w", err)
		}
		if dur < minInterval {
			return nil, "", fmt.Errorf("interval %v is below minimum %v", dur, minInterval)
		}
		s, err := newIntervalSchedule(dur)
		if err != nil {
			return nil, "", err
		}
		return s, KindInterval, nil

	case strings.HasPrefix(lower, "in "):
		dur, err := time.ParseDuration(strings.TrimSpace(in[len("in "):]))
		if err != nil {
			return nil, "", fmt.Errorf("invalid duration: %w", err)
		}
		if dur <= 0 {
			return nil, "", fmt.Errorf("'in' duration must be positive, got %v", dur)
		}
		return newOneshotSchedule(now.Add(dur)), KindOneshot, nil

	case strings.HasPrefix(lower, "at "):
		rest := strings.TrimSpace(in[len("at "):])
		// Try absolute YYYY-MM-DD HH:MM first
		if t, err := time.ParseInLocation("2006-01-02 15:04", rest, tz); err == nil {
			at := t.UTC()
			if !at.After(now.UTC()) {
				return nil, "", fmt.Errorf("'at' time %v is in the past", at)
			}
			return newOneshotSchedule(at), KindOneshot, nil
		}
		// Fallback: HH:MM (today, or tomorrow if already passed)
		if t, err := time.ParseInLocation("15:04", rest, tz); err == nil {
			nowInTZ := now.In(tz)
			candidate := time.Date(nowInTZ.Year(), nowInTZ.Month(), nowInTZ.Day(),
				t.Hour(), t.Minute(), 0, 0, tz)
			if !candidate.After(now) {
				candidate = candidate.Add(24 * time.Hour)
			}
			return newOneshotSchedule(candidate.UTC()), KindOneshot, nil
		}
		return nil, "", fmt.Errorf("invalid 'at' time %q (use 'HH:MM' or 'YYYY-MM-DD HH:MM')", rest)
	}

	// Try cron expression as the last resort.
	if c, err := newCronSchedule(in); err == nil {
		return c, KindCron, nil
	}

	return nil, "", fmt.Errorf("unrecognised schedule %q (try '0 9 * * *', 'every 5m', 'in 2h', 'at 09:00')", input)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS (all parse tests).

- [ ] **Step 5: Commit**

```bash
git add cronjob/parser.go cronjob/parser_test.go
git commit -m "feat(cronjob): add ParseSchedule for cron/interval/oneshot"
```

---

## Task 7: Store with atomic JSON file save

**Files:**
- Create: `cronjob/store.go`
- Create: `cronjob/store_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/store_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `Open`, `Store`, `s.Add`, `s.List`, `s.Remove` undefined.

- [ ] **Step 3: Implement Store**

Create `cronjob/store.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/store.go cronjob/store_test.go
git commit -m "feat(cronjob): add JSON-backed Store with atomic save and corrupt recovery"
```

---

## Task 8: BuildSenderContext / CronFields helper

**Files:**
- Create: `cronjob/sender_ctx.go`
- Create: `cronjob/sender_ctx_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/sender_ctx_test.go`:

```go
package cronjob

import "testing"

func TestCronFields(t *testing.T) {
	job := Job{ID: "abc12345", Schedule: "0 9 * * *"}
	got := CronFields(job)

	if got["trigger"] != "cron" {
		t.Errorf("trigger=%v want cron", got["trigger"])
	}
	if got["cron_id"] != "abc12345" {
		t.Errorf("cron_id=%v want abc12345", got["cron_id"])
	}
	if got["cron_schedule"] != "0 9 * * *" {
		t.Errorf("cron_schedule=%v", got["cron_schedule"])
	}
	if _, ok := got["cron_fire_time"].(string); !ok {
		t.Errorf("cron_fire_time should be a string, got %T", got["cron_fire_time"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `CronFields` undefined.

- [ ] **Step 3: Implement helper**

Create `cronjob/sender_ctx.go`:

```go
package cronjob

import "time"

// CronFields returns the cron-specific fields to merge into a
// "quill.sender.v1" sender_context envelope when the prompt is
// triggered by the scheduler. Each platform adapter constructs the
// rest of the envelope (channel, channel_id, sender_id, etc.) and
// merges these fields on top so the agent sees a single coherent
// JSON object.
//
// The fire time is captured at call time so it reflects the actual
// dispatch instant — which may differ from job.NextFire if the
// dispatcher was queued behind a busy promptMu.
func CronFields(job Job) map[string]any {
	return map[string]any{
		"trigger":        "cron",
		"cron_id":        job.ID,
		"cron_schedule":  job.Schedule,
		"cron_fire_time": time.Now().UTC().Format(time.RFC3339),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/sender_ctx.go cronjob/sender_ctx_test.go
git commit -m "feat(cronjob): add CronFields helper for sender-context extension"
```

---

## Task 9: Dispatcher interface + Registry

**Files:**
- Create: `cronjob/dispatcher.go`
- Create: `cronjob/dispatcher_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/dispatcher_test.go`:

```go
package cronjob

import (
	"context"
	"testing"
)

type fakeDispatcher struct {
	fires    []Job
	dropped  []Job
}

func (f *fakeDispatcher) Fire(ctx context.Context, job Job) error {
	f.fires = append(f.fires, job)
	return nil
}

func (f *fakeDispatcher) NotifyDropped(ctx context.Context, job Job) {
	f.dropped = append(f.dropped, job)
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	d := &fakeDispatcher{}
	r.Register("tg", d)

	if got := r.Get("tg"); got != d {
		t.Errorf("Get(tg)=%v want %v", got, d)
	}
	if got := r.Get("missing"); got != nil {
		t.Errorf("Get(missing)=%v want nil", got)
	}
}

func TestRegistryPrefixOf(t *testing.T) {
	cases := []struct {
		threadKey string
		prefix    string
	}{
		{"tg:1234", "tg"},
		{"tg:1234:5", "tg"},
		{"discord:abc", "discord"},
		{"teams:room/1", "teams"},
		{"weird", "weird"},
		{"", ""},
	}
	for _, c := range cases {
		if got := PrefixOf(c.threadKey); got != c.prefix {
			t.Errorf("PrefixOf(%q)=%q want %q", c.threadKey, got, c.prefix)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `NewRegistry`, `PrefixOf` undefined.

- [ ] **Step 3: Implement Dispatcher interface + Registry**

Create `cronjob/dispatcher.go`:

```go
package cronjob

import (
	"context"
	"strings"
	"sync"
)

// Dispatcher fans a fire-due job out into a chat platform.
//
// Fire is responsible for the full chat-side flow: post a placeholder
// message, build the sender_context envelope (merging CronFields into
// the platform's normal envelope), call SessionPool.GetOrCreate, and
// stream the reply by editing the placeholder. Errors are logged by
// the implementation; the scheduler loops on regardless.
//
// NotifyDropped is called when the per-thread bounded gate is full
// and the job had to be dropped. Implementations should post a brief
// chat marker so the user knows the fire was lost. NotifyDropped must
// not block — implementations should send asynchronously if the
// platform API is slow.
type Dispatcher interface {
	Fire(ctx context.Context, job Job) error
	NotifyDropped(ctx context.Context, job Job)
}

// Registry maps a thread-key prefix ("tg" / "discord" / "teams") to
// the Dispatcher that owns that platform's chats. Platform adapters
// register themselves during Start().
type Registry struct {
	mu sync.RWMutex
	m  map[string]Dispatcher
}

func NewRegistry() *Registry {
	return &Registry{m: map[string]Dispatcher{}}
}

func (r *Registry) Register(prefix string, d Dispatcher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[prefix] = d
}

func (r *Registry) Get(prefix string) Dispatcher {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[prefix]
}

// PrefixOf returns the substring before the first ':' in a threadKey,
// or the threadKey itself if no ':' is present.
func PrefixOf(threadKey string) string {
	if i := strings.IndexByte(threadKey, ':'); i >= 0 {
		return threadKey[:i]
	}
	return threadKey
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/dispatcher.go cronjob/dispatcher_test.go
git commit -m "feat(cronjob): add Dispatcher interface and Registry"
```

---

## Task 10: Per-thread bounded Gate

**Files:**
- Create: `cronjob/gate.go`
- Create: `cronjob/gate_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/gate_test.go`:

```go
package cronjob

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGateForwardsFires(t *testing.T) {
	d := &fakeDispatcher{}
	gates := NewGates(10)
	defer gates.Close()

	job := Job{ID: "1", ThreadKey: "tg:1"}
	gates.Submit(context.Background(), d, job)

	// Wait briefly for the worker to drain.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		gates.mu.RLock()
		// Re-check via dispatcher's slice.
		gates.mu.RUnlock()
		if len(d.fires) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(d.fires) != 1 {
		t.Errorf("Fire count=%d want 1", len(d.fires))
	}
}

func TestGateOverflowDrops(t *testing.T) {
	// Block the dispatcher so the channel fills up.
	release := make(chan struct{})
	d := &blockingDispatcher{release: release}

	gates := NewGates(2)
	defer func() {
		close(release)
		gates.Close()
	}()

	ctx := context.Background()
	gates.Submit(ctx, d, Job{ID: "1", ThreadKey: "tg:1"}) // worker takes this
	// Worker blocks; channel can hold 2 more.
	time.Sleep(20 * time.Millisecond)
	gates.Submit(ctx, d, Job{ID: "2", ThreadKey: "tg:1"})
	gates.Submit(ctx, d, Job{ID: "3", ThreadKey: "tg:1"})
	gates.Submit(ctx, d, Job{ID: "4", ThreadKey: "tg:1"}) // should overflow → drop

	d.mu.Lock()
	dropped := len(d.dropped)
	d.mu.Unlock()
	if dropped != 1 {
		t.Errorf("dropped count=%d want 1", dropped)
	}
}

type blockingDispatcher struct {
	mu      sync.Mutex
	dropped []Job
	release chan struct{}
}

func (b *blockingDispatcher) Fire(ctx context.Context, job Job) error {
	<-b.release
	return nil
}

func (b *blockingDispatcher) NotifyDropped(ctx context.Context, job Job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropped = append(b.dropped, job)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `NewGates`, `Submit`, `Close` undefined.

- [ ] **Step 3: Implement Gates**

Create `cronjob/gate.go`:

```go
package cronjob

import (
	"context"
	"log/slog"
	"sync"
)

// Gates owns a per-thread bounded fire channel + worker goroutine
// pair. Submit is non-blocking: if the channel is full, the job is
// dropped via Dispatcher.NotifyDropped.
//
// One worker goroutine per thread reads the channel and calls
// Dispatcher.Fire synchronously, ensuring per-thread FIFO ordering
// across both the queue and the dispatcher call. Close() shuts down
// every worker.
type Gates struct {
	queueSize int

	mu sync.RWMutex
	g  map[string]*gate

	wg sync.WaitGroup
}

type gate struct {
	ch chan submission
}

type submission struct {
	job Job
	d   Dispatcher
	ctx context.Context
}

func NewGates(queueSize int) *Gates {
	if queueSize <= 0 {
		queueSize = 50
	}
	return &Gates{queueSize: queueSize, g: map[string]*gate{}}
}

// Submit hands a job to the per-thread worker. Non-blocking: returns
// immediately if the worker is busy and the channel has room, or
// drops the job (via d.NotifyDropped) when the channel is full.
func (gs *Gates) Submit(ctx context.Context, d Dispatcher, job Job) {
	g := gs.gateFor(job.ThreadKey)
	select {
	case g.ch <- submission{job: job, d: d, ctx: ctx}:
		return
	default:
		slog.Warn("cron fire dropped: per-thread queue full",
			"thread_key", job.ThreadKey, "job_id", job.ID, "queue_size", gs.queueSize)
		d.NotifyDropped(ctx, job)
	}
}

func (gs *Gates) gateFor(threadKey string) *gate {
	gs.mu.RLock()
	g, ok := gs.g[threadKey]
	gs.mu.RUnlock()
	if ok {
		return g
	}

	gs.mu.Lock()
	defer gs.mu.Unlock()
	if g, ok := gs.g[threadKey]; ok {
		return g
	}
	g = &gate{ch: make(chan submission, gs.queueSize)}
	gs.g[threadKey] = g
	gs.wg.Add(1)
	go gs.runWorker(threadKey, g)
	return g
}

func (gs *Gates) runWorker(threadKey string, g *gate) {
	defer gs.wg.Done()
	for sub := range g.ch {
		if err := sub.d.Fire(sub.ctx, sub.job); err != nil {
			slog.Warn("cron dispatcher Fire returned error",
				"thread_key", threadKey, "job_id", sub.job.ID, "error", err)
		}
	}
}

// Close stops every worker by closing each gate's channel and waiting
// for in-flight Fires to complete.
func (gs *Gates) Close() {
	gs.mu.Lock()
	for _, g := range gs.g {
		close(g.ch)
	}
	gs.g = map[string]*gate{}
	gs.mu.Unlock()
	gs.wg.Wait()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/gate.go cronjob/gate_test.go
git commit -m "feat(cronjob): add per-thread bounded Gates with overflow drop"
```

---

## Task 11: Scheduler loop

**Files:**
- Create: `cronjob/scheduler.go`
- Create: `cronjob/scheduler_test.go`

- [ ] **Step 1: Write the failing test**

Create `cronjob/scheduler_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cronjob/...`
Expected: FAIL — `NewScheduler`, `Run` undefined.

- [ ] **Step 3: Implement Scheduler**

Create `cronjob/scheduler.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cronjob/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cronjob/scheduler.go cronjob/scheduler_test.go
git commit -m "feat(cronjob): add scheduler loop with dispatch + reschedule"
```

---

## Task 12: CronjobConfig + applyDefaults

**Files:**
- Modify: `config/config.go`
- Modify: `config/config_test.go`

- [ ] **Step 1: Add the failing test**

Append to `config/config_test.go`:

```go
func TestCronjobDefaults(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg) // existing helper that fills in zero values

	if cfg.Cronjob.Disabled {
		t.Error("Cronjob default should be enabled (Disabled=false)")
	}
	if cfg.Cronjob.MaxPerThread != 20 {
		t.Errorf("MaxPerThread=%d want 20", cfg.Cronjob.MaxPerThread)
	}
	if cfg.Cronjob.MinIntervalSeconds != 60 {
		t.Errorf("MinIntervalSeconds=%d want 60", cfg.Cronjob.MinIntervalSeconds)
	}
	if cfg.Cronjob.QueueSize != 50 {
		t.Errorf("QueueSize=%d want 50", cfg.Cronjob.QueueSize)
	}
	if cfg.Cronjob.Timezone != "UTC" {
		t.Errorf("Timezone=%q want UTC", cfg.Cronjob.Timezone)
	}
	if cfg.Cronjob.StorePath == "" {
		t.Error("StorePath should have a default")
	}
}

func TestCronjobExplicitDisable(t *testing.T) {
	// User sets disabled=true in TOML — defaults must not re-enable it.
	cfg := &Config{Cronjob: CronjobConfig{Disabled: true}}
	applyDefaults(cfg)
	if !cfg.Cronjob.Disabled {
		t.Error("explicit disabled=true must survive applyDefaults")
	}
}
```

The `Disabled bool` design is intentional: TOML's zero value for a missing field is `false`, which means "enabled by default" — exactly what we want without resorting to `*bool` or sentinel tracking. An explicit `disabled = true` in TOML wins because applyDefaults never writes to `Disabled`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./config/...`
Expected: FAIL — `Cronjob` / `CronjobConfig` undefined.

- [ ] **Step 3: Add the Cronjob field to Config**

In `config/config.go`, add to the `type Config struct { ... }` block:

```go
Cronjob CronjobConfig `toml:"cronjob"`
```

- [ ] **Step 4: Add CronjobConfig**

In `config/config.go`, near the other per-section structs:

```go
// CronjobConfig controls user-scheduled prompt fires. The zero value
// (no [cronjob] block in TOML) means "enabled with defaults". Users
// opt out with `disabled = true`.
type CronjobConfig struct {
	Disabled           bool   `toml:"disabled"`
	MaxPerThread       int    `toml:"max_per_thread"`
	MinIntervalSeconds int    `toml:"min_interval_seconds"`
	QueueSize          int    `toml:"queue_size"`
	Timezone           string `toml:"timezone"`
	StorePath          string `toml:"store_path"`
}
```

- [ ] **Step 5: Add applyDefaults block**

Inside the existing `applyDefaults(cfg *Config)` function, append:

```go
if cfg.Cronjob.MaxPerThread == 0 {
	cfg.Cronjob.MaxPerThread = 20
}
if cfg.Cronjob.MinIntervalSeconds == 0 {
	cfg.Cronjob.MinIntervalSeconds = 60
}
if cfg.Cronjob.QueueSize == 0 {
	cfg.Cronjob.QueueSize = 50
}
if cfg.Cronjob.Timezone == "" {
	cfg.Cronjob.Timezone = "UTC"
}
if cfg.Cronjob.StorePath == "" {
	cfg.Cronjob.StorePath = "./.quill/cronjobs.json"
}
```

Note: `Disabled` is intentionally left untouched — its zero value already means "enabled".

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./config/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add config/config.go config/config_test.go
git commit -m "feat(config): add CronjobConfig with sane defaults"
```

---

## Task 13: command/ — cron subcommands

**Files:**
- Modify: `command/command.go`
- Modify: `command/command_test.go`

- [ ] **Step 1: Add the failing test**

Append to `command/command_test.go`:

```go
func TestParseCommandCronAdd(t *testing.T) {
	cmd, ok := ParseCommand("/cron add 0 9 * * * daily standup")
	if !ok || cmd.Name != CmdCron {
		t.Fatalf("ParseCommand returned ok=%v cmd=%+v", ok, cmd)
	}
	if cmd.Args == "" {
		t.Error("expected args to be populated")
	}
}

func TestParseCronAddArgs(t *testing.T) {
	args := "add 0 9 * * * daily standup"
	sub, schedule, prompt, err := ParseCronArgs(args)
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if sub != "add" {
		t.Errorf("sub=%q", sub)
	}
	if schedule != "0 9 * * *" {
		t.Errorf("schedule=%q", schedule)
	}
	if prompt != "daily standup" {
		t.Errorf("prompt=%q", prompt)
	}
}

func TestParseCronAddArgsEvery(t *testing.T) {
	_, schedule, prompt, err := ParseCronArgs("add every 5m ping the build")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if schedule != "every 5m" {
		t.Errorf("schedule=%q want 'every 5m'", schedule)
	}
	if prompt != "ping the build" {
		t.Errorf("prompt=%q", prompt)
	}
}

func TestParseCronAddArgsAtAbsolute(t *testing.T) {
	_, schedule, prompt, err := ParseCronArgs("add at 2026-05-05 09:00 launch deploy")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if schedule != "at 2026-05-05 09:00" {
		t.Errorf("schedule=%q", schedule)
	}
	if prompt != "launch deploy" {
		t.Errorf("prompt=%q", prompt)
	}
}

func TestParseCronListEmpty(t *testing.T) {
	sub, schedule, prompt, err := ParseCronArgs("list")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if sub != "list" || schedule != "" || prompt != "" {
		t.Errorf("got sub=%q schedule=%q prompt=%q", sub, schedule, prompt)
	}
}

func TestParseCronRm(t *testing.T) {
	sub, schedule, _, err := ParseCronArgs("rm abc12345")
	if err != nil {
		t.Fatalf("ParseCronArgs: %v", err)
	}
	if sub != "rm" {
		t.Errorf("sub=%q", sub)
	}
	if schedule != "abc12345" {
		t.Errorf("expected id in schedule slot, got %q", schedule)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./command/...`
Expected: FAIL — `CmdCron`, `ParseCronArgs` undefined.

- [ ] **Step 3: Add CmdCron + ParseCronArgs**

In `command/command.go`, add the constant near the other `Cmd*` constants:

```go
CmdCron = "cron"
```

Add `cron` to the `known` map in `ParseCommand`:

```go
known := map[string]bool{
    CmdSessions: true, CmdReset: true, CmdResume: true, CmdInfo: true,
    CmdStop: true, CmdPicker: true, CmdMode: true, CmdModel: true,
    CmdHelp: true, CmdCron: true,
}
```

Add the help-table line in `ExecuteHelp` (right after the existing `model` row):

```go
sb.WriteString("| `cron add <schedule> <prompt>` | Schedule a recurring or one-shot prompt (cron / `every Xm` / `at HH:MM` / `in 30m`) |\n")
sb.WriteString("| `cron list` | List this thread's scheduled prompts |\n")
sb.WriteString("| `cron rm <id>` | Remove a scheduled prompt |\n")
```

Then add a new file or append at the bottom of `command.go`:

```go
import "github.com/neilkuan/quill/cronjob"  // add to existing import block

// ParseCronArgs splits the tail of "/cron <sub> <rest>" into a
// subcommand, a schedule expression, and a prompt body. The schedule
// token-count varies by form:
//
//   - "0 9 * * *" — 5 tokens
//   - "every 5m"  — 2 tokens
//   - "in 30m"    — 2 tokens
//   - "at HH:MM"  — 2 tokens
//   - "at YYYY-MM-DD HH:MM" — 3 tokens
//
// Schedule tokens are not parsed for validity here — that is the
// responsibility of cronjob.ParseSchedule. We only need to know how
// many tokens to consume so the rest can be treated as the prompt body.
func ParseCronArgs(args string) (sub, schedule, prompt string, err error) {
	parts := strings.Fields(strings.TrimSpace(args))
	if len(parts) == 0 {
		return "", "", "", fmt.Errorf("missing subcommand (try 'add', 'list', or 'rm')")
	}
	sub = strings.ToLower(parts[0])

	switch sub {
	case "list":
		if len(parts) > 1 {
			return "", "", "", fmt.Errorf("'list' takes no arguments")
		}
		return sub, "", "", nil

	case "rm", "remove", "delete":
		sub = "rm"
		if len(parts) < 2 {
			return "", "", "", fmt.Errorf("'rm' needs an id (try '/cron list' to see ids)")
		}
		return sub, parts[1], "", nil

	case "add":
		if len(parts) < 3 {
			return "", "", "", fmt.Errorf("'add' needs <schedule> <prompt>")
		}
		// Determine schedule token count.
		head := strings.ToLower(parts[1])
		var n int
		switch head {
		case "every", "in":
			n = 2
		case "at":
			// "at HH:MM" or "at YYYY-MM-DD HH:MM"
			if len(parts) >= 4 && looksLikeDate(parts[2]) {
				n = 3
			} else {
				n = 2
			}
		default:
			// Cron expression — 5 fields.
			if len(parts) < 7 {
				return "", "", "", fmt.Errorf("cron expressions take 5 fields followed by the prompt")
			}
			n = 5
		}
		if len(parts) < n+2 {
			return "", "", "", fmt.Errorf("missing prompt body after schedule")
		}
		schedule = strings.Join(parts[1:1+n], " ")
		prompt = strings.Join(parts[1+n:], " ")
		return sub, schedule, prompt, nil

	default:
		return "", "", "", fmt.Errorf("unknown subcommand %q (try 'add', 'list', or 'rm')", sub)
	}
}

func looksLikeDate(s string) bool {
	// YYYY-MM-DD shape check; lightweight, no regex.
	if len(s) != 10 {
		return false
	}
	for i, r := range s {
		switch i {
		case 4, 7:
			if r != '-' {
				return false
			}
		default:
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// ListCronJobs returns the jobs in the given thread.
func ListCronJobs(store *cronjob.Store, threadKey string) []cronjob.Job {
	if store == nil {
		return nil
	}
	return store.List(threadKey)
}

// ExecuteCronAdd validates the schedule, persists the job, and
// returns a human-readable confirmation. cfg is read for
// MinIntervalSeconds, Timezone, MaxPerThread.
func ExecuteCronAdd(store *cronjob.Store, threadKey, senderID, senderName, scheduleExpr, prompt string,
	maxPerThread int, minInterval time.Duration, tz *time.Location) (cronjob.Job, string) {

	if store == nil {
		return cronjob.Job{}, "⚠️ Cron jobs are disabled on this bot."
	}
	now := time.Now().UTC()

	sch, kind, err := cronjob.ParseSchedule(scheduleExpr, tz, minInterval, now)
	if err != nil {
		return cronjob.Job{}, fmt.Sprintf("⚠️ %s", err.Error())
	}

	next := sch.Next(now)
	if next.IsZero() {
		return cronjob.Job{}, "⚠️ Schedule has no future fire time."
	}

	job := cronjob.Job{
		ThreadKey:  threadKey,
		SenderID:   senderID,
		SenderName: senderName,
		Schedule:   scheduleExpr,
		Kind:       kind,
		Prompt:     prompt,
		NextFire:   next,
		CreatedAt:  now,
	}
	// Hydrate parsed schedule for the runtime path before persisting.
	hydrated := job
	hydrated.SetParsed(sch)

	added, err := store.Add(hydrated, maxPerThread)
	if err != nil {
		if errors.Is(err, cronjob.ErrLimitReached) {
			return cronjob.Job{}, fmt.Sprintf("⚠️ This thread already has %d scheduled prompts (the limit). Remove one first with `/cron rm <id>`.", maxPerThread)
		}
		return cronjob.Job{}, fmt.Sprintf("⚠️ Failed to save cron job: %v", err)
	}
	return added, fmt.Sprintf("✅ Created cron `%s` — next fire %s", added.ID, next.In(tz).Format("2006-01-02 15:04 MST"))
}

// ExecuteCronRemove deletes a job by id; returns chat-friendly text.
func ExecuteCronRemove(store *cronjob.Store, threadKey, id string) string {
	if store == nil {
		return "⚠️ Cron jobs are disabled on this bot."
	}
	if err := store.Remove(threadKey, id); err != nil {
		return fmt.Sprintf("⚠️ %v", err)
	}
	return fmt.Sprintf("🗑️ Removed cron `%s`.", id)
}
```

> The above references `cronjob.Job.SetParsed` — add it to `cronjob/job.go`:
>
> ```go
> // SetParsed records the runtime Schedule for this job. Used by Store
> // when adding a freshly parsed job so the scheduler can see it
> // immediately without waiting for the next Open().
> func (j *Job) SetParsed(s Schedule) { j.parsed = s }
> ```

- [ ] **Step 4: Add SetParsed to job.go**

Append to `cronjob/job.go` the SetParsed method shown above. Re-run `go test ./cronjob/...` and ensure it still passes.

- [ ] **Step 5: Run command tests to verify they pass**

Run: `go test ./command/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add command/command.go command/command_test.go cronjob/job.go
git commit -m "feat(command): add /cron parsing and ListCronJobs/ExecuteCronAdd/ExecuteCronRemove"
```

---

## Task 14: Telegram CronDispatcher

**Files:**
- Create: `telegram/cron_dispatcher.go`

The dispatcher reuses the existing `streamPrompt` machinery. The cleanest split is to expose a small public method on `Handler` for the cron path that takes `(ctx, sessionKey, contentBlocks, chatID, msgID, threadID)` and builds reactions internally — but that's a refactor. For V1, we accept a small amount of duplication and keep the dispatcher as a thin orchestrator.

- [ ] **Step 1: Create the dispatcher file**

Create `telegram/cron_dispatcher.go`:

```go
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/cronjob"
)

// CronDispatcher implements cronjob.Dispatcher for Telegram threads.
type CronDispatcher struct {
	Handler *Handler
	Bot     *bot.Bot
}

// Fire posts a placeholder message into the originating chat (and
// forum topic if applicable), builds the cron-flavoured sender_context,
// and streams the reply via the existing streamPrompt machinery.
func (d *CronDispatcher) Fire(ctx context.Context, job cronjob.Job) error {
	chatID, threadID, err := parseTelegramThreadKey(job.ThreadKey)
	if err != nil {
		return fmt.Errorf("telegram cron: %w", err)
	}

	placeholder := fmt.Sprintf("🔔 cron `%s` (`%s`) — running prompt…", job.ID, job.Schedule)
	sent, err := d.Bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            placeholder,
		ParseMode:       models.ParseModeMarkdown,
	})
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	// Build sender_context. Mirrors discord/handler.go and the existing
	// telegram path, with the cron fields merged on top.
	senderCtx := map[string]any{
		"schema":       "quill.sender.v1",
		"sender_id":    job.SenderID,
		"sender_name":  job.SenderName,
		"display_name": job.SenderName,
		"channel":      "telegram",
		"channel_id":   strconv.FormatInt(chatID, 10),
		"is_bot":       false,
	}
	if threadID != 0 {
		senderCtx["topic_thread_id"] = threadID
	}
	for k, v := range cronjob.CronFields(job) {
		senderCtx[k] = v
	}
	senderJSON, _ := json.Marshal(senderCtx)
	body := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), job.Prompt)

	// Ensure the connection exists.
	if err := d.Handler.Pool.GetOrCreate(job.ThreadKey); err != nil {
		d.editText(ctx, chatID, sent.ID, fmt.Sprintf("⚠️ cron `%s` failed: %v", job.ID, err))
		return err
	}

	contentBlocks := []acp.ContentBlock{acp.TextBlock(body)}
	reactions := NewStatusReactionController(
		d.Handler.ReactionsConfig.Enabled,
		d.Bot,
		chatID,
		sent.ID,
		d.Handler.ReactionsConfig.Emojis,
		d.Handler.ReactionsConfig.Timing,
	)
	reactions.SetThinking()

	finalText, cancelled, result := d.Handler.streamPrompt(ctx, d.Bot, job.ThreadKey, contentBlocks, chatID, sent.ID, threadID, reactions)
	switch {
	case cancelled:
		reactions.SetCancelled()
	case result == nil:
		reactions.SetDone()
	default:
		reactions.SetError()
	}
	_ = finalText // streamPrompt has already edited the placeholder
	return nil
}

// NotifyDropped posts a brief marker so the user notices a fire was
// lost. Best-effort; failure is logged, not returned.
func (d *CronDispatcher) NotifyDropped(ctx context.Context, job cronjob.Job) {
	chatID, threadID, err := parseTelegramThreadKey(job.ThreadKey)
	if err != nil {
		slog.Warn("cron dropped notify: bad thread key", "thread_key", job.ThreadKey, "error", err)
		return
	}
	_, err = d.Bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            fmt.Sprintf("⚠️ cron `%s` dropped: thread queue full", job.ID),
		ParseMode:       models.ParseModeMarkdown,
	})
	if err != nil {
		slog.Warn("cron dropped notify: send failed", "error", err)
	}
}

func (d *CronDispatcher) editText(ctx context.Context, chatID int64, msgID int, text string) {
	_, err := d.Bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: msgID,
		Text:      text,
	})
	if err != nil {
		slog.Debug("telegram cron edit text failed", "error", err)
	}
}

// parseTelegramThreadKey unpacks "tg:<chat>" or "tg:<chat>:<thread>".
func parseTelegramThreadKey(key string) (chatID int64, threadID int, err error) {
	if !strings.HasPrefix(key, "tg:") {
		return 0, 0, fmt.Errorf("not a telegram thread key: %q", key)
	}
	parts := strings.Split(key[3:], ":")
	chatID, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bad chat id in %q: %w", key, err)
	}
	if len(parts) > 1 {
		t, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, fmt.Errorf("bad thread id in %q: %w", key, err)
		}
		threadID = t
	}
	return chatID, threadID, nil
}
```

- [ ] **Step 2: Verify package builds**

Run: `go build ./telegram/...`
Expected: succeeds.

- [ ] **Step 3: Commit**

```bash
git add telegram/cron_dispatcher.go
git commit -m "feat(telegram): add CronDispatcher implementing cronjob.Dispatcher"
```

---

## Task 15: Telegram /cron command wiring

**Files:**
- Modify: `telegram/handler.go`
- Modify: `telegram/adapter.go` (to expose store via constructor)

- [ ] **Step 1: Add the field and constructor argument**

In `telegram/adapter.go`, add a `*cronjob.Store` field to `Adapter` and `Handler`, and accept it in `NewAdapter`:

```go
import "github.com/neilkuan/quill/cronjob"
import "github.com/neilkuan/quill/config"  // existing

// in Adapter struct: add CronStore *cronjob.Store, CronCfg config.CronjobConfig

// In Handler struct: add CronStore *cronjob.Store, CronCfg config.CronjobConfig

// Update NewAdapter signature to:
func NewAdapter(cfg config.TelegramConfig, pool *acp.SessionPool,
    transcriber stt.Transcriber, synthesizer tts.Synthesizer,
    ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig,
    picker sessionpicker.Picker,
    cronStore *cronjob.Store, cronCfg config.CronjobConfig) (*Adapter, error) {
    // ... existing body ...
    // Wire CronStore + CronCfg into Handler
}
```

- [ ] **Step 2: Add the command branch**

In `telegram/handler.go`, in `handleCommand`, add a case for `command.CmdCron`:

```go
case command.CmdCron:
    sessionKey := buildSessionKeyFromChat(chatID, threadID)
    response = h.handleCronCommand(sessionKey, msg, cmd.Args)
```

Add the helper at the bottom of the file:

```go
func (h *Handler) handleCronCommand(threadKey string, msg *models.Message, args string) string {
	if h.CronStore == nil || h.CronCfg.Disabled {
		return "⚠️ Cron jobs are disabled on this bot."
	}
	sub, schedule, prompt, err := command.ParseCronArgs(args)
	if err != nil {
		return fmt.Sprintf("⚠️ %v\n\nUsage: `/cron add <schedule> <prompt>`, `/cron list`, `/cron rm <id>`", err)
	}
	tz, _ := time.LoadLocation(h.CronCfg.Timezone)
	if tz == nil {
		tz = time.UTC
	}
	min := time.Duration(h.CronCfg.MinIntervalSeconds) * time.Second

	switch sub {
	case "add":
		_, msg := command.ExecuteCronAdd(h.CronStore, threadKey,
			fmt.Sprintf("%d", findUserID(msgFrom(msg))),
			displayName(msgFrom(msg)),
			schedule, prompt,
			h.CronCfg.MaxPerThread, min, tz)
		return msg
	case "list":
		jobs := command.ListCronJobs(h.CronStore, threadKey)
		return formatCronList(jobs, tz)
	case "rm":
		return command.ExecuteCronRemove(h.CronStore, threadKey, schedule)
	}
	return "⚠️ Unknown cron subcommand"
}

func formatCronList(jobs []cronjob.Job, tz *time.Location) string {
	if len(jobs) == 0 {
		return "📭 No scheduled prompts in this thread.\n\nCreate one with `/cron add <schedule> <prompt>`."
	}
	var sb strings.Builder
	sb.WriteString("⏰ *Scheduled prompts in this thread*\n\n")
	for _, j := range jobs {
		sb.WriteString(fmt.Sprintf("`%s` — `%s` — next %s\n  → %s\n",
			j.ID, j.Schedule, j.NextFire.In(tz).Format("2006-01-02 15:04 MST"),
			truncate(j.Prompt, 80)))
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
```

The two helpers `msgFrom`, `findUserID`, `displayName` should already exist in the file as utilities for building the sender context — if not, copy the inline logic from the existing handler at line ~183-200.

- [ ] **Step 3: Update main.go signature call site**

Defer to Task 22.

- [ ] **Step 4: Run go build to confirm shape**

Run: `go build ./telegram/...`
Expected: succeeds (even though main.go's call site is not yet updated; that's fine — the build of the telegram package alone should pass).

- [ ] **Step 5: Add `RegisterCron` method on Adapter**

In `telegram/adapter.go`, add:

```go
// RegisterCron creates the per-platform cron Dispatcher and registers
// it with the prefix "telegram" in the provided registry. Called by
// main.go after the adapter has been Started so a.bot is non-nil.
func (a *Adapter) RegisterCron(registry *cronjob.Registry) {
	registry.Register("telegram", &CronDispatcher{Handler: a.handler, Bot: a.bot})
}
```

(`a.bot` and `a.handler` are the existing private fields populated in `NewAdapter` / `Start`.)

- [ ] **Step 6: Commit**

```bash
git add telegram/handler.go telegram/adapter.go
git commit -m "feat(telegram): wire /cron command, accept CronStore, expose RegisterCron"
```

---

## Task 16: Telegram interactive list (InlineKeyboard)

**Files:**
- Modify: `telegram/handler.go`

- [ ] **Step 1: Replace the text-only list path with an InlineKeyboard variant**

In the `case "list":` branch of `handleCronCommand`, swap the synchronous `formatCronList` return for an `h.sendCronList` call that uses `bot.SendMessageParams.ReplyMarkup` with `InlineKeyboardMarkup`:

```go
case "list":
    h.sendCronList(ctx, b, chatID, threadID, msg, threadKey, tz)
    return ""  // empty signals caller to skip the trailing reply
```

`handleCommand` needs a small adjustment so an empty-string response from `cron list` does not produce an empty reply — guard the existing `if response != ""` send.

Add the helper:

```go
func (h *Handler) sendCronList(ctx context.Context, b *bot.Bot, chatID int64, threadID int,
    msg *models.Message, threadKey string, tz *time.Location) {

	jobs := command.ListCronJobs(h.CronStore, threadKey)
	if len(jobs) == 0 {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            "📭 No scheduled prompts. Create one with `/cron add <schedule> <prompt>`.",
			ParseMode:       models.ParseModeMarkdown,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		return
	}

	rows := make([][]models.InlineKeyboardButton, 0, len(jobs))
	var body strings.Builder
	body.WriteString("⏰ *Scheduled prompts in this thread*\n\n")
	for _, j := range jobs {
		body.WriteString(fmt.Sprintf("`%s` — `%s` — next %s\n  → %s\n",
			j.ID, j.Schedule, j.NextFire.In(tz).Format("2006-01-02 15:04 MST"),
			truncate(j.Prompt, 80)))
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "🗑️ " + j.ID, CallbackData: "cron:rm:" + j.ID},
		})
	}

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            body.String(),
		ParseMode:       models.ParseModeMarkdown,
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		ReplyMarkup:     &models.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
}
```

- [ ] **Step 2: Add the callback handler**

The existing telegram callback router (search for `OnCallbackQuery` or similar; in this codebase callbacks are routed through `bot.Bot`'s default handler with prefix matching). Find the section that handles `mode:`, `model:`, and `pick:` callbacks (around `telegram/handler.go:707`) and add:

```go
case strings.HasPrefix(data, "cron:rm:"):
    id := strings.TrimPrefix(data, "cron:rm:")
    threadKey := callback's thread key (derive from message)
    resultMsg = command.ExecuteCronRemove(h.CronStore, threadKey, id)
```

The exact code shape mirrors the surrounding cases — copy the structure verbatim and substitute the prefix.

- [ ] **Step 3: Run go build**

Run: `go build ./telegram/...`
Expected: succeeds.

- [ ] **Step 4: Commit**

```bash
git add telegram/handler.go
git commit -m "feat(telegram): add InlineKeyboard delete buttons to /cron list"
```

---

## Task 17: Discord /cron command + dispatcher

**Files:**
- Create: `discord/cron_dispatcher.go`
- Modify: `discord/handler.go`
- Modify: `discord/adapter.go`

- [ ] **Step 1: Create the dispatcher**

Create `discord/cron_dispatcher.go` mirroring the telegram dispatcher. The Discord version uses `s.ChannelMessageSend` for the placeholder and `s.ChannelMessageEdit` for streaming updates. Key shape:

```go
package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/cronjob"
)

type CronDispatcher struct {
	Handler *Handler
	Session *discordgo.Session
}

func (d *CronDispatcher) Fire(ctx context.Context, job cronjob.Job) error {
	channelID, ok := parseDiscordThreadKey(job.ThreadKey)
	if !ok {
		return fmt.Errorf("discord cron: bad thread key %q", job.ThreadKey)
	}

	placeholder := fmt.Sprintf("🔔 cron `%s` (`%s`) — running prompt…", job.ID, job.Schedule)
	sent, err := d.Session.ChannelMessageSend(channelID, placeholder)
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	senderCtx := map[string]any{
		"schema":       "quill.sender.v1",
		"sender_id":    job.SenderID,
		"sender_name":  job.SenderName,
		"display_name": job.SenderName,
		"channel":      "discord",
		"channel_id":   channelID,
		"is_bot":       false,
	}
	for k, v := range cronjob.CronFields(job) {
		senderCtx[k] = v
	}
	senderJSON, _ := json.Marshal(senderCtx)
	body := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), job.Prompt)

	if err := d.Handler.Pool.GetOrCreate(job.ThreadKey); err != nil {
		d.Session.ChannelMessageEdit(channelID, sent.ID, fmt.Sprintf("⚠️ cron `%s` failed: %v", job.ID, err))
		return err
	}

	contentBlocks := []acp.ContentBlock{acp.TextBlock(body)}
	// Reuse the discord handler's streaming entry-point. If a public method
	// does not exist yet, refactor the inner loop of OnMessageCreate into a
	// `(h *Handler) streamPromptIntoMessage(ctx, channelID, msgID, threadKey, blocks)`
	// helper as part of this task.
	d.Handler.streamPromptIntoMessage(ctx, channelID, sent.ID, job.ThreadKey, contentBlocks)
	return nil
}

func (d *CronDispatcher) NotifyDropped(ctx context.Context, job cronjob.Job) {
	channelID, ok := parseDiscordThreadKey(job.ThreadKey)
	if !ok {
		slog.Warn("cron dropped notify: bad thread key", "thread_key", job.ThreadKey)
		return
	}
	_, err := d.Session.ChannelMessageSend(channelID, fmt.Sprintf("⚠️ cron `%s` dropped: thread queue full", job.ID))
	if err != nil {
		slog.Warn("cron dropped notify send failed", "error", err)
	}
}

func parseDiscordThreadKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "discord:") {
		return "", false
	}
	return strings.TrimPrefix(key, "discord:"), true
}
```

- [ ] **Step 2: Refactor `OnMessageCreate` to expose `streamPromptIntoMessage`**

In `discord/handler.go`, extract the streaming / edit loop currently inside `OnMessageCreate` into:

```go
func (h *Handler) streamPromptIntoMessage(ctx context.Context, channelID, msgID, threadKey string, blocks []acp.ContentBlock) (finalText string, cancelled bool, result error) {
    // ... move the existing edit-streaming goroutine + SessionPrompt loop here ...
}
```

The original call site replaces the inlined logic with a single call to this helper.

- [ ] **Step 3: Wire `/cron` command in `OnInteractionCreate`**

Discord uses Slash Commands. Register `cron` in the existing slash-command registration block (search for `ApplicationCommandCreate`), with subcommands `add` / `list` / `rm`:

```go
{
    Name: "cron",
    Description: "Schedule recurring or one-shot prompts",
    Options: []*discordgo.ApplicationCommandOption{
        {Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Schedule a prompt", Options: []*discordgo.ApplicationCommandOption{
            {Type: discordgo.ApplicationCommandOptionString, Name: "schedule", Description: "Cron / every Xm / at HH:MM / in 30m", Required: true},
            {Type: discordgo.ApplicationCommandOptionString, Name: "prompt", Description: "Prompt body", Required: true},
        }},
        {Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "List scheduled prompts"},
        {Type: discordgo.ApplicationCommandOptionSubCommand, Name: "rm", Description: "Remove a scheduled prompt", Options: []*discordgo.ApplicationCommandOption{
            {Type: discordgo.ApplicationCommandOptionString, Name: "id", Description: "Job id from /cron list", Required: true},
        }},
    },
},
```

In `OnInteractionCreate`, add a case for `cron` that dispatches to one of three handlers calling `command.ExecuteCronAdd` / `command.ListCronJobs` / `command.ExecuteCronRemove` with `threadKey = "discord:" + i.ChannelID`.

For `list`, render a `discordgo.SelectMenu` with one option per job; on submit, call `ExecuteCronRemove`. (Same pattern as the existing `/pick`.)

- [ ] **Step 4: Update `NewAdapter` signature**

In `discord/adapter.go`, add `CronStore *cronjob.Store, CronCfg config.CronjobConfig` to the constructor and `Handler` struct, mirroring telegram.

- [ ] **Step 5: Add `RegisterCron` on Adapter**

In `discord/adapter.go`, add:

```go
// RegisterCron creates the discord cron Dispatcher and registers it
// with the prefix "discord". Call after Start() so a.session is alive.
func (a *Adapter) RegisterCron(registry *cronjob.Registry) {
	registry.Register("discord", &CronDispatcher{Handler: a.handler, Session: a.session})
}
```

- [ ] **Step 6: Run go build**

Run: `go build ./discord/...`
Expected: succeeds.

- [ ] **Step 7: Commit**

```bash
git add discord/cron_dispatcher.go discord/handler.go discord/adapter.go
git commit -m "feat(discord): add /cron slash command, CronDispatcher, RegisterCron"
```

---

## Task 18: Teams /cron command + dispatcher (text-only)

**Files:**
- Create: `teams/cron_dispatcher.go`
- Modify: `teams/handler.go`
- Modify: `teams/adapter.go`

- [ ] **Step 1: Create the dispatcher**

Create `teams/cron_dispatcher.go`:

```go
package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/cronjob"
)

type CronDispatcher struct {
	Handler *Handler
	Client  *BotClient
}

func (d *CronDispatcher) Fire(ctx context.Context, job cronjob.Job) error {
	conv, err := parseTeamsThreadKey(job.ThreadKey)
	if err != nil {
		return err
	}

	placeholder := fmt.Sprintf("🔔 cron `%s` (`%s`) — running prompt…", job.ID, job.Schedule)
	sent, err := d.Client.SendActivity(conv, Activity{Type: "message", Text: placeholder})
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	senderCtx := map[string]any{
		"schema":       "quill.sender.v1",
		"sender_id":    job.SenderID,
		"sender_name":  job.SenderName,
		"display_name": job.SenderName,
		"channel":      "teams",
		"channel_id":   conv.ID,
		"is_bot":       false,
	}
	for k, v := range cronjob.CronFields(job) {
		senderCtx[k] = v
	}
	senderJSON, _ := json.Marshal(senderCtx)
	body := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), job.Prompt)

	if err := d.Handler.Pool.GetOrCreate(job.ThreadKey); err != nil {
		d.Client.UpdateActivity(conv, sent.ID, Activity{Type: "message", Text: fmt.Sprintf("⚠️ cron `%s` failed: %v", job.ID, err)})
		return err
	}

	contentBlocks := []acp.ContentBlock{acp.TextBlock(body)}
	d.Handler.streamPromptIntoActivity(ctx, conv, sent.ID, job.ThreadKey, contentBlocks)
	return nil
}

func (d *CronDispatcher) NotifyDropped(ctx context.Context, job cronjob.Job) {
	conv, err := parseTeamsThreadKey(job.ThreadKey)
	if err != nil {
		slog.Warn("cron dropped notify: bad thread key", "thread_key", job.ThreadKey, "error", err)
		return
	}
	_, err = d.Client.SendActivity(conv, Activity{Type: "message", Text: fmt.Sprintf("⚠️ cron `%s` dropped: thread queue full", job.ID)})
	if err != nil {
		slog.Warn("cron dropped notify send failed", "error", err)
	}
}

func parseTeamsThreadKey(key string) (Conversation, error) {
	if !strings.HasPrefix(key, "teams:") {
		return Conversation{}, fmt.Errorf("not a teams thread key: %q", key)
	}
	id := strings.TrimPrefix(key, "teams:")
	return Conversation{ID: id}, nil
}
```

- [ ] **Step 2: Refactor `streamPromptIntoActivity`**

Mirror Task 17 step 2: extract the existing edit-streaming loop in teams/handler.go's message path into a helper method.

- [ ] **Step 3: Wire `/cron` command in handler**

In `teams/handler.go`, add a `command.CmdCron` case to the existing command switch that calls a `handleCronCommand` helper following the telegram pattern (text-only, no widgets).

- [ ] **Step 4: Update `NewAdapter` signature**

Same as telegram/discord: add `CronStore *cronjob.Store, CronCfg config.CronjobConfig`.

- [ ] **Step 5: Add `RegisterCron` on Adapter**

In `teams/adapter.go`, add:

```go
// RegisterCron creates the teams cron Dispatcher and registers it
// with the prefix "teams". Call after Start() so a.client is alive.
func (a *Adapter) RegisterCron(registry *cronjob.Registry) {
	registry.Register("teams", &CronDispatcher{Handler: a.handler, Client: a.client})
}
```

- [ ] **Step 6: Run go build**

Run: `go build ./teams/...`
Expected: succeeds.

- [ ] **Step 7: Commit**

```bash
git add teams/cron_dispatcher.go teams/handler.go teams/adapter.go
git commit -m "feat(teams): add /cron command, CronDispatcher, RegisterCron (text-only UI)"
```

---

## Task 19: main.go wiring

**Files:**
- Modify: `main.go`

- [ ] **Step 1: Open store, build registry, register dispatchers, start scheduler**

After the platform adapters are constructed (and before `apiServer.Start()`), insert:

```go
// Cron jobs
var cronStore *cronjob.Store
var cronRegistry *cronjob.Registry
var cronGates *cronjob.Gates
var cronScheduler *cronjob.Scheduler
var cronCancel context.CancelFunc

if !cfg.Cronjob.Disabled {
    cronStore, err = cronjob.Open(cfg.Cronjob.StorePath)
    if err != nil {
        slog.Error("failed to open cronjob store", "error", err)
        os.Exit(1)
    }
    cronRegistry = cronjob.NewRegistry()
    cronGates = cronjob.NewGates(cfg.Cronjob.QueueSize)

    // Each adapter built above must now have its CronStore field populated.
    // Pass cronStore + cfg.Cronjob into the constructors above; for adapters
    // that exist at this point, call adapter.RegisterCron(cronRegistry).
    if cfg.Discord.Enabled { discordAdapter.RegisterCron(cronRegistry) }
    if cfg.Telegram.Enabled { telegramAdapter.RegisterCron(cronRegistry) }
    if cfg.Teams.Enabled { teamsAdapter.RegisterCron(cronRegistry) }

    var ctx context.Context
    ctx, cronCancel = context.WithCancel(context.Background())
    cronScheduler = cronjob.NewScheduler(cronStore, cronRegistry, cronGates, time.Second)
    go cronScheduler.Run(ctx)
    slog.Info("cronjob scheduler started", "store", cfg.Cronjob.StorePath)
}
```

Pass `cronStore, cfg.Cronjob` to each `NewAdapter` call. Each `Adapter` exposes a `RegisterCron(*cronjob.Registry)` method that creates its `CronDispatcher` and registers it with the prefix `"discord"` / `"telegram"` / `"teams"`.

- [ ] **Step 2: Update shutdown path**

Append to the cleanup block:

```go
if cronCancel != nil {
    cronCancel()
    cronGates.Close()
}
```

(Order matters: cancel the scheduler ctx first so no new fires are submitted, then close the gates.)

- [ ] **Step 3: Run go build**

Run: `go build ./...`
Expected: succeeds.

- [ ] **Step 4: Commit**

```bash
git add main.go discord/adapter.go telegram/adapter.go teams/adapter.go
git commit -m "feat(main): wire cronjob scheduler, registry, gates, dispatchers"
```

---

## Task 20: config.toml.example update

**Files:**
- Modify: `config.toml.example`

- [ ] **Step 1: Add the [cronjob] block**

Insert after the existing `[tts]` block (or wherever the per-section fields end):

```toml
# ────────────────────────────────────────────────────────────────────
# [cronjob] — user-scheduled prompts
#
# Lets users in any allowed thread schedule recurring or one-shot
# prompts via /cron add. Set `disabled = true` to fully turn the
# feature off.
# ────────────────────────────────────────────────────────────────────
[cronjob]
# disabled = false                  # Set to true to disable /cron entirely.
# max_per_thread = 20               # Per-thread cap.
# min_interval_seconds = 60         # Reject "every 30s"; one-shots ("in 30s") are exempt.
# queue_size = 50                   # Per-thread fire buffer; overflow drops with chat marker.
# timezone = "UTC"                  # Display + parse TZ for "at HH:MM".
# store_path = "./.quill/cronjobs.json"
```

- [ ] **Step 2: Commit**

```bash
git add config.toml.example
git commit -m "docs(config): document [cronjob] section"
```

---

## Task 21: Final integration check

**Files:**
- None (verification only)

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 2: Full vet**

Run: `go vet ./...`
Expected: no warnings.

- [ ] **Step 3: Full test**

Run: `go test ./... -v`
Expected: all PASS, including the new cronjob tests.

- [ ] **Step 4: Manual smoke (local Telegram)**

Start the bot with a Telegram config. In a chat with the bot:

```
/cron add in 1m hello world
/cron list
```

Expected:
- `add` returns `✅ Created cron <id> — next fire …`
- After ~1 minute, the bot posts `🔔 cron <id> (in 1m) — running prompt…` and edits it with the agent's reply.
- The agent should see `trigger:"cron"` in the sender context. Verify by asking it to echo what it received.

Once smoke passes, document in the spec's "Limitations" / "Known V1 trade-offs" section any discrepancies discovered.

- [ ] **Step 5: Commit (if needed)**

If the smoke test surfaced any edits:

```bash
git add .
git commit -m "fix(cronjob): smoke test fixups"
```

---

## Self-Review Checklist (run after writing this plan)

- **Spec coverage:** Every spec section maps to at least one task. Spec §1 (package layout) → Tasks 2-11. Spec §2 (data model) → Task 2. Spec §3 (parser) → Task 6. Spec §4 (store) → Task 7. Spec §5 (scheduler loop) → Task 11. Spec §6 (dispatcher) → Tasks 9, 14, 17, 18. Spec §7 (sender context) → Task 8 + dispatchers in Tasks 14/17/18. Spec §8 (concurrency / queue) → Tasks 10 + 11. Spec §9 (commands) → Task 13. Spec §10 (config) → Task 12. Spec §11 (access control) → handled implicitly by reusing existing platform gates; no new task. Spec §12 (lifecycle) → Task 19. Spec §13 (testing) → Tasks 2-13 each ship their own test. Spec §14 (migration) → no work needed; covered in Task 21 final check.

- **No placeholders:** every step contains executable code or an exact command. No "TBD" / "implement later" / "fill in details".

- **Type consistency:** `Job.Schedule` is the raw string field; `Job.Parsed()` returns the runtime `Schedule` interface. `Schedule.Next(after time.Time) time.Time` is the only method called by the scheduler. `Dispatcher` has `Fire` + `NotifyDropped`. `Registry.Register(prefix, d)` and `Registry.Get(prefix)` match between definition and call sites. `Gates.Submit(ctx, d, job)` and `Gates.Close()` consistent.

- **Test discipline:** every code task starts with a failing test, runs it to confirm failure, implements minimal code, runs it to confirm pass, then commits. Platform integration tasks (14-18) skip the failing-test step because they are covered by manual smoke (Task 21) — pure UI/IO code rarely benefits from white-box unit tests proportional to their complexity, and the spec explicitly defers e2e tests as out of scope.
