package telegram

import (
	"context"
	"log/slog"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/transcribe"
)

// Adapter implements platform.Platform for Telegram.
type Adapter struct {
	b       *bot.Bot
	handler *Handler
	cancel  context.CancelFunc
}

func NewAdapter(cfg config.TelegramConfig, pool *acp.SessionPool, transcriber transcribe.Transcriber) (*Adapter, error) {
	allowed := make(map[int64]bool, len(cfg.AllowedChats))
	for _, id := range cfg.AllowedChats {
		allowed[id] = true
	}

	h := &Handler{
		Pool:            pool,
		AllowedChats:    allowed,
		ReactionsConfig: cfg.Reactions,
		Transcriber:     transcriber,
	}

	b, err := bot.New(cfg.BotToken,
		bot.WithDefaultHandler(h.handleUpdate),
	)
	if err != nil {
		return nil, err
	}

	h.Bot = b

	return &Adapter{b: b, handler: h}, nil
}

func (a *Adapter) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel

	// Get bot info (username needed for mention detection)
	me, err := a.b.GetMe(ctx)
	if err != nil {
		cancel()
		return err
	}
	a.handler.botUser = me
	slog.Info("starting telegram adapter", "bot", me.Username)

	// Register bot commands for the / menu
	_, err = a.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "sessions", Description: "List all active agent sessions"},
			{Command: "info", Description: "Show current chat session details"},
			{Command: "reset", Description: "Reset the current session"},
		},
	})
	if err != nil {
		slog.Warn("failed to register telegram bot commands", "error", err)
	} else {
		slog.Info("registered telegram bot commands")
	}

	go a.b.Start(ctx)

	return nil
}

func (a *Adapter) Stop() error {
	slog.Info("stopping telegram adapter")
	if a.cancel != nil {
		a.cancel()
	}
	return nil
}
