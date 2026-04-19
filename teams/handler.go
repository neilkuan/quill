package teams

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
	"github.com/neilkuan/quill/config"
	"github.com/neilkuan/quill/markdown"
	"github.com/neilkuan/quill/platform"
	"github.com/neilkuan/quill/sessionpicker"
	"github.com/neilkuan/quill/stt"
	"github.com/neilkuan/quill/tts"
)

type Handler struct {
	Pool              *acp.SessionPool
	Client            *BotClient
	AllowedChannels   map[string]bool
	AllowedUserIDs    map[string]bool
	AllowAnyUser      bool
	Transcriber       stt.Transcriber
	Synthesizer       tts.Synthesizer
	TTSConfig         config.TTSConfig
	MarkdownTableMode markdown.TableMode
	ToolDisplay       string
	// Picker lists historical sessions for /session-picker. Nil when
	// the configured agent backend is not recognised by sessionpicker.Detect.
	Picker sessionpicker.Picker
}

// OnMessage handles incoming message activities from Teams
func (h *Handler) OnMessage(activity *Activity) {
	if activity.From.ID == "" {
		return
	}

	conversationID := activity.Conversation.ID
	userID := activity.From.ID

	slog.Debug("teams message received",
		"conversation_id", conversationID,
		"user_id", userID,
		"user_name", activity.From.Name,
		"text", activity.Text,
		"attachments", len(activity.Attachments),
		"entities", len(activity.Entities))

	// Access gate: allowed_user_id takes precedence over allowed_channels
	userGateActive := h.AllowAnyUser || len(h.AllowedUserIDs) > 0
	if userGateActive {
		if !h.AllowAnyUser && !h.AllowedUserIDs[userID] {
			slog.Warn("🚨👽🚨 teams message from unlisted user (add to allowed_user_id to enable)",
				"user_id", userID, "user_name", activity.From.Name)
			return
		}
	} else if len(h.AllowedChannels) > 0 && !h.AllowedChannels[conversationID] {
		slog.Warn("🚨👽🚨 teams message from unlisted channel (add to allowed_channels to enable)",
			"conversation_id", conversationID)
		return
	}

	// Check for bot mention and extract prompt
	isMentioned := isBotMentioned(activity.Recipient.ID, activity.Entities)
	prompt := activity.Text
	if isMentioned {
		prompt = stripBotMention(prompt, activity.Recipient.ID, activity.Entities)
	} else {
		prompt = strings.TrimSpace(prompt)
	}

	// Check for command
	if cmd, ok := command.ParseCommand(prompt); ok {
		h.handleCommand(activity, cmd)
		return
	}

	// Check if we have content to process
	hasAttachments := len(activity.Attachments) > 0
	if prompt == "" && !hasAttachments {
		return
	}

	// Inject structured sender context
	senderCtx := map[string]interface{}{
		"schema":       "quill.sender.v1",
		"sender_id":    activity.From.ID,
		"sender_name":  activity.From.Name,
		"display_name": activity.From.Name,
		"channel":      "teams",
		"channel_id":   activity.Conversation.ID,
	}
	senderJSON, _ := json.Marshal(senderCtx)
	promptWithSender := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), prompt)

	// Download attachments (images, files)
	var imagePaths []string
	var fileAttachments []platform.FileAttachment
	if hasAttachments {
		tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
		if err := os.MkdirAll(tmpDir, 0700); err != nil {
			slog.Error("failed to create temp directory", "path", tmpDir, "error", err)
		} else {
			for _, att := range activity.Attachments {
				if isImageContentType(att.ContentType) {
					localPath, err := h.downloadAttachment(att, tmpDir)
					if err != nil {
						slog.Error("failed to download image", "url", att.ContentURL, "error", err)
						continue
					}
					imagePaths = append(imagePaths, localPath)
					slog.Debug("downloaded image", "filename", att.Name, "path", localPath)
				} else if isAudioContentType(att.ContentType) {
					localPath, err := h.downloadAttachment(att, tmpDir)
					if err != nil {
						slog.Error("failed to download audio", "url", att.ContentURL, "error", err)
						continue
					}

					if h.Transcriber == nil {
						slog.Warn("voice message received but [stt] is not configured, skipping audio")
						if err := os.Remove(localPath); err != nil {
							slog.Debug("failed to remove tmp audio", "path", localPath, "error", err)
						}
						continue
					}

					slog.Info("🎙️ stt: transcribing voice message", "filename", att.Name, "user", activity.From.Name)
					text, err := h.Transcriber.Transcribe(localPath)
					if removeErr := os.Remove(localPath); removeErr != nil {
						slog.Debug("failed to remove tmp audio", "path", localPath, "error", removeErr)
					}
					if err != nil {
						slog.Error("transcription failed", "filename", att.Name, "error", err)
						continue
					}
					promptWithSender = fmt.Sprintf("%s\n\n<voice_transcription>\n%s\n</voice_transcription>\nThe above is a transcription of the user's voice message. Please respond to it.", promptWithSender, text)
				} else {
					// File attachment
					localPath, err := h.downloadAttachment(att, tmpDir)
					if err != nil {
						slog.Error("failed to download file attachment", "url", att.ContentURL, "error", err)
						continue
					}
					fileAttachments = append(fileAttachments, platform.FileAttachment{
						Filename:    att.Name,
						ContentType: att.ContentType,
						Size:        0, // Teams doesn't provide size in basic attachment object
						LocalPath:   localPath,
					})
					slog.Debug("downloaded file attachment", "filename", att.Name, "path", localPath)
				}
			}
		}
	}

	// Build content blocks
	contentText := buildPromptContent(promptWithSender, imagePaths, []string{}, fileAttachments)
	contentBlocks := []acp.ContentBlock{acp.TextBlock(contentText)}

	// Build session key
	sessionKey := buildSessionKey(conversationID)

	slog.Debug("processing", "prompt", promptWithSender, "images", len(imagePaths), "files", len(fileAttachments), "session_key", sessionKey)

	// Send initial "thinking" message
	thinkingResp, err := h.Client.SendActivity(
		activity.ServiceURL,
		conversationID,
		&Activity{
			Type:       "message",
			Text:       "💭 _thinking..._",
			TextFormat: "markdown",
		},
	)
	if err != nil {
		slog.Error("failed to send thinking message", "error", err)
		return
	}

	// Get or create ACP session
	if err := h.Pool.GetOrCreate(sessionKey); err != nil {
		h.Client.UpdateActivity(
			activity.ServiceURL,
			conversationID,
			thinkingResp.ID,
			&Activity{
				Type:       "message",
				Text:       fmt.Sprintf("⚠️ Failed to start agent: %v", err),
				TextFormat: "markdown",
			},
		)
		slog.Error("pool error", "error", err)
		return
	}

	finalText, cancelled, result := h.streamPrompt(
		sessionKey,
		contentBlocks,
		activity.ServiceURL,
		conversationID,
		thinkingResp.ID,
	)

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

	// TTS: synthesize voice reply only when user sent audio (skip if cancelled).
	if result == nil && !cancelled && h.Synthesizer != nil && finalText != "" && len(activity.Attachments) > 0 {
		hasAudio := false
		for _, att := range activity.Attachments {
			if isAudioContentType(att.ContentType) {
				hasAudio = true
				break
			}
		}
		if hasAudio {
			go h.sendVoiceReply(activity.ServiceURL, conversationID, userID, finalText)
		}
	}

	if result != nil {
		h.Client.UpdateActivity(
			activity.ServiceURL,
			conversationID,
			thinkingResp.ID,
			&Activity{
				Type:       "message",
				Text:       fmt.Sprintf("⚠️ %v", result),
				TextFormat: "markdown",
			},
		)
	}
}

// handleCommand processes slash commands
func (h *Handler) handleCommand(activity *Activity, cmd *command.Command) {
	sessionKey := buildSessionKey(activity.Conversation.ID)
	var response string

	switch cmd.Name {
	case command.CmdSessions:
		response = command.ExecuteSessions(h.Pool)
	case command.CmdInfo:
		response = command.ExecuteInfo(h.Pool, sessionKey, h.buildVoiceInfo())
	case command.CmdReset:
		response = command.ExecuteReset(h.Pool, sessionKey)
	case command.CmdResume:
		response = command.ExecuteResume(h.Pool, sessionKey)
	case command.CmdStop:
		response = command.ExecuteStop(h.Pool, sessionKey)
	case command.CmdPicker:
		response = command.ExecutePicker(h.Pool, h.Picker, sessionKey, cmd.Args, h.Pool.WorkingDir())
	default:
		return
	}

	chunks := platform.SplitMessage(response, 28000)
	for i, chunk := range chunks {
		resp := &Activity{
			Type:       "message",
			Text:       chunk,
			TextFormat: "markdown",
		}
		if i == 0 && activity.ID != "" {
			// Reply to original command message for first chunk
			resp.ReplyToID = activity.ID
		}
		h.Client.SendActivity(activity.ServiceURL, activity.Conversation.ID, resp)
	}
}

// streamPrompt handles ACP streaming with edit-polling
func (h *Handler) streamPrompt(
	sessionKey string,
	content []acp.ContentBlock,
	serviceURL string,
	conversationID string,
	msgID string,
) (string, bool, error) {
	var finalText string
	var cancelled bool
	err := h.Pool.WithConnection(sessionKey, func(conn *acp.AcpConnection) error {
		rx, _, reset, resumed, err := conn.SessionPrompt(content)
		if err != nil {
			return err
		}

		initial := "💭 _thinking..._"
		if resumed {
			initial = "🔄 _Session restored, continuing..._\n\n..."
		} else if reset {
			initial = "⚠️ _Session expired, starting fresh..._\n\n..."
		}

		var textBuf strings.Builder
		var toolLines []string
		if resumed {
			textBuf.WriteString("🔄 _Session restored from previous conversation._\n\n")
		} else if reset {
			textBuf.WriteString("⚠️ _Session expired, starting fresh..._\n\n")
		}

		// Shared state for edit-streaming
		var displayMu sync.Mutex
		currentDisplay := initial
		done := make(chan struct{})

		// Spawn edit-streaming goroutine — truncate only, never send new messages.
		// Split into multiple messages only on final edit after streaming ends.
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
						preview := platform.TruncateUTF8(content, 28000, "\n…")
						h.Client.UpdateActivity(
							serviceURL,
							conversationID,
							msgID,
							&Activity{
								Type:       "message",
								Text:       preview,
								TextFormat: "markdown",
							},
						)
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
				// Reaction handling would go here for Teams if we had reaction support

			case acp.AcpEventToolStart:
				if event.Title != "" {
					if label, ok := platform.FormatToolTitle(event.Title, h.ToolDisplay); ok {
						toolLines = append(toolLines, fmt.Sprintf("🔧 `%s`...", label))
						display := composeDisplay(toolLines, textBuf.String())
						displayMu.Lock()
						currentDisplay = display
						displayMu.Unlock()
					}
				}

			case acp.AcpEventToolDone:
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
				if label, ok := platform.FormatToolTitle(event.Title, h.ToolDisplay); ok {
					// Update the matching tool line
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

		conn.PromptDone()
		close(done)

		// If the prompt returned an error, surface it
		if promptErr != nil {
			h.Client.UpdateActivity(
				serviceURL,
				conversationID,
				msgID,
				&Activity{
					Type:       "message",
					Text:       fmt.Sprintf("⚠️ %v", promptErr),
					TextFormat: "markdown",
				},
			)
			return promptErr
		}

		// Final edit
		finalText = textBuf.String()
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
		// Rewrite GFM tables before splitting
		finalContent = markdown.ConvertTables(finalContent, h.MarkdownTableMode)

		chunks := platform.SplitMessage(finalContent, 28000)
		for i, chunk := range chunks {
			if i == 0 {
				h.Client.UpdateActivity(
					serviceURL,
					conversationID,
					msgID,
					&Activity{
						Type:       "message",
						Text:       chunk,
						TextFormat: "markdown",
					},
				)
			} else {
				h.Client.SendActivity(
					serviceURL,
					conversationID,
					&Activity{
						Type:       "message",
						Text:       chunk,
						TextFormat: "markdown",
					},
				)
			}
		}

		return nil
	})
	return finalText, cancelled, err
}

// sendVoiceReply synthesizes and sends a voice message
func (h *Handler) sendVoiceReply(serviceURL, conversationID, userID, text string) {
	slog.Info("🔊 tts: synthesizing voice reply", "user", userID, "voice", h.TTSConfig.Voice, "text_length", len(text))
	audioPath, err := h.Synthesizer.Synthesize(text)
	if err != nil {
		slog.Error("tts synthesis failed", "error", err)
		return
	}
	defer os.Remove(audioPath)

	// Teams doesn't have native voice message support in the same way,
	// so we would need to upload the file as an attachment.
	// For now, we'll skip voice reply on Teams.
	slog.Debug("voice reply synthesized but Teams voice support not yet implemented", "path", audioPath)
}

// buildVoiceInfo returns voice feature status
func (h *Handler) buildVoiceInfo() *command.VoiceInfo {
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

func isBotMentioned(botID string, entities []Entity) bool {
	for _, e := range entities {
		if e.Type == "mention" && e.Mentioned != nil && e.Mentioned.ID == botID {
			return true
		}
	}
	return false
}

func stripBotMention(text string, botID string, entities []Entity) string {
	for _, e := range entities {
		if e.Type == "mention" && e.Mentioned != nil && e.Mentioned.ID == botID && e.Text != "" {
			text = strings.Replace(text, e.Text, "", 1)
		}
	}
	return strings.TrimSpace(text)
}

func buildSessionKey(conversationID string) string {
	return fmt.Sprintf("teams:%s", conversationID)
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

// --- Attachment helpers ---

func isImageContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "image/")
}

func isAudioContentType(contentType string) bool {
	audioTypes := []string{
		"audio/",
		"application/ogg",
	}
	for _, t := range audioTypes {
		if strings.HasPrefix(contentType, t) {
			return true
		}
	}
	return false
}

// extensionForContentType returns a best-effort file extension (including the
// leading dot) for a MIME type, or an empty string when no mapping exists.
// Used when the attachment Name is missing so downstream agents can still
// recognize the file format.
func extensionForContentType(contentType string) string {
	// Strip any ";charset=..." suffix.
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	if contentType == "" {
		return ""
	}
	// Curated map — mime.ExtensionsByType returns non-deterministic order and
	// misses some common Teams types.
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/aac":
		return ".m4a"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/webm":
		return ".webm"
	case "audio/flac":
		return ".flac"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	case "text/plain":
		return ".txt"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	// Microsoft Office — OOXML (modern)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	// Microsoft Office — legacy binary formats
	case "application/msword":
		return ".doc"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	}
	// Fallback to the runtime MIME DB.
	if exts, err := mime.ExtensionsByType(contentType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// downloadAttachment downloads an attachment from contentURL with a max size of 25MB
func (h *Handler) downloadAttachment(att Attachment, tmpDir string) (string, error) {
	const maxSize = 25 * 1024 * 1024 // 25MB

	// Sanitize filename: strip any path components, then replace unsafe chars.
	filename := filepath.Base(att.Name)
	if filename == "" || filename == "." || filename == "/" {
		filename = "attachment" + extensionForContentType(att.ContentType)
	}
	filename = regexp.MustCompile(`[^\w\.\-]`).ReplaceAllString(filename, "_")

	filePath := filepath.Join(tmpDir, filename)

	// Get bearer token for authorization
	token, err := h.Client.auth.GetBotToken()
	if err != nil {
		return "", fmt.Errorf("failed to get bot token: %w", err)
	}

	req, err := http.NewRequest("GET", att.ContentURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	resp, err := h.Client.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download attachment: status %d", resp.StatusCode)
	}

	// Check content length
	if resp.ContentLength > maxSize {
		return "", fmt.Errorf("attachment exceeds 25MB limit: %d bytes", resp.ContentLength)
	}

	// Write to file with size limit
	f, err := os.Create(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	limitedReader := io.LimitReader(resp.Body, maxSize+1)
	written, err := io.Copy(f, limitedReader)
	if err != nil {
		os.Remove(filePath)
		return "", err
	}

	if written > maxSize {
		os.Remove(filePath)
		return "", fmt.Errorf("attachment exceeds 25MB limit")
	}

	return filePath, nil
}
