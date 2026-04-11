# openab-go

A lightweight, secure, cloud-native **ACP (Agent Client Protocol) bridge** that connects Discord with any ACP-compatible coding CLI — [Kiro CLI](https://kiro.dev), [Claude Code](https://docs.anthropic.com/en/docs/claude-code), [Codex](https://github.com/openai/codex), [Gemini CLI](https://github.com/google-gemini/gemini-cli), and more.

This is a **Go rewrite** of [openab](https://github.com/neilkuan/openab) (originally in Rust).

---

##### Features

- **Pluggable agent backends** — Kiro, Claude Code, Codex, Gemini (any ACP-compatible CLI)
- **Discord integration** — @mention triggers, auto thread creation, multi-turn conversations
- **Voice message transcription** — transcribes Discord voice messages via OpenAI Whisper API
- **Real-time edit streaming** — updates Discord messages every 1.5s as the agent works
- **Emoji status reactions** — `👀→🤔→🔥/👨‍💻/⚡→🆗` showing processing progress
- **Session pool** — one CLI process per thread, automatic lifecycle management
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
| Telegram | —    | —     | —     | Planned |
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

When configured, Discord voice messages are automatically transcribed and sent to the agent as text. Without this config, voice messages are skipped with a warning log.

See [`config.toml.example`](config.toml.example) for the full reference including alternative agents (Claude, Codex, Gemini).

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
- A Discord bot token with `MESSAGE_CONTENT` intent enabled
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
│   └── pool.go          # Session pool: get-or-create, idle cleanup, shutdown
├── transcribe/
│   └── transcribe.go    # Transcriber interface, OpenAI Whisper implementation
├── discord/
│   ├── adapter.go       # Discord platform adapter (implements Platform interface)
│   ├── handler.go       # Discord message handler, thread creation, edit streaming
│   └── reactions.go     # Status reaction controller: debounce, stall detection
├── Dockerfile           # Kiro CLI variant
├── Dockerfile.claude    # Claude Code variant
├── Dockerfile.codex     # Codex variant
├── Dockerfile.gemini    # Gemini CLI variant
├── config.toml.example  # Configuration reference
├── VERSION              # Semver version (managed by tagpr)
└── RELEASING.md         # Release workflow documentation
```

###### Key Design Decisions

| Aspect | Choice | Why |
|---|---|---|
| Language | Go | Fast compile, single static binary, goroutine concurrency |
| Discord lib | [discordgo](https://github.com/bwmarrin/discordgo) | De facto Go Discord library |
| Config format | TOML | Human-readable, same as original Rust version |
| Logging | `log/slog` (stdlib) | Zero dependency, structured logging |
| Concurrency | goroutines + `sync.Mutex` / `sync.RWMutex` | Idiomatic Go, no external async runtime needed |

---

##### Releasing

Releases use [tagpr](https://github.com/Songmu/tagpr) with a **"what you tested is what you ship"** philosophy:

1. **Merge PRs to main** → tagpr auto-opens a Release PR (bumps `VERSION`)
2. **Tag a pre-release** (`v0.2.0-rc.1`) → full build of 4 image variants x 2 platforms
3. **Merge the Release PR** → tagpr tags stable (`v0.2.0`) → promotes pre-release images (no rebuild)

See [RELEASING.md](RELEASING.md) for the full workflow.

---

##### License

[MIT](LICENSE)
