package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
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

	// Plain text — Telegram's Markdown parser rejects ( ) ` * etc. without
	// escaping, and the placeholder is informational so we lose nothing.
	placeholder := fmt.Sprintf("🔔 cron %s (%s) — running prompt…", job.ID, job.Schedule)
	if conn := d.Handler.Pool.Connection(job.ThreadKey); conn != nil && conn.Alive() {
		if busy, owner := conn.IsBusy(); busy {
			placeholder = fmt.Sprintf("🔔 cron %s (%s) — queued behind %s; will run when current prompt finishes.",
				job.ID, job.Schedule, owner)
		}
	}
	sent, err := d.Bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            placeholder,
	})
	if err != nil {
		return fmt.Errorf("send placeholder: %w", err)
	}

	// Build sender_context. Mirrors the existing handler path with the
	// cron fields merged on top.
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
		d.editText(ctx, chatID, sent.ID, fmt.Sprintf("⚠️ cron %s failed: %v", job.ID, err))
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

	finalText, cancelled, result := d.Handler.streamPrompt(ctx, d.Bot, job.ThreadKey, contentBlocks, chatID, sent.ID, threadID, reactions, "cron "+job.ID)
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
		Text:            fmt.Sprintf("⚠️ cron %s dropped: thread queue full", job.ID),
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
