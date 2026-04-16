package teams

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBotClient_SendActivity(t *testing.T) {
	var receivedActivity Activity
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer mock-token" {
			t.Errorf("expected Bearer mock-token, got %s", auth)
		}

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedActivity)

		json.NewEncoder(w).Encode(Activity{ID: "reply-001"})
	}))
	defer ts.Close()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "mock-token",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	auth := &BotAuth{
		appID:     "app-id",
		appSecret: "secret",
		tenantID:  "tenant",
		tokenURL:  tokenServer.URL,
	}

	client := NewBotClient(auth)

	reply, err := client.SendActivity(ts.URL, "conv-123", &Activity{
		Type: "message",
		Text: "Hello!",
	})
	if err != nil {
		t.Fatalf("SendActivity: %v", err)
	}
	if reply.ID != "reply-001" {
		t.Errorf("expected reply ID reply-001, got %s", reply.ID)
	}
	if receivedActivity.Text != "Hello!" {
		t.Errorf("server received text: %s", receivedActivity.Text)
	}
}

func TestBotClient_UpdateActivity(t *testing.T) {
	var method, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Activity{ID: "msg-001"})
	}))
	defer ts.Close()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "mock-token",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	auth := &BotAuth{
		appID:     "app-id",
		appSecret: "secret",
		tenantID:  "tenant",
		tokenURL:  tokenServer.URL,
	}

	client := NewBotClient(auth)

	err := client.UpdateActivity(ts.URL, "conv-123", "msg-001", &Activity{
		Type: "message",
		Text: "Updated!",
	})
	if err != nil {
		t.Fatalf("UpdateActivity: %v", err)
	}
	if method != http.MethodPut {
		t.Errorf("expected PUT, got %s", method)
	}
	if path != "/v3/conversations/conv-123/activities/msg-001" {
		t.Errorf("unexpected path: %s", path)
	}
}

func TestBotClient_SendTyping(t *testing.T) {
	var receivedActivity Activity
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedActivity)
		json.NewEncoder(w).Encode(Activity{ID: "typing-001"})
	}))
	defer ts.Close()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "mock-token",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	auth := &BotAuth{
		appID:     "app-id",
		appSecret: "secret",
		tenantID:  "tenant",
		tokenURL:  tokenServer.URL,
	}

	client := NewBotClient(auth)

	err := client.SendTyping(ts.URL, "conv-123", Account{ID: "bot-id", Name: "Bot"})
	if err != nil {
		t.Fatalf("SendTyping: %v", err)
	}
	if receivedActivity.Type != "typing" {
		t.Errorf("expected typing activity, got %s", receivedActivity.Type)
	}
}
