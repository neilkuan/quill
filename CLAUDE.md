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
| `Dockerfile.copilot` | `copilot` (native ACP) | `node:22-bookworm-slim` |

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

### Voice data flow

```
User voice 🎤 → STT (Whisper) → text → Agent → text → TTS (OpenAI) → voice 🔊 → User
                [stt/]                                  [tts/]
```

- STT triggers on voice messages only; text messages skip STT
- TTS triggers only when user sent a voice message (voice-in → voice-out)
- Both STT and TTS are optional; each enabled independently via `[stt]` / `[tts]` config sections

### Key packages

- **`platform/`** — Defines the `Platform` interface (`Start`/`Stop`) that every chat adapter must implement. Contains shared utilities like `SplitMessage` (used by all platforms for message-size splitting), `TruncateUTF8`, and `FormatToolTitle` (renders ACP tool-call titles based on `reactions.tool_display` — `full` / `compact` / `none`; defaults to `compact` to keep streaming messages clean when agents like codex put full shell commands in tool titles).

- **`acp/`** — Agent Communication Protocol implementation (platform-agnostic)
  - `connection.go` — Spawns an agent subprocess, manages stdin/stdout JSON-RPC communication, handles request/response correlation via pending map, auto-approves `session/request_permission` requests, streams notifications via a subscriber channel. `promptMu` serializes prompts — only one at a time per connection.
  - `pool.go` — `SessionPool` maps thread keys (`discord:{id}` / `tg:{id}` / `tg:{id}:{threadID}`) to `AcpConnection` instances. LRU eviction when pool is full, TTL-based idle cleanup, and query methods (`ListSessions`, `GetSessionInfo`, `KillSession`, `Stats`). Uses double-checked locking (RLock → RUnlock → Lock) for GetOrCreate.
  - `protocol.go` — JSON-RPC message types and ACP event classification (`ClassifyNotification` parses session updates into typed events: text chunks, thinking, tool calls, status)

- **`command/`** — Shared bot command logic (`sessions`/`info`/`reset`/`setvoice`/`voice-clear`/`voicemode`). `ParseCommand` detects commands from text, `Execute*` functions format responses using pool query methods. `/info` displays session details including STT/TTS status and voice configuration. Used by both Discord and Telegram handlers.

- **`api/`** — Optional HTTP API server for session monitoring. Endpoints: `GET /api/sessions`, `DELETE /api/sessions/{key}`, `GET /api/health`. Enabled via `[api]` config section.

- **`discord/`** — Discord platform adapter
  - `adapter.go` — `Adapter` struct implementing `platform.Platform`, wraps discordgo session lifecycle. Parses `allowed_user_id` into an `AllowedUserIDs` map plus an `AllowAnyUser` flag (set when the list contains `"*"`).
  - `handler.go` — Message handler: mention/thread detection, sender context injection (`openab.sender.v1` schema), image attachment download to `.tmp/`, streaming prompt responses with a background edit-streaming goroutine (1.5s ticker, truncates to 1900 chars during streaming, splits into multiple messages only on final edit). Registers Discord Slash Commands (`/sessions`, `/info`, `/reset`, `/setvoice`, `/voice-clear`, `/voicemode`) on ready, handles interactions via `OnInteractionCreate`. Text command `setvoice` with audio attachment creates a custom voice via OpenAI API.
    - **Access gate**: `allowed_user_id` (matched against `m.Author.ID`, the numeric snowflake) takes precedence over `allowed_channels`; `"*"` wildcards any user. When unset, falls back to channel/thread gate.
    - **Mention detection** covers three cases: (a) `m.Mentions` for user mentions, (b) raw-content scan for `<@BOTID>` and legacy `<@!BOTID>`, (c) `m.MentionRoles` ∩ the bot member's own roles in the message's guild — Discord auto-creates a same-named managed role when the bot joins, and users frequently pick that role in `@BotName` autocomplete instead of the user. Without (c) those messages are silently ignored.
    - Debug-level log `discord message received` dumps `author_id`, `bot_id`, `mentioned`, `mention_ids`, `mention_roles`, raw `content` and `content_len` for diagnosing "bot didn't respond" issues via `OPENAB_GO_LOG=debug`. `OnReady`/`OnResumed`/`OnDisconnect` log gateway lifecycle.
  - `reactions.go` — `StatusReactionController` state machine for emoji-based status indicators (queued → thinking → tool → done/error) with debounce, stall detection (soft 🥱 / hard 😨), and tool classification (coding/web/generic)

- **`telegram/`** — Telegram platform adapter (uses [go-telegram/bot](https://github.com/go-telegram/bot) v1 with native forum topic support)
  - `adapter.go` — `Adapter` struct implementing `platform.Platform`, uses `bot.New()` with `WithDefaultHandler` and context-based lifecycle. Registers BotCommands (`/sessions`, `/info`, `/reset`, `/setvoice`, `/voice_clear`, `/voicemode`) via `SetMyCommands` on startup. Parses `allowed_user_id` (string array — so `"*"` can coexist with numeric IDs) into an `AllowedUserIDs` map (`map[int64]bool`) plus an `AllowAnyUser` flag; non-numeric non-`"*"` entries fail startup.
  - `handler.go` — Message handler: command detection via entity parsing (`bot_command` at offset 0), @mention/reply-to-bot detection in groups, all messages in private chats, sender context injection (`openab.sender.v1` schema), photo download (largest PhotoSize via `GetFile`/`FileDownloadLink`) and voice/audio transcription to `.tmp/`, streaming prompt responses with a background edit-streaming goroutine (2s ticker for Telegram rate limits, 4096-char message limit). Forum topic support: `MessageThreadID` from `models.Message` threaded through all send paths. Session keys: `tg:{chatID}:{threadID}` for forum topics, `tg:{chatID}` otherwise. `/setvoice` requires replying to a voice message. Voice replies sent via `SendVoice` API.
    - **Access gate**: `allowed_user_id` (matched against `msg.From.ID`) takes precedence over `allowed_chats`; `"*"` wildcards any user. When unset, falls back to chat gate. Rejections are logged via `slog.Warn` so they can be diagnosed without a debugger.
  - `reactions.go` — `StatusReactionController` using `SetMessageReaction` API with typed `ReactionType` structs, debounce and stall detection, same state machine as Discord

- **`stt/`** — Speech-to-text via OpenAI Whisper API. Defines a `Transcriber` interface and `OpenAITranscriber` implementation. Enabled when `stt.api_key` is set in config; injected into platform adapters that support voice messages (Discord and Telegram). Logs `🎙️ stt: transcribing voice message` on each invocation.

- **`tts/`** — Text-to-speech via OpenAI TTS API.
  - `synthesizer.go` — Defines `Synthesizer` interface: `Synthesize` (default voice), `SynthesizeWithVoice` (custom voice ID), `CreateVoice` (upload audio to create custom voice via `POST /audio/voices`).
  - `openai.go` — `OpenAISynthesizer` implementation. Supports built-in voices (alloy, ash, ballad, coral, echo, fable, nova, onyx, sage, shimmer, verse, marin, cedar) and custom voice IDs (`voice_xxx`). Custom voice IDs are auto-wrapped as `{"id": "voice_xxx"}` in the API request. Logs `🔊 tts: synthesizing voice reply` on each invocation.
  - `voices.go` — `VoiceStore` manages per-user custom voice IDs. Persisted as `.voices.json` in the agent working directory. Also tracks per-user echo mode.
  - Enabled when `tts.api_key` is set in config. Default voice gender is female (`nova`), configurable via `voice_gender`.

- **`config/`** — TOML configuration with `${ENV_VAR}` expansion and sensible defaults. Platform-specific configs are nested (`discord.reactions.emojis`, etc.). Having a `bot_token` in a platform section auto-enables that platform. `[stt]` and `[tts]` sections auto-enable when `api_key` is set. Reference config: `config.toml.example`.
  - **Access-gate fields**: `discord.allowed_channels` / `telegram.allowed_chats` are the location gate (which channel/chat may talk to the bot). `discord.allowed_user_id` / `telegram.allowed_user_id` are the identity gate — when non-empty they override the location gate. Both use `[]string`; Telegram parses numeric entries to `int64` in the adapter so `"*"` can coexist with numeric IDs in the same TOML array.

- **`main.go`** — Loads config, creates session pool, creates STT transcriber (if configured), creates TTS synthesizer and voice store (if configured), starts optional HTTP API server, registers all enabled `platform.Platform` adapters (passing STT + TTS), starts them, runs idle-session cleanup (60s tick), and handles graceful shutdown via SIGINT/SIGTERM.

### Adding a new platform

1. Create a new package (e.g., `teams/`) with `adapter.go` implementing `platform.Platform`
2. Add config struct to `config/config.go` (stub for Teams already exists)
3. Register in `main.go`: `if cfg.Teams.Enabled { platforms = append(platforms, ...) }`
4. `acp/` and `platform/` require no changes

### Voice commands

| Command | Discord | Telegram | Description |
|---|---|---|---|
| `/setvoice` | Audio attachment + text `setvoice` | Reply to a voice message with `/setvoice` | Create custom voice via OpenAI API (requires org access to `/audio/voices`) |
| `/voice-clear` | Slash command | `/voice_clear` (Telegram uses underscore) | Clear custom voice, revert to default |
| `/voicemode` | Slash command with `echo`/`default` option | `/voicemode echo` or `/voicemode default` | Toggle echo mode |

Voice reply priority: per-user custom voice → default configured voice.

### ACP lifecycle (per thread)

1. `SessionPool.GetOrCreate(threadKey)` — spawns agent process if needed
2. `AcpConnection.Initialize()` → `initialize` → handshake
3. `AcpConnection.SessionNew(cwd)` → `session/new` → creates a session
4. `AcpConnection.SessionPrompt(content)` → sends `session/prompt`, returns a notification channel for streaming
5. Caller reads notifications (text chunks, tool events) until response with matching ID arrives
6. `AcpConnection.PromptDone()` — clears notification subscriber, releases prompt lock

### Releasing

Releases use `scripts/release.sh`. Version lives in `VERSION` file. Run `./scripts/release.sh` to auto-open a Release PR (or CI does it on push to main via `release.yml`). Run `./scripts/release.sh --rc` to create RC tags that trigger CI image builds. Merging the Release PR triggers `tag-on-merge.yml` which auto-creates a stable tag, promoting the tested image without rebuilding. Images are published to GHCR in four variants (see Docker section).
