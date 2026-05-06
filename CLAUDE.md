# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o quill .
./quill                    # uses config.toml in cwd
./quill /path/to/config.toml  # custom config path
./quill --version          # print version
QUILL_LOG=debug ./quill   # enable debug logging

# inject commit hash at build time (main.go uses `commit` variable)
go build -ldflags "-X main.commit=$(git rev-parse --short HEAD)" -o quill .
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

Five Dockerfile variants exist for different agent backends:

| Dockerfile | Agent binary | Base image |
|---|---|---|
| `Dockerfile` | `kiro-cli` | `debian:bookworm-slim` |
| `Dockerfile.claude` | `claude-agent-acp` | `node:22-bookworm-slim` |
| `Dockerfile.codex` | `codex-acp` | `node:22-bookworm-slim` |
| `Dockerfile.copilot` | `copilot` (native ACP) | `node:22-bookworm-slim` |
| `Dockerfile.gemini` | `gemini` (native ACP via `--experimental-acp`) | `node:22-bookworm-slim` |

All use multi-stage builds (Go builder → runtime) and accept a `COMMIT` build arg.

## Architecture

Quill is a multi-platform chat bot that proxies user messages to an AI agent process via the **Agent Communication Protocol (ACP)** — a JSON-RPC 2.0 protocol over stdin/stdout.

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

- **`platform/`** — Defines the `Platform` interface (`Start`/`Stop`) that every chat adapter must implement. Contains shared utilities like `SplitMessage` (used by all platforms for message-size splitting), `TruncateUTF8`, `FormatToolTitle` (renders ACP tool-call titles based on `reactions.tool_display` — `full` / `compact` / `none`; defaults to `compact` to keep streaming messages clean when agents like codex put full shell commands in tool titles), and `FormatSessionFooter(mode, model)` which each platform's `streamPrompt` appends to the final content so every reply ends with `— mode: \`x\` · model: \`y\``. The footer deliberately avoids italic wrapping because Telegram's markdown→HTML converter closes italic spans at backticks, garbling the output.

- **`acp/`** — Agent Communication Protocol implementation (platform-agnostic). `AcpConnection` carries `AvailableModes` + `CurrentModeID` (guarded by `modeMu`) and `AvailableModels` + `CurrentModelID` (guarded by `modelMu`); populated when `session/new` / `session/load` responses include a `modes` / `models` object, mutated by `SessionSetMode(modeID)` / `SessionSetModel(modelID)` or by incoming `current_mode_update` / `current_model_update` notifications routed through `ClassifyNotification` in the read loop. Mode and model state are session-scoped, not connection-scoped — `session/load` replaces them. Full state-flow diagram + field-name asymmetry (mode uses `id`, model uses `modelId` — both with custom unmarshal tolerance) lives in [`docs/session-state-flow.md`](docs/session-state-flow.md).
  - `connection.go` — Spawns an agent subprocess, manages stdin/stdout JSON-RPC communication, handles request/response correlation via pending map, auto-approves `session/request_permission` requests, streams notifications via a subscriber channel. `promptMu` serializes prompts — only one at a time per connection.
    - **`SessionCancel()`** sends a `session/cancel` notification. Intentionally does **not** acquire `promptMu` (the prompt goroutine already holds it) — only `stdinMu` via `sendRaw`. A watchdog (`cancelWatchdogTimeout`, default 10s) snapshots pending request IDs before the send and, after the timeout, force-resolves any still-pending request with a synthetic `{"stopReason":"cancelled"}` response so the streaming loop cannot hang when the agent ignores cancel. The synthetic response is forwarded to `notifyCh` with `notifyMu` held for atomicity; a 2s bound on that send prevents the watchdog goroutine from leaking if the consumer has exited.
  - `pool.go` — `SessionPool` maps thread keys (`discord:{id}` / `tg:{id}` / `tg:{id}:{threadID}` / `teams:{id}`) to `AcpConnection` instances. LRU eviction when pool is full, TTL-based idle cleanup, and query methods (`ListSessions`, `GetSessionInfo`, `KillSession`, `CancelSession`, `Connection`, `Stats`). Uses double-checked locking (RLock → RUnlock → Lock) for GetOrCreate. `Connection(threadKey)` returns a pointer under RLock and releases — captured pointers are safe to retain because the captured object is GC-pinned and `conn.Alive()` gates misuse after eviction/kill.
  - `protocol.go` — JSON-RPC message types (request, response, `JsonRpcNotification`), ACP event classification (`ClassifyNotification` parses session updates into typed events: text chunks, thinking, tool calls, status), and `StopReason(msg)` which reads the `stopReason` field from a session/prompt response result (`"end_turn"` / `"cancelled"` / `"max_tokens"` / `"refusal"`).

- **`command/`** — Shared bot command logic (`sessions`/`info`/`reset`/`resume`/`stop`/`pick`/`mode`/`model`). `ParseCommand` detects commands from text (`cancel` → `stop`; `history`/`session-picker`/`session_picker`/`sessionpicker` → `pick`), `Execute*` functions format responses using pool query methods. `/info` displays session details including STT/TTS status. `/stop` (and `cancel`) sends `session/cancel` to the agent — the current reply is interrupted, the session is preserved. `/pick` lists historical sessions via `sessionpicker.Picker`, caches the last listing per thread for 5 minutes so `/pick <N>` and the interactive keyboard callbacks can both resolve indexes against the same cache. The list path is split from the load path via `ListPickerSessions` / `LoadPickerByIndex` / `LoadPickerByID` so Discord can render a SelectMenu (full session id in the option Value) and Telegram an InlineKeyboard (1-based index in `callback_data` to stay under Telegram's 64-byte cap); Teams keeps the original text-only path via `ExecutePicker`. The actual resume call is always `pool.LoadSessionForThread`. `/mode` reads `conn.Modes()` / calls `conn.SessionSetMode` — on Discord it renders a SelectMenu, on Telegram an InlineKeyboard; `ListModes` / `ExecuteMode` separate the data pull from the switch so platforms can build either a text fallback or an interactive widget. `/model` is the parallel command for the LLM-model axis — `conn.Models()` / `conn.SessionSetModel`, `ListModels` / `ExecuteModel`, with the same Discord SelectMenu / Telegram InlineKeyboard / Teams text-only shape. Canonical name for the picker is `pick` because Telegram's `SetMyCommands` rejects hyphens and camelCase (`^[a-z][a-z0-9_]{0,31}$`); the longer aliases remain for users typing from older docs. Used by all three platform handlers.

- **`sessionpicker/`** — Browses the agent's historical sessions on disk so a user can later pick one and resume. Platform-agnostic; does **not** require the agent process to be running. Each supported agent has a dedicated `Picker` implementation:
  - `picker.go` — `Picker` interface (`AgentType`, `List(cwd, limit)`), `Session` struct, `Detect(agentCommand)` factory that maps the configured agent binary (`kiro-cli` / `claude-agent-acp` / `copilot` / `codex-acp` / `codex` / `gemini`) to the right picker.
  - `kiro.go` — reads `~/.kiro/sessions/cli/<uuid>.json` metadata files (Kiro writes matching `<uuid>.jsonl` for the conversation stream but metadata alone is enough for the picker).
  - `claude.go` — walks `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`; `encodeClaudeCWD` mirrors claude-agent-acp's own encoding (every non-alphanumeric rune → `-`). Title is extracted by scanning up to `titleScanLimit` JSONL lines while skipping `file-history-snapshot`, `isMeta=true`, and `<command-name>` / `<local-command-caveat>` etc. envelopes via `claudeCommandWrapperPrefixes`.
  - `copilot.go` — reads `~/.copilot/session-state/<uuid>/workspace.yaml` (flexible yaml key matching: `session_id`/`id`/`sessionId`, `cwd`/`workdir`/`workspace`, `title`/`name`/`summary`) with a fallback to `events.jsonl` when the yaml lacks a field. Brings in `gopkg.in/yaml.v3` as a direct dep.
  - `codex.go` — reads the flat `~/.codex/history.jsonl` (one prompt per line: `{session_id, ts, text}`), groups by session id, keeps the earliest `text` as title and the latest `ts` as `UpdatedAt`. **Limitation**: history entries carry no cwd, so `List(cwd, limit)` returns an empty slice when a non-empty cwd is passed — the picker refuses to answer a query it cannot actually satisfy, rather than silently returning unfiltered results.
  - `gemini.go` — walks `~/.gemini/tmp/<project-tmp-id>/chats/session-*.json{,l}`. The parent directory name varies (legacy `sha256(cwd)` vs newer registry-based id), so the picker scans every project tmp dir and filters by the per-session `projectHash` field — always `sha256(cwd)` regardless of the parent dir. JSONL stream is parsed with `geminiScanLimit` (256-line cap) handling the initial metadata header, `$set` updates (merged so latest `lastUpdated` wins), and `$rewindTo` markers (skipped). Subagent transcripts (`kind:"subagent"`) are excluded — they belong to a parent session. cwd filter is the OR of `projectHash == sha256(cwd)` and `cwd ∈ directories[]`, so sessions that added cwd via `/dir add` partway through are still discoverable. Title preference: `summary` → first user message (after stripping `<sender_context>` envelope).
  - Manual smoke tests are gated behind `QUILL_PICKER_SMOKE=1` (each picker has its own `*_LocalSmoke` test); hermetic fixture tests live under `testdata/{kiro,claude,copilot,codex,gemini}/`. The `/pick` user-facing command lives in `command/` and is wired into all three platforms; the resume path uses `pool.LoadSessionForThread(threadKey, sessionID, cwd)`, which spawns a fresh agent and calls `conn.SessionLoad` with the session's original cwd, falling back to `session/new` if the agent cannot satisfy the load.

- **`api/`** — Optional HTTP API server for session monitoring. Endpoints: `GET /api/sessions`, `DELETE /api/sessions/{key}`, `GET /api/health`. Enabled via `[api]` config section.

- **`discord/`** — Discord platform adapter
  - `adapter.go` — `Adapter` struct implementing `platform.Platform`, wraps discordgo session lifecycle. Parses `allowed_user_id` into an `AllowedUserIDs` map plus an `AllowAnyUser` flag (set when the list contains `"*"`). Intents include `IntentsGuildMessageReactions` for the tap-to-cancel 🛑 flow — **existing deployments may need to re-invite the bot** after upgrading across that intent.
  - `handler.go` — Message handler: mention/thread detection, sender context injection (`quill.sender.v1` schema), image attachment download to `.tmp/`, streaming prompt responses with a background edit-streaming goroutine (1.5s ticker, truncates to 1900 chars during streaming, splits into multiple messages only on final edit). Registers Discord Slash Commands (`/sessions`, `/info`, `/reset`, `/resume`, `/stop`) on ready, handles interactions via `OnInteractionCreate`. **Cancel UX**: while the bot is streaming, a 🛑 reaction is dropped on the thinking message (gated by `ReactionsConfig.Enabled`); the `streamingMsgs` map (keyed by Discord message ID → `*AcpConnection`, not threadKey) routes reaction clicks to the exact connection that owns the prompt, so a later prompt on the same thread cannot be accidentally cancelled by a stale reaction. `OnMessageReactionAdd` reads that map and calls `conn.SessionCancel()`.
    - **Access gate**: `allowed_user_id` (matched against `m.Author.ID`, the numeric snowflake) takes precedence over `allowed_channels`; `"*"` wildcards any user. When unset, falls back to channel/thread gate.
    - **Mention detection** covers three cases: (a) `m.Mentions` for user mentions, (b) raw-content scan for `<@BOTID>` and legacy `<@!BOTID>`, (c) `m.MentionRoles` ∩ the bot member's own roles in the message's guild — Discord auto-creates a same-named managed role when the bot joins, and users frequently pick that role in `@BotName` autocomplete instead of the user. Without (c) those messages are silently ignored.
    - Debug-level log `discord message received` dumps `author_id`, `bot_id`, `mentioned`, `mention_ids`, `mention_roles`, raw `content` and `content_len` for diagnosing "bot didn't respond" issues via `QUILL_LOG=debug`. `OnReady`/`OnResumed`/`OnDisconnect` log gateway lifecycle.
  - `reactions.go` — `StatusReactionController` state machine for emoji-based status indicators (queued → thinking → tool → done/error) with debounce, stall detection (soft 🥱 / hard 😨), and tool classification (coding/web/generic)

- **`telegram/`** — Telegram platform adapter (uses [go-telegram/bot](https://github.com/go-telegram/bot) v1 with native forum topic support)
  - `adapter.go` — `Adapter` struct implementing `platform.Platform`, uses `bot.New()` with `WithDefaultHandler` and context-based lifecycle. Registers BotCommands (`/sessions`, `/info`, `/reset`, `/resume`, `/stop`) via `SetMyCommands` on startup. Parses `allowed_user_id` (string array — so `"*"` can coexist with numeric IDs) into an `AllowedUserIDs` map (`map[int64]bool`) plus an `AllowAnyUser` flag; non-numeric non-`"*"` entries fail startup.
  - `handler.go` — Message handler: command detection via entity parsing (`bot_command` at offset 0), @mention/reply-to-bot detection in groups, all messages in private chats, sender context injection (`quill.sender.v1` schema), photo download (largest PhotoSize via `GetFile`/`FileDownloadLink`) and voice/audio transcription to `.tmp/`, streaming prompt responses with a background edit-streaming goroutine (2s ticker for Telegram rate limits, 4096-char message limit). Forum topic support: `MessageThreadID` from `models.Message` threaded through all send paths. Session keys: `tg:{chatID}:{threadID}` for forum topics, `tg:{chatID}` otherwise. Voice replies sent via `SendVoice` API.
    - **Access gate**: `allowed_user_id` (matched against `msg.From.ID`) takes precedence over `allowed_chats`; `"*"` wildcards any user. When unset, falls back to chat gate. Rejections are logged via `slog.Warn` so they can be diagnosed without a debugger.
  - `reactions.go` — `StatusReactionController` using `SetMessageReaction` API with typed `ReactionType` structs, debounce and stall detection, same state machine as Discord. `SetCancelled` finishes with 🙊 ("speak-no-evil") within Telegram's fixed allowed reaction set; Discord's `SetCancelled` uses 🛑.

- **`stt/`** — Speech-to-text via OpenAI Whisper API. Defines a `Transcriber` interface and `OpenAITranscriber` implementation. Enabled when `stt.api_key` is set in config; injected into platform adapters that support voice messages (Discord and Telegram). Logs `🎙️ stt: transcribing voice message` on each invocation.

- **`tts/`** — Text-to-speech synthesis.
  - `synthesizer.go` — Defines `Synthesizer` interface: `Synthesize(text string) (audioPath, error)`.
  - `openai.go` — `OpenAISynthesizer` using OpenAI TTS API.
  - `gemini.go` — `GeminiSynthesizer` using Google Gemini API (`google.golang.org/genai`). Streams audio via `GenerateContentStream`, assembles raw PCM chunks into WAV files.
  - Enabled when `tts.api_key` is set in config. Provider selected via `tts.provider` field (`"openai"` default, `"gemini"`).

- **`teams/`** — Microsoft Teams platform adapter (Bot Framework webhook)
  - `adapter.go` — `Adapter` struct implementing `platform.Platform`, runs an HTTP server on `:3978` (configurable). `buildMux` wires `POST /api/messages` with JWT validation via `BotAuth.ValidateInbound`. Incoming activities are dispatched async (`go handler.OnMessage`). Parses `allowed_user_id` (string array, `"*"` wildcard) and `allowed_channels` from config into Handler maps.
  - `auth.go` — `BotAuth` handles Azure AD OAuth2 client credentials flow (tenant-specific token URL: `login.microsoftonline.com/{tenantID}/oauth2/v2.0/token`, scope `api.botframework.com/.default`). Token caching with mutex + expiry buffer. `ValidateInbound` checks inbound JWT issuer (Bot Framework + tenant-specific issuers) and audience (appID). Uses `ParseUnverified` — no JWKS signature verification yet.
  - `client.go` — `BotClient` wraps Bot Framework REST API: `SendActivity` (POST), `UpdateActivity` (PUT), `SendTyping`. All calls use bearer token from `BotAuth.GetBotToken`.
  - `handler.go` — `Handler` processes incoming message activities: access gate (`allowed_user_id` > `allowed_channels`), bot mention detection/stripping via entities, command parsing (`/sessions`, `/info`, `/reset`, `/resume`, `/stop`), image/audio/file attachment download (25MB limit, bearer token auth), sender context injection (`quill.sender.v1`), streaming prompt via `streamPrompt` (2s edit ticker, 28000-char split, tool visualization with icons). STT transcription for audio attachments. TTS voice reply is a stub. Teams has no reactions controller — cancel feedback surfaces as a `🛑 — 已取消` text marker appended to the final message when `stopReason="cancelled"`.
  - `types.go` — Bot Framework Activity, Account, Conversation, Attachment, Entity structs (JSON serializable).
  - `serviceurl_store.go` — `ServiceURLStore` persists per-conversation Bot Framework `serviceURL`s to disk (default `./.quill/teams-serviceurls.json`, configurable via `teams.service_url_store_path`). Without this, a process restart loses the destination URL for every cron-fired proactive message until each user happens to talk to the bot again — `cron_dispatcher.go` would log `no cached serviceURL` and skip. The store is in-memory + write-through to disk; writes only fsync when the value changes (so the steady "every inbound activity refreshes the same URL" path causes no churn). Methods are nil-safe so `&Handler{...}` test fixtures keep compiling. Empty path = in-memory only (legacy behaviour).
  - `appmanifest/` — Teams App Manifest v1.19 (`manifest.json`), color icon (192x192), outline icon (32x32), packaging README.

- **`config/`** — TOML configuration with `${ENV_VAR}` expansion and sensible defaults. Platform-specific configs are nested (`discord.reactions.emojis`, etc.). Having a `bot_token` in a platform section auto-enables that platform. Teams auto-enables when `app_id` and `app_secret` are set. `[stt]` and `[tts]` sections auto-enable when `api_key` is set. Reference config: `config.toml.example`.
  - **Access-gate fields**: `discord.allowed_channels` / `teams.allowed_channels` / `telegram.allowed_chats` are the location gate (which channel/chat may talk to the bot). `discord.allowed_user_id` / `teams.allowed_user_id` / `telegram.allowed_user_id` are the identity gate — when non-empty they override the location gate. All use `[]string`; Telegram parses numeric entries to `int64` in the adapter so `"*"` can coexist with numeric IDs in the same TOML array.

- **`main.go`** — Loads config, creates session pool, creates STT transcriber (if configured), creates TTS synthesizer (if configured, routed via `provider` field), starts optional HTTP API server, registers all enabled `platform.Platform` adapters (Discord, Telegram, Teams — each passing STT + TTS), starts them, runs idle-session cleanup (60s tick), and handles graceful shutdown via SIGINT/SIGTERM.

### Adding a new platform

1. Create a new package with `adapter.go` implementing `platform.Platform` (`Start`/`Stop`)
2. Add config struct to `config/config.go` with auto-enable logic in `applyDefaults`
3. Register in `main.go`: `if cfg.NewPlatform.Enabled { platforms = append(platforms, ...) }`
4. `acp/` and `platform/` require no changes — see `discord/`, `telegram/`, `teams/` for reference implementations

### ACP lifecycle (per thread)

1. `SessionPool.GetOrCreate(threadKey)` — spawns agent process if needed
2. `AcpConnection.Initialize()` → `initialize` → handshake
3. `AcpConnection.SessionNew(cwd)` → `session/new` → creates a session
4. `AcpConnection.SessionPrompt(content)` → sends `session/prompt`, returns a notification channel for streaming
5. Caller reads notifications (text chunks, tool events) until response with matching ID arrives
6. `AcpConnection.PromptDone()` — clears notification subscriber, releases prompt lock

### Cancelling an in-flight prompt

A user-triggered cancel (slash command, text `/stop`, or Discord 🛑 reaction) calls `conn.SessionCancel()` from a goroutine distinct from the one blocked inside `SessionPrompt`. It sends a `session/cancel` notification without touching `promptMu`, so the prompt goroutine continues to hold that lock until it sees the response. Well-behaved agents reply to the pending `session/prompt` with `stopReason="cancelled"`; the streaming loop sees the response, flushes any buffered text with a cancelled marker, and calls `PromptDone`. If the agent ignores cancel, a watchdog (`cancelWatchdogTimeout`, 10s) synthesizes the cancelled response itself and forwards it to both the pending channel and the notification subscriber. Confirmed working end-to-end against `claude-agent-acp` and `kiro-cli`; `codex` / `copilot` behavior is agent-specific — the watchdog is the safety net.

### Releasing

Releases use `scripts/release.sh`. Version lives in `VERSION` file. Run `./scripts/release.sh` to auto-open a Release PR (or CI does it on push to main via `release.yml`). Run `./scripts/release.sh --rc` to create RC tags that trigger CI image builds. Merging the Release PR triggers `tag-on-merge.yml` which auto-creates a stable tag, promoting the tested image without rebuilding. Images are published to GHCR in four variants (see Docker section).
