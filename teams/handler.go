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
	"github.com/neilkuan/quill/cronjob"
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
	// Picker lists historical sessions for /pick. Nil when
	// the configured agent backend is not recognised by sessionpicker.Detect.
	Picker sessionpicker.Picker
	// Mentions records display_name → Teams Account so the agent's
	// <at>Name</at> output can be turned back into Bot Framework mention
	// entities on outbound activities. Nil disables the feature.
	Mentions *MentionDirectory
	CronStore *cronjob.Store
	CronCfg   config.CronjobConfig

	// Per-conversation member-roster cache. The first message we see in
	// a conversation triggers a background fetch of GET
	// /v3/conversations/{id}/members so the bot can @ members who
	// haven't spoken yet. Failed fetches clear their slot to allow a
	// retry on the next message.
	memberSeedingMu     sync.Mutex
	seededConversations map[string]bool

	// ServiceURLs caches the most-recent serviceURL per conversationID
	// and persists it to disk (when configured) so cron jobs can fire
	// proactive messages across pod restarts. Populated by every
	// incoming activity; read by CronDispatcher when firing a scheduled
	// job. nil is permitted and behaves as an in-memory-only no-op,
	// which keeps `&Handler{...}` test fixtures compiling.
	ServiceURLs *ServiceURLStore

	// Test-only override hooks. When non-nil, replace the default
	// dispatch so adapter-level routing tests don't have to spin up the
	// full message pipeline.
	invokeForTest      func()
	messageForTest     func()
	messageForTestFlag bool
}

// OnMessage handles incoming message activities from Teams
func (h *Handler) OnMessage(activity *Activity) {
	if h.messageForTest != nil {
		h.messageForTestFlag = true
		h.messageForTest()
		return
	}

	if activity.From.ID == "" {
		return
	}

	h.rememberServiceURL(activity.Conversation.ID, activity.ServiceURL)

	slog.Debug("teams message received",
		"conversation_id", activity.Conversation.ID,
		"user_id", activity.From.ID,
		"aad_object_id", activity.From.AADObjectID,
		"user_name", activity.From.Name,
		"text", activity.Text,
		"attachments", len(activity.Attachments),
		"entities", len(activity.Entities))

	if !h.accessGateAllows(activity) {
		return
	}

	// Build up the mention directory before doing anything else — even
	// messages that fail later command parsing teach us about users.
	h.Mentions.Record(activity.From)
	h.Mentions.RecordEntities(activity.Entities)
	// Once per conversation, fetch the full member roster in the
	// background so the bot can @ users who haven't spoken yet.
	h.seedMembersAsync(activity.ServiceURL, activity.Conversation.ID)

	conversationID := activity.Conversation.ID
	userID := activity.From.ID

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
				// Teams attaches non-file payloads (text/html mention rendering,
				// Adaptive Cards) to ordinary messages — they carry no contentUrl
				// and would otherwise blow up the HTTP downloader.
				if !isDownloadableAttachment(att) {
					slog.Debug("skipping non-downloadable attachment",
						"content_type", att.ContentType, "name", att.Name)
					continue
				}
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

	// Build content blocks. Images go through the same `<attached_images>`
	// text path Discord/Telegram use — the agent's read tool opens them by
	// path. The earlier base64 ImageBlock path was reverted because
	// kiro-cli v2.2.0 advertises promptCapabilities.image=true but exits
	// silently when fed a base64 image block; downloadAttachment now
	// guarantees a usable file extension so kiro's read tool with
	// mode=Image can identify the format.
	contentText := buildPromptContent(promptWithSender, imagePaths, []string{}, fileAttachments)
	contentBlocks := []acp.ContentBlock{acp.TextBlock(contentText)}

	// Build session key
	sessionKey := buildSessionKey(conversationID)

	slog.Debug("processing", "prompt", promptWithSender, "images", len(imagePaths), "files", len(fileAttachments), "session_key", sessionKey)

	// Owner descriptor for IsBusy / queued-placeholder rendering.
	ownerDesc := "user"
	if activity.From.Name != "" {
		ownerDesc = "user " + activity.From.Name
	}

	thinkingText := "💭 _thinking..._"
	if conn := h.Pool.Connection(sessionKey); conn != nil && conn.Alive() {
		if busy, owner := conn.IsBusy(); busy {
			thinkingText = fmt.Sprintf("⏳ _queued behind %s — agent is busy. Use /stop to cancel and run yours first._", owner)
		}
	}

	// Send initial "thinking" / "queued" message
	thinkingResp, err := h.sendActivity(
		activity.ServiceURL,
		conversationID,
		&Activity{
			Type:       "message",
			Text:       thinkingText,
			TextFormat: "markdown",
		},
	)
	if err != nil {
		slog.Error("failed to send thinking message", "error", err)
		return
	}

	// Get or create ACP session
	if err := h.Pool.GetOrCreate(sessionKey); err != nil {
		h.updateActivity(
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
		ownerDesc,
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
		h.updateActivity(
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

// accessGateAllows returns true when the activity is allowed past the
// access gate. allowed_user_id (with "*" as wildcard) takes precedence
// over allowed_channels. User IDs are matched against the Entra ID
// (AAD) Object ID first — the GUID shown as "Object ID" in the Teams
// profile card — and fall back to the Bot Framework channel ID
// (`29:xxx`) for back-compat with existing configs. Rejected activities
// emit slog.Warn so misconfigurations are diagnosable from logs.
func (h *Handler) accessGateAllows(activity *Activity) bool {
	userID := activity.From.ID
	aadObjectID := activity.From.AADObjectID
	conversationID := activity.Conversation.ID

	userGateActive := h.AllowAnyUser || len(h.AllowedUserIDs) > 0
	if userGateActive {
		userAllowed := h.AllowAnyUser ||
			(aadObjectID != "" && h.AllowedUserIDs[aadObjectID]) ||
			h.AllowedUserIDs[userID]
		if !userAllowed {
			slog.Warn("🚨👽🚨 teams message from unlisted user (add to allowed_user_id to enable)",
				"user_id", userID, "aad_object_id", aadObjectID, "user_name", activity.From.Name)
			return false
		}
		return true
	}
	if len(h.AllowedChannels) > 0 && !h.AllowedChannels[conversationID] {
		slog.Warn("🚨👽🚨 teams message from unlisted channel (add to allowed_channels to enable)",
			"conversation_id", conversationID)
		return false
	}
	return true
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
	case command.CmdMode:
		if strings.TrimSpace(cmd.Args) == "" {
			listing := command.ListModes(h.Pool, sessionKey)
			if listing.Err == nil && len(listing.Available) > 0 {
				h.sendModeCard(activity, sessionKey, listing)
				return
			}
			response = listing.Message
			break
		}
		response = command.ExecuteMode(h.Pool, sessionKey, cmd.Args)
	case command.CmdModel:
		if strings.TrimSpace(cmd.Args) == "" {
			listing := command.ListModels(h.Pool, sessionKey)
			if listing.Err == nil && len(listing.Available) > 0 {
				h.sendModelCard(activity, sessionKey, listing)
				return
			}
			response = listing.Message
			break
		}
		response = command.ExecuteModel(h.Pool, sessionKey, cmd.Args)
	case command.CmdCron:
		response = h.handleCronCommand(activity, sessionKey, cmd.Args)
	case command.CmdHelp:
		response = command.ExecuteHelp()
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
		h.sendActivity(activity.ServiceURL, activity.Conversation.ID, resp)
	}
}

// streamPrompt handles ACP streaming with edit-polling
func (h *Handler) streamPrompt(
	sessionKey string,
	content []acp.ContentBlock,
	serviceURL string,
	conversationID string,
	msgID string,
	owner string,
) (string, bool, error) {
	var finalText string
	var cancelled bool
	err := h.Pool.WithConnection(sessionKey, func(conn *acp.AcpConnection) error {
		rx, _, reset, resumed, err := conn.SessionPrompt(content, owner)
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
						h.updateActivity(
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

		// If the prompt returned an error, surface it
		if promptErr != nil {
			h.updateActivity(
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
		// Rewrite GFM tables before splitting
		finalContent = markdown.ConvertTables(finalContent, h.MarkdownTableMode)

		chunks := platform.SplitMessage(finalContent, 28000)
		for i, chunk := range chunks {
			if i == 0 {
				h.updateActivity(
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
				h.sendActivity(
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

// sendModeCard sends an Adaptive Card with mode picker dropdown
func (h *Handler) sendModeCard(activity *Activity, threadKey string, listing command.ModeListing) {
	att := BuildModeCard(listing, threadKey)
	resp := &Activity{
		Type:        "message",
		Attachments: []Attachment{att},
	}
	if _, err := h.sendActivity(activity.ServiceURL, activity.Conversation.ID, resp); err != nil {
		slog.Warn("teams: failed to send mode card", "error", err)
	}
}

// sendModelCard sends an Adaptive Card with model picker dropdown
func (h *Handler) sendModelCard(activity *Activity, threadKey string, listing command.ModelListing) {
	att := BuildModelCard(listing, threadKey)
	resp := &Activity{
		Type:        "message",
		Attachments: []Attachment{att},
	}
	if _, err := h.sendActivity(activity.ServiceURL, activity.Conversation.ID, resp); err != nil {
		slog.Warn("teams: failed to send model card", "error", err)
	}
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

// seedMembersAsync triggers a one-shot background load of a
// conversation's full member roster into the mention directory. Runs
// at most once per conversation per process lifetime; on failure the
// flag is cleared so the next inbound message can retry. Returns
// immediately — the directory is populated asynchronously, so the very
// first outbound reply right after a cold start may still miss
// previously-silent users.
func (h *Handler) seedMembersAsync(serviceURL, conversationID string) {
	if h.Mentions == nil || h.Client == nil {
		return
	}
	if serviceURL == "" || conversationID == "" {
		return
	}

	h.memberSeedingMu.Lock()
	if h.seededConversations == nil {
		h.seededConversations = make(map[string]bool)
	}
	if h.seededConversations[conversationID] {
		h.memberSeedingMu.Unlock()
		return
	}
	h.seededConversations[conversationID] = true
	h.memberSeedingMu.Unlock()

	go func() {
		members, err := h.Client.GetConversationMembers(serviceURL, conversationID)
		if err != nil {
			slog.Warn("teams: failed to seed mention directory from conversation roster",
				"conversation_id", conversationID, "error", err)
			h.memberSeedingMu.Lock()
			delete(h.seededConversations, conversationID)
			h.memberSeedingMu.Unlock()
			return
		}
		for _, m := range members {
			h.Mentions.Record(Account{
				ID:          m.ID,
				Name:        m.Name,
				AADObjectID: m.AADObjectID,
			})
		}
		slog.Info("teams: seeded mention directory from conversation roster",
			"conversation_id", conversationID, "members", len(members))
	}()
}

// applyMentions populates `a.Entities` with Bot Framework mention
// entities derived from any <at>Name</at> tags in `a.Text`. Existing
// entries on `a.Entities` are preserved — Teams ignores duplicates
// keyed by `text` + `mentioned.id` so appending is safe. No-op when
// the handler has no mention directory or `a.Text` carries no <at> tags.
func (h *Handler) applyMentions(a *Activity) {
	if a == nil || h.Mentions == nil {
		return
	}
	entities := h.Mentions.BuildMentionEntities(a.Text)
	if len(entities) == 0 {
		return
	}
	a.Entities = append(a.Entities, entities...)
}

// sendActivity wraps BotClient.SendActivity, adding mention entities
// before dispatch.
func (h *Handler) sendActivity(serviceURL, conversationID string, activity *Activity) (*Activity, error) {
	h.applyMentions(activity)
	return h.Client.SendActivity(serviceURL, conversationID, activity)
}

// updateActivity wraps BotClient.UpdateActivity, adding mention
// entities before dispatch.
func (h *Handler) updateActivity(serviceURL, conversationID, activityID string, activity *Activity) error {
	h.applyMentions(activity)
	return h.Client.UpdateActivity(serviceURL, conversationID, activityID, activity)
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

// ensureFileExtension makes sure `path` has a usable file extension so
// downstream agents (especially kiro-cli's read tool with mode=Image)
// can recognize the file's format. Teams Bot Framework often delivers
// inline mobile photos with `name=""` AND a missing/blank ContentType,
// which leaves the downloaded file extensionless and unusable.
//
// Resolution order:
//  1. If `path` already carries an extension, keep it.
//  2. Try the content-type hint (image/jpeg → .jpg).
//  3. Fall back to magic-byte sniffing via http.DetectContentType.
//  4. Give up and return the original path — caller should still proceed
//     because text/* and other unknown formats may still be useful.
//
// The on-disk file is renamed to `<path><ext>` when an extension is
// inferred. The new path is returned (or the original on failure).
func ensureFileExtension(path string, contentTypeHint string) (string, error) {
	if filepath.Ext(path) != "" {
		return path, nil
	}

	ext := extensionForContentType(contentTypeHint)
	if ext == "" {
		// Magic-byte sniff — read up to 512 bytes (DetectContentType limit).
		f, err := os.Open(path)
		if err != nil {
			return path, err
		}
		head := make([]byte, 512)
		n, _ := f.Read(head)
		_ = f.Close()
		if n > 0 {
			detected := http.DetectContentType(head[:n])
			if i := strings.Index(detected, ";"); i >= 0 {
				detected = strings.TrimSpace(detected[:i])
			}
			ext = extensionForContentType(detected)
		}
	}

	if ext == "" {
		return path, nil
	}

	newPath := path + ext
	if err := os.Rename(path, newPath); err != nil {
		// Rename can fail (target exists, fs perms). Caller still gets a
		// path that points at the downloaded bytes.
		return path, err
	}
	return newPath, nil
}

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

// isDownloadableAttachment reports whether the attachment carries a real
// file we can fetch over HTTP. Teams sends non-file attachments alongside
// regular messages — most commonly a `text/html` rendering of formatted
// text (e.g. when the user @mentions the bot) and Adaptive/Hero/Thumbnail
// cards from invokes — that have no `contentUrl`. The HTTP downloader
// would otherwise fail with `unsupported protocol scheme ""`, so we filter
// them out at the call site instead.
func isDownloadableAttachment(att Attachment) bool {
	if strings.HasPrefix(att.ContentType, "application/vnd.microsoft.card.") {
		return false
	}
	if att.ContentType == "text/html" {
		return false
	}
	url := strings.TrimSpace(att.ContentURL)
	if url == "" {
		return false
	}
	return strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")
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

	// Close before rename so Windows doesn't reject a held-open file. The
	// outer `defer f.Close()` will be a no-op once Close already returned.
	if err := f.Close(); err != nil {
		return "", err
	}

	finalPath, extErr := ensureFileExtension(filePath, att.ContentType)
	if extErr != nil {
		// Non-fatal — caller still has the bytes at filePath, but log so
		// kiro/agent failures stemming from missing extension are visible.
		slog.Debug("could not normalize attachment extension",
			"path", filePath, "content_type", att.ContentType, "error", extErr)
	}
	return finalPath, nil
}

// rememberServiceURL caches the most-recent serviceURL for a
// conversation. Idempotent and cheap; called on every incoming
// activity that has both fields set. Persistence (when ServiceURLs is
// configured with a path) only fsyncs when the URL changes, so the
// steady state of "every inbound activity refreshes the same URL"
// causes no disk churn.
func (h *Handler) rememberServiceURL(conversationID, serviceURL string) {
	if conversationID == "" || serviceURL == "" {
		return
	}
	if err := h.ServiceURLs.Set(conversationID, serviceURL); err != nil {
		slog.Warn("teams: failed to persist serviceURL",
			"conversation_id", conversationID, "error", err)
	}
}

// ServiceURLFor returns the cached serviceURL for a conversation,
// or "" if none has been seen yet.
func (h *Handler) ServiceURLFor(conversationID string) string {
	return h.ServiceURLs.Get(conversationID)
}

// handleCronCommand routes /cron <sub> <args> to the cronjob package.
// Captures the activity's serviceURL into the cache so future cron
// fires know where to post.
func (h *Handler) handleCronCommand(activity *Activity, threadKey, args string) string {
	if h.CronStore == nil || h.CronCfg.Disabled {
		return "⚠️ Cron jobs are disabled on this bot."
	}
	// Make sure we know the serviceURL for this conversation (idempotent).
	h.rememberServiceURL(activity.Conversation.ID, activity.ServiceURL)

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
		senderID := activity.From.AADObjectID
		if senderID == "" {
			senderID = activity.From.ID
		}
		_, replyMsg := command.ExecuteCronAdd(h.CronStore, threadKey,
			senderID, activity.From.Name,
			schedule, prompt,
			h.CronCfg.MaxPerThread, minInterval, tz)
		return replyMsg
	case "list":
		jobs := command.ListCronJobs(h.CronStore, threadKey)
		return formatTeamsCronList(jobs, tz)
	case "rm":
		return command.ExecuteCronRemove(h.CronStore, threadKey, schedule)
	}
	return "⚠️ Unknown cron subcommand"
}

func formatTeamsCronList(jobs []cronjob.Job, tz *time.Location) string {
	if len(jobs) == 0 {
		return "📭 No scheduled prompts in this channel.\n\nCreate one with `/cron add <schedule> <prompt>`."
	}
	var sb strings.Builder
	sb.WriteString("⏰ **Scheduled prompts in this channel**\n\n")
	for _, j := range jobs {
		body := j.Prompt
		if len(body) > 80 {
			body = body[:79] + "…"
		}
		sb.WriteString(fmt.Sprintf("`%s` — `%s` — next %s\n  → %s\n",
			j.ID, j.Schedule, j.NextFire.In(tz).Format("2006-01-02 15:04 MST"),
			body))
	}
	return sb.String()
}
