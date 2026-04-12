package discord

import (
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/stt"
	"github.com/neilkuan/openab-go/tts"
)

// Adapter implements platform.Platform for Discord.
type Adapter struct {
	session *discordgo.Session
}

func NewAdapter(cfg config.DiscordConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, voiceStore *tts.VoiceStore, ttsCfg config.TTSConfig) (*Adapter, error) {
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
		Synthesizer:     synthesizer,
		VoiceStore:      voiceStore,
		TTSConfig:       ttsCfg,
	}

	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent |
		discordgo.IntentsGuilds

	dg.AddHandler(h.OnMessageCreate)
	dg.AddHandler(h.OnReady)
	dg.AddHandler(h.OnInteractionCreate)

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
