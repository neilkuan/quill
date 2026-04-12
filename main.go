package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neilkuan/openab-go/acp"
	"github.com/neilkuan/openab-go/api"
	appconfig "github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/discord"
	"github.com/neilkuan/openab-go/platform"
	"github.com/neilkuan/openab-go/telegram"
	"github.com/neilkuan/openab-go/stt"
	"github.com/neilkuan/openab-go/tts"
)

var commit = "unknown"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("openab-go (%s)\n", commit)
		os.Exit(0)
	}

	// Setup structured logging
	logLevel := slog.LevelInfo
	if os.Getenv("OPENAB_GO_LOG") == "debug" {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	// Load config
	configPath := "config.toml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := appconfig.LoadConfig(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.Info("config loaded",
		"agent_cmd", cfg.Agent.Command,
		"pool_max", cfg.Pool.MaxSessions,
	)

	// Create session pool
	pool := acp.NewSessionPool(
		cfg.Agent.Command,
		cfg.Agent.Args,
		cfg.Agent.WorkingDir,
		cfg.Agent.Env,
		cfg.Pool.MaxSessions,
	)

	ttlSecs := int64(cfg.Pool.SessionTTLHours) * 3600

	// Create STT (speech-to-text) if configured
	var t stt.Transcriber
	if cfg.STT.Enabled {
		switch cfg.STT.Provider {
		case "openai":
			t = stt.NewOpenAITranscriber(stt.OpenAIConfig{
				APIKey:   cfg.STT.APIKey,
				Model:    cfg.STT.Model,
				Language: cfg.STT.Language,
				Prompt:   cfg.STT.Prompt,
				BaseURL:  cfg.STT.BaseURL,
			})
			slog.Info("🎙️ stt enabled", "provider", "openai", "model", cfg.STT.Model, "language", cfg.STT.Language)
		default:
			slog.Warn("unknown stt provider, voice transcription disabled", "provider", cfg.STT.Provider)
		}
	}

	// Create TTS (text-to-speech) synthesizer if configured
	var synth tts.Synthesizer
	var voiceStore *tts.VoiceStore
	if cfg.TTS.Enabled {
		synth = tts.NewOpenAISynthesizer(tts.OpenAIConfig{
			APIKey:     cfg.TTS.APIKey,
			Model:      cfg.TTS.Model,
			Voice:      cfg.TTS.Voice,
			BaseURL:    cfg.TTS.BaseURL,
			TimeoutSec: cfg.TTS.TimeoutSec,
		})
		slog.Info("🔊 tts enabled", "model", cfg.TTS.Model, "voice", cfg.TTS.Voice, "voice_gender", cfg.TTS.VoiceGender)
		var err error
		voiceStore, err = tts.NewVoiceStore(cfg.Agent.WorkingDir)
		if err != nil {
			slog.Warn("failed to create voice store, per-user voices disabled", "error", err)
		}
	}

	// Build platforms
	var platforms []platform.Platform

	if cfg.Discord.Enabled {
		adapter, err := discord.NewAdapter(cfg.Discord, pool, t, synth, voiceStore, cfg.TTS)
		if err != nil {
			slog.Error("failed to create discord adapter", "error", err)
			os.Exit(1)
		}
		platforms = append(platforms, adapter)
		slog.Info("discord adapter registered", "channels", cfg.Discord.AllowedChannels)
	}

	if cfg.Telegram.Enabled {
		adapter, err := telegram.NewAdapter(cfg.Telegram, pool, t, synth, voiceStore, cfg.TTS)
		if err != nil {
			slog.Error("failed to create telegram adapter", "error", err)
			os.Exit(1)
		}
		platforms = append(platforms, adapter)
		slog.Info("telegram adapter registered", "allowed_chats", cfg.Telegram.AllowedChats)
	}

	// Future: Teams adapter goes here

	// Start HTTP API server (optional)
	var apiServer *api.Server
	if cfg.API.Enabled && cfg.API.Listen != "" {
		apiServer = api.New(cfg.API.Listen, pool)
		if err := apiServer.Start(); err != nil {
			slog.Error("failed to start api server", "error", err)
			os.Exit(1)
		}
	}

	if len(platforms) == 0 {
		slog.Error("no platform enabled, nothing to do")
		os.Exit(1)
	}

	// Start all platforms
	for _, p := range platforms {
		if err := p.Start(); err != nil {
			slog.Error("failed to start platform", "error", err)
			os.Exit(1)
		}
	}

	// Spawn cleanup goroutine
	stopCleanup := make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pool.CleanupIdle(ttlSecs)
			case <-stopCleanup:
				return
			}
		}
	}()

	slog.Info("openab-go started", "platforms", len(platforms))

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutdown signal received")

	// Cleanup
	close(stopCleanup)
	if apiServer != nil {
		if err := apiServer.Stop(); err != nil {
			slog.Warn("api server stop error", "error", err)
		}
	}
	for _, p := range platforms {
		if err := p.Stop(); err != nil {
			slog.Warn("platform stop error", "error", err)
		}
	}
	pool.Shutdown()
	slog.Info("openab-go shut down")
}
