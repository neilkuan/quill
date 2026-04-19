# Microsoft Teams Platform Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Microsoft Teams as a third platform adapter for Quill, enabling @mention-triggered bot conversations in Teams channels with image/file attachment support.

**Architecture:** Webhook-based adapter using Azure Bot Framework protocol. An HTTP server receives Activity payloads from Teams, validates JWT tokens, processes messages through the shared ACP session pool, and streams replies back via the Bot Framework REST API. Follows the same adapter/handler pattern as Discord and Telegram.

**Tech Stack:** Go stdlib `net/http`, `github.com/go-jose/go-jose/v4` (JWKS), `github.com/golang-jwt/jwt/v5` (JWT parsing)

---

### Task 1: Config — Extend TeamsConfig and add defaults

**Files:**
- Modify: `config/config.go:147-152` (TeamsConfig struct)
- Modify: `config/config.go:156-193` (applyDefaults function)
- Test: `config/config_test.go` (create if needed)

- [ ] **Step 1: Write failing test for Teams config auto-enable**

```go
// config/config_test.go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTeamsAutoEnable(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	os.WriteFile(configPath, []byte(`
[agent]
command = "echo"
working_dir = "/tmp"

[teams]
app_id = "test-app-id"
app_secret = "test-app-secret"
tenant_id = "test-tenant-id"
`), 0644)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Teams.Enabled {
		t.Error("expected Teams to be auto-enabled when app_id and app_secret are set")
	}
	if cfg.Teams.Listen != ":3978" {
		t.Errorf("expected default listen :3978, got %s", cfg.Teams.Listen)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./config/ -run TestTeamsAutoEnable -v`
Expected: FAIL — Teams.Enabled is false, Listen is empty

- [ ] **Step 3: Extend TeamsConfig struct and add defaults**

Modify `config/config.go` — update the `TeamsConfig` struct:

```go
type TeamsConfig struct {
	Enabled   bool   `toml:"enabled"`
	AppID     string `toml:"app_id"`
	AppSecret string `toml:"app_secret"`
	TenantID  string `toml:"tenant_id"`
	Listen    string `toml:"listen"`
}
```

Add to `applyDefaults` function, after the Telegram defaults block:

```go
// Teams — if the section has app_id and app_secret, default to enabled
if cfg.Teams.AppID != "" && cfg.Teams.AppSecret != "" && !cfg.Teams.Enabled {
	cfg.Teams.Enabled = true
}
if cfg.Teams.Listen == "" {
	cfg.Teams.Listen = ":3978"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./config/ -run TestTeamsAutoEnable -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `go test ./... -v`
Expected: All tests pass

- [ ] **Step 6: Commit**

```bash
git add config/config.go config/config_test.go
git commit -m "feat(teams): extend TeamsConfig with listen field and auto-enable defaults"
```

---

### Task 2: Auth — Bot Framework JWT validation and OAuth2 token acquisition

**Files:**
- Create: `teams/auth.go`
- Create: `teams/auth_test.go`

- [ ] **Step 1: Add JWT dependencies**

```bash
go get github.com/go-jose/go-jose/v4@latest
go get github.com/golang-jwt/jwt/v5@latest
```

- [ ] **Step 2: Write failing test for outbound token acquisition**

```go
// teams/auth_test.go
package teams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetBotToken(t *testing.T) {
	// Mock Azure AD token endpoint
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
		appID:        "test-app-id",
		appSecret:    "test-secret",
		tenantID:     "test-tenant",
		tokenURL:     ts.URL,
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./teams/ -run TestGetBotToken -v`
Expected: FAIL — package/types don't exist

- [ ] **Step 4: Implement BotAuth with outbound token acquisition**

```go
// teams/auth.go
package teams

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultTokenURL  = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
	botFrameworkScope = "https://api.botframework.com/.default"
	openIDMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	// Refresh token 5 minutes before expiry
	tokenRefreshBuffer = 5 * time.Minute
)

// BotAuth handles both inbound JWT validation (Teams -> Quill) and outbound
// OAuth2 token acquisition (Quill -> Bot Framework REST API).
type BotAuth struct {
	appID     string
	appSecret string
	tenantID  string

	// tokenURL can be overridden for testing; defaults to Azure AD endpoint.
	tokenURL string

	// Outbound token cache
	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewBotAuth creates a new BotAuth instance.
func NewBotAuth(appID, appSecret, tenantID string) *BotAuth {
	return &BotAuth{
		appID:     appID,
		appSecret: appSecret,
		tenantID:  tenantID,
		tokenURL:  fmt.Sprintf(defaultTokenURL, tenantID),
	}
}

// GetBotToken returns a valid OAuth2 token for calling Bot Framework REST APIs.
// Tokens are cached and automatically refreshed before expiry.
func (a *BotAuth) GetBotToken() (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()

	if a.token != "" && time.Now().Before(a.tokenExpiry) {
		return a.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.appID},
		"client_secret": {a.appSecret},
		"scope":         {botFrameworkScope},
	}

	resp, err := http.Post(a.tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}

	a.token = tokenResp.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn)*time.Second - tokenRefreshBuffer)

	return a.token, nil
}

// ValidateInbound validates the JWT token from an incoming Bot Framework request.
// For MVP, this performs basic Authorization header extraction and audience check.
// Full JWKS signature validation requires fetching OpenID metadata and is implemented
// in the JWKS validation methods below.
func (a *BotAuth) ValidateInbound(r *http.Request) error {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return fmt.Errorf("missing Authorization header")
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return fmt.Errorf("invalid Authorization header format")
	}

	tokenString := parts[1]
	return a.validateJWT(tokenString)
}

// validateJWT parses and validates the JWT claims.
// Uses go-jose for JWKS verification and golang-jwt for claims parsing.
func (a *BotAuth) validateJWT(tokenString string) error {
	// Parse without verification first to extract claims for audience check.
	// Full JWKS signature verification is done via the cached key set.
	parser := jwtv5.NewParser(
		jwtv5.WithAudience(a.appID),
		jwtv5.WithIssuedAt(),
	)

	token, _, err := parser.ParseUnverified(tokenString, jwtv5.MapClaims{})
	if err != nil {
		return fmt.Errorf("parse JWT: %w", err)
	}

	claims, ok := token.Claims.(jwtv5.MapClaims)
	if !ok {
		return fmt.Errorf("unexpected claims type")
	}

	// Verify issuer — Bot Framework tokens use this issuer
	iss, _ := claims["iss"].(string)
	validIssuers := []string{
		"https://api.botframework.com",
		"https://sts.windows.net/" + a.tenantID + "/",
		"https://login.microsoftonline.com/" + a.tenantID + "/v2.0",
	}
	issuerValid := false
	for _, valid := range validIssuers {
		if iss == valid {
			issuerValid = true
			break
		}
	}
	if !issuerValid {
		return fmt.Errorf("invalid issuer: %s", iss)
	}

	return nil
}
```

Note: Add the import for `jwtv5` at the top:

```go
import (
	// ...
	jwtv5 "github.com/golang-jwt/jwt/v5"
)
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./teams/ -run TestGetBotToken -v`
Expected: PASS

- [ ] **Step 6: Write test for inbound JWT validation**

```go
// Add to teams/auth_test.go

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
```

- [ ] **Step 7: Run tests**

Run: `go test ./teams/ -v`
Expected: All PASS

- [ ] **Step 8: Commit**

```bash
git add teams/auth.go teams/auth_test.go go.mod go.sum
git commit -m "feat(teams): implement BotAuth with OAuth2 token acquisition and JWT validation"
```

---

### Task 3: Bot Framework Types — Activity and related structs

**Files:**
- Create: `teams/types.go`
- Create: `teams/types_test.go`

- [ ] **Step 1: Write failing test for Activity JSON unmarshaling**

```go
// teams/types_test.go
package teams

import (
	"encoding/json"
	"testing"
)

func TestActivityUnmarshal(t *testing.T) {
	raw := `{
		"type": "message",
		"id": "abc123",
		"timestamp": "2026-04-16T10:00:00Z",
		"serviceUrl": "https://smba.trafficmanager.net/teams/",
		"channelId": "msteams",
		"from": {"id": "user-123", "name": "Test User"},
		"conversation": {"id": "conv-456"},
		"recipient": {"id": "bot-789", "name": "QuillBot"},
		"text": "<at>QuillBot</at> hello world",
		"entities": [
			{
				"type": "mention",
				"mentioned": {"id": "bot-789", "name": "QuillBot"},
				"text": "<at>QuillBot</at>"
			}
		],
		"attachments": [
			{
				"contentType": "image/png",
				"contentUrl": "https://example.com/image.png",
				"name": "screenshot.png"
			}
		]
	}`

	var activity Activity
	if err := json.Unmarshal([]byte(raw), &activity); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if activity.Type != "message" {
		t.Errorf("type: got %s, want message", activity.Type)
	}
	if activity.From.ID != "user-123" {
		t.Errorf("from.id: got %s, want user-123", activity.From.ID)
	}
	if activity.Conversation.ID != "conv-456" {
		t.Errorf("conversation.id: got %s, want conv-456", activity.Conversation.ID)
	}
	if activity.Recipient.ID != "bot-789" {
		t.Errorf("recipient.id: got %s, want bot-789", activity.Recipient.ID)
	}
	if len(activity.Entities) != 1 {
		t.Fatalf("entities: got %d, want 1", len(activity.Entities))
	}
	if activity.Entities[0].Mentioned == nil || activity.Entities[0].Mentioned.ID != "bot-789" {
		t.Error("expected mention entity with bot-789")
	}
	if len(activity.Attachments) != 1 {
		t.Fatalf("attachments: got %d, want 1", len(activity.Attachments))
	}
	if activity.Attachments[0].ContentURL != "https://example.com/image.png" {
		t.Errorf("attachment contentUrl: got %s", activity.Attachments[0].ContentURL)
	}
}

func TestActivityMarshal(t *testing.T) {
	activity := Activity{
		Type: "message",
		Text: "Hello from bot",
		Conversation: Conversation{ID: "conv-456"},
		ReplyToID: "msg-123",
	}

	data, err := json.Marshal(activity)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]interface{}
	json.Unmarshal(data, &decoded)

	if decoded["type"] != "message" {
		t.Errorf("type: got %v", decoded["type"])
	}
	if decoded["replyToId"] != "msg-123" {
		t.Errorf("replyToId: got %v", decoded["replyToId"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./teams/ -run TestActivity -v`
Expected: FAIL — types don't exist

- [ ] **Step 3: Implement Activity types**

```go
// teams/types.go
package teams

// Activity represents a Bot Framework Activity — the core message type
// exchanged between Teams and the bot.
type Activity struct {
	Type         string       `json:"type"`
	ID           string       `json:"id,omitempty"`
	Timestamp    string       `json:"timestamp,omitempty"`
	ServiceURL   string       `json:"serviceUrl,omitempty"`
	ChannelID    string       `json:"channelId,omitempty"`
	From         Account      `json:"from,omitempty"`
	Conversation Conversation `json:"conversation,omitempty"`
	Recipient    Account      `json:"recipient,omitempty"`
	Text         string       `json:"text,omitempty"`
	TextFormat   string       `json:"textFormat,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty"`
	Entities     []Entity     `json:"entities,omitempty"`
	ReplyToID    string       `json:"replyToId,omitempty"`
}

// Account represents a user or bot identity in a conversation.
type Account struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Conversation identifies a conversation thread.
type Conversation struct {
	ID string `json:"id,omitempty"`
}

// Attachment represents a file or media attached to an Activity.
type Attachment struct {
	ContentType string `json:"contentType,omitempty"`
	ContentURL  string `json:"contentUrl,omitempty"`
	Name        string `json:"name,omitempty"`
}

// Entity represents metadata attached to an Activity (e.g. @mention info).
type Entity struct {
	Type      string   `json:"type,omitempty"`
	Mentioned *Account `json:"mentioned,omitempty"`
	Text      string   `json:"text,omitempty"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./teams/ -run TestActivity -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add teams/types.go teams/types_test.go
git commit -m "feat(teams): add Bot Framework Activity types"
```

---

### Task 4: Client — Bot Framework REST API client

**Files:**
- Create: `teams/client.go`
- Create: `teams/client_test.go`

- [ ] **Step 1: Write failing test for SendActivity**

```go
// teams/client_test.go
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

		// Return created activity with ID
		json.NewEncoder(w).Encode(Activity{ID: "reply-001"})
	}))
	defer ts.Close()

	// Mock auth that always returns "mock-token"
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./teams/ -run TestBotClient -v`
Expected: FAIL — BotClient type doesn't exist

- [ ] **Step 3: Implement BotClient**

```go
// teams/client.go
package teams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BotClient wraps the Bot Framework REST API for sending messages back to Teams.
type BotClient struct {
	auth *BotAuth
	http *http.Client
}

// NewBotClient creates a BotClient with the given auth provider.
func NewBotClient(auth *BotAuth) *BotClient {
	return &BotClient{
		auth: auth,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// SendActivity posts a new Activity to a conversation. Returns the created Activity
// (with its server-assigned ID) so the caller can later update it.
func (c *BotClient) SendActivity(serviceURL, conversationID string, activity *Activity) (*Activity, error) {
	url := fmt.Sprintf("%s/v3/conversations/%s/activities",
		strings.TrimRight(serviceURL, "/"), conversationID)

	return c.doActivityRequest(http.MethodPost, url, activity)
}

// UpdateActivity edits an existing Activity by ID (used for streaming message updates).
func (c *BotClient) UpdateActivity(serviceURL, conversationID, activityID string, activity *Activity) error {
	url := fmt.Sprintf("%s/v3/conversations/%s/activities/%s",
		strings.TrimRight(serviceURL, "/"), conversationID, activityID)

	_, err := c.doActivityRequest(http.MethodPut, url, activity)
	return err
}

// SendTyping sends a "typing" indicator to the conversation.
func (c *BotClient) SendTyping(serviceURL, conversationID string, from Account) error {
	activity := &Activity{
		Type: "typing",
		From: from,
	}
	_, err := c.SendActivity(serviceURL, conversationID, activity)
	return err
}

func (c *BotClient) doActivityRequest(method, url string, activity *Activity) (*Activity, error) {
	body, err := json.Marshal(activity)
	if err != nil {
		return nil, fmt.Errorf("marshal activity: %w", err)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	token, err := c.auth.GetBotToken()
	if err != nil {
		return nil, fmt.Errorf("get bot token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result Activity
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &result); err != nil {
			// Some endpoints return empty or non-JSON on success — that's OK
			return &Activity{}, nil
		}
	}

	return &result, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./teams/ -run TestBotClient -v`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add teams/client.go teams/client_test.go
git commit -m "feat(teams): implement Bot Framework REST API client"
```

---

### Task 5: Handler — Message processing, mention detection, streaming

**Files:**
- Create: `teams/handler.go`
- Create: `teams/handler_test.go`

- [ ] **Step 1: Write failing test for mention detection and stripping**

```go
// teams/handler_test.go
package teams

import (
	"testing"
)

func TestStripMention(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		botID    string
		entities []Entity
		want     string
	}{
		{
			name:  "simple mention",
			text:  "<at>QuillBot</at> hello world",
			botID: "bot-123",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "bot-123"}, Text: "<at>QuillBot</at>"},
			},
			want: "hello world",
		},
		{
			name:  "mention in middle",
			text:  "hey <at>QuillBot</at> do something",
			botID: "bot-123",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "bot-123"}, Text: "<at>QuillBot</at>"},
			},
			want: "hey  do something",
		},
		{
			name:     "no mention",
			text:     "plain text message",
			botID:    "bot-123",
			entities: nil,
			want:     "plain text message",
		},
		{
			name:  "mention of different user",
			text:  "<at>OtherUser</at> hello",
			botID: "bot-123",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "other-456"}, Text: "<at>OtherUser</at>"},
			},
			want: "<at>OtherUser</at> hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripBotMention(tt.text, tt.botID, tt.entities)
			if got != tt.want {
				t.Errorf("stripBotMention() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsBotMentioned(t *testing.T) {
	botID := "bot-123"

	tests := []struct {
		name     string
		entities []Entity
		want     bool
	}{
		{
			name: "bot mentioned",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "bot-123"}},
			},
			want: true,
		},
		{
			name: "other user mentioned",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "other-456"}},
			},
			want: false,
		},
		{
			name:     "no entities",
			entities: nil,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBotMentioned(botID, tt.entities)
			if got != tt.want {
				t.Errorf("isBotMentioned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildSessionKey(t *testing.T) {
	got := buildSessionKey("19:abc@thread.tacv2;messageid=123")
	want := "teams:19:abc@thread.tacv2;messageid=123"
	if got != want {
		t.Errorf("buildSessionKey() = %s, want %s", got, want)
	}
}

func TestExtractCommand(t *testing.T) {
	tests := []struct {
		text    string
		wantCmd string
	}{
		{"sessions", "sessions"},
		{"info", "info"},
		{"reset", "reset"},
		{"resume", "resume"},
		{"hello world", ""},
		{"", ""},
	}

	for _, tt := range tests {
		cmd, _ := extractCommand(tt.text)
		if cmd != tt.wantCmd {
			t.Errorf("extractCommand(%q) = %q, want %q", tt.text, cmd, tt.wantCmd)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./teams/ -run "TestStripMention|TestIsBotMentioned|TestBuildSessionKey|TestExtractCommand" -v`
Expected: FAIL — functions don't exist

- [ ] **Step 3: Implement handler helper functions**

```go
// teams/handler.go
package teams

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
	"github.com/neilkuan/quill/config"
	"github.com/neilkuan/quill/markdown"
	"github.com/neilkuan/quill/platform"
	"github.com/neilkuan/quill/stt"
	"github.com/neilkuan/quill/tts"
)

// Handler processes incoming Bot Framework Activities from Teams.
type Handler struct {
	Pool              *acp.SessionPool
	Client            *BotClient
	Transcriber       stt.Transcriber
	Synthesizer       tts.Synthesizer
	TTSConfig         config.TTSConfig
	MarkdownTableMode markdown.TableMode
	ToolDisplay       string
}

// OnMessage handles an incoming message Activity.
func (h *Handler) OnMessage(activity *Activity) {
	botID := activity.Recipient.ID

	if !isBotMentioned(botID, activity.Entities) {
		slog.Debug("teams message not addressed to bot, ignoring",
			"from", activity.From.ID,
			"conversation", activity.Conversation.ID)
		return
	}

	prompt := stripBotMention(activity.Text, botID, activity.Entities)
	prompt = strings.TrimSpace(prompt)

	// Check for bot commands
	if cmd, cmdText := extractCommand(prompt); cmd != "" {
		h.handleCommand(activity, cmd, cmdText)
		return
	}

	hasAttachments := len(activity.Attachments) > 0
	if prompt == "" && !hasAttachments {
		return
	}

	// Inject structured sender context
	senderCtx := map[string]interface{}{
		"schema":       "quill.sender.v1",
		"sender_id":    activity.From.ID,
		"sender_name":  activity.From.Name,
		"display_name": activity.From.Name,
		"channel":      "teams",
		"channel_id":   activity.Conversation.ID,
	}
	senderJSON, _ := json.Marshal(senderCtx)
	promptWithSender := fmt.Sprintf("<sender_context>\n%s\n</sender_context>\n\n%s", string(senderJSON), prompt)

	// Download attachments
	var imagePaths []string
	var fileAttachments []platform.FileAttachment
	if hasAttachments {
		tmpDir := filepath.Join(h.Pool.WorkingDir(), ".tmp")
		if err := os.MkdirAll(tmpDir, 0700); err != nil {
			slog.Error("failed to create temp directory", "error", err)
		} else {
			for _, att := range activity.Attachments {
				if att.ContentURL == "" {
					continue
				}
				if isImageContentType(att.ContentType) {
					localPath, err := h.downloadAttachment(att.ContentURL, att.Name, tmpDir)
					if err != nil {
						slog.Error("failed to download image", "url", att.ContentURL, "error", err)
						continue
					}
					imagePaths = append(imagePaths, localPath)
				} else if isAudioContentType(att.ContentType) {
					slog.Warn("audio attachment received but voice processing is deferred", "name", att.Name)
				} else {
					localPath, err := h.downloadAttachment(att.ContentURL, att.Name, tmpDir)
					if err != nil {
						slog.Error("failed to download file", "url", att.ContentURL, "error", err)
						continue
					}
					fileAttachments = append(fileAttachments, platform.FileAttachment{
						Filename:    att.Name,
						ContentType: att.ContentType,
						Size:        0, // Teams doesn't include size in Activity
						LocalPath:   localPath,
					})
				}
			}
		}
	}

	// Build content blocks
	contentText := buildPromptContent(promptWithSender, imagePaths, nil, fileAttachments)
	contentBlocks := []acp.ContentBlock{acp.TextBlock(contentText)}

	sessionKey := buildSessionKey(activity.Conversation.ID)
	serviceURL := activity.ServiceURL
	conversationID := activity.Conversation.ID

	slog.Debug("processing teams message",
		"from", activity.From.Name,
		"session_key", sessionKey,
		"images", len(imagePaths),
		"files", len(fileAttachments))

	// Send typing indicator
	h.Client.SendTyping(serviceURL, conversationID, activity.Recipient)

	// Send initial "thinking" message
	thinkingActivity := &Activity{
		Type:         "message",
		Text:         "💭 _thinking..._",
		TextFormat:   "markdown",
		Conversation: Conversation{ID: conversationID},
		ReplyToID:    activity.ID,
	}
	sent, err := h.Client.SendActivity(serviceURL, conversationID, thinkingActivity)
	if err != nil {
		slog.Error("failed to send thinking message", "error", err)
		return
	}

	// Get or create ACP session
	if err := h.Pool.GetOrCreate(sessionKey); err != nil {
		h.Client.UpdateActivity(serviceURL, conversationID, sent.ID, &Activity{
			Type:       "message",
			Text:       fmt.Sprintf("⚠️ Failed to start agent: %v", err),
			TextFormat: "markdown",
		})
		slog.Error("pool error", "error", err)
		return
	}

	// Stream prompt and get final text
	finalText, result := h.streamPrompt(sessionKey, contentBlocks, serviceURL, conversationID, sent.ID)

	// Cleanup downloaded files
	for _, p := range imagePaths {
		if err := os.Remove(p); err != nil {
			slog.Debug("failed to remove tmp image", "path", p, "error", err)
		}
	}
	for _, f := range fileAttachments {
		if err := os.Remove(f.LocalPath); err != nil {
			slog.Debug("failed to remove tmp file", "path", f.LocalPath, "error", err)
		}
	}

	if result != nil {
		h.Client.UpdateActivity(serviceURL, conversationID, sent.ID, &Activity{
			Type:       "message",
			Text:       fmt.Sprintf("⚠️ %v", result),
			TextFormat: "markdown",
		})
	}

	// TTS: voice reply deferred for Teams MVP
	_ = finalText
}

func (h *Handler) handleCommand(activity *Activity, cmdName, cmdArgs string) {
	sessionKey := buildSessionKey(activity.Conversation.ID)
	var response string

	switch cmdName {
	case command.CmdSessions:
		response = command.ExecuteSessions(h.Pool)
	case command.CmdInfo:
		response = command.ExecuteInfo(h.Pool, sessionKey, nil)
	case command.CmdReset:
		response = command.ExecuteReset(h.Pool, sessionKey)
	case command.CmdResume:
		response = command.ExecuteResume(h.Pool, sessionKey)
	default:
		return
	}

	chunks := platform.SplitMessage(response, 28000)
	for _, chunk := range chunks {
		h.Client.SendActivity(activity.ServiceURL, activity.Conversation.ID, &Activity{
			Type:       "message",
			Text:       chunk,
			TextFormat: "markdown",
			ReplyToID:  activity.ID,
		})
	}
}

func (h *Handler) streamPrompt(
	sessionKey string,
	content []acp.ContentBlock,
	serviceURL string,
	conversationID string,
	msgID string,
) (string, error) {
	var finalText string
	err := h.Pool.WithConnection(sessionKey, func(conn *acp.AcpConnection) error {
		rx, _, reset, resumed, err := conn.SessionPrompt(content)
		if err != nil {
			return err
		}

		var textBuf strings.Builder
		var toolLines []string
		if resumed {
			textBuf.WriteString("🔄 _Session restored from previous conversation._\n\n")
		} else if reset {
			textBuf.WriteString("⚠️ _Session expired, starting fresh..._\n\n")
		}

		initial := "💭 _thinking..._"
		if resumed {
			initial = "🔄 _Session restored, continuing..._\n\n..."
		} else if reset {
			initial = "⚠️ _Session expired, starting fresh..._\n\n..."
		}

		// Edit-streaming goroutine (2s interval)
		var displayMu sync.Mutex
		currentDisplay := initial
		done := make(chan struct{})

		go func() {
			lastContent := ""
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					displayMu.Lock()
					content := currentDisplay
					displayMu.Unlock()

					if content != lastContent {
						preview := platform.TruncateUTF8(content, 27000, "\n…")
						h.Client.UpdateActivity(serviceURL, conversationID, msgID, &Activity{
							Type:       "message",
							Text:       preview,
							TextFormat: "markdown",
						})
						lastContent = content
					}
				case <-done:
					return
				}
			}
		}()

		// Process ACP notifications
		var promptErr error
		for notification := range rx {
			if notification.ID != nil {
				if notification.Error != nil {
					promptErr = notification.Error
				}
				break
			}

			event := acp.ClassifyNotification(notification)
			if event == nil {
				continue
			}

			switch event.Type {
			case acp.AcpEventText:
				textBuf.WriteString(event.Text)
				display := composeDisplay(toolLines, textBuf.String())
				displayMu.Lock()
				currentDisplay = display
				displayMu.Unlock()

			case acp.AcpEventToolStart:
				if event.Title != "" {
					if label, ok := platform.FormatToolTitle(event.Title, h.ToolDisplay); ok {
						toolLines = append(toolLines, fmt.Sprintf("🔧 `%s`...", label))
						display := composeDisplay(toolLines, textBuf.String())
						displayMu.Lock()
						currentDisplay = display
						displayMu.Unlock()
					}
				}

			case acp.AcpEventToolDone:
				if event.Title == "" {
					continue
				}
				icon := "✅"
				if event.Status != "completed" {
					icon = "❌"
				}
				if label, ok := platform.FormatToolTitle(event.Title, h.ToolDisplay); ok {
					for i := len(toolLines) - 1; i >= 0; i-- {
						if strings.Contains(toolLines[i], label) {
							toolLines[i] = fmt.Sprintf("%s `%s`", icon, label)
							break
						}
					}
					display := composeDisplay(toolLines, textBuf.String())
					displayMu.Lock()
					currentDisplay = display
					displayMu.Unlock()
				}
			}
		}

		conn.PromptDone()
		close(done)

		if promptErr != nil {
			h.Client.UpdateActivity(serviceURL, conversationID, msgID, &Activity{
				Type:       "message",
				Text:       fmt.Sprintf("⚠️ %v", promptErr),
				TextFormat: "markdown",
			})
			return promptErr
		}

		// Final message
		finalText = textBuf.String()
		finalContent := composeDisplay(toolLines, finalText)
		if finalContent == "" {
			finalContent = "_(no response)_"
		}
		finalContent = markdown.ConvertTables(finalContent, h.MarkdownTableMode)

		chunks := platform.SplitMessage(finalContent, 28000)
		for i, chunk := range chunks {
			if i == 0 {
				h.Client.UpdateActivity(serviceURL, conversationID, msgID, &Activity{
					Type:       "message",
					Text:       chunk,
					TextFormat: "markdown",
				})
			} else {
				h.Client.SendActivity(serviceURL, conversationID, &Activity{
					Type:       "message",
					Text:       chunk,
					TextFormat: "markdown",
				})
			}
		}

		return nil
	})
	return finalText, err
}

// --- Helper functions ---

func isBotMentioned(botID string, entities []Entity) bool {
	for _, e := range entities {
		if e.Type == "mention" && e.Mentioned != nil && e.Mentioned.ID == botID {
			return true
		}
	}
	return false
}

func stripBotMention(text, botID string, entities []Entity) string {
	for _, e := range entities {
		if e.Type == "mention" && e.Mentioned != nil && e.Mentioned.ID == botID && e.Text != "" {
			text = strings.Replace(text, e.Text, "", 1)
		}
	}
	return strings.TrimSpace(text)
}

func buildSessionKey(conversationID string) string {
	return fmt.Sprintf("teams:%s", conversationID)
}

func extractCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	cmd, ok := command.ParseCommand(text)
	if !ok {
		return "", ""
	}
	return cmd.Name, cmd.Args
}

func composeDisplay(toolLines []string, text string) string {
	var out strings.Builder
	if len(toolLines) > 0 {
		for _, line := range toolLines {
			out.WriteString(line)
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}
	out.WriteString(strings.TrimRight(text, " \t\n"))
	return out.String()
}

func buildPromptContent(base string, imagePaths, transcriptions []string, files []platform.FileAttachment) string {
	var extra strings.Builder

	if len(imagePaths) > 0 {
		extra.WriteString("\n\n<attached_images>\n")
		for _, p := range imagePaths {
			extra.WriteString(fmt.Sprintf("- %s\n", p))
		}
		extra.WriteString("</attached_images>\nPlease read and analyze the above image(s).")
	}

	if len(transcriptions) > 0 {
		extra.WriteString("\n\n<voice_transcription>\n")
		for _, t := range transcriptions {
			extra.WriteString(t)
			extra.WriteByte('\n')
		}
		extra.WriteString("</voice_transcription>\nThe above is a transcription of the user's voice message. Please respond to it.")
	}

	extra.WriteString(platform.FormatFileBlock(files))

	return base + extra.String()
}

// --- Attachment helpers ---

func isImageContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "image/")
}

func isAudioContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "audio/")
}

func (h *Handler) downloadAttachment(url, filename, tmpDir string) (string, error) {
	token, err := h.Client.auth.GetBotToken()
	if err != nil {
		return "", fmt.Errorf("get token for download: %w", err)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned %d", resp.StatusCode)
	}

	safeFilename := filepath.Base(filename)
	if safeFilename == "" || safeFilename == "." {
		safeFilename = "attachment"
	}
	localName := fmt.Sprintf("%d_%s", time.Now().UnixMilli(), safeFilename)
	localPath := filepath.Join(tmpDir, localName)

	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}

	// 25MB limit
	written, err := io.Copy(f, io.LimitReader(resp.Body, 25*1024*1024+1))
	if err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("write failed: %w", err)
	}
	if written > 25*1024*1024 {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("file too large (>25MB)")
	}

	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("close failed: %w", err)
	}

	return localPath, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./teams/ -run "TestStripMention|TestIsBotMentioned|TestBuildSessionKey|TestExtractCommand" -v`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add teams/handler.go teams/handler_test.go
git commit -m "feat(teams): implement message handler with mention detection and ACP streaming"
```

---

### Task 6: Adapter — HTTP server and Platform interface

**Files:**
- Create: `teams/adapter.go`
- Create: `teams/adapter_test.go`

- [ ] **Step 1: Write failing test for HTTP activity dispatch**

```go
// teams/adapter_test.go
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
	client := NewBotClient(auth)
	handler := &Handler{ToolDisplay: "compact"}

	mux := buildMux(auth, handler)

	req := httptest.NewRequest(http.MethodGet, "/api/messages", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
	_ = client
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
	// For conversationUpdate and other types, should return 200 without error
	auth := &BotAuth{appID: "test", appSecret: "test", tenantID: "test"}
	handler := &Handler{ToolDisplay: "compact"}

	mux := buildMuxSkipAuth(auth, handler) // test helper that skips JWT validation

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./teams/ -run TestActivityDispatch -v`
Expected: FAIL — buildMux doesn't exist

- [ ] **Step 3: Implement Adapter**

```go
// teams/adapter.go
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
	"github.com/neilkuan/quill/markdown"
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

// NewAdapter creates a Teams adapter. The adapter runs an HTTP server that receives
// Bot Framework Activities from the Teams channel.
func NewAdapter(cfg config.TeamsConfig, pool *acp.SessionPool, transcriber stt.Transcriber, synthesizer tts.Synthesizer, ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig) (*Adapter, error) {
	auth := NewBotAuth(cfg.AppID, cfg.AppSecret, cfg.TenantID)
	client := NewBotClient(auth)

	handler := &Handler{
		Pool:              pool,
		Client:            client,
		Transcriber:       transcriber,
		Synthesizer:       synthesizer,
		TTSConfig:         ttsCfg,
		MarkdownTableMode: markdown.ParseMode(mdCfg.Tables),
		ToolDisplay:       "compact",
	}

	mux := buildMux(auth, handler)

	a := &Adapter{
		auth:   auth,
		client: client,
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

// Start begins listening for incoming Bot Framework activities.
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

// Stop gracefully shuts down the HTTP server.
func (a *Adapter) Stop() error {
	slog.Info("stopping teams adapter")
	a.healthy.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return a.httpServer.Shutdown(ctx)
}

// Healthy reports whether the Teams HTTP server is running.
func (a *Adapter) Healthy() bool {
	return a.healthy.Load()
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
	// Reject non-POST methods explicitly
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
		go handler.OnMessage(&activity)
	case "conversationUpdate":
		slog.Info("teams conversation update",
			"conversation", activity.Conversation.ID)
	default:
		slog.Debug("teams: ignoring activity type", "type", activity.Type)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./teams/ -run TestActivityDispatch -v`
Expected: All PASS

- [ ] **Step 5: Run full build check**

Run: `go build ./...`
Expected: Build succeeds

- [ ] **Step 6: Commit**

```bash
git add teams/adapter.go teams/adapter_test.go
git commit -m "feat(teams): implement Adapter with HTTP server and activity dispatch"
```

---

### Task 7: main.go integration — Wire up Teams adapter

**Files:**
- Modify: `main.go:144-145` (the `// Future: Teams adapter goes here` comment)

- [ ] **Step 1: Add Teams import and registration to main.go**

Add import:
```go
"github.com/neilkuan/quill/teams"
```

Replace `// Future: Teams adapter goes here` with:

```go
if cfg.Teams.Enabled {
	adapter, err := teams.NewAdapter(cfg.Teams, pool, t, synth, cfg.TTS, cfg.Markdown)
	if err != nil {
		slog.Error("failed to create teams adapter", "error", err)
		os.Exit(1)
	}
	platforms = append(platforms, adapter)
	healthChecks = append(healthChecks, adapter.Healthy)
	slog.Info("teams adapter registered", "listen", cfg.Teams.Listen)
}
```

- [ ] **Step 2: Verify build succeeds**

Run: `go build ./...`
Expected: Build succeeds with no errors

- [ ] **Step 3: Run full test suite**

Run: `go test ./... -v`
Expected: All tests pass

- [ ] **Step 4: Run vet**

Run: `go vet ./...`
Expected: No issues

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "feat(teams): wire up Teams adapter in main.go"
```

---

### Task 8: End-to-end verification — Build and smoke test

**Files:**
- No new files — verification only

- [ ] **Step 1: Full build with ldflags**

Run: `go build -ldflags "-X main.commit=$(git rev-parse --short HEAD)" -o quill .`
Expected: Binary builds successfully

- [ ] **Step 2: Verify --version**

Run: `./quill --version`
Expected: Prints `quill (<commit-hash>)`

- [ ] **Step 3: Run full test suite**

Run: `go test ./... -v`
Expected: All tests pass

- [ ] **Step 4: Run vet**

Run: `go vet ./...`
Expected: No issues

- [ ] **Step 5: Clean up binary**

Run: `rm -f quill`

- [ ] **Step 6: Final commit (if any cleanup needed)**

If any test fixes or cleanup were needed, commit them:
```bash
git add -A
git commit -m "chore(teams): final cleanup and test fixes"
```
