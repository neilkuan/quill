# Session mode / model state flow

How quill tracks the current mode (agent persona) and model (LLM
backend) of each ACP session. This doc is the reference for anyone
touching `AcpConnection`, `ClassifyNotification`, `platform.FormatSessionFooter`,
or the `/mode` `/model` command paths.

## Where the state lives

Every `AcpConnection` carries four mutable fields, one pair per axis,
each pair guarded by its own `sync.RWMutex`:

| Axis  | Available list   | Current id       | Mutex       |
| ----- | ---------------- | ---------------- | ----------- |
| Mode  | `AvailableModes` | `CurrentModeID`  | `modeMu`    |
| Model | `AvailableModels`| `CurrentModelID` | `modelMu`   |

State is **session-scoped, not connection-scoped**: a `session/load`
replaces the catalogue with whatever the loaded session advertises.

Readers use the accessors `conn.Modes()` / `conn.Models()` — these
take the RLock and return a **defensive copy** of the slice plus the
current id, so callers can't accidentally mutate internal state.

## How the state is populated

Two ingress points feed into the same connection state:

### 1. Initial: `session/new` or `session/load` response

Both RPCs return a payload that may contain `modes` and/or `models`
objects:

```json
{
  "sessionId": "f0437230-…",
  "modes":  { "currentModeId":  "…", "availableModes":  [ … ] },
  "models": { "currentModelId": "…", "availableModels": [ … ] }
}
```

`SessionNew` and `SessionLoad` in `acp/connection.go` unmarshal the
response and hand the two sub-objects to:

- `applyModeSet(*ModeSet)`   — calls `setModeState(current, available)`
- `applyModelSet(*ModelSet)` — calls `setModelState(current, available)`

Both methods tolerate `nil`: older agents that omit `modes` / `models`
from the response don't clobber the prior state.

#### Field-name asymmetry — important

ACP spec (and Kiro in the wild) names the id field differently for the
two axes:

- **Mode**:  `AvailableMode.id`      ← `ModeInfo.ID` uses `json:"id"`
- **Model**: `AvailableModel.modelId` ← `ModelInfo.ID` has a custom
  `UnmarshalJSON` that accepts **either** `modelId` (canonical) or
  `id` (legacy).

If a new agent emits a third shape, extend
`ModelInfo.UnmarshalJSON` rather than weakening the canonical key.

### 2. Live: `session/update` notifications

Mid-session, the agent can change mode or model unilaterally
(e.g. the user ran `/plan` in Kiro's own TUI, or some internal
orchestration swapped models). Kiro emits:

```json
{
  "method": "session/update",
  "params": {
    "sessionId": "…",
    "update": {
      "sessionUpdate": "current_mode_update",
      "currentModeId": "朝比奈實玖瑠學姊"
    }
  }
}
```

```json
{
  "method": "session/update",
  "params": {
    "sessionId": "…",
    "update": {
      "sessionUpdate": "current_model_update",
      "modelId": "minimax-m2.5"
    }
  }
}
```

`ClassifyNotification` in `acp/protocol.go` recognises these and
returns `AcpEventModeUpdate` / `AcpEventModelUpdate` with the new id.
The variance in payload shapes it tolerates:

- `current_mode_update`: flat `currentModeId` *or* nested
  `currentMode.id`
- `current_model_update`: flat `currentModelId`, flat `modelId`
  (some Kiro builds), *or* nested `currentModel.{modelId,id}`

The read loop in `acp/connection.go` dispatches each classified event
to `SetCurrentMode(id)` / `SetCurrentModel(id)`, which update only the
current id — the `available*` list is untouched because the
notification payload carries only the new id.

### 3. quill-initiated switches

When a user taps `/mode` or `/model`:

1. Platform handler calls `command.ExecuteMode(…)` / `ExecuteModel(…)`.
2. That calls `conn.SessionSetMode(modeID)` / `SessionSetModel(modelID)`,
   which send `session/set_mode` / `session/set_model` RPCs.
3. On success, the connection **optimistically** calls
   `SetCurrentMode(id)` / `SetCurrentModel(id)` itself.
4. Kiro usually also emits a matching `current_*_update` notification
   shortly after; the read loop processes it and writes the same id —
   a harmless no-op because step 3 already wrote it.

This belt-and-braces design keeps the UI in sync even if the agent
forgets to emit the notification, and avoids a stale-footer window
between RPC completion and notification arrival.

## How the state is rendered

Every streamed reply's final content ends with a footer. In each of
`discord/handler.go`, `telegram/handler.go`, `teams/handler.go`,
`streamPrompt` pulls the current pair before emitting the last edit:

```go
_, mode := conn.Modes()
_, model := conn.Models()
finalContent += platform.FormatSessionFooter(mode, model)
```

`FormatSessionFooter` returns `"\n\n— mode: `x` · model: `y`"` (with
one side omitted if blank, empty string if both are blank). The
italic wrapping some of us reached for was intentionally dropped —
Telegram's markdown→HTML converter closes italic spans at backtick
boundaries, garbling the output. Plain text + inline code renders
cleanly everywhere.

## Full diagram

```
┌──────────────────────────────────────────────────────────────────┐
│ INGRESS 1: session/new or session/load response                   │
│   { sessionId, modes{currentModeId, availableModes},              │
│                 models{currentModelId, availableModels} }         │
│   → SessionNew / SessionLoad                                      │
│   → applyModeSet()  → setModeState(cur, avail)  (modeMu write)    │
│   → applyModelSet() → setModelState(cur, avail) (modelMu write)   │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│ INGRESS 2: session/update notifications (live)                    │
│   sessionUpdate: current_mode_update  { currentModeId / currentMode.id }│
│   sessionUpdate: current_model_update { currentModelId / modelId       │
│                                         / currentModel.modelId }       │
│   → ClassifyNotification → AcpEventModeUpdate/AcpEventModelUpdate │
│   → read loop → SetCurrentMode(id) / SetCurrentModel(id)          │
│     (only current id changes, available list preserved)           │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│ INGRESS 3: quill-initiated switch (/mode, /model)                 │
│   SessionSetMode(id)  → session/set_mode RPC  → SetCurrentMode    │
│   SessionSetModel(id) → session/set_model RPC → SetCurrentModel   │
│   (Kiro will usually also emit ingress-2 notification — no-op)    │
└──────────────────────────────────────────────────────────────────┘

                                │
                                ▼
┌──────────────────────────────────────────────────────────────────┐
│ AcpConnection state (per session, replaced on session/load)       │
│   modeMu  → AvailableModes  []ModeInfo                            │
│             CurrentModeID   string                                │
│   modelMu → AvailableModels []ModelInfo                           │
│             CurrentModelID  string                                │
└──────────────────────────────────────────────────────────────────┘

                                │
                                ▼
┌──────────────────────────────────────────────────────────────────┐
│ READ: platform streamPrompt final edit                            │
│   _, mode  := conn.Modes()    (modeMu RLock, defensive copy)      │
│   _, model := conn.Models()   (modelMu RLock, defensive copy)     │
│   finalContent += FormatSessionFooter(mode, model)                │
│                  →  "\n\n— mode: `…` · model: `…`"               │
│                     (no italic; Telegram-HTML converter closes    │
│                      italic at backtick, garbling output)         │
└──────────────────────────────────────────────────────────────────┘
```

## File map

| File                            | What it owns                                                                                        |
| ------------------------------- | --------------------------------------------------------------------------------------------------- |
| `acp/protocol.go`               | `ModeInfo` / `ModelInfo` / `ModeSet` / `ModelSet`, `ClassifyNotification` event types + dispatch    |
| `acp/connection.go` (state)     | `modeMu` / `modelMu` fields, `Modes()` / `Models()` accessors, `SetCurrentMode/Model` setters       |
| `acp/connection.go` (ingress)   | `SessionNew` / `SessionLoad` `applyModeSet/ModelSet`, read loop dispatch of current_*_update events |
| `acp/connection.go` (RPC)       | `SessionSetMode` / `SessionSetModel` (session/set_mode, session/set_model)                          |
| `command/command.go`            | `ListModes` / `ExecuteMode`, `ListModels` / `ExecuteModel` — data pull vs actual switch             |
| `platform/platform.go`          | `FormatSessionFooter(mode, model)` — shared footer renderer                                         |
| `discord/handler.go`            | `/mode` `/model` SelectMenus + component dispatch; streamPrompt final edit appends footer          |
| `telegram/handler.go`           | `/mode` `/model` InlineKeyboards; streamPrompt final edit appends footer                           |
| `teams/handler.go`              | Text-only `/mode` `/model`; streamPrompt final edit appends footer                                  |
