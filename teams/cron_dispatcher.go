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

// CronDispatcher implements cronjob.Dispatcher for Microsoft Teams.
type CronDispatcher struct {
	Handler *Handler
	Client  *BotClient
}

// Fire posts a placeholder activity into the originating conversation,
// builds the cron-flavoured sender_context, and streams the reply via
// the existing streamPrompt helper.
//
// Requires that the conversation's serviceURL has been seen at least
// once since process start (cached on every incoming activity). If
// the cache is empty, the fire is logged and skipped — the user must
// send any message into the conversation to repopulate.
func (d *CronDispatcher) Fire(ctx context.Context, job cronjob.Job) error {
	conversationID, ok := parseTeamsThreadKey(job.ThreadKey)
	if !ok {
		return fmt.Errorf("teams cron: bad thread key %q", job.ThreadKey)
	}
	serviceURL := d.Handler.ServiceURLFor(conversationID)
	if serviceURL == "" {
		slog.Warn("teams cron: no cached serviceURL for conversation; user must send a message to repopulate after restart",
			"conversation_id", conversationID, "job_id", job.ID)
		return fmt.Errorf("no cached serviceURL")
	}

	placeholderText := fmt.Sprintf("🔔 cron `%s` (`%s`) — running prompt…", job.ID, job.Schedule)
	if conn := d.Handler.Pool.Connection(job.ThreadKey); conn != nil && conn.Alive() {
		if busy, owner := conn.IsBusy(); busy {
			placeholderText = fmt.Sprintf("🔔 cron `%s` (`%s`) — queued behind %s; will run when current prompt finishes.",
				job.ID, job.Schedule, owner)
		}
	}
	placeholder := &Activity{
		Type:       "message",
		Text:       placeholderText,
		TextFormat: "markdown",
	}
	sent, err := d.Client.SendActivity(serviceURL, conversationID, placeholder)
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	senderCtx := map[string]any{
		"schema":       "quill.sender.v1",
		"sender_id":    job.SenderID,
		"sender_name":  job.SenderName,
		"display_name": job.SenderName,
		"channel":      "teams",
		"channel_id":   conversationID,
		"is_bot":       false,
	}
	for k, v := range cronjob.CronFields(job) {
		senderCtx[k] = v
	}
	senderJSON, _ := json.Marshal(senderCtx)
	body := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), job.Prompt)

	if err := d.Handler.Pool.GetOrCreate(job.ThreadKey); err != nil {
		_ = d.Client.UpdateActivity(serviceURL, conversationID, sent.ID, &Activity{
			Type:       "message",
			Text:       fmt.Sprintf("⚠️ cron `%s` failed: %v", job.ID, err),
			TextFormat: "markdown",
		})
		return err
	}

	contentBlocks := []acp.ContentBlock{acp.TextBlock(body)}
	finalText, _, _ := d.Handler.streamPrompt(job.ThreadKey, contentBlocks, serviceURL, conversationID, sent.ID, "cron "+job.ID)
	_ = finalText // streamPrompt has already updated the placeholder
	return nil
}

// NotifyDropped posts a brief marker so the user notices a fire was
// lost. Best-effort; failure is logged, not returned.
func (d *CronDispatcher) NotifyDropped(ctx context.Context, job cronjob.Job) {
	conversationID, ok := parseTeamsThreadKey(job.ThreadKey)
	if !ok {
		slog.Warn("cron dropped notify: bad thread key", "thread_key", job.ThreadKey)
		return
	}
	serviceURL := d.Handler.ServiceURLFor(conversationID)
	if serviceURL == "" {
		slog.Warn("teams cron dropped notify: no cached serviceURL", "conversation_id", conversationID)
		return
	}
	_, err := d.Client.SendActivity(serviceURL, conversationID, &Activity{
		Type:       "message",
		Text:       fmt.Sprintf("⚠️ cron `%s` dropped: thread queue full", job.ID),
		TextFormat: "markdown",
	})
	if err != nil {
		slog.Warn("teams cron dropped notify send failed", "error", err)
	}
}

// parseTeamsThreadKey unpacks "teams:<conversationID>".
func parseTeamsThreadKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "teams:") {
		return "", false
	}
	return strings.TrimPrefix(key, "teams:"), true
}
