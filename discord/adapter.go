package discord

import (
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/transcribe"
)

// Adapter implements platform.Platform for Discord.
type Adapter struct {
	session *discordgo.Session
}

func NewAdapter(cfg config.DiscordConfig, pool *acp.SessionPool, transcriber transcribe.Transcriber) (*Adapter, error) {
	dg, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		return nil, err
	}

	allowed := make(map[string]bool, len(cfg.AllowedChannels))
	for _, ch := range cfg.AllowedChannels {
		allowed[ch] = true
	}

	h := &Handler{
		Pool:            pool,
		AllowedChannels: allowed,
		ReactionsConfig: cfg.Reactions,
		Transcriber:     transcriber,
	}

	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent |
		discordgo.IntentsGuilds

	dg.AddHandler(h.OnMessageCreate)
	dg.AddHandler(h.OnReady)

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
