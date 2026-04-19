package discord

import (
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/quill/config"
)

var (
	codingTokens = []string{"exec", "process", "read", "write", "edit", "bash", "shell"}
	webTokens    = []string{"web_search", "web_fetch", "web-search", "web-fetch", "browser"}
)

func classifyTool(name string, emojis *config.ReactionEmojis) string {
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
	session        *discordgo.Session
	channelID      string
	messageID      string
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
	session *discordgo.Session,
	channelID string,
	messageID string,
	emojis config.ReactionEmojis,
	timing config.ReactionTiming,
) *StatusReactionController {
	return &StatusReactionController{
		session:   session,
		channelID: channelID,
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
	emoji := classifyTool(toolName, &c.emojis)
	c.scheduleDebounced(emoji)
}

func (c *StatusReactionController) SetDone() {
	if !c.enabled {
		return
	}
	c.finish(c.emojis.Done)
	// Add a random mood face
	faces := []string{"😊", "😎", "🫡", "🤓", "😏", "✌️", "💪", "🦾"}
	face := faces[rand.Intn(len(faces))]
	c.addReaction(face)
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
func (c *StatusReactionController) SetCancelled() {
	if !c.enabled {
		return
	}
	c.finish("🛑")
}

func (c *StatusReactionController) Clear() {
	if !c.enabled {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cancelTimers()
	if c.current != "" {
		c.removeReactionLocked(c.current)
		c.current = ""
	}
}

func (c *StatusReactionController) applyImmediate(emoji string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.finished || emoji == c.current {
		return
	}
	c.cancelDebounce()

	old := c.current
	c.current = emoji

	c.addReactionLocked(emoji)
	if old != "" && old != emoji {
		c.removeReactionLocked(old)
	}
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
		old := c.current
		c.current = emoji

		c.addReactionLocked(emoji)
		if old != "" && old != emoji {
			c.removeReactionLocked(old)
		}
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

	old := c.current
	c.current = emoji

	c.addReactionLocked(emoji)
	if old != "" && old != emoji {
		c.removeReactionLocked(old)
	}
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
		old := c.current
		c.current = "🥱"
		c.addReactionLocked("🥱")
		if old != "" && old != "🥱" {
			c.removeReactionLocked(old)
		}
	})

	c.stallHardTimer = time.AfterFunc(time.Duration(c.timing.StallHardMs)*time.Millisecond, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.finished {
			return
		}
		old := c.current
		c.current = "😨"
		c.addReactionLocked("😨")
		if old != "" && old != "😨" {
			c.removeReactionLocked(old)
		}
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

func (c *StatusReactionController) addReaction(emoji string) {
	if err := c.session.MessageReactionAdd(c.channelID, c.messageID, emoji); err != nil {
		slog.Debug("failed to add reaction", "emoji", emoji, "error", err)
	}
}

func (c *StatusReactionController) addReactionLocked(emoji string) {
	// Called while holding c.mu — fire and forget in a goroutine to avoid blocking
	go c.addReaction(emoji)
}

func (c *StatusReactionController) removeReactionLocked(emoji string) {
	go func() {
		if err := c.session.MessageReactionRemove(c.channelID, c.messageID, emoji, "@me"); err != nil {
			slog.Debug("failed to remove reaction", "emoji", emoji, "error", err)
		}
	}()
}
