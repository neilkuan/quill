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
