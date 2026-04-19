package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/config"
	"github.com/neilkuan/quill/markdown"
	"github.com/neilkuan/quill/stt"
	"github.com/neilkuan/quill/tts"
)

// maxConsecutivePollingErrors is the number of consecutive getUpdates failures
// before the adapter cancels the current polling loop and restarts. Each failed
// getUpdates request takes up to the poll timeout (~60 s), so 3 errors ≈ 3 min.
const maxConsecutivePollingErrors int64 = 3

// Adapter implements platform.Platform for Telegram.
type Adapter struct {
	b         *bot.Bot
	handler   *Handler
	transport *http.Transport // kept to call CloseIdleConnections on restart

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped bool

	consecutiveErrors atomic.Int64
}

func NewAdapter(cfg config.TelegramConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig) (*Adapter, error) {
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
		Pool:              pool,
		AllowedChats:      allowed,
		AllowedUserIDs:    allowedUsers,
		AllowAnyUser:      allowAnyUser,
		ReactionsConfig:   cfg.Reactions,
		Transcriber:       transcriber,
		Synthesizer:       synthesizer,
		TTSConfig:         ttsCfg,
		MarkdownTableMode: markdown.ParseMode(mdCfg.Tables),
	}

	a := &Adapter{
		handler: h,
		transport: &http.Transport{
			IdleConnTimeout:       30 * time.Second,
			MaxIdleConns:          10,
			MaxIdleConnsPerHost:   5,
			ResponseHeaderTimeout: 70 * time.Second,
		},
	}

	httpClient := &http.Client{
		Timeout:   time.Minute,
		Transport: a.transport,
	}

	// Wrap the default handler so that every successful update resets the
	// consecutive-error counter — proving that getUpdates is working.
	wrappedHandler := func(ctx context.Context, b *bot.Bot, update *models.Update) {
		a.consecutiveErrors.Store(0)
		h.handleUpdate(ctx, b, update)
	}

	b, err := bot.New(cfg.BotToken,
		bot.WithDefaultHandler(wrappedHandler),
		bot.WithErrorsHandler(a.handlePollingError),
		bot.WithHTTPClient(time.Minute, httpClient),
	)
	if err != nil {
		return nil, err
	}

	h.Bot = b
	a.b = b

	return a, nil
}

func (a *Adapter) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Get bot info (username needed for mention detection)
	me, err := a.b.GetMe(ctx)
	if err != nil {
		return err
	}
	a.handler.botUser = me
	slog.Info("✅ starting telegram adapter ✅", "bot", "🤖"+me.Username+"🤖")

	// Register bot commands for the / menu
	_, err = a.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "sessions", Description: "List all active agent sessions"},
			{Command: "info", Description: "Show current chat session details"},
			{Command: "reset", Description: "Reset the current session"},
			{Command: "resume", Description: "Attempt to restore a previous session for this chat"},
			{Command: "stop", Description: "Interrupt the agent's current reply (session kept alive)"},
		},
	})
	if err != nil {
		slog.Warn("failed to register telegram bot commands", "error", err)
	} else {
		slog.Info("registered telegram bot commands")
	}

	go a.supervise()

	return nil
}

// supervise runs the polling loop with automatic restart. When the polling
// context is canceled (either by Stop or by the error handler after too many
// consecutive failures), it purges stale HTTP connections and restarts.
func (a *Adapter) supervise() {
	for {
		ctx, cancel := context.WithCancel(context.Background())
		a.mu.Lock()
		a.cancel = cancel
		a.mu.Unlock()

		a.consecutiveErrors.Store(0)

		// b.Start blocks until ctx is canceled.
		a.b.Start(ctx)

		a.mu.Lock()
		stopped := a.stopped
		a.mu.Unlock()

		if stopped {
			return
		}

		// Purge stale connections so the next polling cycle gets fresh TCP sockets.
		a.transport.CloseIdleConnections()

		slog.Warn("telegram polling restarting after network errors, waiting 5s...")
		time.Sleep(5 * time.Second)
	}
}

func (a *Adapter) Stop() error {
	slog.Info("🛑 stopping telegram adapter 🛑")
	a.mu.Lock()
	a.stopped = true
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	return nil
}

// handlePollingError routes errors from the go-telegram/bot getUpdates loop
// through slog and triggers a restart when errors accumulate.
func (a *Adapter) handlePollingError(err error) {
	if err == nil {
		return
	}
	if isTransientNetworkError(err) {
		slog.Warn("telegram getUpdates transient network error (auto-retry)", "error", err)
	} else {
		slog.Error("telegram bot error", "error", err)
	}

	n := a.consecutiveErrors.Add(1)
	if n == maxConsecutivePollingErrors {
		slog.Warn("telegram: consecutive polling errors reached threshold, restarting polling",
			"count", n)
		a.mu.Lock()
		if a.cancel != nil && !a.stopped {
			a.cancel()
		}
		a.mu.Unlock()
	}
}

// Healthy reports whether the Telegram polling loop is operating normally.
// Returns false when consecutive getUpdates errors have reached the restart
// threshold, indicating the adapter is degraded (e.g. after a network change).
func (a *Adapter) Healthy() bool {
	return a.consecutiveErrors.Load() < maxConsecutivePollingErrors
}

func isTransientNetworkError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Connection dropped mid-response (common after WiFi switch).
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// Connection reset or refused by peer.
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// Catch "use of closed network connection" and similar.
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	// Fallback: check for common transient error substrings that may be
	// wrapped in ways that defeat errors.Is (e.g. fmt.Errorf wrapping).
	msg := err.Error()
	if strings.Contains(msg, "unexpected EOF") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") {
		return true
	}
	return false
}
