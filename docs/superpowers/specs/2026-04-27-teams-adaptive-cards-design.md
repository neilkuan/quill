# Teams Adaptive Cards — `/mode` and `/model` Pickers

**Date:** 2026-04-27
**Branch:** `feat/teams-adaptive-cards`
**Status:** Spec — pending implementation plan

## Background

Quill's Discord and Telegram adapters render `/mode` and `/model` as
interactive widgets (SelectMenu, InlineKeyboard) — Teams falls back to
a numbered text list because the original adapter shipped without
Adaptive Card support. Users in Teams have to read the list, count to
the right entry, then type `/mode 4` or `/mode kiro_default`.

This spec covers raising Teams to feature parity for these two
commands by emitting an Adaptive Card with a dropdown + Submit button.
Other commands (`/pick`, `/help`, `/reset`, welcome card) are explicitly
out of scope and tracked separately.

## Goals

- Send a tappable Adaptive Card when the user types `/mode` or `/model`
  with no argument.
- Use a single dropdown (`Input.ChoiceSet` style=compact) so the same
  card scales from 3 modes up to 12+ models.
- After the user clicks "Switch", update the same card in place to a
  ✅ confirmation — no extra chat noise.
- Preserve the existing text path: `/mode <id>` and `/mode <N>` still
  switch directly without surfacing a card.
- Touch only the `teams/` package — `acp/`, `command/`, Discord, and
  Telegram adapters stay untouched.

## Non-goals

- Cards for `/pick`, `/help`, `/reset`, welcome on install.
- Stale card refresh (`Action.Refresh`) when the agent reloads modes.
- Localization — English text matches existing message style.
- Per-scope variation — same card payload in personal / team / group.
- Throttling beyond what Bot Framework already enforces.

## Architecture

```
User: /mode (no args)
     ↓
teams/handler.go  handleCommand → CmdMode case
     ↓
command.ListModes(pool, threadKey)              (existing API)
     ↓
listing.Err == nil && len(Available) > 0 ?
  ├── yes → sendModeCard(activity, listing)     (NEW)
  │           ↓
  │         client.SendActivity(card payload)   (existing API, signature change)
  │
  └── no  → existing text fallback via ExecuteMode

User: picks dropdown option, clicks "Switch"
     ↓
Bot Framework POST /api/messages
  type:"message", value:{quill.action, thread, mode}, replyToId:<cardID>
     ↓
adapter.go handleActivity (type=="message")
     ↓
UnmarshalInvokeData → has quill.action key ?
  ├── yes → handler.OnInvokeAction(activity)    (NEW)
  └── no  → handler.OnMessage(activity)         (existing)
     ↓
OnInvokeAction routes by quill.action:
  "switch_mode"  → command.ExecuteMode  → BuildModeConfirmation
  "switch_model" → command.ExecuteModel → BuildModelConfirmation
     ↓
client.UpdateActivity(replyToId, confirmationPayload)   (existing API)
```

`acp/`, `command/`, Discord, and Telegram are unchanged. The
`command.ListModes` / `ListModels` / `ExecuteMode` / `ExecuteModel`
functions already separate the data-pull from the switch — we reuse
them as-is.

## Components

### New files

| File | Responsibility | Approx. LoC |
|---|---|---|
| `teams/cards.go` | Adaptive Card payload builders: `BuildModeCard(listing, threadKey)`, `BuildModelCard(...)`, `BuildModeConfirmation(prev, new, label, errMsg)`, `BuildModelConfirmation(...)`. Returns an `Attachment` with `contentType="application/vnd.microsoft.card.adaptive"` and a typed `content` struct (no hand-written JSON). | ~250 |
| `teams/cards_test.go` | Table-driven tests: feed each builder a representative `ModeListing` / `ModelListing` (0, 1, 12 entries) and assert structure: contentType, ChoiceSet id, choices length, default value, Action.Submit data shape. | ~200 |
| `teams/invoke.go` | `InvokeData` struct, `UnmarshalInvokeData(*Activity)` helper, action constants (`actionSwitchMode = "switch_mode"`, `actionSwitchModel = "switch_model"`), `(h *Handler) OnInvokeAction(activity *Activity)` method. | ~120 |
| `teams/invoke_test.go` | Unit tests: malformed `value`, missing fields, thread mismatch, happy-path with fake `BotClient` capturing `UpdateActivity`. | ~150 |

### Modified files

| File | Change | Notes |
|---|---|---|
| `teams/types.go` | Add `Value json.RawMessage \`json:"value,omitempty"\`` to `Activity`. | Carries the messageBack payload. |
| `teams/adapter.go` | In `handleActivity`'s `case "message":` branch — try `UnmarshalInvokeData(&activity)` first; if it returns an `InvokeData` with a known action, route to `OnInvokeAction`; otherwise fall through to `OnMessage`. | +6 LoC. |
| `teams/handler.go` | `handleCommand` `CmdMode` / `CmdModel` cases: when `cmd.Args` is empty, call `command.ListModes` / `ListModels`; if the listing carries options, render a card via the new `sendModeCard` / `sendModelCard` helpers; otherwise keep existing text behavior. | +30 LoC. |
| `teams/handler.go` | New private methods `sendModeCard`, `sendModelCard` — package the listing into the card builder, call `client.SendActivity`, log on send failure. | +40 LoC. |
| `teams/client.go` | Change `SendActivity` to return `(activityID string, err error)`. The activity ID is required by `UpdateActivity` to target the same card. | All call sites updated; ~5 LoC plus call sites. |

### Adaptive Card payload (mode picker example)

```json
{
  "type": "AdaptiveCard",
  "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
  "version": "1.5",
  "body": [
    {"type": "TextBlock", "text": "Switch agent mode", "weight": "Bolder", "size": "Medium"},
    {"type": "TextBlock", "text": "Current: `kiro_default`", "isSubtle": true, "wrap": true},
    {
      "type": "Input.ChoiceSet",
      "id": "mode",
      "style": "compact",
      "value": "kiro_default",
      "choices": [
        {"title": "kiro_default — General agent", "value": "kiro_default"},
        {"title": "kiro_spec — Spec planner",     "value": "kiro_spec"}
      ]
    }
  ],
  "actions": [
    {
      "type": "Action.Submit",
      "title": "Switch",
      "data": {"quill.action": "switch_mode", "thread": "teams:a:1JEz..."}
    }
  ]
}
```

When the user clicks Submit, the inbound activity's `value` will be:

```json
{"quill.action": "switch_mode", "thread": "teams:a:1JEz...", "mode": "kiro_spec"}
```

Plain `Action.Submit` (no `msteams.type`) keeps the click silent —
the user does not see their submission as a message in the chat.

## Error handling

| Scenario | Behavior | UX impact |
|---|---|---|
| `/mode` no args, no active session | `ListModes` → `Err != nil` → existing text fallback (`"send a message first…"`). | Same as today. |
| `/mode` no args, agent advertises 0 modes | `ListModes` → `Available` empty → existing text fallback (`"current agent did not advertise…"`). | No empty card. |
| `Action.Submit` `value.mode` empty | `Input.ChoiceSet`'s `value` defaults to `Current`, so it's never empty in practice. If still empty, `OnInvokeAction` updates the card to `❌ Selection missing — please re-open the picker with /mode.` | Card flips to error, no broken widget. |
| `value.thread` does not match current conversation key | `OnInvokeAction` updates card to `❌ This picker belongs to a different conversation.` | Prevents stale-card cross-thread misuse (e.g., a thread reply triggering a parent-thread card). |
| `command.ExecuteMode` returns an error string | `BuildModeConfirmation` renders the ❌ branch with that string. | Reuses existing wording. |
| `client.UpdateActivity` fails (403 / 404 / 5xx) | Log warn; fall back to `client.SendActivity` of `⚠️ Switched to {x}, but card update failed.` | User still sees the result. |
| Malformed `value` JSON | `UnmarshalInvokeData` returns error; fall through to `OnMessage` (treat as plain text). | Robust to weird clients. |
| Connection evicted between card send and Submit | `command.ExecuteMode` returns the existing "no active session" message; ❌ branch renders. | Same as today's stale-session UX. |

### Out of scope (explicit non-handling)

- Stale card auto-refresh — defer to v2.
- i18n — English only.
- Personal vs channel card variation — identical payload everywhere.
- Custom rate limiting — Bot Framework's defaults are sufficient.

## Testing strategy

### Unit tests (hermetic, run in CI)

| Target | Coverage |
|---|---|
| `BuildModeCard` / `BuildModelCard` | Feed `ModeListing` with 0, 1, and 12 entries. Assert: contentType, ChoiceSet id (`"mode"` / `"model"`), choices length, default `value` equals `Current`, `Action.Submit.data["quill.action"]` and `data["thread"]` match. |
| `BuildModeConfirmation` | Success path: title contains `✅`, body mentions previous + new label. Error path: title contains `❌`, body contains the supplied error string. |
| `BuildModelConfirmation` | Same matrix as above. |
| `UnmarshalInvokeData` | Table-driven: well-formed payload, missing `quill.action`, non-JSON-object `value`, unknown action — each yields the documented error / state. |
| `OnInvokeAction` (fake `BotClient`) | (a) `switch_mode` happy path: `ExecuteMode` called with the right id, `UpdateActivity` called with confirmation. (b) thread mismatch: card updated to error, `ExecuteMode` not called. (c) `ExecuteMode` returns error string: ❌ branch. (d) `UpdateActivity` returns error: fallback `SendActivity` invoked. |

### Modified tests

| Test | Change |
|---|---|
| `teams/handler_test.go` (if present) | New cases: empty-args `CmdMode` / `CmdModel` produce a card attachment, not a plain-text body. |
| `teams/adapter_test.go` | New case: activity with `value` containing `quill.action` routes to `OnInvokeAction` rather than `OnMessage`. |
| `teams/client_test.go` | `SendActivity` signature change cascades into existing tests. |

### Integration / smoke

No hermetic integration test for card rendering — only Teams clients
can actually paint Adaptive Cards. The PR description's Test plan will
list the manual sideload steps:

1. Repackage `teams/appmanifest/` (bump `version` if Microsoft cache
   refuses).
2. Re-sideload to a personal chat and a team channel.
3. `/mode` → expect dropdown card → switch → expect ✅ in-place update.
4. `/model` → same flow.
5. Ask a follow-up question — confirm the agent is on the new mode /
   model (look at quill's `QUILL_LOG=debug` for the `mode` /
   `model` field).

### Coverage targets

`teams/cards.go` and `teams/invoke.go` ≥ 80% line coverage. The error
fallback path (`UpdateActivity` failing → `SendActivity`) is exercised
once via the fake client; we don't pursue 100% on rare HTTP paths.

### Definition of Done

- [ ] `go build ./...`, `go vet ./...`, `go test ./... -count=1` all green.
- [ ] `BuildModeCard` sample output passes `python3 -m json.tool`.
- [ ] Manual sideload verification (steps above) reproduced in personal
      chat, team channel, and private channel — all three flip the card
      to ✅ on Submit.
- [ ] After flip, sending a fresh prompt logs the new mode / model.

## Open questions

- None at the time of writing. The agreed defaults cover every
  decision point reached during brainstorming.

## References

- Adaptive Cards schema: <https://adaptivecards.io/schemas/adaptive-card.json>
- Teams `Action.Submit` semantics:
  <https://learn.microsoft.com/microsoftteams/platform/task-modules-and-cards/cards/cards-actions>
- Existing text path: `command/command.go` (`ListModes`, `ListModels`,
  `ExecuteMode`, `ExecuteModel`).
