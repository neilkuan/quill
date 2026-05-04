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
