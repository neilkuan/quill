package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neilkuan/quill/acp"
)

func newTestPool() *acp.SessionPool {
	return acp.NewSessionPool("echo", nil, "/tmp", nil, 10)
}

func TestHealthEndpoint(t *testing.T) {
	pool := newTestPool()
	srv := New(":0", pool)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %q", resp.Status)
	}
	if resp.MaxSessions != 10 {
		t.Errorf("expected max_sessions=10, got %d", resp.MaxSessions)
	}
	if resp.ActiveSessions != 0 {
		t.Errorf("expected active_sessions=0, got %d", resp.ActiveSessions)
	}
}

func TestHealthEndpoint_Degraded(t *testing.T) {
	pool := newTestPool()
	unhealthy := func() bool { return false }
	srv := New(":0", pool, unhealthy)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if resp.Status != "degraded" {
		t.Errorf("expected status=degraded, got %q", resp.Status)
	}
}

func TestListSessionsEndpoint_Empty(t *testing.T) {
	pool := newTestPool()
	srv := New(":0", pool)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	count, ok := resp["count"].(float64)
	if !ok || count != 0 {
		t.Errorf("expected count=0, got %v", resp["count"])
	}
}

func TestDeleteSessionEndpoint_NotFound(t *testing.T) {
	pool := newTestPool()
	srv := New(":0", pool)

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
