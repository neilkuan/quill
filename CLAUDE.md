# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o openab-go .
./openab-go                    # uses config.toml in cwd
./openab-go /path/to/config.toml  # custom config path
./openab-go --version          # print version
OPENAB_GO_LOG=debug ./openab-go   # enable debug logging

# inject version at build time
go build -ldflags "-X main.version=0.1.0" -o openab-go .
```

## Development Commands

```bash
go build ./...        # compile all packages
go vet ./...          # static analysis
go test ./...         # run all tests
go test ./acp/...     # run tests for a single package
go test -v -run TestClassifyNotification ./acp/...  # run a specific test
```

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

- **`platform/`** — Defines the `Platform` interface (`Start`/`Stop`) that every chat adapter must implement. Contains shared utilities like `SplitMessage` (used by all platforms for message-size splitting).

- **`acp/`** — Agent Communication Protocol implementation (platform-agnostic)
  - `connection.go` — Spawns an agent subprocess, manages stdin/stdout JSON-RPC communication, handles request/response correlation via pending map, auto-approves `session/request_permission` requests, streams notifications via a subscriber channel
  - `pool.go` — `SessionPool` maps thread keys to `AcpConnection` instances with max-session limits and TTL-based idle cleanup
  - `protocol.go` — JSON-RPC message types and ACP event classification (`ClassifyNotification` parses session updates into typed events: text chunks, thinking, tool calls, status)

- **`discord/`** — Discord platform adapter
  - `adapter.go` — `Adapter` struct implementing `platform.Platform`, wraps discordgo session lifecycle
  - `handler.go` — Message handler: mention/thread detection, sender context injection, streaming prompt responses with live message editing
  - `reactions.go` — `StatusReactionController` state machine for emoji-based status indicators (queued → thinking → tool → done/error) with debounce and stall detection

- **`config/`** — TOML configuration with `${ENV_VAR}` expansion and sensible defaults. Platform-specific configs are nested (`discord.reactions.emojis`, etc.)

- **`main.go`** — Loads config, creates session pool, registers all enabled `platform.Platform` adapters, starts them, runs idle-session cleanup, and handles graceful shutdown

### Adding a new platform

1. Create a new package (e.g., `telegram/`) with `adapter.go` implementing `platform.Platform`
2. Add config struct to `config/config.go` (stubs for Telegram/Teams already exist)
3. Register in `main.go`: `if cfg.Telegram.Enabled { platforms = append(platforms, ...) }`
4. `acp/` and `platform/` require no changes

### ACP lifecycle (per thread)

1. `SessionPool.GetOrCreate(threadKey)` — spawns agent process if needed
2. `AcpConnection.Initialize()` → `session/new` → creates a session
3. `AcpConnection.SessionPrompt(text)` → sends `session/prompt`, returns a notification channel for streaming
4. Caller reads notifications (text chunks, tool events) until response with matching ID arrives
5. `AcpConnection.PromptDone()` — clears notification subscriber

### Configuration

Config is TOML format. The `agent.command` and `agent.args` fields define what subprocess to spawn (e.g., `claude` CLI). Environment variables in config values use `${VAR}` syntax and are expanded at load time. Having a `bot_token` in a platform section auto-enables that platform.
