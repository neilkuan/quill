package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/api"
	appconfig "github.com/neilkuan/quill/config"
	"github.com/neilkuan/quill/discord"
	"github.com/neilkuan/quill/platform"
	"github.com/neilkuan/quill/sessionpicker"
	"github.com/neilkuan/quill/stt"
	"github.com/neilkuan/quill/teams"
	"github.com/neilkuan/quill/telegram"
	"github.com/neilkuan/quill/tts"
)

var commit = "unknown"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("quill (%s)\n", commit)
		os.Exit(0)
	}

	// Setup structured logging
	logLevel := slog.LevelInfo
	if os.Getenv("QUILL_LOG") == "debug" {
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
	if cfg.TTS.Enabled {
		switch cfg.TTS.Provider {
		case "openai":
			synth = tts.NewOpenAISynthesizer(tts.OpenAIConfig{
				APIKey:       cfg.TTS.APIKey,
				Model:        cfg.TTS.Model,
				Voice:        cfg.TTS.Voice,
				Instructions: cfg.TTS.Instructions,
				BaseURL:      cfg.TTS.BaseURL,
				TimeoutSec:   cfg.TTS.TimeoutSec,
			})
		case "gemini":
			synth = tts.NewGeminiSynthesizer(tts.GeminiConfig{
				APIKey:       cfg.TTS.APIKey,
				Model:        cfg.TTS.Model,
				Voice:        cfg.TTS.Voice,
				Instructions: cfg.TTS.Instructions,
				Style:        cfg.TTS.Style,
				StylePrefix:  cfg.TTS.StylePrefix,
				StyleSuffix:  cfg.TTS.StyleSuffix,
				TimeoutSec:   cfg.TTS.TimeoutSec,
			})
		default:
			slog.Warn("unknown tts provider, voice synthesis disabled", "provider", cfg.TTS.Provider)
		}
		if synth != nil {
			slog.Info("🔊 tts enabled", "provider", cfg.TTS.Provider, "model", cfg.TTS.Model, "voice", cfg.TTS.Voice)
		}
	}

	// Resolve a session picker for the configured agent binary. Nil
	// when the binary is unknown — handlers treat a nil picker as
	// "session-picker not available" and respond with a friendly
	// message instead of crashing.
	picker, pickerOK := sessionpicker.Detect(cfg.Agent.Command)
	if pickerOK {
		slog.Info("session picker enabled", "agent_type", picker.AgentType())
	} else {
		slog.Info("session picker not available for this agent", "agent_cmd", cfg.Agent.Command)
	}

	// Build platforms
	var platforms []platform.Platform
	var healthChecks []api.HealthCheck

	if cfg.Discord.Enabled {
		adapter, err := discord.NewAdapter(cfg.Discord, pool, t, synth, cfg.TTS, cfg.Markdown, picker)
		if err != nil {
			slog.Error("failed to create discord adapter", "error", err)
			os.Exit(1)
		}
		platforms = append(platforms, adapter)
		healthChecks = append(healthChecks, adapter.Healthy)
		slog.Info("discord adapter registered",
			"channels", cfg.Discord.AllowedChannels,
			"allowed_user_id", cfg.Discord.AllowedUserIDs)
	}

	if cfg.Telegram.Enabled {
		adapter, err := telegram.NewAdapter(cfg.Telegram, pool, t, synth, cfg.TTS, cfg.Markdown, picker)
		if err != nil {
			slog.Error("failed to create telegram adapter", "error", err)
			os.Exit(1)
		}
		platforms = append(platforms, adapter)
		healthChecks = append(healthChecks, adapter.Healthy)
		slog.Info("telegram adapter registered",
			"allowed_chats", cfg.Telegram.AllowedChats,
			"allowed_user_id", cfg.Telegram.AllowedUserIDs)
	}

	if cfg.Teams.Enabled {
		adapter, err := teams.NewAdapter(cfg.Teams, pool, t, synth, cfg.TTS, cfg.Markdown, picker)
		if err != nil {
			slog.Error("failed to create teams adapter", "error", err)
			os.Exit(1)
		}
		platforms = append(platforms, adapter)
		healthChecks = append(healthChecks, adapter.Healthy)
		slog.Info("teams adapter registered", "listen", cfg.Teams.Listen)
	}

	// Start HTTP API server (optional)
	var apiServer *api.Server
	if cfg.API.Enabled && cfg.API.Listen != "" {
		apiServer = api.New(cfg.API.Listen, pool, healthChecks...)
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

	slog.Info("quill started", "platforms", len(platforms))

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
	slog.Info("🦆 quill shut down 🦆🦆🦆")
}
