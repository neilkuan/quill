package teams

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestActivityDispatch_RejectsGet(t *testing.T) {
	auth := &BotAuth{appID: "test", appSecret: "test", tenantID: "test"}
	handler := &Handler{ToolDisplay: "compact"}

	mux := buildMux(auth, handler)

	req := httptest.NewRequest(http.MethodGet, "/api/messages", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestActivityDispatch_RejectsMissingAuth(t *testing.T) {
	auth := &BotAuth{appID: "test", appSecret: "test", tenantID: "test"}
	handler := &Handler{ToolDisplay: "compact"}

	mux := buildMux(auth, handler)

	activity := Activity{Type: "message", Text: "hello"}
	body, _ := json.Marshal(activity)
	req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestActivityDispatch_IgnoresNonMessageTypes(t *testing.T) {
	auth := &BotAuth{appID: "test", appSecret: "test", tenantID: "test"}
	handler := &Handler{ToolDisplay: "compact"}

	mux := buildMuxSkipAuth(auth, handler)

	activity := Activity{Type: "conversationUpdate"}
	body, _ := json.Marshal(activity)
	req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}
