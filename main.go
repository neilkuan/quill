package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neilkuan/openab-go/acp"
	appconfig "github.com/neilkuan/openab-go/config"
	"github.com/neilkuan/openab-go/discord"
	"github.com/neilkuan/openab-go/platform"
	"github.com/neilkuan/openab-go/transcribe"
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

	// Create transcriber if configured
	var t transcribe.Transcriber
	if cfg.Transcribe.Enabled {
		switch cfg.Transcribe.Provider {
		case "openai":
			t = transcribe.NewOpenAITranscriber(transcribe.OpenAIConfig{
				APIKey:   cfg.Transcribe.APIKey,
				Model:    cfg.Transcribe.Model,
				Language: cfg.Transcribe.Language,
				Prompt:   cfg.Transcribe.Prompt,
				BaseURL:  cfg.Transcribe.BaseURL,
			})
			slog.Info("transcriber enabled", "provider", "openai", "model", cfg.Transcribe.Model)
		default:
			slog.Warn("unknown transcribe provider, voice transcription disabled", "provider", cfg.Transcribe.Provider)
		}
	}

	// Build platforms
	var platforms []platform.Platform

	if cfg.Discord.Enabled {
		adapter, err := discord.NewAdapter(cfg.Discord, pool, t)
		if err != nil {
			slog.Error("failed to create discord adapter", "error", err)
			os.Exit(1)
		}
		platforms = append(platforms, adapter)
		slog.Info("discord adapter registered", "channels", cfg.Discord.AllowedChannels)
	}

	// Future: Telegram, Teams adapters go here

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
	for _, p := range platforms {
		if err := p.Stop(); err != nil {
			slog.Warn("platform stop error", "error", err)
		}
	}
	pool.Shutdown()
	slog.Info("openab-go shut down")
}
