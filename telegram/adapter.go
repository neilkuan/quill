package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/stt"
	"github.com/neilkuan/openab-go/tts"
)

// Adapter implements platform.Platform for Telegram.
type Adapter struct {
	b       *bot.Bot
	handler *Handler
	cancel  context.CancelFunc
}

func NewAdapter(cfg config.TelegramConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, voiceStore *tts.VoiceStore, ttsCfg config.TTSConfig) (*Adapter, error) {
	allowed := make(map[int64]bool, len(cfg.AllowedChats))
	for _, id := range cfg.AllowedChats {
		allowed[id] = true
	}

	allowedUsers := make(map[int64]bool, len(cfg.AllowedUserIDs))
	allowAnyUser := false
	for _, raw := range cfg.AllowedUserIDs {
		if raw == "*" {
			allowAnyUser = true
			continue
		}
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("telegram.allowed_user_id: invalid entry %q (expected integer ID or \"*\"): %w", raw, err)
		}
		allowedUsers[uid] = true
	}

	h := &Handler{
		Pool:            pool,
		AllowedChats:    allowed,
		AllowedUserIDs:  allowedUsers,
		AllowAnyUser:    allowAnyUser,
		ReactionsConfig: cfg.Reactions,
		Transcriber:     transcriber,
		Synthesizer:     synthesizer,
		VoiceStore:      voiceStore,
		TTSConfig:       ttsCfg,
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
			{Command: "setvoice", Description: "Set custom bot voice (reply to a voice message)"},
			{Command: "voice_clear", Description: "Clear your custom voice"},
			{Command: "voicemode", Description: "Set voice mode: echo or default"},
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
