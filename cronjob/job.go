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
