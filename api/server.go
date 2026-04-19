package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/neilkuan/quill/acp"
)

var startTime = time.Now()

// HealthCheck returns true when the component is healthy.
type HealthCheck func() bool

type Server struct {
	pool         *acp.SessionPool
	server       *http.Server
	healthChecks []HealthCheck
}

type healthResponse struct {
	Status         string    `json:"status"`
	Uptime         string    `json:"uptime"`
	ActiveSessions int       `json:"active_sessions"`
	MaxSessions    int       `json:"max_sessions"`
	Timestamp      time.Time `json:"timestamp"`
}

func New(listenAddr string, pool *acp.SessionPool, checks ...HealthCheck) *Server {
	s := &Server{pool: pool, healthChecks: checks}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("DELETE /api/sessions/{key}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/health", s.handleHealth)

	s.server = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}
	return s
}

func (s *Server) Start() error {
	slog.Info("starting api server", "listen", s.server.Addr)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("api server error", "error", err)
		}
	}()
	return nil
}

func (s *Server) Stop() error {
	slog.Info("🛑 stopping api server 🛑")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.pool.ListSessions()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing session key"})
		return
	}

	// Support Telegram-style keys like "tg:12345" — the colon may be URL-encoded
	key = strings.ReplaceAll(key, "%3A", ":")

	if err := s.pool.KillSession(key); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "session terminated", "key": key})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	active, max := s.pool.Stats()

	status := "ok"
	httpStatus := http.StatusOK
	for _, check := range s.healthChecks {
		if !check() {
			status = "degraded"
			httpStatus = http.StatusServiceUnavailable
			break
		}
	}

	writeJSON(w, httpStatus, healthResponse{
		Status:         status,
		Uptime:         time.Since(startTime).Truncate(time.Second).String(),
		ActiveSessions: active,
		MaxSessions:    max,
		Timestamp:      time.Now(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
