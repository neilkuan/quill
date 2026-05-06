package teams

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/config"
	"github.com/neilkuan/quill/cronjob"
	"github.com/neilkuan/quill/markdown"
	"github.com/neilkuan/quill/sessionpicker"
	"github.com/neilkuan/quill/stt"
	"github.com/neilkuan/quill/tts"
)

// Adapter implements platform.Platform for Microsoft Teams.
type Adapter struct {
	auth       *BotAuth
	client     *BotClient
	handler    *Handler
	httpServer *http.Server
	healthy    atomic.Bool
	listen     string
}

func NewAdapter(cfg config.TeamsConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig, picker sessionpicker.Picker, cronStore *cronjob.Store, cronCfg config.CronjobConfig) (*Adapter, error) {
	auth := NewBotAuth(cfg.AppID, cfg.AppSecret, cfg.TenantID)
	client := NewBotClient(auth)

	serviceURLStore, err := OpenServiceURLStore(cfg.ServiceURLStorePath)
	if err != nil {
		return nil, err
	}
	if cfg.ServiceURLStorePath != "" {
		slog.Info("teams serviceURL store opened",
			"path", cfg.ServiceURLStorePath, "cached", serviceURLStore.Len())
	}

	allowedChannels := make(map[string]bool, len(cfg.AllowedChannels))
	for _, ch := range cfg.AllowedChannels {
		allowedChannels[ch] = true
	}

	allowedUserIDs := make(map[string]bool)
	allowAnyUser := false
	for _, uid := range cfg.AllowedUserIDs {
		if uid == "*" {
			allowAnyUser = true
		} else {
			allowedUserIDs[uid] = true
		}
	}

	toolDisplay := cfg.ToolDisplay
	if toolDisplay == "" {
		toolDisplay = "compact"
	}

	handler := &Handler{
		Pool:              pool,
		Client:            client,
		AllowedChannels:   allowedChannels,
		AllowedUserIDs:    allowedUserIDs,
		AllowAnyUser:      allowAnyUser,
		Transcriber:       transcriber,
		Synthesizer:       synthesizer,
		TTSConfig:         ttsCfg,
		MarkdownTableMode: markdown.ParseMode(mdCfg.Tables),
		ToolDisplay:       toolDisplay,
		Picker:            picker,
		Mentions:          NewMentionDirectory(),
		CronStore:         cronStore,
		CronCfg:           cronCfg,
		ServiceURLs:       serviceURLStore,
	}

	mux := buildMux(auth, handler)

	a := &Adapter{
		auth:    auth,
		client:  client,
		handler: handler,
		listen:  cfg.Listen,
		httpServer: &http.Server{
			Addr:         cfg.Listen,
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		},
	}

	return a, nil
}

func (a *Adapter) Start() error {
	ln, err := net.Listen("tcp", a.listen)
	if err != nil {
		return err
	}

	a.healthy.Store(true)
	slog.Info("✅ starting teams adapter ✅", "listen", a.listen)

	go func() {
		if err := a.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("teams http server error", "error", err)
			a.healthy.Store(false)
		}
	}()

	return nil
}

func (a *Adapter) Stop() error {
	slog.Info("🛑 stopping teams adapter 🛑")
	a.healthy.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.httpServer.Shutdown(ctx)
}

func (a *Adapter) Healthy() bool {
	return a.healthy.Load()
}

// RegisterCron creates the teams cron Dispatcher and registers it
// with the prefix "teams". Call after NewAdapter.
func (a *Adapter) RegisterCron(registry *cronjob.Registry) {
	registry.Register("teams", &CronDispatcher{Handler: a.handler, Client: a.client})
}

// buildMux creates the HTTP handler mux with JWT validation.
func buildMux(auth *BotAuth, handler *Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/messages", func(w http.ResponseWriter, r *http.Request) {
		if err := auth.ValidateInbound(r); err != nil {
			slog.Warn("teams auth failed", "error", err, "remote", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		handleActivity(w, r, handler)
	})
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	})
	return mux
}

// buildMuxSkipAuth creates a mux without JWT validation (for testing).
func buildMuxSkipAuth(auth *BotAuth, handler *Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/messages", func(w http.ResponseWriter, r *http.Request) {
		handleActivity(w, r, handler)
	})
	mux.HandleFunc("/api/messages", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	})
	return mux
}

func handleActivity(w http.ResponseWriter, r *http.Request, handler *Handler) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	var activity Activity
	if err := json.Unmarshal(body, &activity); err != nil {
		slog.Warn("teams: failed to parse activity", "error", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	slog.Debug("teams activity received",
		"type", activity.Type,
		"from", activity.From.Name,
		"conversation", activity.Conversation.ID,
		"text_len", len(activity.Text))

	// Always respond 200 OK immediately — process asynchronously
	w.WriteHeader(http.StatusOK)

	switch activity.Type {
	case "message":
		if _, err := UnmarshalInvokeData(&activity); err == nil {
			go handler.OnInvokeAction(&activity)
		} else {
			go handler.OnMessage(&activity)
		}
	case "conversationUpdate":
		slog.Info("teams conversation update", "conversation", activity.Conversation.ID)
	default:
		slog.Debug("teams: ignoring activity type", "type", activity.Type)
	}
}
