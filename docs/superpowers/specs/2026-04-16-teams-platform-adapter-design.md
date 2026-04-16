# Microsoft Teams Platform Adapter Design

**Date:** 2026-04-16
**Status:** Approved
**Author:** Neil Kuan

## Overview

Add Microsoft Teams as a third platform adapter for Quill, following the same architecture pattern as the existing Discord and Telegram adapters. Users will @mention the bot in a Teams channel, and the bot responds in the same thread via the Azure Bot Framework protocol.

## Requirements

- Bidirectional chat in Teams channels via @mention trigger
- Reply in the same thread (reply chain)
- Full attachment support: images + files (audio/voice deferred)
- Typing indicator (no emoji reactions)
- Tenant-level access control for MVP (fine-grained `allowed_channels` / `allowed_user_id` deferred)
- Shared bot commands: `/sessions`, `/info`, `/reset`, `/resume`
- Deployment: cloud with public ingress (k8s / VM / container)

## Architecture

### Data Flow

```
Teams user @mentions bot
  -> Microsoft Bot Framework Service
    -> POST /api/messages (Activity JSON)
      -> teams/adapter.go (HTTP server)
        -> teams/auth.go (JWT validation)
          -> teams/handler.go
            -> SessionPool -> AcpConnection -> Agent process
              -> Reply via Bot Framework REST API -> Teams
```

### File Structure

```
teams/
  adapter.go   -- Platform interface, HTTP server lifecycle
  auth.go      -- Azure AD JWT inbound validation + OAuth2 outbound token
  client.go    -- Bot Framework REST API client (send, update, typing)
  handler.go   -- Message processing, mention detection, attachments, ACP streaming
```

### Integration Point (main.go)

```go
if cfg.Teams.Enabled {
    adapter, err := teams.NewAdapter(cfg.Teams, pool, t, synth, ttsCfg, mdCfg)
    platforms = append(platforms, adapter)
    healthChecks = append(healthChecks, adapter.Healthy)
}
```

## Config

Extend the existing `TeamsConfig` stub in `config/config.go`:

```go
type TeamsConfig struct {
    Enabled    bool   `toml:"enabled"`
    AppID      string `toml:"app_id"`       // Azure Bot Registration App ID
    AppSecret  string `toml:"app_secret"`   // Azure Bot Registration App Secret
    TenantID   string `toml:"tenant_id"`    // Azure AD Tenant ID
    Listen     string `toml:"listen"`       // HTTP listen address (default ":3978")
}
```

Auto-enable when both `app_id` and `app_secret` are set. Default `listen` to `:3978`.

TOML example:

```toml
[teams]
app_id     = "${TEAMS_APP_ID}"
app_secret = "${TEAMS_APP_SECRET}"
tenant_id  = "${TEAMS_TENANT_ID}"
listen     = ":3978"
```

## Auth (auth.go)

### BotAuth struct

```go
type BotAuth struct {
    appID     string
    appSecret string
    tenantID  string

    // Outbound token cache
    tokenMu     sync.Mutex
    token       string
    tokenExpiry time.Time

    // Inbound JWKS cache
    jwksMu     sync.Mutex
    jwksKeys   *jose.JSONWebKeySet
    jwksExpiry time.Time
}
```

### Inbound Validation (Teams -> Quill)

- Extract `Authorization: Bearer <JWT>` from request header
- Fetch OpenID metadata from `https://login.botframework.com/v1/.well-known/openidconfiguration`
- Validate JWT signature against JWKS public keys (cached ~24h)
- Validate claims: `iss` (issuer), `aud` (must match `app_id`), `serviceurl`
- Reject requests that fail validation with HTTP 401

### Outbound Token (Quill -> Teams)

- POST `https://login.microsoftonline.com/{tenant_id}/oauth2/v2.0/token`
- Body: `grant_type=client_credentials`, `client_id={app_id}`, `client_secret={app_secret}`, `scope=https://api.botframework.com/.default`
- Cache token until near expiry (refresh 5 min before `expires_in`)
- Thread-safe via mutex

### Dependencies

- `github.com/go-jose/go-jose/v4` — JWKS parsing and JWS verification
- `github.com/golang-jwt/jwt/v5` — JWT claims parsing and validation

## Client (client.go)

### BotClient struct

```go
type BotClient struct {
    auth *BotAuth
    http *http.Client
}
```

### Methods

- `SendActivity(serviceURL, conversationID string, activity *Activity) (*Activity, error)` — POST new message
- `UpdateActivity(serviceURL, conversationID, activityID string, activity *Activity) error` — PUT edit existing message
- `SendTyping(serviceURL, conversationID string, from Account) error` — Send typing indicator (from = bot account)

### API Endpoints

- New message: `POST {serviceUrl}/v3/conversations/{conversationId}/activities`
- Edit: `PUT {serviceUrl}/v3/conversations/{conversationId}/activities/{activityId}`
- All requests carry `Authorization: Bearer {botToken}`

## Handler (handler.go)

### Activity Types

```go
type Activity struct {
    Type         string       `json:"type"`         // "message", "conversationUpdate", "typing"
    ID           string       `json:"id"`
    Timestamp    string       `json:"timestamp"`
    ServiceURL   string       `json:"serviceUrl"`
    ChannelID    string       `json:"channelId"`    // "msteams"
    From         Account      `json:"from"`
    Conversation Conversation `json:"conversation"`
    Recipient    Account      `json:"recipient"`
    Text         string       `json:"text"`
    Attachments  []Attachment `json:"attachments"`
    Entities     []Entity     `json:"entities"`
    ReplyToID    string       `json:"replyToId"`
}

type Account struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

type Conversation struct {
    ID string `json:"id"`
}

type Attachment struct {
    ContentType string `json:"contentType"`
    ContentURL  string `json:"contentUrl"`
    Name        string `json:"name"`
    Content     any    `json:"content,omitempty"`
}

type Entity struct {
    Type      string   `json:"type"`
    Mentioned *Account `json:"mentioned,omitempty"`
    Text      string   `json:"text,omitempty"`
}
```

### Message Processing Flow

1. **@mention detection** — Scan `Entities` for type `"mention"` where `mentioned.id` matches bot's `Recipient.ID`. Strip `<at>...</at>` tags from `Text` to get clean prompt.
2. **Command detection** — Check if cleaned text matches a known command (`/sessions`, `/info`, `/reset`, `/resume`). Delegate to `command/` package.
3. **Sender context injection** — Inject `quill.sender.v1` schema (consistent with Discord/Telegram):
   ```json
   {
     "schema": "quill.sender.v1",
     "sender_id": "{from.id}",
     "sender_name": "{from.name}",
     "display_name": "{from.name}",
     "channel": "teams",
     "channel_id": "{conversation.id}"
   }
   ```
4. **Attachment download** — Images and files from `Activity.Attachments`:
   - Download from `contentUrl` with `Authorization: Bearer {botToken}`
   - Save to `.tmp/` directory (consistent with Discord/Telegram)
   - Classify by content type: image vs file
   - Audio attachments: log warning, skip (deferred)
5. **Session key** — Format: `teams:{conversationId}`. Teams conversation IDs include thread context, so the same thread maps to the same session.
6. **Typing indicator** — Send typing activity via `BotClient.SendTyping()` before starting ACP prompt.
7. **ACP streaming** — Same pattern as Discord/Telegram:
   - Send initial "thinking..." message via `BotClient.SendActivity()`
   - Background goroutine: 2s ticker, `BotClient.UpdateActivity()` with truncated content
   - Process ACP notifications (text chunks, tool events)
   - Final: `platform.SplitMessage()` with ~28KB limit, send multiple messages if needed
   - Apply markdown table conversion via `markdown.ConvertTables()`

### Session Key Format

```
teams:{conversationId}
```

Teams conversation IDs already encode thread context (e.g., `19:xxx@thread.tacv2;messageid=1234`), so no special thread handling needed.

## Adapter (adapter.go)

### Struct

```go
type Adapter struct {
    auth       *BotAuth
    client     *BotClient
    handler    *Handler
    httpServer *http.Server
    healthy    atomic.Bool
}
```

### NewAdapter

```go
func NewAdapter(cfg config.TeamsConfig, pool *acp.SessionPool,
    transcriber stt.Transcriber, synthesizer tts.Synthesizer,
    ttsCfg config.TTSConfig, mdCfg config.MarkdownConfig) (*Adapter, error)
```

- Create `BotAuth` with app_id, app_secret, tenant_id
- Pre-fetch bot token + JWKS to validate credentials on startup
- Create `BotClient`
- Create `Handler` (inject pool, client, auth, STT, TTS)
- Set up HTTP mux: `POST /api/messages` -> activity dispatch

### Start()

1. Start HTTP server in background goroutine
2. Set `healthy = true`
3. Log listen address

### Stop()

1. `httpServer.Shutdown(ctx)` with 5s timeout
2. Set `healthy = false`

### Healthy()

Return `healthy.Load()`

### HTTP Dispatch

```
POST /api/messages
  -> auth.ValidateInbound(r)
  -> json.Decode(body) -> Activity
  -> switch activity.Type:
      "message"            -> handler.OnMessage(activity)
      "conversationUpdate" -> log join/leave events
      _                    -> 200 OK (ignore)
  -> respond 200 OK
```

## Streaming Strategy

- Initial message: send "thinking..." via `SendActivity`, capture returned activity ID
- Background edit: 2s ticker, `UpdateActivity` with truncated preview
- Tool lines: same `composeDisplay` pattern as Discord (tool status lines + text)
- Final output: `markdown.ConvertTables()` + `platform.SplitMessage(text, 28000)`
- If split into multiple chunks: first chunk updates the initial message, subsequent chunks are new messages

## Deferred (Not in MVP)

- **Fine-grained access control**: `allowed_channels`, `allowed_user_id` (same pattern as Discord/Telegram)
- **Emoji reactions**: `StatusReactionController` with Teams reaction API
- **Voice/audio attachments**: STT transcription for Teams voice messages
- **Adaptive Cards**: Rich card formatting for bot responses
- **Proactive messaging**: Bot-initiated messages outside of reply flow
- **Shared HTTP server**: Merge Teams webhook endpoint into the existing `api/` server

## Azure Setup Prerequisites

Users need to:

1. Create an Azure Bot Registration in Azure Portal
2. Note the App ID and generate an App Secret
3. Configure the messaging endpoint to `https://{your-domain}/api/messages`
4. Enable the Microsoft Teams channel in the Bot Registration
5. Install the bot in the target Teams tenant/team
