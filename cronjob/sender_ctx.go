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
