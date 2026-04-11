# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o openab-go .
./openab-go                    # uses config.toml in cwd
./openab-go /path/to/config.toml  # custom config path
./openab-go --version          # print version
OPENAB_GO_LOG=debug ./openab-go   # enable debug logging

# inject commit hash at build time (main.go uses `commit` variable)
go build -ldflags "-X main.commit=$(git rev-parse --short HEAD)" -o openab-go .
```

## Development Commands

```bash
go build ./...        # compile all packages
go vet ./...          # static analysis
go test ./...         # run all tests
go test ./acp/...     # run tests for a single package
go test -v -run TestClassifyNotification ./acp/...  # run a specific test
```

CI runs `go build ./...`, `go vet ./...`, and `go test ./... -v` on every PR and push to main.

## Docker

Four Dockerfile variants exist for different agent backends:

| Dockerfile | Agent binary | Base image |
|---|---|---|
| `Dockerfile` | `kiro-cli` | `debian:bookworm-slim` |
| `Dockerfile.claude` | `claude-agent-acp` | `node:22-bookworm-slim` |
| `Dockerfile.codex` | `codex-acp` | `node:22-bookworm-slim` |
| `Dockerfile.gemini` | `gemini` | `node:22-bookworm-slim` |

All use multi-stage builds (Go builder → runtime) and accept a `COMMIT` build arg.

## Architecture

openab-go is a multi-platform chat bot that proxies user messages to an AI agent process via the **Agent Communication Protocol (ACP)** — a JSON-RPC 2.0 protocol over stdin/stdout.

### Data flow

```
Chat message → Platform Adapter → SessionPool → AcpConnection (stdin/stdout) → Agent process
(Discord/     (implements          (platform-     (JSON-RPC 2.0)
 Telegram/     platform.Platform)   agnostic)
 Teams)
```

### Key packages

- **`platform/`** — Defines the `Platform` interface (`Start`/`Stop`) that every chat adapter must implement. Contains shared utilities like `SplitMessage` (used by all platforms for message-size splitting) and `TruncateUTF8`.

- **`acp/`** — Agent Communication Protocol implementation (platform-agnostic)
  - `connection.go` — Spawns an agent subprocess, manages stdin/stdout JSON-RPC communication, handles request/response correlation via pending map, auto-approves `session/request_permission` requests, streams notifications via a subscriber channel. `promptMu` serializes prompts — only one at a time per connection.
  - `pool.go` — `SessionPool` maps thread keys to `AcpConnection` instances with max-session limits and TTL-based idle cleanup. Uses double-checked locking (RLock → RUnlock → Lock) for GetOrCreate.
  - `protocol.go` — JSON-RPC message types and ACP event classification (`ClassifyNotification` parses session updates into typed events: text chunks, thinking, tool calls, status)

- **`discord/`** — Discord platform adapter
  - `adapter.go` — `Adapter` struct implementing `platform.Platform`, wraps discordgo session lifecycle
  - `handler.go` — Message handler: mention/thread detection, sender context injection (`openab.sender.v1` schema), image attachment download to `.tmp/`, streaming prompt responses with a background edit-streaming goroutine (1.5s ticker, truncates to 1900 chars during streaming, splits into multiple messages only on final edit)
  - `reactions.go` — `StatusReactionController` state machine for emoji-based status indicators (queued → thinking → tool → done/error) with debounce, stall detection (soft 🥱 / hard 😨), and tool classification (coding/web/generic)

- **`config/`** — TOML configuration with `${ENV_VAR}` expansion and sensible defaults. Platform-specific configs are nested (`discord.reactions.emojis`, etc.). Having a `bot_token` in a platform section auto-enables that platform.

- **`main.go`** — Loads config, creates session pool, registers all enabled `platform.Platform` adapters, starts them, runs idle-session cleanup (60s tick), and handles graceful shutdown via SIGINT/SIGTERM.

### Adding a new platform

1. Create a new package (e.g., `telegram/`) with `adapter.go` implementing `platform.Platform`
2. Add config struct to `config/config.go` (stubs for Telegram/Teams already exist)
3. Register in `main.go`: `if cfg.Telegram.Enabled { platforms = append(platforms, ...) }`
4. `acp/` and `platform/` require no changes

### ACP lifecycle (per thread)

1. `SessionPool.GetOrCreate(threadKey)` — spawns agent process if needed
2. `AcpConnection.Initialize()` → `initialize` → handshake
3. `AcpConnection.SessionNew(cwd)` → `session/new` → creates a session
4. `AcpConnection.SessionPrompt(content)` → sends `session/prompt`, returns a notification channel for streaming
5. Caller reads notifications (text chunks, tool events) until response with matching ID arrives
6. `AcpConnection.PromptDone()` — clears notification subscriber, releases prompt lock

### Releasing

Releases use [tagpr](https://github.com/Songmu/tagpr). Version lives in `VERSION` file. Pre-release tags (`v0.2.1-rc.1`) trigger CI image builds; merging the tagpr Release PR promotes the tested image without rebuilding. Images are published to GHCR in four variants (see Docker section).
