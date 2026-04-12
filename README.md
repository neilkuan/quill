# openab-go

[繁體中文](README-zh-tw.md) | English

A lightweight, secure, cloud-native **ACP (Agent Client Protocol) bridge** that connects **Discord** and **Telegram** with any ACP-compatible coding CLI — [Kiro CLI](https://kiro.dev), [Claude Code](https://docs.anthropic.com/en/docs/claude-code), [Codex](https://github.com/openai/codex), [Gemini CLI](https://github.com/google-gemini/gemini-cli), and more.

This is a **Go rewrite** of [openab](https://github.com/openabdev/openab) (originally in Rust).

---

##### Features

- **Pluggable agent backends** — Kiro, Claude Code, Codex, Gemini (any ACP-compatible CLI)
- **Discord integration** — @mention triggers, auto thread creation, multi-turn conversations
- **Telegram integration** — @mention / reply-to-bot in groups, private chat, voice auto-accepted in groups, forum topic support (one session per topic)
- **Voice message transcription** — transcribes voice messages via OpenAI Whisper API (Discord & Telegram)
- **Real-time edit streaming** — updates messages as the agent works (Discord: 1.5s, Telegram: 2s)
- **Emoji status reactions** — processing progress via platform-native reactions
- **Session pool** — one CLI process per thread/chat, automatic lifecycle management
- **Session management** — bot commands (`sessions`/`reset`/`info`), LRU eviction, HTTP API for monitoring
- **ACP protocol** — JSON-RPC over stdio
- **Kubernetes ready** — includes Dockerfile for containerized deployment

---

##### Pluggable Agent Backends

Supports Kiro CLI, Claude Code, Codex, Gemini, and any ACP-compatible CLI.

| Agent key | CLI | ACP Adapter | Auth |
|---|---|---|---|
| `kiro` (default) | Kiro CLI | Native `kiro-cli acp` | `kiro-cli login --use-device-flow` |
| `codex` | Codex | [@zed-industries/codex-acp](https://github.com/zed-industries/codex-acp) | `codex login --device-auth` |
| `claude` | Claude Code | [@agentclientprotocol/claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp) | `claude auth login` or `claude setup-token` |
| `gemini` | Gemini CLI | Native `gemini --acp` | Google OAuth or `GEMINI_API_KEY` |

---

##### Platform Support

| Platform | Text | Image | Voice | Status |
|----------|------|-------|-------|--------|
| Discord  | ✅   | ✅    | ✅    | Available |
| Telegram | ✅   | ✅    | ✅    | Available |
| Teams    | —    | —     | —     | Planned |

---

##### Quick Start

```bash
# Clone
git clone https://github.com/neilkuan/openab-go.git
cd openab-go

# Copy and edit config
cp config.toml.example config.toml
# Edit config.toml with your Discord bot token and channel IDs

# Run
go run . config.toml
```

##### Configuration

Configuration uses TOML with environment variable expansion (`${VAR_NAME}` syntax):

```toml
[discord]
bot_token = "${DISCORD_BOT_TOKEN}"
allowed_channels = ["1234567890"]

[telegram]
bot_token = "${TELEGRAM_BOT_TOKEN}"
allowed_chats = [-100123456789]

[agent]
command = "kiro-cli"
args = ["acp", "--trust-all-tools"]
working_dir = "/home/agent"

[pool]
max_sessions = 10
session_ttl_hours = 24

[discord.reactions]
enabled = true
remove_after_reply = false
```

##### Voice Transcription (Optional)

To enable voice message support, add a `[transcribe]` section with an OpenAI API key:

```toml
[transcribe]
api_key = "${OPENAI_API_KEY}"
# provider = "openai"       # default
# model = "whisper-1"       # default
# language = "zh"           # ISO-639-1 code, default "zh"
# prompt = "以下是繁體中文語音的逐字稿："  # hint for Traditional Chinese output
```

When configured, voice messages (Discord & Telegram) are automatically transcribed and sent to the agent as text. Without this config, voice-only messages return a warning to the user.

See [`config.toml.example`](config.toml.example) for the full reference including alternative agents (Claude, Codex, Gemini).

---

##### Session Management

Built-in bot commands and HTTP API for managing agent sessions.

###### Bot Commands

Commands are registered as native platform commands — Discord Slash Commands and Telegram BotCommands — so they appear in the `/` autocomplete menu. Plain text (e.g., `@bot sessions`) is also supported as fallback.

| Command | Description |
|---------|-------------|
| `/sessions` | List all active sessions with stats |
| `/info` | Show current thread/chat session details |
| `/reset` | Kill current session (new one on next message) |

###### HTTP API (Optional)

Enable in config:

```toml
[api]
enabled = true
listen = ":8080"
```

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/health` | GET | Health check with pool stats |
| `/api/sessions` | GET | List all sessions as JSON |
| `/api/sessions/{key}` | DELETE | Kill a specific session |

###### Pool Behavior

- **LRU eviction** — when pool is full, the least recently used session is evicted automatically
- **TTL cleanup** — idle sessions are cleaned up after `session_ttl_hours` (default: 24h)
- **Per-session stats** — created time, last active, message count

---

##### Discord vs Telegram

| | Discord | Telegram |
|---|---|---|
| **Trigger (channel/group)** | @mention or in-thread | @mention, reply-to-bot, or voice message |
| **Trigger (DM/private)** | — | All messages |
| **Thread model** | Auto-creates Discord threads | One session per chat; forum supergroups get one session per topic |
| **Message limit** | 2,000 chars | 4,096 chars |
| **Edit streaming interval** | 1.5s | 2s (Telegram rate limit is stricter) |
| **Markdown** | Native GFM support | `**bold**` auto-converted to `*bold*` (Telegram Markdown v1) |
| **Status reactions** | Add/remove per emoji | `setMessageReaction` replaces all (one emoji at a time) |
| **Reaction emojis** | Queued `👀` → Thinking `🤔` → Done `🆗` + random face | Queued `👌` → Thinking `🤔` → Done = random face from allowed set |
| **Voice in groups** | Requires @mention | Auto-accepted (can't @mention while recording) |
| **Image handling** | Download from CDN by URL | Download via Bot API `getFile` (largest PhotoSize) |
| **Bot library** | [discordgo](https://github.com/bwmarrin/discordgo) | [go-telegram/bot](https://github.com/go-telegram/bot) |
| **Update mechanism** | WebSocket gateway | Long polling |

##### Telegram Setup Notes

1. Create a bot via [@BotFather](https://t.me/BotFather) and get the bot token
2. **Disable privacy mode** via BotFather (`/setprivacy` → Disable) so the bot receives @mentions in groups, then remove and re-add the bot to the group
3. Get the group chat ID: start the bot without `allowed_chats`, send a message in the group — the log will show `🚨👽🚨 telegram message from unlisted chat ... chat_id=XXXXX`
4. Add the `chat_id` to `allowed_chats` in your config

---

##### Docker

Four image variants are published for each release:

| Image | Agent |
|---|---|
| `ghcr.io/neilkuan/openab-go` | Kiro CLI |
| `ghcr.io/neilkuan/openab-go-claude` | Claude Code |
| `ghcr.io/neilkuan/openab-go-codex` | Codex |
| `ghcr.io/neilkuan/openab-go-gemini` | Gemini CLI |

```bash
docker run -v $(pwd)/config.toml:/etc/openab-go/config.toml \
  ghcr.io/neilkuan/openab-go:latest
```

---

##### Development

###### Prerequisites

- Go 1.23+
- A Discord bot token with `MESSAGE_CONTENT` intent enabled, and/or a Telegram bot token
- An ACP-compatible CLI installed (e.g., `kiro-cli`, `claude`, `codex`, `gemini`)

###### Build

```bash
go build -o openab-go .

# with version info
go build -ldflags "-X main.version=$(cat VERSION)" -o openab-go .
```

###### Run with debug logging

```bash
OPENAB_GO_LOG=debug ./openab-go config.toml
```

###### Project Structure

```
openab-go/
├── main.go              # Entry point: config, platform registration, graceful shutdown
├── platform/
│   └── platform.go      # Platform interface, shared message splitting
├── config/
│   └── config.go        # TOML config parsing, env var expansion, validation
├── acp/
│   ├── protocol.go      # JSON-RPC types, ACP event classification
│   ├── connection.go    # Child process management, stdio JSON-RPC, auto-permission
│   └── pool.go          # Session pool: get-or-create, LRU eviction, idle cleanup
├── command/
│   └── command.go       # Bot command parsing and execution (sessions/reset/info)
├── api/
│   └── server.go        # HTTP API server for session monitoring
├── transcribe/
│   └── transcribe.go    # Transcriber interface, OpenAI Whisper implementation
├── discord/
│   ├── adapter.go       # Discord platform adapter (implements Platform interface)
│   ├── handler.go       # Discord message handler, thread creation, edit streaming
│   └── reactions.go     # Status reaction controller: debounce, stall detection
├── telegram/
│   ├── adapter.go       # Telegram platform adapter (implements Platform interface)
│   ├── handler.go       # Telegram message handler, mention/reply detection, edit streaming
│   └── reactions.go     # Telegram reaction controller via setMessageReaction API
├── scripts/
│   └── release.sh       # Release automation (stable PR + RC tags)
├── Dockerfile           # Kiro CLI variant
├── Dockerfile.claude    # Claude Code variant
├── Dockerfile.codex     # Codex variant
├── Dockerfile.gemini    # Gemini CLI variant
├── config.toml.example  # Configuration reference
├── VERSION              # Semver version
└── RELEASING.md         # Release workflow documentation
```

###### Key Design Decisions

| Aspect | Choice | Why |
|---|---|---|
| Language | Go | Fast compile, single static binary, goroutine concurrency |
| Discord lib | [discordgo](https://github.com/bwmarrin/discordgo) | De facto Go Discord library |
| Telegram lib | [go-telegram/bot](https://github.com/go-telegram/bot) | Actively maintained, native forum topic support |
| Config format | TOML | Human-readable, same as original Rust version |
| Logging | `log/slog` (stdlib) | Zero dependency, structured logging |
| Concurrency | goroutines + `sync.Mutex` / `sync.RWMutex` | Idiomatic Go, no external async runtime needed |

---

##### Releasing

Releases follow a **"what you tested is what you ship"** philosophy using `scripts/release.sh`:

1. **Merge PRs to main** → `release.yml` auto-opens a Release PR (`release/v0.4.1`, only bumps `VERSION`)
2. **Create RC tag** → checkout release branch → `./scripts/release.sh --rc` → full build of 4 image variants x 2 platforms
3. **Merge the Release PR** → `tag-on-merge.yml` auto-tags stable → promotes pre-release images (no rebuild)

See [RELEASING.md](RELEASING.md) for the full workflow.

---

##### License

[MIT](LICENSE)
