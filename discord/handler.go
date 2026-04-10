package discord

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/platform"
)

type Handler struct {
	Pool            *acp.SessionPool
	AllowedChannels map[string]bool
	ReactionsConfig config.ReactionsConfig
}

func (h *Handler) OnMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot {
		return
	}

	botID := s.State.User.ID
	channelID := m.ChannelID
	inAllowedChannel := len(h.AllowedChannels) == 0 || h.AllowedChannels[channelID]

	isMentioned := false
	for _, u := range m.Mentions {
		if u.ID == botID {
			isMentioned = true
			break
		}
	}
	if !isMentioned {
		isMentioned = strings.Contains(m.Content, fmt.Sprintf("<@%s>", botID))
	}

	inThread := false
	if !inAllowedChannel {
		ch, err := s.Channel(channelID)
		if err == nil && ch.ParentID != "" {
			if h.AllowedChannels[ch.ParentID] {
				inThread = true
			}
		}
	}

	if !inAllowedChannel && !inThread {
		return
	}
	if !inThread && !isMentioned {
		return
	}

	prompt := m.Content
	if isMentioned {
		prompt = stripMention(prompt)
	} else {
		prompt = strings.TrimSpace(prompt)
	}
	if prompt == "" {
		return
	}

	// Inject structured sender context
	displayName := m.Author.Username
	if m.Member != nil && m.Member.Nick != "" {
		displayName = m.Member.Nick
	}

	senderCtx := map[string]interface{}{
		"schema":       "openab.sender.v1",
		"sender_id":    m.Author.ID,
		"sender_name":  m.Author.Username,
		"display_name": displayName,
		"channel":      "discord",
		"channel_id":   m.ChannelID,
		"is_bot":       m.Author.Bot,
	}
	senderJSON, _ := json.Marshal(senderCtx)
	promptWithSender := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), prompt)

	slog.Debug("processing", "prompt", promptWithSender, "in_thread", inThread)

	var threadID string
	if inThread {
		threadID = channelID
	} else {
		var err error
		threadID, err = getOrCreateThread(s, m.Message, prompt)
		if err != nil {
			slog.Error("failed to create thread", "error", err)
			return
		}
	}

	thinkingMsg, err := s.ChannelMessageSend(threadID, "💭 _thinking..._")
	if err != nil {
		slog.Error("failed to post", "error", err)
		return
	}

	threadKey := threadID
	if err := h.Pool.GetOrCreate(threadKey); err != nil {
		s.ChannelMessageEdit(threadID, thinkingMsg.ID, fmt.Sprintf("⚠️ Failed to start agent: %v", err))
		slog.Error("pool error", "error", err)
		return
	}

	reactions := NewStatusReactionController(
		h.ReactionsConfig.Enabled,
		s,
		m.ChannelID,
		m.ID,
		h.ReactionsConfig.Emojis,
		h.ReactionsConfig.Timing,
	)
	reactions.SetQueued()

	result := streamPrompt(h.Pool, threadKey, promptWithSender, s, threadID, thinkingMsg.ID, reactions)

	if result == nil {
		reactions.SetDone()
	} else {
		reactions.SetError()
	}

	// Hold emoji briefly then clear
	holdMs := h.ReactionsConfig.Timing.DoneHoldMs
	if result != nil {
		holdMs = h.ReactionsConfig.Timing.ErrorHoldMs
	}
	if h.ReactionsConfig.RemoveAfterReply {
		go func() {
			time.Sleep(time.Duration(holdMs) * time.Millisecond)
			reactions.Clear()
		}()
	}

	if result != nil {
		s.ChannelMessageEdit(threadID, thinkingMsg.ID, fmt.Sprintf("⚠️ %v", result))
	}
}

func (h *Handler) OnReady(s *discordgo.Session, r *discordgo.Ready) {
	slog.Info("discord bot connected", "user", r.User.Username)
}

func streamPrompt(
	pool *acp.SessionPool,
	threadKey string,
	prompt string,
	s *discordgo.Session,
	channelID string,
	msgID string,
	reactions *StatusReactionController,
) error {
	return pool.WithConnection(threadKey, func(conn *acp.AcpConnection) error {
		reset := conn.SessionReset
		conn.SessionReset = false

		rx, _, err := conn.SessionPrompt(prompt)
		if err != nil {
			return err
		}
		reactions.SetThinking()

		initial := "💭 _thinking..._"
		if reset {
			initial = "⚠️ _Session expired, starting fresh..._\n\n..."
		}

		var textBuf strings.Builder
		var toolLines []string
		if reset {
			textBuf.WriteString("⚠️ _Session expired, starting fresh..._\n\n")
		}

		// Shared state for edit-streaming
		var displayMu sync.Mutex
		currentDisplay := initial
		currentMsgID := msgID
		done := make(chan struct{})

		// Spawn edit-streaming goroutine — truncate only, never send new messages.
		// Split into multiple messages only on final edit after streaming ends.
		go func() {
			lastContent := ""
			ticker := time.NewTicker(1500 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					displayMu.Lock()
					content := currentDisplay
					displayMu.Unlock()

					if content != lastContent {
						preview := platform.TruncateUTF8(content, 1900, "\n…")
						s.ChannelMessageEdit(channelID, msgID, preview)
						lastContent = content
					}
				case <-done:
					return
				}
			}
		}()

		// Process ACP notifications
		var promptErr error
		for notification := range rx {
			if notification.ID != nil {
				if notification.Error != nil {
					promptErr = notification.Error
				}
				break
			}

			event := acp.ClassifyNotification(notification)
			if event == nil {
				continue
			}

			switch event.Type {
			case acp.AcpEventText:
				textBuf.WriteString(event.Text)
				display := composeDisplay(toolLines, textBuf.String())
				displayMu.Lock()
				currentDisplay = display
				displayMu.Unlock()

			case acp.AcpEventThinking:
				reactions.SetThinking()

			case acp.AcpEventToolStart:
				if event.Title != "" {
					reactions.SetTool(event.Title)
					toolLines = append(toolLines, fmt.Sprintf("🔧 `%s`...", event.Title))
					display := composeDisplay(toolLines, textBuf.String())
					displayMu.Lock()
					currentDisplay = display
					displayMu.Unlock()
				}

			case acp.AcpEventToolDone:
				reactions.SetThinking()
				icon := "✅"
				if event.Status != "completed" {
					icon = "❌"
				}
				// Update the matching tool line
				for i := len(toolLines) - 1; i >= 0; i-- {
					if strings.Contains(toolLines[i], event.Title) {
						toolLines[i] = fmt.Sprintf("%s `%s`", icon, event.Title)
						break
					}
				}
				display := composeDisplay(toolLines, textBuf.String())
				displayMu.Lock()
				currentDisplay = display
				displayMu.Unlock()
			}
		}

		conn.PromptDone()
		close(done)

		// If the prompt returned an error, surface it
		if promptErr != nil {
			s.ChannelMessageEdit(channelID, currentMsgID, fmt.Sprintf("⚠️ %v", promptErr))
			return promptErr
		}

		// Final edit
		finalContent := composeDisplay(toolLines, textBuf.String())
		if finalContent == "" {
			finalContent = "_(no response)_"
		}

		chunks := platform.SplitMessage(finalContent, 2000)
		for i, chunk := range chunks {
			if i == 0 {
				s.ChannelMessageEdit(channelID, currentMsgID, chunk)
			} else {
				s.ChannelMessageSend(channelID, chunk)
			}
		}

		return nil
	})
}

func composeDisplay(toolLines []string, text string) string {
	var out strings.Builder
	if len(toolLines) > 0 {
		for _, line := range toolLines {
			out.WriteString(line)
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}
	out.WriteString(strings.TrimRight(text, " \t\n"))
	return out.String()
}

var mentionRe = regexp.MustCompile(`<@[!&]?\d+>`)

func stripMention(content string) string {
	return strings.TrimSpace(mentionRe.ReplaceAllString(content, ""))
}

var githubURLRe = regexp.MustCompile(`https?://github\.com/([^/]+/[^/]+)/(issues|pull)/(\d+)`)

func shortenThreadName(prompt string) string {
	shortened := githubURLRe.ReplaceAllString(prompt, "$1#$3")
	runes := []rune(shortened)
	if len(runes) > 40 {
		return string(runes[:40]) + "..."
	}
	return shortened
}

func getOrCreateThread(s *discordgo.Session, msg *discordgo.Message, prompt string) (string, error) {
	ch, err := s.Channel(msg.ChannelID)
	if err == nil && ch.IsThread() {
		return msg.ChannelID, nil
	}

	threadName := shortenThreadName(prompt)

	thread, err := s.MessageThreadStartComplex(msg.ChannelID, msg.ID, &discordgo.ThreadStart{
		Name:                threadName,
		AutoArchiveDuration: 1440, // OneDay = 1440 minutes
	})
	if err != nil {
		return "", err
	}

	return thread.ID, nil
}
