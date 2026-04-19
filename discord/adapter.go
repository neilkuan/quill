package discord

import (
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/config"
	"github.com/neilkuan/quill/markdown"
	"github.com/neilkuan/quill/stt"
	"github.com/neilkuan/quill/tts"
)

// Adapter implements platform.Platform for Discord.
type Adapter struct {
	session *discordgo.Session
}

func NewAdapter(cfg config.DiscordConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig) (*Adapter, error) {
	dg, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		return nil, err
	}

	allowed := make(map[string]bool, len(cfg.AllowedChannels))
	for _, ch := range cfg.AllowedChannels {
		allowed[ch] = true
	}

	allowedUsers := make(map[string]bool, len(cfg.AllowedUserIDs))
	allowAnyUser := false
	for _, uid := range cfg.AllowedUserIDs {
		if uid == "*" {
			allowAnyUser = true
			continue
		}
		allowedUsers[uid] = true
	}

	h := &Handler{
		Pool:             pool,
		AllowedChannels:  allowed,
		AllowedUserIDs:   allowedUsers,
		AllowAnyUser:     allowAnyUser,
		ReactionsConfig:  cfg.Reactions,
		Transcriber:      transcriber,
		Synthesizer:      synthesizer,
		TTSConfig:        ttsCfg,
		MarkdownTableMode: markdown.ParseMode(mdCfg.Tables),
	}

	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent |
		discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessageReactions

	dg.AddHandler(h.OnMessageCreate)
	dg.AddHandler(h.OnReady)
	dg.AddHandler(h.OnResumed)
	dg.AddHandler(h.OnDisconnect)
	dg.AddHandler(h.OnInteractionCreate)
	dg.AddHandler(h.OnMessageReactionAdd)

	return &Adapter{session: dg}, nil
}

func (a *Adapter) Start() error {
	slog.Info("starting discord adapter")
	return a.session.Open()
}

func (a *Adapter) Stop() error {
	slog.Info("stopping discord adapter")
	return a.session.Close()
}

// Healthy reports whether the Discord gateway connection is alive.
// discordgo manages its own WebSocket reconnect, so this mostly reflects
// whether the initial handshake succeeded and the session hasn't been closed.
func (a *Adapter) Healthy() bool {
	return a.session.DataReady
}
