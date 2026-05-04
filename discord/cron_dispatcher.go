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

// CronDispatcher implements cronjob.Dispatcher for Discord channels.
type CronDispatcher struct {
	Handler *Handler
	Session *discordgo.Session
}

// Fire posts a placeholder message into the originating channel,
// builds the cron-flavoured sender_context, and streams the reply
// via the package-level streamPrompt helper.
func (d *CronDispatcher) Fire(ctx context.Context, job cronjob.Job) error {
	channelID, ok := parseDiscordThreadKey(job.ThreadKey)
	if !ok {
		return fmt.Errorf("discord cron: bad thread key %q", job.ThreadKey)
	}

	placeholder := fmt.Sprintf("🔔 cron `%s` (`%s`) — running prompt…", job.ID, job.Schedule)
	if conn := d.Handler.Pool.Connection(job.ThreadKey); conn != nil && conn.Alive() {
		if busy, owner := conn.IsBusy(); busy {
			placeholder = fmt.Sprintf("🔔 cron `%s` (`%s`) — queued behind %s; will run when current prompt finishes.",
				job.ID, job.Schedule, owner)
		}
	}
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
		_, editErr := d.Session.ChannelMessageEdit(channelID, sent.ID, fmt.Sprintf("⚠️ cron `%s` failed: %v", job.ID, err))
		if editErr != nil {
			slog.Debug("discord cron edit error message failed", "error", editErr)
		}
		return err
	}

	contentBlocks := []acp.ContentBlock{acp.TextBlock(body)}
	reactions := NewStatusReactionController(
		d.Handler.ReactionsConfig.Enabled,
		d.Session,
		channelID,
		sent.ID,
		d.Handler.ReactionsConfig.Emojis,
		d.Handler.ReactionsConfig.Timing,
	)
	reactions.SetThinking()

	finalText, cancelled, result := streamPrompt(
		d.Handler.Pool,
		job.ThreadKey,
		contentBlocks,
		d.Session,
		channelID,
		sent.ID,
		reactions,
		d.Handler.MarkdownTableMode,
		d.Handler.ReactionsConfig.ToolDisplay,
		"cron "+job.ID,
	)
	switch {
	case cancelled:
		reactions.SetCancelled()
	case result == nil:
		reactions.SetDone()
	default:
		reactions.SetError()
	}
	_ = finalText
	return nil
}

// NotifyDropped posts a brief marker into the channel.
func (d *CronDispatcher) NotifyDropped(ctx context.Context, job cronjob.Job) {
	channelID, ok := parseDiscordThreadKey(job.ThreadKey)
	if !ok {
		slog.Warn("cron dropped notify: bad thread key", "thread_key", job.ThreadKey)
		return
	}
	if _, err := d.Session.ChannelMessageSend(channelID, fmt.Sprintf("⚠️ cron `%s` dropped: thread queue full", job.ID)); err != nil {
		slog.Warn("cron dropped notify send failed", "error", err)
	}
}

// parseDiscordThreadKey unpacks "discord:<channelID>".
func parseDiscordThreadKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "discord:") {
		return "", false
	}
	return strings.TrimPrefix(key, "discord:"), true
}
