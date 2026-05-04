package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
	"github.com/neilkuan/quill/config"
	"github.com/neilkuan/quill/cronjob"
	"github.com/neilkuan/quill/markdown"
	"github.com/neilkuan/quill/platform"
	"github.com/neilkuan/quill/sessionpicker"
	"github.com/neilkuan/quill/stt"
	"github.com/neilkuan/quill/tts"
)

type Handler struct {
	Bot            *bot.Bot
	Pool           *acp.SessionPool
	AllowedChats   map[int64]bool
	AllowedUserIDs map[int64]bool
	// AllowAnyUser is true when allowed_user_id contains "*" — anyone can talk
	// to the bot from any chat.
	AllowAnyUser    bool
	ReactionsConfig config.ReactionsConfig
	Transcriber     stt.Transcriber
	Synthesizer     tts.Synthesizer
	TTSConfig       config.TTSConfig
	// MarkdownTableMode controls how GFM tables in agent replies are rewritten
	// before being sent to Telegram. See markdown.TableMode for options.
	MarkdownTableMode markdown.TableMode
	// Picker lists historical sessions for /pick. Nil when
	// the configured agent backend is not recognised by sessionpicker.Detect.
	Picker    sessionpicker.Picker
	CronStore *cronjob.Store
	CronCfg   config.CronjobConfig
	botUser   *models.User
}

func (h *Handler) handleUpdate(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery != nil {
		h.handleCallbackQuery(ctx, b, update.CallbackQuery)
		return
	}
	if update.Message == nil {
		return
	}
	msg := update.Message

	var fromID int64
	var fromUsername string
	if msg.From != nil {
		fromID = msg.From.ID
		fromUsername = msg.From.Username
	}

	slog.Debug("telegram update",
		"chat_id", msg.Chat.ID,
		"chat_type", msg.Chat.Type,
		"user_id", fromID,
		"username", fromUsername,
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

	// AllowedUserIDs (or AllowAnyUser for "*"), when set, overrides AllowedChats:
	// listed users (or any user for "*") are accepted from any chat.
	userGateActive := h.AllowAnyUser || len(h.AllowedUserIDs) > 0
	if userGateActive {
		if !h.AllowAnyUser && !h.AllowedUserIDs[msg.From.ID] {
			slog.Warn("🚨👽🚨 telegram message from unlisted user (add to allowed_user_id to enable)",
				"user_id", msg.From.ID, "username", msg.From.Username, "chat_id", chatID)
			return
		}
	} else if len(h.AllowedChats) > 0 && !h.AllowedChats[chatID] {
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
			slog.Debug("telegram command dispatched",
				"raw", cmdName, "name", cmd.Name, "args", cmd.Args,
				"chat_id", chatID, "user_id", msg.From.ID)
			h.handleCommand(ctx, b, chatID, threadID, msg, cmd)
			return
		}
		slog.Debug("telegram text looked like a command but was not recognised",
			"extracted", cmdName, "text", msg.Text, "chat_id", chatID)
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
			// Dump entities so mention-detection failures are diagnosable
			// (e.g. a @mention that came through as a different entity type,
			// or a text mention keyed by user_id rather than @username).
			entityTypes := make([]string, 0, len(msg.Entities))
			for _, e := range msg.Entities {
				entityTypes = append(entityTypes, string(e.Type))
			}
			slog.Debug("telegram group message not addressed to bot, ignoring",
				"chat_id", chatID,
				"user_id", msg.From.ID,
				"bot_username", botUsername,
				"mentioned", mentioned,
				"replied_to_bot", repliedToBot,
				"has_voice_or_audio", hasVoiceOrAudio,
				"text", msg.Text,
				"entity_types", entityTypes,
				"entity_count", len(msg.Entities))
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
		"schema":       "quill.sender.v1",
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

	// Owner descriptor used both for the placeholder text and the
	// later SessionPrompt call so /info-style introspection can name
	// who is currently running on this connection.
	ownerDesc := "user"
	if msg.From != nil && msg.From.Username != "" {
		ownerDesc = "user @" + msg.From.Username
	} else if msg.From != nil && msg.From.FirstName != "" {
		ownerDesc = "user " + msg.From.FirstName
	}

	// If the connection already has another prompt running (e.g. a
	// long cron fire), tell the user up front instead of showing a
	// silent "thinking…" they'll mistake for the bot dying.
	thinkingText := "💭 <i>thinking…</i>"
	if conn := h.Pool.Connection(sessionKey); conn != nil && conn.Alive() {
		if busy, owner := conn.IsBusy(); busy {
			thinkingText = fmt.Sprintf("⏳ <i>queued behind %s — agent is busy. Use /stop to cancel and run yours first.</i>", html.EscapeString(owner))
		}
	}

	// Send initial "thinking" / "queued" message as a reply
	sent, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            thinkingText,
		ParseMode:       models.ParseModeHTML,
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

	finalText, cancelled, result := h.streamPrompt(ctx, b, sessionKey, contentBlocks, chatID, sent.ID, threadID, reactions, ownerDesc)

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

	switch {
	case cancelled:
		reactions.SetCancelled()
	case result == nil:
		reactions.SetDone()
	default:
		reactions.SetError()
	}

	// TTS: synthesize voice reply only when the user sent a voice/audio message
	// (skip if cancelled — the text is partial).
	if result == nil && !cancelled && h.Synthesizer != nil && finalText != "" && (hasVoice || hasAudio) {
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
	case command.CmdResume:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		response = command.ExecuteResume(h.Pool, sessionKey)
	case command.CmdStop:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		response = command.ExecuteStop(h.Pool, sessionKey)
	case command.CmdPicker:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		args := strings.TrimSpace(cmd.Args)
		// Empty args or `all` → interactive keyboard; `/pick <N>`,
		// `/pick load <id>` and typos keep the text path so the
		// existing workflow is unchanged.
		bypassCWD := strings.EqualFold(args, "all")
		if args == "" || bypassCWD {
			h.sendPickPicker(ctx, b, chatID, threadID, msg, sessionKey, bypassCWD)
			return
		}
		response = command.ExecutePicker(h.Pool, h.Picker, sessionKey, args, h.Pool.WorkingDir())
	case command.CmdMode:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		if strings.TrimSpace(cmd.Args) != "" {
			response = command.ExecuteMode(h.Pool, sessionKey, cmd.Args)
			break
		}
		h.sendModePicker(ctx, b, chatID, threadID, msg, sessionKey)
		return
	case command.CmdModel:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		if strings.TrimSpace(cmd.Args) != "" {
			response = command.ExecuteModel(h.Pool, sessionKey, cmd.Args)
			break
		}
		h.sendModelPicker(ctx, b, chatID, threadID, msg, sessionKey)
		return
	case command.CmdHelp:
		response = command.ExecuteHelp()
	case command.CmdCron:
		sessionKey := buildSessionKeyFromChat(chatID, threadID)
		sub, _, _, _ := command.ParseCronArgs(strings.TrimSpace(cmd.Args))
		if sub == "list" {
			h.sendCronList(ctx, b, chatID, threadID, msg, sessionKey)
			return
		}
		response = h.handleCronCommand(sessionKey, msg, cmd.Args)
	default:
		return
	}

	chunks := platform.SplitMessage(response, 4096)
	for _, chunk := range chunks {
		converted := convertToTelegramHTML(chunk)
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            converted,
			ParseMode:       models.ParseModeHTML,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		if err != nil {
			// Fallback to plain text if HTML parse fails (e.g. unbalanced
			// tag escaped from agent output).
			slog.Debug("telegram html send failed, retrying plain", "error", err)
			b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          chatID,
				MessageThreadID: threadID,
				Text:            chunk,
				ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
			})
		}
	}
}

// modeCallbackPrefix marks callback_data produced by the /mode inline
// keyboard. Format: "mode|<sessionKey>|<modeID>". Session keys and
// mode ids emitted by Kiro do not contain the `|` separator, and the
// whole payload must fit into Telegram's 64-byte callback_data cap
// (session key ~22 bytes + mode id ~20 bytes + prefix 5 = 47 typical).
const modeCallbackPrefix = "mode|"

// modelCallbackPrefix is the /model analogue of modeCallbackPrefix.
// Format: "model|<sessionKey>|<modelID>".
const modelCallbackPrefix = "model|"

// pickCallbackPrefix is the /pick analogue, but carries a 1-based
// index into the cached listing (not the full session id) because
// session UUIDs would blow Telegram's 64-byte callback_data cap for
// typical threadKeys. Format: "pick|<sessionKey>|<N>".
const pickCallbackPrefix = "pick|"

// cronCallbackPrefix marks callback_data produced by the /cron list
// inline keyboard. Format: "cron|rm|<sessionKey>|<id>" — the sub-action
// is currently always "rm" but kept explicit for future extensions.
const cronCallbackPrefix = "cron|"

// telegramCallbackDataMax is Telegram's hard limit for
// callback_data payloads (bytes). The BotAPI rejects buttons above
// this and we lose the whole SendMessage if we try anyway, so
// pickers defensively skip entries that would overflow instead.
const telegramCallbackDataMax = 64

// sendModePicker posts a message with an inline keyboard, one button
// per available mode, so users can tap to switch instead of typing
// `/mode <id>`. The current mode is marked with ➤ in the button label.
func (h *Handler) sendModePicker(ctx context.Context, b *bot.Bot, chatID int64, threadID int, msg *models.Message, sessionKey string) {
	listing := command.ListModes(h.Pool, sessionKey)
	if listing.Err != nil || len(listing.Available) == 0 {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            listing.Message,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		return
	}

	rows := make([][]models.InlineKeyboardButton, 0, len(listing.Available))
	var skipped int
	for _, m := range listing.Available {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		if m.ID == listing.Current {
			label = "➤ " + label
		}
		cb := modeCallbackPrefix + sessionKey + "|" + m.ID
		if len(cb) > telegramCallbackDataMax {
			// Skip rather than lose the whole SendMessage to a too-long
			// callback_data. This is rare — only hit when thread key +
			// mode id exceed ~60 bytes combined — but we log so users
			// at least have a breadcrumb if a mode vanishes from the UI.
			slog.Warn("skipping telegram mode button: callback_data over cap",
				"mode_id", m.ID, "len", len(cb), "cap", telegramCallbackDataMax)
			skipped++
			continue
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         label,
			CallbackData: cb,
		}})
	}
	if len(rows) == 0 {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            fmt.Sprintf("No mode fits in an interactive button (skipped %d). Use <code>/mode &lt;id&gt;</code> directly instead.", skipped),
			ParseMode:       models.ParseModeHTML,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		return
	}

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            fmt.Sprintf("Select a mode (current: <code>%s</code>)", listing.Current),
		ParseMode:       models.ParseModeHTML,
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		ReplyMarkup:     &models.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
}

// sendModelPicker is the /model analogue of sendModePicker.
func (h *Handler) sendModelPicker(ctx context.Context, b *bot.Bot, chatID int64, threadID int, msg *models.Message, sessionKey string) {
	listing := command.ListModels(h.Pool, sessionKey)
	if listing.Err != nil || len(listing.Available) == 0 {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            listing.Message,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		return
	}

	rows := make([][]models.InlineKeyboardButton, 0, len(listing.Available))
	var skipped int
	for _, m := range listing.Available {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		if m.ID == listing.Current {
			label = "➤ " + label
		}
		cb := modelCallbackPrefix + sessionKey + "|" + m.ID
		if len(cb) > telegramCallbackDataMax {
			slog.Warn("skipping telegram model button: callback_data over cap",
				"model_id", m.ID, "len", len(cb), "cap", telegramCallbackDataMax)
			skipped++
			continue
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         label,
			CallbackData: cb,
		}})
	}
	if len(rows) == 0 {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            fmt.Sprintf("No model fits in an interactive button (skipped %d). Use <code>/model &lt;id&gt;</code> directly instead.", skipped),
			ParseMode:       models.ParseModeHTML,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		return
	}

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            fmt.Sprintf("Select a model (current: <code>%s</code>)", listing.Current),
		ParseMode:       models.ParseModeHTML,
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		ReplyMarkup:     &models.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
}

// sendPickPicker renders /pick as an InlineKeyboard. Each button
// carries the 1-based index of the session in the cached listing —
// the same cache /pick <N> reads — so a tap is resolved by
// command.LoadPickerByIndex.
func (h *Handler) sendPickPicker(ctx context.Context, b *bot.Bot, chatID int64, threadID int, msg *models.Message, sessionKey string, bypassCWD bool) {
	cwd := h.Pool.WorkingDir()
	listing := command.ListPickerSessions(h.Picker, sessionKey, cwd, bypassCWD)
	if listing.Err != nil || len(listing.Sessions) == 0 {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            listing.Message,
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		})
		return
	}

	rows := make([][]models.InlineKeyboardButton, 0, len(listing.Sessions))
	for i, sess := range listing.Sessions {
		title := sess.Title
		if title == "" {
			title = "(untitled)"
		}
		// Telegram InlineKeyboardButton.Text has a practical 64-char
		// visual cap; truncating keeps long titles readable.
		label := fmt.Sprintf("%d. %s", i+1, truncateForButton(title, 48))
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         label,
			CallbackData: pickCallbackPrefix + sessionKey + "|" + strconv.Itoa(i+1),
		}})
	}

	header := fmt.Sprintf("Select a session (%s)", listing.AgentType)
	if bypassCWD {
		header += " — all cwds"
	} else if listing.CWD != "" {
		header += fmt.Sprintf(" — cwd: <code>%s</code>", listing.CWD)
	}
	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            header,
		ParseMode:       models.ParseModeHTML,
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		ReplyMarkup:     &models.InlineKeyboardMarkup{InlineKeyboard: rows},
	})
}

// sendCronList renders the thread's scheduled prompts as an inline
// keyboard with a 🗑️ button per row. Tap to delete via the
// cronCallbackPrefix path.
func (h *Handler) sendCronList(ctx context.Context, b *bot.Bot, chatID int64, threadID int,
	msg *models.Message, sessionKey string) {

	if h.CronStore == nil || h.CronCfg.Disabled {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            "⚠️ Cron jobs are disabled on this bot.",
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		}); err != nil {
			slog.Warn("telegram /cron list (disabled) send failed", "error", err)
		}
		return
	}

	tz, _ := time.LoadLocation(h.CronCfg.Timezone)
	if tz == nil {
		tz = time.UTC
	}

	jobs := command.ListCronJobs(h.CronStore, sessionKey)
	if len(jobs) == 0 {
		if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: threadID,
			Text:            "📭 No scheduled prompts. Create one with /cron add <schedule> <prompt>.",
			ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		}); err != nil {
			slog.Warn("telegram /cron list (empty) send failed", "error", err)
		}
		return
	}

	// Plain text body: cron expressions contain '*' which Telegram's
	// legacy Markdown mode would try to interpret as bold markers and
	// silently reject the whole message. The list reads fine without
	// formatting; the InlineKeyboard buttons carry the interactive bit.
	rows := make([][]models.InlineKeyboardButton, 0, len(jobs))
	var body strings.Builder
	body.WriteString("⏰ Scheduled prompts in this thread\n\n")
	for _, j := range jobs {
		body.WriteString(fmt.Sprintf("%s — %s — next %s\n  → %s\n",
			j.ID, j.Schedule, j.NextFire.In(tz).Format("2006-01-02 15:04 MST"),
			truncate(j.Prompt, 80)))

		cb := cronCallbackPrefix + "rm|" + sessionKey + "|" + j.ID
		if len(cb) > telegramCallbackDataMax {
			// Skip the button if the callback_data would exceed Telegram's cap.
			// User can still delete via `/cron rm <id>`.
			slog.Warn("skipping telegram cron button: callback_data over cap",
				"job_id", j.ID, "len", len(cb), "cap", telegramCallbackDataMax)
			continue
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{Text: "🗑️ " + j.ID, CallbackData: cb},
		})
	}

	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: threadID,
		Text:            body.String(),
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		ReplyMarkup:     &models.InlineKeyboardMarkup{InlineKeyboard: rows},
	}); err != nil {
		slog.Warn("telegram /cron list send failed", "error", err)
	}
}

func truncateForButton(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// handleCallbackQuery routes button taps from inline keyboards. The
// known sources are /mode ("mode|"), /model ("model|") and /pick
// ("pick|"); unknown callback_data is acknowledged silently so the
// spinner on the client clears without producing user-visible noise.
func (h *Handler) handleCallbackQuery(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	data := cq.Data
	// Always answer first, even on unrelated callbacks, otherwise the
	// Telegram client keeps the loading spinner on the button.
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID})

	// Order matters: "model|" has to be checked before "mode|" because
	// "model" contains "mode" as a prefix-string (though not as our
	// full prefix including "|", the safer check is to test the longer
	// prefix first anyway in case the format ever changes).
	var resultMsg string
	switch {
	case strings.HasPrefix(data, modelCallbackPrefix):
		rest := strings.TrimPrefix(data, modelCallbackPrefix)
		parts := strings.SplitN(rest, "|", 2)
		if len(parts) != 2 {
			return
		}
		resultMsg = command.ExecuteModel(h.Pool, parts[0], parts[1])
	case strings.HasPrefix(data, modeCallbackPrefix):
		rest := strings.TrimPrefix(data, modeCallbackPrefix)
		parts := strings.SplitN(rest, "|", 2)
		if len(parts) != 2 {
			return
		}
		resultMsg = command.ExecuteMode(h.Pool, parts[0], parts[1])
	case strings.HasPrefix(data, pickCallbackPrefix):
		rest := strings.TrimPrefix(data, pickCallbackPrefix)
		parts := strings.SplitN(rest, "|", 2)
		if len(parts) != 2 {
			return
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}
		resultMsg = command.LoadPickerByIndex(h.Pool, parts[0], n)
	case strings.HasPrefix(data, cronCallbackPrefix):
		// Format: cron|<action>|<sessionKey>|<id>; only "rm" is implemented in V1.
		rest := strings.TrimPrefix(data, cronCallbackPrefix)
		parts := strings.SplitN(rest, "|", 3)
		if len(parts) != 3 || parts[0] != "rm" {
			return
		}
		if h.CronStore == nil {
			resultMsg = "⚠️ Cron jobs are disabled on this bot."
		} else {
			resultMsg = command.ExecuteCronRemove(h.CronStore, parts[1], parts[2])
		}
	default:
		return
	}

	if cq.Message.Message != nil {
		chatID := cq.Message.Message.Chat.ID
		msgID := cq.Message.Message.ID
		b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      chatID,
			MessageID:   msgID,
			Text:        resultMsg,
			ReplyMarkup: nil, // drop the keyboard — prevents a second tap re-firing
		})
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
	owner string,
) (string, bool, error) {
	var finalText string
	var cancelled bool
	err := h.Pool.WithConnection(sessionKey, func(conn *acp.AcpConnection) error {
		rx, _, reset, resumed, err := conn.SessionPrompt(content, owner)
		if err != nil {
			return err
		}
		reactions.SetThinking()

		var textBuf strings.Builder
		var toolLines []string
		if resumed {
			textBuf.WriteString("🔄 _Session restored from previous conversation._\n\n")
		} else if reset {
			textBuf.WriteString("⚠️ _Session expired, starting fresh..._\n\n")
		}

		// Edit-streaming goroutine (2s interval for Telegram rate limits)
		var displayMu sync.Mutex
		currentDisplay := "💭 _thinking..._"
		if resumed {
			currentDisplay = "🔄 _Session restored, continuing..._\n\n..."
		} else if reset {
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

		// Process ACP notifications — wrapped in a retry loop so we can
		// recover from Copilot's reasoning_effort mismatch by switching
		// model and re-sending the same prompt once.
		var promptErr error
		promptHeld := true
		retryAttempted := false
	retryLoop:
		for {
			for notification := range rx {
				if notification.ID != nil {
					if notification.Error != nil {
						promptErr = notification.Error
					} else {
						reason := acp.StopReason(notification)
						if reason == "cancelled" {
							cancelled = true
						}
						if reason != "" {
							slog.Info("acp: prompt completed",
								"thread_key", conn.ThreadKey,
								"session_id", conn.SessionID,
								"stop_reason", reason)
						}
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
						if label, ok := platform.FormatToolTitle(event.Title, h.ReactionsConfig.ToolDisplay); ok {
							toolLines = append(toolLines, fmt.Sprintf("🔧 `%s`...", label))
							display := composeDisplay(toolLines, textBuf.String())
							displayMu.Lock()
							currentDisplay = display
							displayMu.Unlock()
						}
					}

				case acp.AcpEventToolDone:
					reactions.SetThinking()
					if event.Title == "" {
						// tool_call_update may omit `title` when the agent only
						// reports a status change. Ignore — we don't know which
						// line to update, and matching with an empty string would
						// hit every line via strings.Contains(s, "") == true.
						continue
					}
					icon := "✅"
					if event.Status != "completed" {
						icon = "❌"
					}
					if label, ok := platform.FormatToolTitle(event.Title, h.ReactionsConfig.ToolDisplay); ok {
						for i := len(toolLines) - 1; i >= 0; i-- {
							if strings.Contains(toolLines[i], label) {
								toolLines[i] = fmt.Sprintf("%s `%s`", icon, label)
								break
							}
						}
						display := composeDisplay(toolLines, textBuf.String())
						displayMu.Lock()
						currentDisplay = display
						displayMu.Unlock()
					}
				}
			}

			if retryAttempted || promptErr != nil || cancelled {
				break retryLoop
			}
			cleanSoFar := platform.StripAgentRetryNoise(textBuf.String())
			if !platform.IsCopilotReasoningEffortError(cleanSoFar) {
				break retryLoop
			}
			available, current := conn.Models()
			ids := make([]string, len(available))
			for i, m := range available {
				ids[i] = m.ID
			}
			newModel, ok := platform.PickFallbackModel(ids, current)
			if !ok {
				break retryLoop
			}
			retryAttempted = true
			conn.PromptDone()
			promptHeld = false
			if setErr := conn.SessionSetModel(newModel); setErr != nil {
				slog.Warn("copilot auto-retry: set_model failed",
					"thread_key", conn.ThreadKey, "session_id", conn.SessionID,
					"fallback_model", newModel, "error", setErr)
				break retryLoop
			}
			textBuf.Reset()
			toolLines = toolLines[:0]
			textBuf.WriteString(fmt.Sprintf(
				"🔄 _Copilot 拒絕 `%s` + reasoning_effort；已切換到 `%s` 並重試..._\n\n",
				current, newModel))
			displayMu.Lock()
			currentDisplay = textBuf.String()
			displayMu.Unlock()
			slog.Info("copilot auto-retry: switching model after reasoning_effort rejection",
				"thread_key", conn.ThreadKey, "session_id", conn.SessionID,
				"previous_model", current, "fallback_model", newModel)
			newRx, _, _, _, promptErr2 := conn.SessionPrompt(content, owner)
			if promptErr2 != nil {
				slog.Warn("copilot auto-retry: session_prompt failed",
					"thread_key", conn.ThreadKey, "session_id", conn.SessionID,
					"error", promptErr2)
				break retryLoop
			}
			rx = newRx
			promptHeld = true
		}

		if promptHeld {
			conn.PromptDone()
		}
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
		// Copilot CLI streams retry spam and its own execution errors as
		// agent_message_chunk text (stopReason stays "end_turn"), so strip
		// the noise and flag self-reported errors before composing the reply.
		finalText = platform.StripAgentRetryNoise(finalText)
		agentErrored := platform.DetectAgentError(finalText)
		finalContent := composeDisplay(toolLines, finalText)
		if finalContent == "" {
			if cancelled {
				finalContent = "🛑 _已取消_"
			} else {
				finalContent = "_(no response)_"
			}
		} else if cancelled {
			finalContent = strings.TrimRight(finalContent, " \t\n") + "\n\n🛑 _— 已取消_"
		}
		if agentErrored {
			// The agent failed before producing a real answer — wrap the
			// output as an error block and omit the mode/model footer,
			// which would misleadingly imply a successful reply from the
			// advertised backend.
			finalContent = "⚠️ **Agent error**\n" + finalContent
		} else {
			// Append a mode/model footer so users know which persona and
			// backend produced the reply without having to run /info.
			_, mode := conn.Modes()
			_, model := conn.Models()
			finalContent += platform.FormatSessionFooter(mode, model)
		}
		// Rewrite GFM tables before splitting — Telegram Markdown v1 doesn't
		// render table syntax, so we wrap them in fenced blocks (or convert
		// to bullets) for readable rendering. Skipped during streaming preview.
		finalContent = markdown.ConvertTables(finalContent, h.MarkdownTableMode)

		chunks := platform.SplitMessage(finalContent, 4096)
		for i, chunk := range chunks {
			converted := convertToTelegramHTML(chunk)
			if i == 0 {
				_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    chatID,
					MessageID: msgID,
					Text:      converted,
					ParseMode: models.ParseModeHTML,
				})
				if err != nil {
					slog.Debug("telegram html edit failed, retrying plain", "error", err)
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
					ParseMode:       models.ParseModeHTML,
				})
				if err != nil {
					slog.Debug("telegram html send failed, retrying plain", "error", err)
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
	return finalText, cancelled, err
}

// sendVoiceReply synthesizes and sends a voice message.
func (h *Handler) sendVoiceReply(ctx context.Context, b *bot.Bot, chatID int64, replyToMsgID int, userID, text string) {
	slog.Info("🔊 tts: synthesizing voice reply", "user", userID, "voice", h.TTSConfig.Voice, "text_length", len(text))
	audioPath, err := h.Synthesizer.Synthesize(text)
	if err != nil {
		slog.Error("tts synthesis failed", "error", err)
		return
	}
	defer os.Remove(audioPath)

	file, err := os.Open(audioPath)
	if err != nil {
		slog.Error("failed to open audio file", "error", err)
		return
	}
	defer file.Close()

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
	return vi
}

// --- Helper functions ---

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

// extractCommand returns the command and any trailing args exactly as
// ParseCommand expects (e.g. "pick 3"). It strips the leading slash
// and any `@botname` suffix from the command, then appends whatever
// text follows so numeric or string arguments are preserved.
//
// Prefers the bot_command entity Telegram attaches to /commands, but
// falls back to parsing msg.Text directly when no entity is present —
// this happens in edge cases such as captions copied as text or
// forwarded commands, and the fallback is cheap enough to always run.
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
			rest := strings.TrimLeft(msg.Text[e.Offset+e.Length:], " \t")
			if rest != "" {
				return cmd + " " + rest
			}
			return cmd
		}
	}
	// Fallback: text starts with `/` but Telegram did not attach a
	// bot_command entity (observed for some clients / forwarded
	// messages). Only the first whitespace-delimited token is treated
	// as the command name, to keep shapes like "/mode ask" working.
	text := strings.TrimLeft(msg.Text, " \t")
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	text = text[1:]
	head := text
	rest := ""
	if idx := strings.IndexAny(text, " \t"); idx != -1 {
		head = text[:idx]
		rest = strings.TrimLeft(text[idx:], " \t")
	}
	if idx := strings.Index(head, "@"); idx != -1 {
		head = head[:idx]
	}
	if head == "" {
		return ""
	}
	if rest != "" {
		return head + " " + rest
	}
	return head
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

// handleCronCommand routes /cron <sub> <args> to the cronjob package
// and returns the chat-friendly text response.
func (h *Handler) handleCronCommand(threadKey string, msg *models.Message, args string) string {
	if h.CronStore == nil || h.CronCfg.Disabled {
		return "⚠️ Cron jobs are disabled on this bot."
	}
	sub, schedule, prompt, err := command.ParseCronArgs(args)
	if err != nil {
		return fmt.Sprintf("⚠️ %v\n\nUsage: `/cron add <schedule> <prompt>`, `/cron list`, `/cron rm <id>`", err)
	}
	tz, _ := time.LoadLocation(h.CronCfg.Timezone)
	if tz == nil {
		tz = time.UTC
	}
	minInterval := time.Duration(h.CronCfg.MinIntervalSeconds) * time.Second

	switch sub {
	case "add":
		senderID := fmt.Sprintf("%d", msg.From.ID)
		senderName := msg.From.Username
		if senderName == "" {
			senderName = msg.From.FirstName
			if msg.From.LastName != "" {
				senderName += " " + msg.From.LastName
			}
		}
		_, replyMsg := command.ExecuteCronAdd(h.CronStore, threadKey,
			senderID, senderName,
			schedule, prompt,
			h.CronCfg.MaxPerThread, minInterval, tz)
		return replyMsg
	case "list":
		jobs := command.ListCronJobs(h.CronStore, threadKey)
		return formatCronList(jobs, tz)
	case "rm":
		// schedule slot carries the id for rm
		return command.ExecuteCronRemove(h.CronStore, threadKey, schedule)
	}
	return "⚠️ Unknown cron subcommand"
}

func formatCronList(jobs []cronjob.Job, tz *time.Location) string {
	if len(jobs) == 0 {
		return "📭 No scheduled prompts in this thread.\n\nCreate one with `/cron add <schedule> <prompt>`."
	}
	var sb strings.Builder
	sb.WriteString("⏰ *Scheduled prompts in this thread*\n\n")
	for _, j := range jobs {
		sb.WriteString(fmt.Sprintf("`%s` — `%s` — next %s\n  → %s\n",
			j.ID, j.Schedule, j.NextFire.In(tz).Format("2006-01-02 15:04 MST"),
			truncate(j.Prompt, 80)))
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
