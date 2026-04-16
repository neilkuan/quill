package teams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetBotToken(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("unexpected grant_type: %s", r.Form.Get("grant_type"))
		}
		if r.Form.Get("client_id") != "test-app-id" {
			t.Errorf("unexpected client_id: %s", r.Form.Get("client_id"))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "mock-bot-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	auth := &BotAuth{
		appID:     "test-app-id",
		appSecret: "test-secret",
		tenantID:  "test-tenant",
		tokenURL:  ts.URL,
	}

	token, err := auth.GetBotToken()
	if err != nil {
		t.Fatalf("GetBotToken: %v", err)
	}
	if token != "mock-bot-token" {
		t.Errorf("expected mock-bot-token, got %s", token)
	}

	// Second call should return cached token
	token2, err := auth.GetBotToken()
	if err != nil {
		t.Fatalf("GetBotToken (cached): %v", err)
	}
	if token2 != "mock-bot-token" {
		t.Errorf("expected cached token, got %s", token2)
	}
}

func TestValidateInbound_MissingHeader(t *testing.T) {
	auth := NewBotAuth("test-app-id", "test-secret", "test-tenant")
	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)

	err := auth.ValidateInbound(r)
	if err == nil {
		t.Error("expected error for missing Authorization header")
	}
}

func TestValidateInbound_InvalidFormat(t *testing.T) {
	auth := NewBotAuth("test-app-id", "test-secret", "test-tenant")
	r := httptest.NewRequest(http.MethodPost, "/api/messages", nil)
	r.Header.Set("Authorization", "InvalidFormat")

	err := auth.ValidateInbound(r)
	if err == nil {
		t.Error("expected error for invalid Authorization format")
	}
}
