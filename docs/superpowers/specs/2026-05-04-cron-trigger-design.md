# Cron-Triggered Agent Messages

**Date:** 2026-05-04
**Status:** Draft

## Summary

Let users schedule recurring or one-shot prompts that fire automatically into their existing chat thread. The agent receives the prompt with a `trigger: "cron"` marker in the existing `<sender_context>` envelope so it can distinguish scheduled triggers from real user turns. Schedules are user-managed via `/cron` slash commands, persisted to a JSON file, and executed by a single in-process scheduler that dispatches to whichever platform owns the thread.

## Motivation

- Quill is operated as a personal/team chat bot; the most-asked use case beyond ad-hoc Q&A is recurring tasks: daily standup digests, periodic build-status checks, "remind me in 30 minutes to deploy", etc.
- Today there is no way to invoke the agent without a human typing in the thread.
- The two reference projects the user pointed at (`openclaw/openclaw`, `openabdev/openab`) either expose the feature only in docs without public code, or do not have it at all — there is no prior art to copy. A first-principles design is appropriate.
- Quill already injects a structured `<sender_context>` JSON envelope into every prompt (`quill.sender.v1`). Extending it with a `trigger` discriminator is the natural place to signal "this turn was started by a cron job, not by a human" without a new schema.

## Scope

### In scope

- New `cronjob/` package with: data model, three `Schedule` implementations (cron / interval / one-shot), JSON file store, scheduler loop, dispatcher registry.
- Three platform-agnostic commands wired into `command/` package: `/cron add`, `/cron list`, `/cron rm`.
- Discord SelectMenu and Telegram InlineKeyboard for `/cron list` so users can delete a job in one tap (mirrors `/pick`, `/mode`, `/model` patterns). Teams gets the text-only fallback.
- Extension of the existing `quill.sender.v1` schema with optional `trigger`, `cron_id`, `cron_schedule`, `cron_fire_time` fields. The schema string stays at `v1` because additions are backwards compatible.
- New `[cronjob]` config section in `config.toml.example`.
- Tests: parser, scheduler with fake clock, store load/save/atomic-rename, sender-context injection, queue cap behavior.

### Out of scope (V1)

- `/cron pause` / `/cron resume` — `Disabled` field is reserved on the struct but no UI ships in V1.
- Execution history / audit log — V1 relies on `slog`; adding a queryable history is V2.
- Coalescing of backlogged interval fires when the user holds `promptMu` for a long time. V1 ships pure FIFO queue; V2 adds an opt-in "skip if backlog > N" knob.
- Per-job timezone. V1 uses a single bot-wide TZ (default UTC).
- Multi-instance deployments. If two Quill processes share the same `cronjobs.json`, both will fire — V1 documents this and does not solve it.

## Design

### 1. Package layout

```
cronjob/
  job.go          # Job struct, Schedule interface, Kind constants
  schedule.go     # cronSchedule, intervalSchedule, oneshotSchedule
  parser.go       # ParseSchedule(input string) (Schedule, Kind, error)
  store.go        # Store: load, atomic save, list/add/remove
  scheduler.go    # Scheduler: 1s ticker, dispatch, requeue/delete
  dispatcher.go   # Dispatcher interface + Registry: map[prefix]Dispatcher
  gate.go         # per-thread bounded fire channel + worker goroutine
  sender_ctx.go   # BuildSenderContext helper
  *_test.go
```

The package depends only on the standard library plus `github.com/robfig/cron/v3` for the cron expression parser. It does **not** import any platform package, `acp`, or `command` — all interaction with the chat side flows through the `Dispatcher` interface so the dependency graph stays one-way.

### 2. Data model

```go
package cronjob

type Kind string

const (
    KindCron     Kind = "cron"
    KindInterval Kind = "interval"
    KindOneshot  Kind = "oneshot"
)

type Job struct {
    ID         string    `json:"id"`               // 8 hex chars, unique within thread
    ThreadKey  string    `json:"thread_key"`       // "tg:123" / "discord:456" / "tg:123:78" / "teams:..."
    SenderID   string    `json:"sender_id"`        // creator's platform user id
    SenderName string    `json:"sender_name"`      // creator's display name (snapshot)
    Schedule   string    `json:"schedule"`         // raw input string ("0 9 * * *" / "every 5m" / "in 2h")
    Kind       Kind      `json:"kind"`
    Prompt     string    `json:"prompt"`           // user-supplied prompt body
    NextFire   time.Time `json:"next_fire"`        // UTC
    LastFire   time.Time `json:"last_fire"`        // UTC, zero means never fired
    CreatedAt  time.Time `json:"created_at"`       // UTC
    Disabled   bool      `json:"disabled"`         // reserved for V2 pause UI
}

type Schedule interface {
    Next(after time.Time) time.Time // returns zero time when no next fire
    Kind() Kind
}
```

`time.Time` values are always UTC. The configured display timezone (default UTC) only affects rendering in `/cron list` and the "next fire" line in `/cron add` confirmation.

### 3. Schedule parser

`parser.ParseSchedule(input)` accepts the following forms (case-insensitive on the keyword):

| Input                              | Kind     | Behaviour                                                                                         |
|------------------------------------|----------|---------------------------------------------------------------------------------------------------|
| `0 9 * * *`                        | cron     | Standard 5-field, parsed via `robfig/cron/v3`. Min granularity is 1 minute by definition.         |
| `every 5m`, `every 2h`, `every 1m` | interval | Suffix parsed by `time.ParseDuration`; rejected if `< min_interval` (default 1m).                 |
| `at 2026-05-05 09:00`              | oneshot  | Parsed as RFC-3339-ish in the configured TZ; rejected if the resulting UTC time is in the past.   |
| `at 09:00`                         | oneshot  | Today at 09:00 in TZ; if already passed, rolls forward to tomorrow.                               |
| `in 30m`, `in 2h`, `in 90s`        | oneshot  | `time.ParseDuration` from now. `< min_interval` is allowed for one-shots (a "remind me in 1m" is fine). |

Invalid input returns a typed error that command handlers map to a chat-friendly message.

### 4. Store

Single file at `<workdir>/.quill/cronjobs.json`. Format:

```json
{
  "version": 1,
  "jobs": [ { /* Job */ }, ... ]
}
```

```go
type Store struct {
    path string
    mu   sync.RWMutex
    jobs map[string]*Job // key: ID
}

func Open(path string) (*Store, error)         // creates empty file if missing
func (s *Store) List(threadKey string) []Job   // snapshot; thread filter ""→all
func (s *Store) Add(j Job) error               // generates ID; enforces max_per_thread
func (s *Store) Remove(threadKey, id string) error
func (s *Store) Update(j Job) error            // for NextFire / LastFire writebacks
func (s *Store) save() error                   // write tmp + os.Rename
```

`save` writes to `cronjobs.json.tmp` then `os.Rename` for atomicity. On `Open`, a corrupt file is renamed to `cronjobs.json.broken-<unix>` and a fresh empty store starts — the broken file stays on disk for forensics.

### 5. Scheduler loop

Single goroutine started by `main.go` after the platform adapters have registered themselves with the dispatcher.

```
for tick := range time.Tick(1 * time.Second) {
    now := time.Now().UTC()
    due := store snapshot where !Disabled && NextFire <= now
    for each job in due:
        prefix := strings.SplitN(job.ThreadKey, ":", 2)[0]
        dispatcher := registry.Get(prefix)
        if dispatcher == nil:
            slog.Warn(...); skip
            continue
        gate := gates.For(job.ThreadKey, dispatcher)   // lazy-create per-thread chan + worker
        gate.Submit(job)                                // non-blocking; drops on overflow (see §8)

        job.LastFire = now
        next := job.Schedule.Next(now)
        if next.IsZero():              // oneshot fired
            store.Remove(job.ThreadKey, job.ID)
        else:
            job.NextFire = next
            store.Update(job)
}
```

The 1-second tick is fine: cron and interval granularities are >= 1 minute, and one-shots don't need sub-second precision in a chat-bot context.

### 6. Dispatcher pattern

```go
package cronjob

type Dispatcher interface {
    Fire(ctx context.Context, job Job) error
    NotifyDropped(ctx context.Context, job Job) // chat marker when queue overflows
}

type Registry struct {
    mu sync.RWMutex
    m  map[string]Dispatcher // key: thread-key prefix ("tg" / "discord" / "teams")
}

func (r *Registry) Register(prefix string, d Dispatcher)
func (r *Registry) Get(prefix string) Dispatcher
```

Each platform adapter implements `Dispatcher` — typically a small struct that captures the adapter's `*SessionPool`, the platform-native send/edit functions, and the bot's identity. `Fire` is responsible for:

1. Posting a placeholder message to the thread: `🔔 cron \`<id>\` (\`<schedule>\`) — running prompt…`
2. Calling `pool.GetOrCreate(threadKey)` (re-spawns agent if needed).
3. Building the `<sender_context>` envelope with `trigger="cron"` (see §7).
4. Calling `conn.SessionPrompt(promptWithSender)` and streaming the reply by editing the placeholder message — same code path the platform already uses for human prompts, so reactions/streaming/footer all work for free.
5. Acquiring the per-thread serialisation primitive (see §8) before step 3 so cron-vs-cron and cron-vs-user fires queue cleanly.

`Fire` errors are logged and surfaced in-thread as `⚠️ cron \`<id>\` failed: <err>` but **never** retried automatically. The next scheduled fire still happens on time.

### 7. Sender-context extension

The existing injection sites (`discord/handler.go:206`, `telegram/handler.go:192`, `teams/handler.go:110`) write a JSON object inside `<sender_context>...</sender_context>`. Cron fires use the **same envelope** with extra optional fields:

```json
{
  "schema": "quill.sender.v1",
  "sender_id": "12345",
  "sender_name": "neil",
  "display_name": "neil",
  "channel": "telegram",
  "channel_id": "12345",
  "is_bot": false,

  "trigger": "cron",
  "cron_id": "a3f5b201",
  "cron_schedule": "0 9 * * *",
  "cron_fire_time": "2026-05-05T01:00:00Z"
}
```

Rules:
- Absent `trigger` (or `trigger == "user"`) means a human-originated turn — backwards compatible with every existing prompt.
- `trigger == "cron"` means scheduler-originated. The four `cron_*` fields are only present in this case.
- The schema string remains `quill.sender.v1`. Any agent that pattern-matches on the schema continues to work; an upgraded agent can opt into the new fields.

A small helper `cronjob.BuildSenderContext(job Job, channel string) []byte` returns the JSON byte slice. Each platform's `Dispatcher.Fire` passes its own channel string (`"discord"`, `"telegram"`, `"teams"`); the helper writes the cron fields and lets the caller merge platform-specific extras (`chat_type`, `topic_thread_id`, etc.) before serialising. This keeps `cronjob/` independent of platform packages while avoiding duplicated envelope code.

### 8. Concurrency: the queue policy

There are two serialisation layers, working together:

**Layer 1 — ACP-level mutual exclusion.** `AcpConnection.promptMu` already serialises `session/prompt` calls. Both human prompts and cron fires take this lock; the existing code in `acp/connection.go` is unchanged.

**Layer 2 — per-thread fire bounding.** A small helper inside `cronjob/` owns a `map[threadKey]chan Job` (channel capacity = `cfg.Cronjob.QueueSize`, default 50). When the scheduler tick decides a job is due, it does:

```go
gate := registry.GateFor(job.ThreadKey)   // lazy create the chan + worker goroutine
select {
case gate.ch <- job:
    // queued
default:
    slog.Warn("cron fire dropped, thread queue full", ...)
    dispatcher.NotifyDropped(job)         // chat marker: "⚠️ cron <id> dropped"
}
```

A single worker goroutine per thread reads from the channel and calls `dispatcher.Fire(ctx, job)` synchronously. This means:
- The scheduler tick goroutine never blocks on `promptMu`.
- Per-thread ordering is FIFO across both the channel buffer and the dispatcher call.
- Memory is bounded: at most `QueueSize` queued fires per thread.
- Different threads have independent gates and `AcpConnection`s, so they fire fully concurrently.

The worker goroutine for a thread shuts down when the channel is closed during graceful shutdown.

**Known consequence** (documented in `/cron list` help and in the "Limitations" section): if a user holds the connection for 30 minutes, six `every 5m` fires queue and drain back-to-back when the user finishes. The 51st queued fire would be dropped — extremely unlikely in practice with a 1-minute minimum interval. V2 may add coalescing.

### 9. Commands (`command/` package)

Three subcommands of `/cron`, parsed by extending the existing `ParseCommand`:

| Command syntax                                  | Result                                                                 |
|-------------------------------------------------|------------------------------------------------------------------------|
| `/cron add <schedule> <prompt...>`              | Validates, persists, replies with `✅ created \`<id>\`, next fire <ts>`. |
| `/cron list`                                    | Discord SelectMenu / Telegram InlineKeyboard / Teams text fallback.   |
| `/cron rm <id>`                                 | Deletes; replies `🗑️ removed \`<id>\``.                                  |
| `/cron` (no subcommand)                         | Prints inline help.                                                    |

Schedule and prompt are separated by the **first run of whitespace after the schedule token has been fully consumed**:
- Cron: 5 whitespace-separated tokens, then prompt is the rest.
- `every <dur>` / `at <ts>` / `in <dur>`: 2 tokens (3 for `at YYYY-MM-DD HH:MM`), then prompt is the rest.

Following the existing pattern in `command/`, a thin pair of functions splits data fetch from action:

```go
func ListCronJobs(threadKey string) []Job
func ExecuteCronAdd(threadKey, senderID, senderName, schedule, prompt string) (Job, error)
func ExecuteCronRemove(threadKey, id string) error
```

Each platform handler renders the result to its native widgets. Discord adds a slash command `/cron` with `add`/`list`/`rm` subcommands; Telegram registers `/cron` via `SetMyCommands` and parses the subcommand from the message text; Teams parses text.

### 10. Config

```toml
[cronjob]
enabled         = true                      # default true; false fully disables the package
max_per_thread  = 20                        # store-level cap; /cron add returns "limit reached"
min_interval    = "1m"                      # rejected if interval < this; one-shots exempt
queue_size      = 50                        # per-thread async fire buffer; overflow = drop
timezone        = "UTC"                     # display + parser TZ for `at HH:MM` / `at YYYY-MM-DD HH:MM`
store_path      = "./.quill/cronjobs.json"  # relative paths resolved against working dir
```

Auto-enable rule: present and `enabled = true` ⇒ scheduler starts. Absent or `enabled = false` ⇒ commands return `cron jobs are disabled on this bot`. Mirrors the auto-enable patterns elsewhere in `config/config.go`.

### 11. Access control

Cron commands ride on top of the existing per-platform `allowed_user_id` / `allowed_channels` access gate (`discord/adapter.go`, `telegram/handler.go`, `teams/handler.go`). If a user can talk to the bot in a thread, they can `/cron add` in that thread. No separate `allowed_cron_users`. A user can only see / remove their own thread's jobs (`/cron list` filters by `ThreadKey == current thread`). Cross-thread visibility is intentionally absent.

### 12. Lifecycle

- `main.go`:
  1. `store, _ := cronjob.Open(cfg.Cronjob.StorePath)`
  2. `registry := cronjob.NewRegistry()`
  3. Build platform adapters, passing `registry` so each can `registry.Register("tg", &telegram.CronDispatcher{...})` etc. inside `Start()`.
  4. `scheduler := cronjob.NewScheduler(store, registry, cfg.Cronjob); go scheduler.Run(ctx)`.
- Graceful shutdown: `ctx` cancellation stops the ticker, closes every per-thread fire channel, and waits for the per-thread worker goroutines to exit. Any in-flight `dispatcher.Fire` is given a best-effort 5-second deadline before the program exits; the underlying `SessionPrompt` honours `ctx` via the existing `acp` cancel path.

### 13. Testing

| Layer                  | Test                                                                                        |
|------------------------|---------------------------------------------------------------------------------------------|
| `parser_test.go`       | All three Kind round-trips; reject `every 30s`, past `at`, malformed cron; case-insensitive. |
| `schedule_test.go`     | `Next` correctness for cron (DST not relevant in UTC), interval (drift-free), oneshot (zero after fire). |
| `store_test.go`        | Load empty / load existing / atomic save / corrupt-file fallback creates `.broken-*`.        |
| `scheduler_test.go`    | Fake clock; assert ordering, queue overflow drops, dispatcher errors don't poison ticks.    |
| `dispatcher_test.go`   | Registry get/register, prefix routing, missing-prefix logs but doesn't panic.               |
| `command/` extension   | `/cron add` parsing, error messages, subcommand routing.                                    |
| Platform handlers      | Mock `Dispatcher` + `SessionPool`; assert sender-context bytes contain `trigger:"cron"`.    |
| Skipped                | End-to-end against real Discord/Telegram/Teams APIs — out of scope, mock instead.           |

### 14. Migration / rollout

- New code only; no data migration.
- `cronjobs.json` is created on first start; existing deployments see no change unless they set `[cronjob]`.
- Backwards compatible: agents that ignore the new `trigger` field continue to work.

## Limitations

1. **Backlogged interval fires.** A user who holds `promptMu` for N minutes will see `⌈N / interval⌉` cron messages back-to-back when their turn ends. V2 may add coalescing.
2. **No pause/resume UI.** The `Disabled` field exists but is not user-settable in V1.
3. **No execution history.** `slog` is the audit trail. V2 may add a rotating history file.
4. **Single instance only.** Two processes sharing `cronjobs.json` will both fire every job. Documented but not solved in V1.
5. **Single TZ per bot.** No per-job timezone; bot-wide `cronjob.timezone` only.
6. **Best-effort delivery.** If the bot is offline when a fire is due, the fire is **lost** — V1 does not run "missed" cron jobs on startup. Documented behaviour.

## Open questions

None at design time. All five clarifying questions resolved during brainstorming.

## Future work (V2 candidates)

- `/cron pause <id>` / `/cron resume <id>` toggling `Disabled`.
- Coalesce flag on interval jobs to skip backlogged fires when the queue depth exceeds N.
- `/cron history <id>` reading from a rotated execution log.
- Per-job timezone field.
- Catch-up policy: on startup, optionally fire missed `oneshot` jobs whose `NextFire` is in the past.
