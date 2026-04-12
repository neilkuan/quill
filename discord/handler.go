package discord

import (
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

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/command"
	"github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/platform"
	"github.com/neilkuan/openab-go/stt"
	"github.com/neilkuan/openab-go/tts"
)

type Handler struct {
	Pool            *acp.SessionPool
	AllowedChannels map[string]bool
	ReactionsConfig config.ReactionsConfig
	Transcriber     stt.Transcriber
	Synthesizer     tts.Synthesizer
	VoiceStore      *tts.VoiceStore
	TTSConfig       config.TTSConfig
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

	hasImages := len(m.Attachments) > 0 && hasImageAttachments(m.Attachments)
	hasAudio := len(m.Attachments) > 0 && hasAudioAttachments(m.Attachments)
	hasFiles := len(m.Attachments) > 0 && hasFileAttachments(m.Attachments)
	if prompt == "" && !hasImages && !hasAudio && !hasFiles {
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

	// Download images to .tmp/ and append paths to prompt
	var imagePaths []string
	if hasImages {
		tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
		if err := os.MkdirAll(tmpDir, 0700); err != nil {
			slog.Error("failed to create temp image directory", "path", tmpDir, "error", err)
			return
		}

		for _, att := range m.Attachments {
			if !isImageMime(att.ContentType, att.Filename) {
				continue
			}
			if att.Size > 10*1024*1024 {
				slog.Warn("skipping large image", "filename", att.Filename, "size", att.Size)
				continue
			}
			localPath, err := downloadImageToFile(att.URL, att.Filename, tmpDir)
			if err != nil {
				slog.Error("failed to download image", "url", att.URL, "error", err)
				continue
			}
			imagePaths = append(imagePaths, localPath)
			slog.Debug("downloaded image", "filename", att.Filename, "path", localPath)
		}
	}

	// Transcribe audio attachments via external API (e.g. Whisper)
	// ACP agents cannot process binary audio files directly, so transcription is required.
	var transcriptions []string
	if hasAudio {
		if h.Transcriber == nil {
			slog.Warn("voice message received but [stt] is not configured, skipping audio")
			if prompt == "" && !hasImages {
				s.ChannelMessageSend(m.ChannelID, "⚠️ Voice transcription is not configured. Please set up `[stt]` in config.")
				return
			}
		} else {
			tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
			if err := os.MkdirAll(tmpDir, 0700); err != nil {
				slog.Error("failed to create temp audio directory", "path", tmpDir, "error", err)
			} else {
				for _, att := range m.Attachments {
					if !isAudioMime(att.ContentType, att.Filename) {
						continue
					}
					if att.Size > 25*1024*1024 {
						slog.Warn("skipping large audio", "filename", att.Filename, "size", att.Size)
						continue
					}
					localPath, err := downloadAudioToFile(att.URL, att.Filename, tmpDir)
					if err != nil {
						slog.Error("failed to download audio", "url", att.URL, "error", err)
						continue
					}
					slog.Debug("downloaded audio", "filename", att.Filename, "path", localPath)

					// Handle "setvoice" command: create custom voice via OpenAI API
					if strings.EqualFold(strings.TrimSpace(prompt), "setvoice") && h.VoiceStore != nil && h.Synthesizer != nil {
						voiceID, createErr := h.Synthesizer.CreateVoice(m.Author.Username, localPath)
						os.Remove(localPath)
						if createErr != nil {
							s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⚠️ Failed to create voice: %v", createErr))
						} else {
							if sErr := h.VoiceStore.SetVoice(m.Author.ID, voiceID); sErr != nil {
								s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⚠️ Voice created but failed to save: %v", sErr))
							} else {
								s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("✅ Your custom voice has been created! (ID: `%s`)", voiceID))
							}
						}
						return
					}

					slog.Info("🎙️ stt: transcribing voice message", "filename", att.Filename, "user", m.Author.Username)
					text, err := h.Transcriber.Transcribe(localPath)
					if removeErr := os.Remove(localPath); removeErr != nil {
						slog.Debug("failed to remove tmp audio", "path", localPath, "error", removeErr)
					}
					if err != nil {
						slog.Error("transcription failed", "filename", att.Filename, "error", err)
						continue
					}
					transcriptions = append(transcriptions, text)
					slog.Debug("transcribed audio", "filename", att.Filename, "text_length", len(text))
				}
			}
		}
	}

	// Download non-image, non-audio file attachments to .tmp/
	var fileAttachments []platform.FileAttachment
	if hasFiles {
		tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
		if err := os.MkdirAll(tmpDir, 0700); err != nil {
			slog.Error("failed to create temp file directory", "path", tmpDir, "error", err)
		} else {
			for _, att := range m.Attachments {
				if isImageMime(att.ContentType, att.Filename) || isAudioMime(att.ContentType, att.Filename) {
					continue
				}
				localPath, err := downloadFileToDisk(att.URL, att.Filename, tmpDir)
				if err != nil {
					slog.Error("failed to download file attachment", "url", att.URL, "error", err)
					continue
				}
				fileAttachments = append(fileAttachments, platform.FileAttachment{
					Filename:    att.Filename,
					ContentType: att.ContentType,
					Size:        att.Size,
					LocalPath:   localPath,
				})
				slog.Debug("downloaded file attachment", "filename", att.Filename, "path", localPath)
			}
		}
	}

	// Build content blocks
	contentText := buildPromptContent(promptWithSender, imagePaths, transcriptions, fileAttachments)
	var contentBlocks []acp.ContentBlock
	contentBlocks = append(contentBlocks, acp.TextBlock(contentText))

	slog.Debug("processing", "prompt", promptWithSender, "images", len(imagePaths), "audio_transcriptions", len(transcriptions), "files", len(fileAttachments), "in_thread", inThread)

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

	threadKey := buildSessionKey(threadID)
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

	finalText, result := streamPrompt(h.Pool, threadKey, contentBlocks, s, threadID, thinkingMsg.ID, reactions)

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

	// TTS: synthesize voice reply only when the user sent a voice message
	if result == nil && h.Synthesizer != nil && finalText != "" && hasAudio {
		go h.sendVoiceReply(s, threadID, m.Author.ID, finalText)
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
	h.registerSlashCommands(s, r.User.ID)
}

// slashCommands defines the Discord Application Commands to register.
var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "sessions",
		Description: "List all active agent sessions",
	},
	{
		Name:        "info",
		Description: "Show current thread/channel session details",
	},
	{
		Name:        "reset",
		Description: "Reset the current session (kills agent, fresh start on next message)",
	},
	{
		Name:        "setvoice",
		Description: "Set your custom bot voice (attach a 3-10s audio file)",
	},
	{
		Name:        "voice-clear",
		Description: "Clear your custom voice, revert to default",
	},
	{
		Name:        "voicemode",
		Description: "Set voice reply mode: echo (use your voice) or default",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "mode",
				Description: "echo = bot uses your voice, default = normal",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "echo", Value: "echo"},
					{Name: "default", Value: "default"},
				},
			},
		},
	},
}

func (h *Handler) registerSlashCommands(s *discordgo.Session, appID string) {
	for _, cmd := range slashCommands {
		if _, err := s.ApplicationCommandCreate(appID, "", cmd); err != nil {
			slog.Error("failed to register slash command", "command", cmd.Name, "error", err)
		} else {
			slog.Info("registered slash command", "command", "/"+cmd.Name)
		}
	}
}

func (h *Handler) OnInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	data := i.ApplicationCommandData()
	userID := ""
	if i.Member != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}
	var response string

	switch data.Name {
	case command.CmdSessions:
		response = command.ExecuteSessions(h.Pool)
	case command.CmdInfo:
		threadKey := buildSessionKey(i.ChannelID)
		response = command.ExecuteInfo(h.Pool, threadKey, h.buildVoiceInfo(userID))
	case command.CmdReset:
		threadKey := buildSessionKey(i.ChannelID)
		response = command.ExecuteReset(h.Pool, threadKey)
	case command.CmdSetVoice:
		response = h.handleSetVoice(s, i, userID)
	case command.CmdVoiceClear:
		response = h.handleVoiceClear(userID)
	case command.CmdVoiceMode:
		mode := ""
		for _, opt := range data.Options {
			if opt.Name == "mode" {
				mode = opt.StringValue()
			}
		}
		response = h.handleVoiceMode(userID, mode)
	default:
		return
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: response,
		},
	})
}

func streamPrompt(
	pool *acp.SessionPool,
	threadKey string,
	content []acp.ContentBlock,
	s *discordgo.Session,
	channelID string,
	msgID string,
	reactions *StatusReactionController,
) (string, error) {
	var finalText string
	err := pool.WithConnection(threadKey, func(conn *acp.AcpConnection) error {
		reset := conn.SessionReset
		conn.SessionReset = false

		rx, _, err := conn.SessionPrompt(content)
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
		finalText = textBuf.String()
		finalContent := composeDisplay(toolLines, finalText)
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
	return finalText, err
}

func buildSessionKey(threadID string) string {
	return fmt.Sprintf("discord:%s", threadID)
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

// --- Voice command handlers ---

func (h *Handler) handleSetVoice(s *discordgo.Session, i *discordgo.InteractionCreate, userID string) string {
	if h.VoiceStore == nil || h.Synthesizer == nil {
		return "TTS is not configured."
	}

	// Discord slash commands don't natively support file attachments.
	// Guide the user to send audio + "setvoice" as a regular message.
	return "To set your voice: send a voice/audio message with the text `setvoice`.\n" +
		"The bot will upload it to OpenAI and create your custom voice."
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

func (h *Handler) handleVoiceMode(userID, mode string) string {
	if h.VoiceStore == nil {
		return "TTS is not configured."
	}
	switch mode {
	case "echo":
		h.VoiceStore.SetEchoMode(userID, true)
		return "Voice mode set to **echo** — bot will reply using your voice."
	case "default":
		h.VoiceStore.SetEchoMode(userID, false)
		return "Voice mode set to **default** — bot will use the configured voice."
	default:
		return "Unknown mode. Use `echo` or `default`."
	}
}

// sendVoiceReply synthesizes and sends a voice message.
// Uses per-user custom voice if set, otherwise default voice.
func (h *Handler) sendVoiceReply(s *discordgo.Session, channelID, userID, text string) {
	var audioPath string
	var err error

	// Priority: per-user custom voice → default configured voice
	voice := h.TTSConfig.Voice
	if h.VoiceStore != nil {
		if voiceID := h.VoiceStore.GetVoice(userID); voiceID != "" {
			voice = voiceID
			audioPath, err = h.Synthesizer.SynthesizeWithVoice(text, voiceID)
		}
	}
	if audioPath == "" && err == nil {
		audioPath, err = h.Synthesizer.Synthesize(text)
	}
	slog.Info("🔊 tts: synthesizing voice reply", "user", userID, "voice", voice, "text_length", len(text))
	if err != nil {
		slog.Error("tts synthesis failed", "error", err)
		return
	}
	defer os.Remove(audioPath)

	f, err := os.Open(audioPath)
	if err != nil {
		slog.Error("failed to open tts audio", "error", err)
		return
	}
	defer f.Close()

	if _, err := s.ChannelFileSend(channelID, "voice_reply.mp3", f); err != nil {
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

// --- Prompt content builder ---

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

// --- Image attachment helpers ---

var imageExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

func hasImageAttachments(attachments []*discordgo.MessageAttachment) bool {
	for _, att := range attachments {
		if isImageMime(att.ContentType, att.Filename) {
			return true
		}
	}
	return false
}

func isImageMime(contentType, filename string) bool {
	if strings.HasPrefix(contentType, "image/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	_, ok := imageExtensions[ext]
	return ok
}

func downloadImageToFile(url, filename, tmpDir string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned %d", resp.StatusCode)
	}

	// Sanitize filename to prevent path traversal
	safeFilename := filepath.Base(filename)
	localName := fmt.Sprintf("%d_%s", time.Now().UnixMilli(), safeFilename)
	localPath := filepath.Join(tmpDir, localName)

	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create file failed: %w", err)
	}

	written, err := io.Copy(f, io.LimitReader(resp.Body, 10*1024*1024+1))
	if err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("write failed: %w", err)
	}
	if written > 10*1024*1024 {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("image too large (>10MB)")
	}

	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("close file failed: %w", err)
	}

	return localPath, nil
}

// --- Audio attachment helpers ---

var audioExtensions = map[string]string{
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".webm": "audio/webm",
	".mp4":  "audio/mp4",
}

func hasAudioAttachments(attachments []*discordgo.MessageAttachment) bool {
	for _, att := range attachments {
		if isAudioMime(att.ContentType, att.Filename) {
			return true
		}
	}
	return false
}

func isAudioMime(contentType, filename string) bool {
	if strings.HasPrefix(contentType, "audio/") {
		return true
	}
	// Discord voice messages may use video/webm or video/ogg container
	if contentType == "video/webm" || contentType == "video/ogg" {
		return true
	}
	ext := strings.ToLower(filepath.Ext(filename))
	_, ok := audioExtensions[ext]
	return ok
}

// --- File attachment helpers ---

func hasFileAttachments(attachments []*discordgo.MessageAttachment) bool {
	for _, att := range attachments {
		if !isImageMime(att.ContentType, att.Filename) && !isAudioMime(att.ContentType, att.Filename) {
			return true
		}
	}
	return false
}

func downloadFileToDisk(url, filename, tmpDir string) (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
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

	// Discord client already enforces upload limits (25/50/100 MB by boost level),
	// so no server-side size check needed here — just stream the file.
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("write failed: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("close file failed: %w", err)
	}

	return localPath, nil
}

func downloadAudioToFile(url, filename, tmpDir string) (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
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

	// Whisper API limit is 25MB
	written, err := io.Copy(f, io.LimitReader(resp.Body, 25*1024*1024+1))
	if err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("write failed: %w", err)
	}
	if written > 25*1024*1024 {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("audio too large (>25MB)")
	}

	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("close file failed: %w", err)
	}

	return localPath, nil
}
