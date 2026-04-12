package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/command"
	"github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/platform"
	"github.com/neilkuan/openab-go/stt"
	"github.com/neilkuan/openab-go/tts"
)

type Handler struct {
	Bot             *bot.Bot
	Pool            *acp.SessionPool
	AllowedChats    map[int64]bool
	ReactionsConfig config.ReactionsConfig
	Transcriber     stt.Transcriber
	Synthesizer     tts.Synthesizer
	VoiceStore      *tts.VoiceStore
	TTSConfig       config.TTSConfig
	botUser         *models.User
}

func (h *Handler) handleUpdate(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	msg := update.Message

	slog.Debug("telegram update",
		"chat_id", msg.Chat.ID,
		"chat_type", msg.Chat.Type,
		"text", msg.Text,
		"is_forum", msg.Chat.IsForum,
		"thread_id", msg.MessageThreadID,
	)
	h.handleMessage(ctx, b, msg)
}

func (h *Handler) handleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil || msg.From.IsBot {
		return
	}

	chatID := msg.Chat.ID
	threadID := topicThreadID(msg)

	// Check allowed chats
	if len(h.AllowedChats) > 0 && !h.AllowedChats[chatID] {
		slog.Warn("🚨👽🚨 telegram message from unlisted chat (add to allowed_chats to enable)",
			"chat_id", chatID, "chat_type", msg.Chat.Type, "chat_title", msg.Chat.Title)
		return
	}

	// Handle native /commands (works in both private and group chats)
	if cmdName := extractCommand(msg); cmdName != "" {
		// Telegram uses underscore for multi-word commands
		if cmdName == "voice_clear" {
			cmdName = "voice-clear"
		}
		if cmd, ok := command.ParseCommand(cmdName); ok {
			h.handleCommand(ctx, b, chatID, threadID, msg, cmd)
			return
		}
	}

	isPrivate := msg.Chat.Type == models.ChatTypePrivate

	// In group chats, respond to @mentions, replies to the bot,
	// or voice/audio messages (since Telegram UI doesn't allow @mentions in voice recordings).
	if !isPrivate {
		botUsername := h.botUser.Username
		mentioned := isBotMentioned(msg, botUsername)
		repliedToBot := msg.ReplyToMessage != nil &&
			msg.ReplyToMessage.From != nil &&
			msg.ReplyToMessage.From.ID == h.botUser.ID
		hasVoiceOrAudio := msg.Voice != nil || msg.Audio != nil

		if !mentioned && !repliedToBot && !hasVoiceOrAudio {
			return
		}
	}

	// Extract text from message or caption (photos use caption)
	prompt := msg.Text
	if prompt == "" && msg.Caption != "" {
		prompt = msg.Caption
	}

	// Strip @mention
	if !isPrivate {
		prompt = stripBotMention(prompt, h.botUser.Username)
	} else {
		prompt = strings.TrimSpace(prompt)
	}

	hasPhoto := len(msg.Photo) > 0
	hasVoice := msg.Voice != nil
	hasAudio := msg.Audio != nil
	hasDocument := msg.Document != nil

	// Also pick up attachments from the replied-to message.
	// On mobile Telegram, users often can't attach a file and @mention the bot
	// in the same message, so they send the file first, then reply to it.
	replyMsg := msg.ReplyToMessage
	hasReplyPhoto := replyMsg != nil && len(replyMsg.Photo) > 0 && !hasPhoto
	hasReplyDocument := replyMsg != nil && replyMsg.Document != nil && !hasDocument

	if prompt == "" && !hasPhoto && !hasVoice && !hasAudio && !hasDocument && !hasReplyPhoto && !hasReplyDocument {
		return
	}

	// Inject structured sender context
	senderName := msg.From.Username
	displayName := msg.From.FirstName
	if msg.From.LastName != "" {
		displayName += " " + msg.From.LastName
	}
	if senderName == "" {
		senderName = displayName
	}

	senderCtx := map[string]interface{}{
		"schema":       "openab.sender.v1",
		"sender_id":    fmt.Sprintf("%d", msg.From.ID),
		"sender_name":  senderName,
		"display_name": displayName,
		"channel":      "telegram",
		"channel_id":   fmt.Sprintf("%d", chatID),
		"is_bot":       false,
		"chat_type":    msg.Chat.Type,
	}
	if isForumTopic(msg) {
		senderCtx["topic_thread_id"] = threadID
	}
	senderJSON, _ := json.Marshal(senderCtx)
	promptWithSender := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), prompt)

	// Download photos (from current message or replied-to message)
	var imagePaths []string
	if hasPhoto || hasReplyPhoto {
		tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
		if err := os.MkdirAll(tmpDir, 0700); err != nil {
			slog.Error("failed to create temp directory", "error", err)
		} else {
			var photos []models.PhotoSize
			if hasPhoto {
				photos = msg.Photo
			} else {
				photos = replyMsg.Photo
			}
			// Telegram sends photos as []PhotoSize — last element is largest
			largest := photos[len(photos)-1]
			localPath, err := h.downloadFile(ctx, b, largest.FileID, "photo.jpg", tmpDir)
			if err != nil {
				slog.Error("failed to download photo", "error", err)
			} else {
				imagePaths = append(imagePaths, localPath)
				slog.Debug("downloaded telegram photo", "path", localPath)
			}
		}
	}

	// Transcribe voice/audio messages
	var transcriptions []string
	if hasVoice || hasAudio {
		if h.Transcriber == nil {
			slog.Warn("voice message received but [stt] is not configured, skipping")
			if prompt == "" {
				b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID:          chatID,
					MessageThreadID: threadID,
					Text:            "⚠️ Voice transcription is not configured. Please set up `[stt]` in config.",
					ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
				})
				return
			}
		} else {
			tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
			if err := os.MkdirAll(tmpDir, 0700); err != nil {
				slog.Error("failed to create temp directory", "error", err)
			} else {
				var fileID, filename string
				if hasVoice {
					fileID = msg.Voice.FileID
					filename = "voice.ogg"
				} else {
					fileID = msg.Audio.FileID
					filename = msg.Audio.FileName
					if filename == "" {
						filename = "audio.mp3"
					}
				}

				localPath, err := h.downloadFile(ctx, b, fileID, filename, tmpDir)
				if err != nil {
					slog.Error("failed to download audio", "error", err)
				} else {
					slog.Info("🎙️ stt: transcribing voice message", "filename", filename, "user", msg.From.Username)
					text, tErr := h.Transcriber.Transcribe(localPath)
					if removeErr := os.Remove(localPath); removeErr != nil {
						slog.Debug("failed to remove tmp audio", "path", localPath, "error", removeErr)
					}
					if tErr != nil {
						slog.Error("transcription failed", "error", tErr)
					} else {
						transcriptions = append(transcriptions, text)
						slog.Debug("transcribed audio", "text_length", len(text))
					}
				}
			}
		}
	}

	// Download document attachments (from current message or replied-to message)
	var fileAttachments []platform.FileAttachment
	if hasDocument || hasReplyDocument {
		tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
		if err := os.MkdirAll(tmpDir, 0700); err != nil {
			slog.Error("failed to create temp directory", "error", err)
		} else {
			var doc *models.Document
			if hasDocument {
				doc = msg.Document
			} else {
				doc = replyMsg.Document
			}
			// Telegram Bot API getFile limit is 20MB
			if doc.FileSize > 20*1024*1024 {
				slog.Warn("skipping large document", "filename", doc.FileName, "size", doc.FileSize)
				b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID:          chatID,
					MessageThreadID: threadID,
					Text:            fmt.Sprintf("⚠️ File `%s` exceeds the 20 MB limit (%d MB), skipping.", doc.FileName, doc.FileSize/(1024*1024)),
					ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
				})
			} else {
				filename := doc.FileName
				if filename == "" {
					filename = "document"
				}
				localPath, err := h.downloadFile(ctx, b, doc.FileID, filename, tmpDir)
				if err != nil {
					slog.Error("failed to download document", "filename", filename, "error", err)
				} else {
					contentType := doc.MimeType
					if contentType == "" {
						contentType = "application/octet-stream"
					}
					fileAttachments = append(fileAttachments, platform.FileAttachment{
						Filename:    filename,
						ContentType: contentType,
						Size:        int(doc.FileSize),
						LocalPath:   localPath,
					})
					slog.Debug("downloaded telegram document", "filename", filename, "path", localPath)
				}
			}
		}
	}

	// Build content blocks
	contentText := buildPromptContent(promptWithSender, imagePaths, transcriptions, fileAttachments)
	contentBlocks := []acp.ContentBlock{acp.TextBlock(contentText)}

	sessionKey := buildSessionKey(msg)

	slog.Debug("processing telegram message",
		"chat_id", chatID,
		"session_key", sessionKey,
		"thread_id", threadID,
		"has_photo", hasPhoto || hasReplyPhoto,
		"has_voice", hasVoice || hasAudio,
		"has_document", hasDocument || hasReplyDocument,
	)

	// Send initial "thinking" message as a reply
	sent, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            "💭 _thinking..._",
		ParseMode:       models.ParseModeMarkdownV1,
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
	})
	if err != nil {
		slog.Error("failed to send thinking message", "error", err)
		return
	}

	// Get or create ACP session
	if err := h.Pool.GetOrCreate(sessionKey); err != nil {
		b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    chatID,
			MessageID: sent.ID,
			Text:      fmt.Sprintf("⚠️ Failed to start agent: %v", err),
		})
		slog.Error("pool error", "error", err)
		return
	}

	reactions := NewStatusReactionController(
		h.ReactionsConfig.Enabled,
		b,
		chatID,
		msg.ID,
		h.ReactionsConfig.Emojis,
		h.ReactionsConfig.Timing,
	)
	reactions.SetQueued()

	finalText, result := h.streamPrompt(ctx, b, sessionKey, contentBlocks, chatID, sent.ID, threadID, reactions)

	// Cleanup downloaded images and file attachments
	for _, p := range imagePaths {
		if err := os.Remove(p); err != nil {
			slog.Debug("failed to remove tmp image", "path", p, "error", err)
		}
	}
	for _, f := range fileAttachments {
		if err := os.Remove(f.LocalPath); err != nil {
			slog.Debug("failed to remove tmp file", "path", f.LocalPath, "error", err)
		}
	}

	if result == nil {
		reactions.SetDone()
	} else {
		reactions.SetError()
	}

	// TTS: synthesize voice reply only when the user sent a voice/audio message
	if result == nil && h.Synthesizer != nil && finalText != "" && (hasVoice || hasAudio) {
		userID := fmt.Sprintf("%d", msg.From.ID)
		go h.sendVoiceReply(ctx, b, chatID, sent.ID, userID, finalText)
	}
}

func (h *Handler) handleCommand(ctx context.Context, b *bot.Bot, chatID int64, threadID int, msg *models.Message, cmd *command.Command) {
	userID := fmt.Sprintf("%d", msg.From.ID)
	var response string

	switch cmd.Name {
	case command.CmdSessions:
		response = command.ExecuteSessions(h.Pool)
	case command.CmdInfo:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		response = command.ExecuteInfo(h.Pool, sessionKey, h.buildVoiceInfo(userID))
	case command.CmdReset:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		response = command.ExecuteReset(h.Pool, sessionKey)
	case command.CmdSetVoice:
		response = h.handleSetVoice(ctx, b, msg, userID)
	case command.CmdVoiceClear:
		response = h.handleVoiceClear(userID)
	case command.CmdVoiceMode:
		response = h.handleVoiceMode(userID, cmd.Args)
	default:
		return
	}

	chunks := platform.SplitMessage(response, 4096)
	for _, chunk := range chunks {
		converted := convertToTelegramMarkdown(chunk)
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            converted,
			ParseMode:       models.ParseModeMarkdownV1,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		if err != nil {
			// Fallback to plain text if markdown fails
			slog.Debug("telegram markdown send failed, retrying plain", "error", err)
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          chatID,
				MessageThreadID: threadID,
				Text:            chunk,
				ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
			})
		}
	}
}

func (h *Handler) streamPrompt(
	ctx context.Context,
	b *bot.Bot,
	sessionKey string,
	content []acp.ContentBlock,
	chatID int64,
	msgID int,
	threadID int,
	reactions *StatusReactionController,
) (string, error) {
	var finalText string
	err := h.Pool.WithConnection(sessionKey, func(conn *acp.AcpConnection) error {
		reset := conn.SessionReset
		conn.SessionReset = false

		rx, _, err := conn.SessionPrompt(content)
		if err != nil {
			return err
		}
		reactions.SetThinking()

		var textBuf strings.Builder
		var toolLines []string
		if reset {
			textBuf.WriteString("⚠️ _Session expired, starting fresh..._\n\n")
		}

		// Edit-streaming goroutine (2s interval for Telegram rate limits)
		var displayMu sync.Mutex
		currentDisplay := "💭 _thinking..._"
		if reset {
			currentDisplay = "⚠️ _Session expired, starting fresh..._\n\n..."
		}
		done := make(chan struct{})

		go func() {
			lastContent := ""
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					displayMu.Lock()
					content := currentDisplay
					displayMu.Unlock()

					if content != lastContent {
						preview := platform.TruncateUTF8(content, 4000, "\n…")
						_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
							ChatID:    chatID,
							MessageID: msgID,
							Text:      preview,
						})
						if err != nil {
							slog.Debug("telegram edit message failed", "error", err)
						}
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

		if promptErr != nil {
			b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: msgID,
				Text:      fmt.Sprintf("⚠️ %v", promptErr),
			})
			return promptErr
		}

		// Final message — split for 4096-char Telegram limit
		finalText = textBuf.String()
		finalContent := composeDisplay(toolLines, finalText)
		if finalContent == "" {
			finalContent = "_(no response)_"
		}

		chunks := platform.SplitMessage(finalContent, 4096)
		for i, chunk := range chunks {
			converted := convertToTelegramMarkdown(chunk)
			if i == 0 {
				_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    chatID,
					MessageID: msgID,
					Text:      converted,
					ParseMode: models.ParseModeMarkdownV1,
				})
				if err != nil {
					slog.Debug("telegram markdown edit failed, retrying plain", "error", err)
					b.EditMessageText(ctx, &bot.EditMessageTextParams{
						ChatID:    chatID,
						MessageID: msgID,
						Text:      chunk,
					})
				}
			} else {
				_, err := b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID:          chatID,
					MessageThreadID: threadID,
					Text:            converted,
					ParseMode:       models.ParseModeMarkdownV1,
				})
				if err != nil {
					slog.Debug("telegram markdown send failed, retrying plain", "error", err)
					b.SendMessage(ctx, &bot.SendMessageParams{
						ChatID:          chatID,
						MessageThreadID: threadID,
						Text:            chunk,
					})
				}
			}
		}

		return nil
	})
	return finalText, err
}

// --- Voice command handlers ---

func (h *Handler) handleSetVoice(ctx context.Context, b *bot.Bot, msg *models.Message, userID string) string {
	if h.VoiceStore == nil || h.Synthesizer == nil {
		return "TTS is not configured."
	}

	replyMsg := msg.ReplyToMessage
	if replyMsg == nil || (replyMsg.Voice == nil && replyMsg.Audio == nil) {
		return "Please reply to a voice message with /setvoice to set your custom voice."
	}

	var fileID, filename string
	if replyMsg.Voice != nil {
		fileID = replyMsg.Voice.FileID
		filename = "voice.ogg"
	} else {
		fileID = replyMsg.Audio.FileID
		filename = replyMsg.Audio.FileName
		if filename == "" {
			filename = "audio.mp3"
		}
	}

	tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return fmt.Sprintf("Failed: %v", err)
	}

	localPath, err := h.downloadFile(ctx, b, fileID, filename, tmpDir)
	if err != nil {
		return fmt.Sprintf("Failed to download voice: %v", err)
	}
	defer os.Remove(localPath)

	voiceID, err := h.Synthesizer.CreateVoice(msg.From.Username, localPath)
	if err != nil {
		return fmt.Sprintf("Failed to create voice: %v", err)
	}

	if err := h.VoiceStore.SetVoice(userID, voiceID); err != nil {
		return fmt.Sprintf("Voice created but failed to save: %v", err)
	}
	return fmt.Sprintf("Your custom voice has been created! (ID: `%s`)", voiceID)
}

func (h *Handler) handleVoiceClear(userID string) string {
	if h.VoiceStore == nil {
		return "TTS is not configured."
	}
	if err := h.VoiceStore.RemoveVoice(userID); err != nil {
		return fmt.Sprintf("Failed to clear voice: %v", err)
	}
	return "Your custom voice has been cleared. Bot will use the default voice."
}

func (h *Handler) handleVoiceMode(userID, args string) string {
	if h.VoiceStore == nil {
		return "TTS is not configured."
	}
	mode := strings.TrimSpace(strings.ToLower(args))
	switch mode {
	case "echo":
		h.VoiceStore.SetEchoMode(userID, true)
		return "Voice mode set to *echo* — bot will reply using your voice."
	case "default", "":
		h.VoiceStore.SetEchoMode(userID, false)
		return "Voice mode set to *default* — bot will use the configured voice."
	default:
		return "Unknown mode. Use `/voicemode echo` or `/voicemode default`."
	}
}

// sendVoiceReply synthesizes and sends a voice message.
// Uses per-user custom voice if set, otherwise default voice.
func (h *Handler) sendVoiceReply(ctx context.Context, b *bot.Bot, chatID int64, replyToMsgID int, userID, text string) {
	var audioPath string
	var err error

	usedVoice := h.TTSConfig.Voice
	if h.VoiceStore != nil {
		if voiceID := h.VoiceStore.GetVoice(userID); voiceID != "" {
			usedVoice = voiceID
			audioPath, err = h.Synthesizer.SynthesizeWithVoice(text, voiceID)
		}
	}
	if audioPath == "" && err == nil {
		audioPath, err = h.Synthesizer.Synthesize(text)
	}
	slog.Info("🔊 tts: synthesizing voice reply", "user", userID, "voice", usedVoice, "text_length", len(text))
	if err != nil {
		slog.Error("tts synthesis failed", "error", err)
		return
	}
	defer os.Remove(audioPath)

	// Open the audio file for reading
	file, err := os.Open(audioPath)
	if err != nil {
		slog.Error("failed to open audio file", "error", err)
		return
	}
	defer file.Close()

	// Send voice message using new go-telegram/bot API
	_, err = b.SendVoice(ctx, &bot.SendVoiceParams{
		ChatID:          chatID,
		MessageThreadID: 0,
		Voice: &models.InputFileUpload{
			Filename: "voice.ogg",
			Data:     file,
		},
		ReplyParameters: &models.ReplyParameters{MessageID: replyToMsgID},
	})
	if err != nil {
		slog.Error("failed to send tts voice", "error", err)
	}
}

func (h *Handler) buildVoiceInfo(userID string) *command.VoiceInfo {
	vi := &command.VoiceInfo{
		STTEnabled: h.Transcriber != nil,
		TTSEnabled: h.Synthesizer != nil,
		TTSModel:   h.TTSConfig.Model,
		TTSVoice:   h.TTSConfig.Voice,
	}
	if h.Transcriber != nil {
		vi.STTProvider = "openai"
	}
	if h.VoiceStore != nil && userID != "" {
		vi.CustomVoice = h.VoiceStore.GetVoice(userID)
	}
	return vi
}

// --- Helper functions ---

// convertToTelegramMarkdown converts GFM-style markdown to Telegram Markdown v1.
// Main conversion: **bold** → *bold* (Telegram uses single asterisk for bold).
var gfmBoldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

func convertToTelegramMarkdown(text string) string {
	return gfmBoldRe.ReplaceAllString(text, "*$1*")
}

// isForumTopic returns true when this message is in a forum topic thread.
func isForumTopic(msg *models.Message) bool {
	return msg.Chat.IsForum && msg.IsTopicMessage
}

// topicThreadID returns the topic thread ID if the message is in a forum topic, 0 otherwise.
func topicThreadID(msg *models.Message) int {
	if msg.IsTopicMessage {
		return msg.MessageThreadID
	}
	return 0
}

// buildSessionKey creates a session pool key that includes the topic thread ID
// for forum supergroups: "tg:{chatID}:{threadID}", or "tg:{chatID}" otherwise.
func buildSessionKey(msg *models.Message) string {
	if isForumTopic(msg) {
		return fmt.Sprintf("tg:%d:%d", msg.Chat.ID, msg.MessageThreadID)
	}
	return fmt.Sprintf("tg:%d", msg.Chat.ID)
}

// buildSessionKeyFromChat creates a session key from chatID and optional threadID.
// Used by command handlers where the full Message is not available.
func buildSessionKeyFromChat(chatID int64, threadID int) string {
	if threadID != 0 {
		return fmt.Sprintf("tg:%d:%d", chatID, threadID)
	}
	return fmt.Sprintf("tg:%d", chatID)
}

// extractCommand returns the bot command name (without /) if the message starts
// with a /command entity, or empty string otherwise.
func extractCommand(msg *models.Message) string {
	for _, e := range msg.Entities {
		if e.Type == models.MessageEntityTypeBotCommand && e.Offset == 0 {
			cmd := msg.Text[e.Offset : e.Offset+e.Length]
			if strings.HasPrefix(cmd, "/") {
				cmd = cmd[1:]
			}
			// Remove @botname suffix (e.g., "reset@mybot" → "reset")
			if idx := strings.Index(cmd, "@"); idx != -1 {
				cmd = cmd[:idx]
			}
			return cmd
		}
	}
	return ""
}

func isBotMentioned(msg *models.Message, botUsername string) bool {
	// Check text entities
	for _, entity := range msg.Entities {
		if entity.Type == models.MessageEntityTypeMention {
			mention := msg.Text[entity.Offset : entity.Offset+entity.Length]
			if strings.EqualFold(mention, "@"+botUsername) {
				return true
			}
		}
	}
	// Check caption entities (photos with captions)
	for _, entity := range msg.CaptionEntities {
		if entity.Type == models.MessageEntityTypeMention {
			mention := msg.Caption[entity.Offset : entity.Offset+entity.Length]
			if strings.EqualFold(mention, "@"+botUsername) {
				return true
			}
		}
	}
	return false
}

func stripBotMention(text, botUsername string) string {
	lower := strings.ToLower(text)
	target := strings.ToLower("@" + botUsername)
	idx := strings.Index(lower, target)
	if idx >= 0 {
		text = text[:idx] + text[idx+len(target):]
	}
	return strings.TrimSpace(text)
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

func buildPromptContent(base string, imagePaths, transcriptions []string, files []platform.FileAttachment) string {
	var extra strings.Builder

	if len(imagePaths) > 0 {
		extra.WriteString("\n\n<attached_images>\n")
		for _, p := range imagePaths {
			extra.WriteString(fmt.Sprintf("- %s\n", p))
		}
		extra.WriteString("</attached_images>\nPlease read and analyze the above image(s).")
	}

	if len(transcriptions) > 0 {
		extra.WriteString("\n\n<voice_transcription>\n")
		for _, t := range transcriptions {
			extra.WriteString(t)
			extra.WriteByte('\n')
		}
		extra.WriteString("</voice_transcription>\nThe above is a transcription of the user's voice message. Please respond to it.")
	}

	extra.WriteString(platform.FormatFileBlock(files))

	return base + extra.String()
}

func (h *Handler) downloadFile(ctx context.Context, b *bot.Bot, fileID, filename, tmpDir string) (string, error) {
	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("get file: %w", err)
	}

	fileURL := b.FileDownloadLink(file)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(fileURL)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned %d", resp.StatusCode)
	}

	safeFilename := filepath.Base(filename)
	localName := fmt.Sprintf("%d_%s", time.Now().UnixMilli(), safeFilename)
	localPath := filepath.Join(tmpDir, localName)

	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create file failed: %w", err)
	}

	// 25MB limit (Whisper API limit for audio, generous for images)
	written, err := io.Copy(f, io.LimitReader(resp.Body, 25*1024*1024+1))
	if err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("write failed: %w", err)
	}
	if written > 25*1024*1024 {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("file too large (>25MB)")
	}

	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("close file failed: %w", err)
	}

	return localPath, nil
}
