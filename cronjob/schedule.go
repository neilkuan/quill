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
