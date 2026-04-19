package telegram

import (
	"context"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/quill/config"
)

// Tool classification tokens (shared with Discord)
var (
	codingTokens = []string{"exec", "process", "read", "write", "edit", "bash", "shell"}
	webTokens    = []string{"web_search", "web_fetch", "web-search", "web-fetch", "browser"}
)

// doneFaces are emojis from Telegram's allowed reaction set used for random done reactions.
var doneFaces = []string{"😎", "🫡", "🤓", "🤗", "👏", "🎉", "💯", "🤩"}

func classifyToolTelegram(name string, emojis *config.ReactionEmojis) string {
	n := strings.ToLower(name)
	for _, t := range webTokens {
		if strings.Contains(n, t) {
			return emojis.Web
		}
	}
	for _, t := range codingTokens {
		if strings.Contains(n, t) {
			return emojis.Coding
		}
	}
	return emojis.Tool
}

type StatusReactionController struct {
	mu             sync.Mutex
	b              *bot.Bot
	chatID         int64
	messageID      int
	emojis         config.ReactionEmojis
	timing         config.ReactionTiming
	current        string
	finished       bool
	enabled        bool
	debounceTimer  *time.Timer
	stallSoftTimer *time.Timer
	stallHardTimer *time.Timer
}

func NewStatusReactionController(
	enabled bool,
	b *bot.Bot,
	chatID int64,
	messageID int,
	emojis config.ReactionEmojis,
	timing config.ReactionTiming,
) *StatusReactionController {
	return &StatusReactionController{
		b:         b,
		chatID:    chatID,
		messageID: messageID,
		emojis:    emojis,
		timing:    timing,
		enabled:   enabled,
	}
}

func (c *StatusReactionController) SetQueued() {
	if !c.enabled {
		return
	}
	c.applyImmediate(c.emojis.Queued)
}

func (c *StatusReactionController) SetThinking() {
	if !c.enabled {
		return
	}
	c.scheduleDebounced(c.emojis.Thinking)
}

func (c *StatusReactionController) SetTool(toolName string) {
	if !c.enabled {
		return
	}
	emoji := classifyToolTelegram(toolName, &c.emojis)
	c.scheduleDebounced(emoji)
}

func (c *StatusReactionController) SetDone() {
	if !c.enabled {
		return
	}
	face := doneFaces[rand.Intn(len(doneFaces))]
	c.finish(face)
}

func (c *StatusReactionController) SetError() {
	if !c.enabled {
		return
	}
	c.finish(c.emojis.Error)
}

// SetCancelled is a terminal state for user-triggered cancellation
// (session/cancel). Distinct from SetError so downstream UI can tell the
// difference between a crash and an intentional stop.
// Telegram requires reactions to come from a fixed allowed set — 🙊
// ("speak-no-evil" monkey) matches the semantic of "agent stopped
// talking" within that set.
func (c *StatusReactionController) SetCancelled() {
	if !c.enabled {
		return
	}
	c.finish("🙊")
}

func (c *StatusReactionController) Clear() {
	if !c.enabled {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cancelTimers()
	if c.current != "" {
		c.current = ""
		go c.setReaction("")
	}
}

func (c *StatusReactionController) applyImmediate(emoji string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished || emoji == c.current {
		return
	}
	c.cancelDebounce()

	c.current = emoji
	go c.setReaction(emoji)
	c.resetStallTimersLocked()
}

func (c *StatusReactionController) scheduleDebounced(emoji string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished || emoji == c.current {
		c.resetStallTimersLocked()
		return
	}
	c.cancelDebounce()

	c.debounceTimer = time.AfterFunc(time.Duration(c.timing.DebounceMs)*time.Millisecond, func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.finished {
			return
		}
		c.current = emoji
		go c.setReaction(emoji)
	})
	c.resetStallTimersLocked()
}

func (c *StatusReactionController) finish(emoji string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished {
		return
	}
	c.finished = true
	c.cancelTimers()

	c.current = emoji
	go c.setReaction(emoji)
}

func (c *StatusReactionController) resetStallTimersLocked() {
	if c.stallSoftTimer != nil {
		c.stallSoftTimer.Stop()
	}
	if c.stallHardTimer != nil {
		c.stallHardTimer.Stop()
	}

	c.stallSoftTimer = time.AfterFunc(time.Duration(c.timing.StallSoftMs)*time.Millisecond, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.finished {
			return
		}
		c.current = "🥱"
		go c.setReaction("🥱")
	})

	c.stallHardTimer = time.AfterFunc(time.Duration(c.timing.StallHardMs)*time.Millisecond, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.finished {
			return
		}
		c.current = "🤯"
		go c.setReaction("🤯")
	})
}

func (c *StatusReactionController) cancelDebounce() {
	if c.debounceTimer != nil {
		c.debounceTimer.Stop()
		c.debounceTimer = nil
	}
}

func (c *StatusReactionController) cancelTimers() {
	c.cancelDebounce()
	if c.stallSoftTimer != nil {
		c.stallSoftTimer.Stop()
		c.stallSoftTimer = nil
	}
	if c.stallHardTimer != nil {
		c.stallHardTimer.Stop()
		c.stallHardTimer = nil
	}
}

// setReaction uses the Telegram Bot API setMessageReaction method.
// Telegram replaces all bot reactions on a message with each call.
// An empty emoji clears the reaction.
func (c *StatusReactionController) setReaction(emoji string) {
	var reaction []models.ReactionType
	if emoji != "" {
		reaction = []models.ReactionType{
			{
				Type: models.ReactionTypeTypeEmoji,
				ReactionTypeEmoji: &models.ReactionTypeEmoji{
					Emoji: emoji,
				},
			},
		}
	}

	_, err := c.b.SetMessageReaction(context.Background(), &bot.SetMessageReactionParams{
		ChatID:    c.chatID,
		MessageID: c.messageID,
		Reaction:  reaction,
	})
	if err != nil {
		slog.Debug("telegram setMessageReaction failed", "emoji", emoji, "error", err)
	}
}
