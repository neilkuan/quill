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
	Pool            *acp.SessionPool
	AllowedChannels map[string]bool
	AllowedUserIDs  map[string]bool
	// AllowAnyUser is true when allowed_user_id contains "*" — anyone can talk
	// to the bot from any channel.
	AllowAnyUser    bool
	ReactionsConfig config.ReactionsConfig
	Transcriber     stt.Transcriber
	Synthesizer     tts.Synthesizer
	TTSConfig       config.TTSConfig
	// MarkdownTableMode controls how GFM tables in agent replies are rewritten
	// before being sent to Discord. See markdown.TableMode for options.
	MarkdownTableMode markdown.TableMode
	// Picker lists historical sessions for /pick. Nil when
	// the configured agent backend is not recognised by sessionpicker.Detect.
	Picker sessionpicker.Picker

	// streamingMu guards streamingMsgs.
	streamingMu sync.Mutex
	// streamingMsgs maps a bot streaming message ID to the specific
	// connection whose current prompt should be cancelled when the user
	// taps 🛑. Using the connection pointer (not threadKey) means a stale
	// entry cannot accidentally cancel a later prompt on the same thread
	// — if the connection has since been evicted or replaced, the cancel
	// targets the original (now dead) connection and is a no-op.
	streamingMsgs map[string]*acp.AcpConnection
}

// registerStreamingMsg records a bot streaming message ID so reaction-triggered
// cancels can find the owning connection.
func (h *Handler) registerStreamingMsg(msgID string, conn *acp.AcpConnection) {
	h.streamingMu.Lock()
	defer h.streamingMu.Unlock()
	if h.streamingMsgs == nil {
		h.streamingMsgs = make(map[string]*acp.AcpConnection)
	}
	h.streamingMsgs[msgID] = conn
}

func (h *Handler) unregisterStreamingMsg(msgID string) {
	h.streamingMu.Lock()
	defer h.streamingMu.Unlock()
	delete(h.streamingMsgs, msgID)
}

func (h *Handler) lookupStreamingMsg(msgID string) (*acp.AcpConnection, bool) {
	h.streamingMu.Lock()
	defer h.streamingMu.Unlock()
	conn, ok := h.streamingMsgs[msgID]
	return conn, ok
}

func (h *Handler) OnMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot {
		return
	}

	botID := s.State.User.ID
	channelID := m.ChannelID

	isMentioned := false
	for _, u := range m.Mentions {
		if u.ID == botID {
			isMentioned = true
			break
		}
	}
	if !isMentioned {
		// Discord clients may emit either <@BOTID> or the legacy nickname
		// form <@!BOTID>. discordgo normally populates m.Mentions, but fall
		// back to raw content scanning if it doesn't (seen in some clients).
		isMentioned = strings.Contains(m.Content, fmt.Sprintf("<@%s>", botID)) ||
			strings.Contains(m.Content, fmt.Sprintf("<@!%s>", botID))
	}
	// Role mention (<@&ROLE_ID>): Discord auto-creates a managed role with the
	// bot's name when the bot is added; users often autocomplete @BotName and
	// pick that role instead of the bot user. Treat as a mention when the
	// pinged role is assigned to the bot member.
	if !isMentioned && len(m.MentionRoles) > 0 && m.GuildID != "" {
		botMember, err := s.State.Member(m.GuildID, botID)
		if err != nil {
			botMember, err = s.GuildMember(m.GuildID, botID)
		}
		if err == nil && botMember != nil {
			botRoles := make(map[string]struct{}, len(botMember.Roles))
			for _, rid := range botMember.Roles {
				botRoles[rid] = struct{}{}
			}
			for _, rid := range m.MentionRoles {
				if _, ok := botRoles[rid]; ok {
					isMentioned = true
					break
				}
			}
		} else if err != nil {
			slog.Debug("failed to fetch bot member for role-mention check",
				"guild_id", m.GuildID, "error", err)
		}
	}

	// Dump mention IDs for diagnostics when mention detection fails.
	mentionIDs := make([]string, 0, len(m.Mentions))
	for _, u := range m.Mentions {
		mentionIDs = append(mentionIDs, u.ID)
	}

	slog.Debug("discord message received",
		"author_id", m.Author.ID,
		"author", m.Author.Username,
		"channel_id", channelID,
		"guild_id", m.GuildID,
		"bot_id", botID,
		"mentioned", isMentioned,
		"mention_ids", mentionIDs,
		"mention_roles", m.MentionRoles,
		"content_len", len(m.Content),
		"content", m.Content,
		"attachments", len(m.Attachments))

	// AllowedUserIDs (or AllowAnyUser for "*"), when set, overrides the
	// channel-based gate: listed users (or any user for "*") are accepted
	// from any channel/thread.
	var inAllowedChannel, inThread bool
	userGateActive := h.AllowAnyUser || len(h.AllowedUserIDs) > 0
	if userGateActive {
		if !h.AllowAnyUser && !h.AllowedUserIDs[m.Author.ID] {
			// Only log when the user actually tried to address the bot,
			// otherwise background messages in busy guilds would flood logs.
			if isMentioned {
				slog.Warn("🚨👽🚨 discord message from unlisted user (add to allowed_user_id to enable)",
					"user_id", m.Author.ID,
					"username", m.Author.Username,
					"channel_id", m.ChannelID)
			}
			return
		}
		// Allowed user — accept regardless of channel.
		inAllowedChannel = true
	} else {
		inAllowedChannel = len(h.AllowedChannels) == 0 || h.AllowedChannels[channelID]

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
		"schema":       "quill.sender.v1",
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

	// Track the streaming message by connection pointer so 🛑 reactions
	// route to the exact prompt that was running at registration time.
	// Only drop the 🛑 reaction when reactions are enabled — otherwise the
	// bot would leave emoji noise even when the user asked for a clean UI.
	if conn := h.Pool.Connection(threadKey); conn != nil {
		h.registerStreamingMsg(thinkingMsg.ID, conn)
		defer h.unregisterStreamingMsg(thinkingMsg.ID)
		if h.ReactionsConfig.Enabled {
			go func() {
				if err := s.MessageReactionAdd(threadID, thinkingMsg.ID, "🛑"); err != nil {
					slog.Debug("failed to add cancel reaction", "error", err)
				}
			}()
		}
	}

	finalText, cancelled, result := streamPrompt(h.Pool, threadKey, contentBlocks, s, threadID, thinkingMsg.ID, reactions, h.MarkdownTableMode, h.ReactionsConfig.ToolDisplay)

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

	// TTS: synthesize voice reply only when the user sent a voice message
	// (skip if cancelled — the text is partial).
	if result == nil && !cancelled && h.Synthesizer != nil && finalText != "" && hasAudio {
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
	slog.Info("✅ discord bot connected",
		"user", "🤖"+r.User.Username+"🤖",
		"user_id", r.User.ID,
		"session_id", r.SessionID,
		"guilds", len(r.Guilds),
		"api_version", r.Version)
	// Log each guild we're in at debug level — handy for confirming the bot
	// actually has access to the expected servers / channels.
	for _, g := range r.Guilds {
		slog.Debug("discord guild available", "guild_id", g.ID, "unavailable", g.Unavailable)
	}
	h.registerSlashCommands(s, r.User.ID)
}

// OnDisconnect fires when the Discord gateway websocket drops.
func (h *Handler) OnDisconnect(s *discordgo.Session, d *discordgo.Disconnect) {
	slog.Warn("⚠️  discord gateway disconnected")
}

// OnResumed fires when the gateway session resumes after a disconnect.
func (h *Handler) OnResumed(s *discordgo.Session, r *discordgo.Resumed) {
	slog.Info("🔄 discord gateway resumed")
}

// OnMessageReactionAdd fires when any user adds a reaction. When the
// reaction is 🛑 on one of our currently-streaming bot messages, route
// it to SessionCancel on the exact connection that owned the prompt.
// The bot's own initial 🛑 reaction is ignored via the author check.
func (h *Handler) OnMessageReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if s.State.User != nil && r.UserID == s.State.User.ID {
		return
	}
	if r.Emoji.Name != "🛑" {
		return
	}
	conn, ok := h.lookupStreamingMsg(r.MessageID)
	if !ok || conn == nil {
		return
	}
	slog.Info("discord cancel via reaction",
		"user_id", r.UserID, "message_id", r.MessageID,
		"session_id", conn.SessionID, "thread_key", conn.ThreadKey)
	if err := conn.SessionCancel(); err != nil {
		slog.Debug("cancel via reaction failed", "thread_key", conn.ThreadKey, "error", err)
	}
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
		Name:        "resume",
		Description: "Attempt to restore a previous session for this thread",
	},
	{
		Name:        "stop",
		Description: "Interrupt the agent's current reply (session kept alive)",
	},
	{
		Name:        "pick",
		Description: "Browse and load historical agent sessions. Use `/pick <N>` or `/pick all`.",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "args",
				Description: "Empty to list, `<N>` to load by index, `load <id>` for direct load, `all` to skip cwd filter",
				Required:    false,
			},
		},
	},
	{
		Name:        "mode",
		Description: "List or switch the session's agent mode. No args = interactive picker.",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "mode_id",
				Description: "Mode id (or 1-based index from a previous listing). Omit to open the select menu.",
				Required:    false,
			},
		},
	},
	{
		Name:        "model",
		Description: "List or switch the session's LLM model. No args = interactive picker.",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "model_id",
				Description: "Model id (or 1-based index from a previous listing). Omit to open the select menu.",
				Required:    false,
			},
		},
	},
}

// modeSelectCustomIDPrefix prefixes the CustomID of the <SelectMenu>
// that the mode picker renders. The suffix is the threadKey so a
// stale menu cannot route a selection to the wrong conversation.
const modeSelectCustomIDPrefix = "mode-select:"

// modelSelectCustomIDPrefix is the /model analogue of
// modeSelectCustomIDPrefix.
const modelSelectCustomIDPrefix = "model-select:"

// pickSelectCustomIDPrefix marks a SelectMenu produced by /pick. The
// suffix is the threadKey; the option Value carries the full session
// id (Discord Value cap is 100 chars, comfortably above a UUID).
const pickSelectCustomIDPrefix = "pick-select:"

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
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		h.handleSlashCommand(s, i)
	case discordgo.InteractionMessageComponent:
		h.handleComponentInteraction(s, i)
	}
}

func (h *Handler) handleSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	userID := ""
	if i.Member != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}

	// Mode, Model and Pick are special: with no argument we respond
	// with an interactive SelectMenu instead of plain text, so each
	// gets its own branch.
	if data.Name == command.CmdMode {
		h.handleModeSlash(s, i, data)
		return
	}
	if data.Name == command.CmdModel {
		h.handleModelSlash(s, i, data)
		return
	}
	if data.Name == command.CmdPicker {
		h.handlePickSlash(s, i, data)
		return
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
	case command.CmdResume:
		threadKey := buildSessionKey(i.ChannelID)
		response = command.ExecuteResume(h.Pool, threadKey)
	case command.CmdStop:
		threadKey := buildSessionKey(i.ChannelID)
		response = command.ExecuteStop(h.Pool, threadKey)
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

// handleModeSlash responds to /mode. With an explicit mode_id the
// switch happens inline and we reply with the confirmation text. With
// no argument we send a SelectMenu listing every advertised mode so
// the user can tap to pick one.
func (h *Handler) handleModeSlash(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	threadKey := buildSessionKey(i.ChannelID)

	arg := ""
	for _, opt := range data.Options {
		if opt.Name == "mode_id" {
			arg = strings.TrimSpace(opt.StringValue())
			break
		}
	}

	if arg != "" {
		msg := command.ExecuteMode(h.Pool, threadKey, arg)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: msg},
		})
		return
	}

	listing := command.ListModes(h.Pool, threadKey)
	if listing.Err != nil || len(listing.Available) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: listing.Message},
		})
		return
	}

	options := make([]discordgo.SelectMenuOption, 0, len(listing.Available))
	for _, m := range listing.Available {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		options = append(options, discordgo.SelectMenuOption{
			Label:       label,
			Value:       m.ID,
			Description: m.Description,
			Default:     m.ID == listing.Current,
		})
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("**Select a mode** (current: `%s`)", listing.Current),
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    modeSelectCustomIDPrefix + threadKey,
							Placeholder: "Pick a mode",
							MinValues:   intPtr(1),
							MaxValues:   1,
							Options:     options,
						},
					},
				},
			},
		},
	})
}

func intPtr(v int) *int { return &v }

// handleModelSlash is the /model analogue of handleModeSlash — lists
// available models via a SelectMenu when called without args, switches
// directly when called with `model_id`.
func (h *Handler) handleModelSlash(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	threadKey := buildSessionKey(i.ChannelID)

	arg := ""
	for _, opt := range data.Options {
		if opt.Name == "model_id" {
			arg = strings.TrimSpace(opt.StringValue())
			break
		}
	}

	if arg != "" {
		msg := command.ExecuteModel(h.Pool, threadKey, arg)
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: msg},
		})
		return
	}

	listing := command.ListModels(h.Pool, threadKey)
	if listing.Err != nil || len(listing.Available) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: listing.Message},
		})
		return
	}

	options := make([]discordgo.SelectMenuOption, 0, len(listing.Available))
	for _, m := range listing.Available {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		options = append(options, discordgo.SelectMenuOption{
			Label:       label,
			Value:       m.ID,
			Description: m.Description,
			Default:     m.ID == listing.Current,
		})
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("**Select a model** (current: `%s`)", listing.Current),
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    modelSelectCustomIDPrefix + threadKey,
							Placeholder: "Pick a model",
							MinValues:   intPtr(1),
							MaxValues:   1,
							Options:     options,
						},
					},
				},
			},
		},
	})
}

func (h *Handler) handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	switch {
	case strings.HasPrefix(data.CustomID, modeSelectCustomIDPrefix):
		threadKey := strings.TrimPrefix(data.CustomID, modeSelectCustomIDPrefix)
		if len(data.Values) == 0 {
			return
		}
		msg := command.ExecuteMode(h.Pool, threadKey, data.Values[0])
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    msg,
				Components: []discordgo.MessageComponent{}, // clear the menu so it cannot fire twice
				Flags:      discordgo.MessageFlagsEphemeral,
			},
		})
	case strings.HasPrefix(data.CustomID, modelSelectCustomIDPrefix):
		threadKey := strings.TrimPrefix(data.CustomID, modelSelectCustomIDPrefix)
		if len(data.Values) == 0 {
			return
		}
		msg := command.ExecuteModel(h.Pool, threadKey, data.Values[0])
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    msg,
				Components: []discordgo.MessageComponent{},
				Flags:      discordgo.MessageFlagsEphemeral,
			},
		})
	case strings.HasPrefix(data.CustomID, pickSelectCustomIDPrefix):
		threadKey := strings.TrimPrefix(data.CustomID, pickSelectCustomIDPrefix)
		if len(data.Values) == 0 {
			return
		}
		sessionID := data.Values[0]
		// LoadPickerByID kills the current conn, spawns a fresh agent,
		// and calls session/load — that can easily run past Discord's
		// 3-second interaction deadline, producing a "此交互失敗" UX
		// failure. Defer the update so Discord accepts the interaction
		// immediately, then edit the original (ephemeral) message with
		// the actual result once the load returns.
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredMessageUpdate,
		}); err != nil {
			slog.Debug("pick interaction defer failed", "error", err)
		}
		go func() {
			msg := command.LoadPickerByID(h.Pool, h.Picker, threadKey, sessionID)
			emptyComponents := []discordgo.MessageComponent{}
			if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content:    &msg,
				Components: &emptyComponents,
			}); err != nil {
				slog.Debug("pick interaction edit failed", "error", err)
			}
		}()
	}
}

// handlePickSlash responds to /pick. With no args (or `all`) we render
// a SelectMenu populated with the listing; `/pick <N>` and
// `/pick load <id>` fall through to the plain-text path so experienced
// users retain the typing workflow.
func (h *Handler) handlePickSlash(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	threadKey := buildSessionKey(i.ChannelID)

	args := ""
	for _, opt := range data.Options {
		if opt.Name == "args" {
			args = strings.TrimSpace(opt.StringValue())
			break
		}
	}

	// Only two shapes render the interactive picker: empty args and
	// the `all` bypass. Everything else is load-by-index / load-by-id /
	// typos, which go through the text path.
	bypassCWD := strings.EqualFold(args, "all")
	if args != "" && !bypassCWD {
		msg := command.ExecutePicker(h.Pool, h.Picker, threadKey, args, h.Pool.WorkingDir())
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: msg},
		})
		return
	}

	listing := command.ListPickerSessions(h.Picker, threadKey, h.Pool.WorkingDir(), bypassCWD)
	if listing.Err != nil || len(listing.Sessions) == 0 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: listing.Message},
		})
		return
	}

	options := make([]discordgo.SelectMenuOption, 0, len(listing.Sessions))
	for _, sess := range listing.Sessions {
		title := sess.Title
		if title == "" {
			title = "(untitled)"
		}
		options = append(options, discordgo.SelectMenuOption{
			Label:       truncateForSelectLabel(title),
			Value:       sess.ID,
			Description: truncateForSelectDesc(formatPickOptionDescription(sess)),
		})
	}

	header := fmt.Sprintf("**Select a session to resume** (%s)", listing.AgentType)
	if bypassCWD {
		header += " _(all cwds)_"
	} else if listing.CWD != "" {
		header += fmt.Sprintf(" _(cwd: `%s`)_", listing.CWD)
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: header,
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    pickSelectCustomIDPrefix + threadKey,
							Placeholder: "Pick a session",
							MinValues:   intPtr(1),
							MaxValues:   1,
							Options:     options,
						},
					},
				},
			},
		},
	})
}

// formatPickOptionDescription composes the small-print description
// shown under each SelectMenu option: age + cwd, kept short.
func formatPickOptionDescription(s sessionpicker.Session) string {
	age := formatPickDuration(s.UpdatedAt)
	cwd := s.CWD
	if cwd == "" {
		cwd = "(no cwd)"
	}
	return fmt.Sprintf("%s ago · %s", age, cwd)
}

// formatPickDuration is a lightweight duration formatter, kept local
// so the discord package doesn't leak the command-layer one.
func formatPickDuration(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// truncateForSelectLabel and truncateForSelectDesc enforce Discord's
// 100-character cap on SelectMenuOption Label / Description.
func truncateForSelectLabel(s string) string { return truncateUTF8WithEllipsis(s, 100) }
func truncateForSelectDesc(s string) string  { return truncateUTF8WithEllipsis(s, 100) }

func truncateUTF8WithEllipsis(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Reserve 3 bytes for "…" (rendered as … one rune ≈ 3 bytes UTF-8).
	cut := max - 3
	if cut < 0 {
		cut = 0
	}
	// Trim to a rune boundary to avoid half-characters.
	r := []rune(s)
	out := ""
	for _, c := range r {
		n := len(string(c))
		if len(out)+n > cut {
			break
		}
		out += string(c)
	}
	return out + "…"
}

func streamPrompt(
	pool *acp.SessionPool,
	threadKey string,
	content []acp.ContentBlock,
	s *discordgo.Session,
	channelID string,
	msgID string,
	reactions *StatusReactionController,
	tableMode markdown.TableMode,
	toolDisplay string,
) (string, bool, error) {
	var finalText string
	var cancelled bool
	err := pool.WithConnection(threadKey, func(conn *acp.AcpConnection) error {
		rx, _, reset, resumed, err := conn.SessionPrompt(content)
		if err != nil {
			return err
		}
		reactions.SetThinking()

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
					if label, ok := platform.FormatToolTitle(event.Title, toolDisplay); ok {
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
				if label, ok := platform.FormatToolTitle(event.Title, toolDisplay); ok {
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
			s.ChannelMessageEdit(channelID, currentMsgID, fmt.Sprintf("⚠️ %v", promptErr))
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
		// Rewrite GFM tables before splitting — Discord ignores table syntax,
		// so we wrap them in fenced code blocks (or convert to bullets) for
		// readable rendering. Skipped during streaming preview.
		finalContent = markdown.ConvertTables(finalContent, tableMode)

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
	return finalText, cancelled, err
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

// sendVoiceReply synthesizes and sends a voice message.
func (h *Handler) sendVoiceReply(s *discordgo.Session, channelID, userID, text string) {
	slog.Info("🔊 tts: synthesizing voice reply", "user", userID, "voice", h.TTSConfig.Voice, "text_length", len(text))
	audioPath, err := h.Synthesizer.Synthesize(text)
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
