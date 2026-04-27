# Teams Adaptive Cards (`/mode`, `/model`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Teams' text-only `/mode` and `/model` listings with an Adaptive Card that renders a dropdown + Submit button, matching the interactive-widget UX Discord and Telegram already provide.

**Architecture:** Plain `Action.Submit` (no `msteams.type`) keeps the click silent — the messageBack lands at the existing `/api/messages` endpoint as a `type=message` activity with a populated `value` field. A new `OnInvokeAction` path peels those off before `OnMessage` handles them, dispatches to the existing `command.ExecuteMode` / `ExecuteModel`, then `UpdateActivity` rewrites the original card to a ✅ confirmation. `acp/`, `command/`, Discord, and Telegram are untouched.

**Tech Stack:** Go 1.x, Bot Framework v3 REST API, Adaptive Card schema 1.5, `httptest` for unit tests, existing fixtures in `teams/client_test.go` style.

**Spec:** [`docs/superpowers/specs/2026-04-27-teams-adaptive-cards-design.md`](../specs/2026-04-27-teams-adaptive-cards-design.md)

---

## File Plan

**New files**

- `teams/cards.go` — Adaptive Card builders. Single responsibility: turn a `command.ModeListing` / `ModelListing` (or a switch outcome) into a `teams.Attachment` with a typed `AdaptiveCard` payload.
- `teams/cards_test.go` — Unit tests for every builder. Hermetic (no network).
- `teams/invoke.go` — `InvokeData` struct, `UnmarshalInvokeData` helper, action constants, `(h *Handler) OnInvokeAction` method, `(h *Handler) sendModeCard` / `sendModelCard` send-side helpers.
- `teams/invoke_test.go` — Unit tests for `UnmarshalInvokeData` plus `OnInvokeAction`'s guard / fallback paths against an `httptest.Server` Bot Framework stub (same pattern as `client_test.go`).

**Modified files**

- `teams/types.go` — Add `Value json.RawMessage` to `Activity`.
- `teams/adapter.go` — In `handleActivity` `case "message":`, peek `activity.Value`; if it carries a known `quill.action`, route to `OnInvokeAction` instead of `OnMessage`.
- `teams/handler.go` — In `handleCommand`, when `cmd.Args == ""` for `CmdMode` / `CmdModel`, call `command.ListModes` / `ListModels` and forward to `sendModeCard` / `sendModelCard`. The text fallback path is preserved when the listing has no usable rows.

**Untouched** (sanity checklist for the implementer)

- `acp/`, `command/`, `discord/`, `telegram/` — not in scope.
- `teams/client.go` — `SendActivity` already returns `(*Activity, error)`, no signature change needed.
- `teams/auth.go` — bot auth is unrelated.

---

## Task 1: Add `Value` field to `Activity`

**Goal:** Make the inbound messageBack payload visible to handlers without disturbing existing fields.

**Files:**
- Modify: `teams/types.go`
- Test: `teams/types_test.go`

- [ ] **Step 1: Write the failing test** in `teams/types_test.go` (append to the existing test file)

```go
func TestActivity_DecodesValueField(t *testing.T) {
	raw := []byte(`{
		"type": "message",
		"text": "",
		"value": {"quill.action": "switch_mode", "thread": "teams:a:abc", "mode": "kiro_spec"},
		"replyToId": "msg-1"
	}`)

	var a Activity
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if a.ReplyToID != "msg-1" {
		t.Errorf("ReplyToID = %q, want %q", a.ReplyToID, "msg-1")
	}
	if len(a.Value) == 0 {
		t.Fatalf("Value is empty — field did not decode")
	}

	var v map[string]string
	if err := json.Unmarshal(a.Value, &v); err != nil {
		t.Fatalf("Value re-decode: %v", err)
	}
	if v["quill.action"] != "switch_mode" {
		t.Errorf(`Value["quill.action"] = %q, want "switch_mode"`, v["quill.action"])
	}
	if v["mode"] != "kiro_spec" {
		t.Errorf(`Value["mode"] = %q, want "kiro_spec"`, v["mode"])
	}
}
```

The test file currently has no `encoding/json` import — make sure to add it: `import "encoding/json"`.

- [ ] **Step 2: Run test to verify it fails**

```
go test ./teams/ -run TestActivity_DecodesValueField -count=1
```

Expected: FAIL with `Value is empty — field did not decode`.

- [ ] **Step 3: Add the field**

In `teams/types.go`, add the `encoding/json` import (or move existing imports into a parenthesized block) and add the field. The full updated `Activity` struct:

```go
package teams

import "encoding/json"

type Activity struct {
	Type         string          `json:"type"`
	ID           string          `json:"id,omitempty"`
	Timestamp    string          `json:"timestamp,omitempty"`
	ServiceURL   string          `json:"serviceUrl,omitempty"`
	ChannelID    string          `json:"channelId,omitempty"`
	From         Account         `json:"from,omitempty"`
	Conversation Conversation    `json:"conversation,omitempty"`
	Recipient    Account         `json:"recipient,omitempty"`
	Text         string          `json:"text,omitempty"`
	TextFormat   string          `json:"textFormat,omitempty"`
	Attachments  []Attachment    `json:"attachments,omitempty"`
	Entities     []Entity        `json:"entities,omitempty"`
	ReplyToID    string          `json:"replyToId,omitempty"`
	Value        json.RawMessage `json:"value,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

```
go test ./teams/ -run TestActivity_DecodesValueField -count=1
```

Expected: PASS.

- [ ] **Step 5: Run the full teams package test suite**

```
go test ./teams/... -count=1
```

Expected: all existing tests still PASS.

- [ ] **Step 6: Commit**

```bash
git add teams/types.go teams/types_test.go
git commit -m "feat(teams): add Value field to Activity for messageBack payload"
```

---

## Task 2: Define typed Adaptive Card payload structs

**Goal:** Provide strongly-typed Go structs for the AdaptiveCard JSON shape so card builders don't hand-roll string concatenation. Marshalling these structs is the only thing the rest of the plan relies on.

**Files:**
- Create: `teams/cards.go`
- Create: `teams/cards_test.go`

- [ ] **Step 1: Write the failing test** in `teams/cards_test.go`

```go
package teams

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAdaptiveCard_MarshalsExpectedShape(t *testing.T) {
	card := AdaptiveCard{
		Type:    "AdaptiveCard",
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Version: "1.5",
		Body: []CardElement{
			TextBlock{Type: "TextBlock", Text: "Hello", Weight: "Bolder", Size: "Medium"},
		},
		Actions: []CardAction{
			SubmitAction{Type: "Action.Submit", Title: "OK", Data: map[string]string{"k": "v"}},
		},
	}

	out, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		`"type":"AdaptiveCard"`,
		`"$schema":"http://adaptivecards.io/schemas/adaptive-card.json"`,
		`"version":"1.5"`,
		`"type":"TextBlock"`,
		`"text":"Hello"`,
		`"type":"Action.Submit"`,
		`"title":"OK"`,
		`"data":{"k":"v"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshalled card missing %q\nfull: %s", want, got)
		}
	}
}

func TestAdaptiveCardAttachment_HasRightContentType(t *testing.T) {
	att := AdaptiveCardAttachment(AdaptiveCard{Type: "AdaptiveCard", Version: "1.5"})

	if att.ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("ContentType = %q, want %q", att.ContentType, "application/vnd.microsoft.card.adaptive")
	}
	if att.Content == nil {
		t.Fatal("Content is nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./teams/ -run TestAdaptiveCard -count=1
```

Expected: FAIL with `undefined: AdaptiveCard` (and friends).

- [ ] **Step 3: Implement the structs and helper** in `teams/cards.go`

```go
package teams

// AdaptiveCard is a minimal subset of the Adaptive Card 1.5 schema —
// just enough for the picker / confirmation cards we send. Hand-written
// JSON would be error-prone (typos in schema fields silently render as
// blank cards in Teams), so every field the codebase touches is typed.
type AdaptiveCard struct {
	Type    string        `json:"type"`               // always "AdaptiveCard"
	Schema  string        `json:"$schema,omitempty"`  // optional in v1.5
	Version string        `json:"version"`            // "1.5"
	Body    []CardElement `json:"body,omitempty"`
	Actions []CardAction  `json:"actions,omitempty"`
}

// CardElement is the marker for anything that can appear in body[]. We
// only model the elements we render, not the full schema.
type CardElement interface{ cardElement() }

// CardAction is the marker for anything that can appear in actions[].
type CardAction interface{ cardAction() }

// TextBlock renders a paragraph of (optionally markdown-flavoured) text.
type TextBlock struct {
	Type     string `json:"type"`               // always "TextBlock"
	Text     string `json:"text"`
	Wrap     bool   `json:"wrap,omitempty"`
	Weight   string `json:"weight,omitempty"`   // "Default" | "Lighter" | "Bolder"
	Size     string `json:"size,omitempty"`     // "Small" | "Default" | "Medium" | "Large" | "ExtraLarge"
	IsSubtle bool   `json:"isSubtle,omitempty"`
	Color    string `json:"color,omitempty"`    // "Default" | "Good" | "Warning" | "Attention" | ...
}

func (TextBlock) cardElement() {}

// ChoiceSet is an Input.ChoiceSet — used as the dropdown.
type ChoiceSet struct {
	Type    string   `json:"type"`             // always "Input.ChoiceSet"
	ID      string   `json:"id"`
	Style   string   `json:"style,omitempty"`  // "compact" | "expanded"
	Value   string   `json:"value,omitempty"`  // pre-selected choice
	Choices []Choice `json:"choices"`
}

func (ChoiceSet) cardElement() {}

// Choice is one entry inside a ChoiceSet.
type Choice struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

// SubmitAction is an Action.Submit. Plain (no msteams.type) — the post
// is silent; the user does not see their submission as a chat message.
type SubmitAction struct {
	Type  string         `json:"type"`            // always "Action.Submit"
	Title string         `json:"title"`
	Data  map[string]any `json:"data,omitempty"`
}

func (SubmitAction) cardAction() {}

// AdaptiveCardAttachment wraps an AdaptiveCard into the teams.Attachment
// envelope expected by Bot Framework's Activity.Attachments.
//
// Note: teams.Attachment currently models ContentURL/Name only — the
// Content field is required for inline cards. We add it here so callers
// don't have to construct the attachment by hand.
func AdaptiveCardAttachment(card AdaptiveCard) Attachment {
	return Attachment{
		ContentType: "application/vnd.microsoft.card.adaptive",
		Content:     card,
	}
}
```

- [ ] **Step 4: Update `teams/types.go` to allow `Content` on `Attachment`**

The current `Attachment` struct only has `ContentType`, `ContentURL`, `Name`. Add `Content any` to carry inline card payloads:

```go
type Attachment struct {
	ContentType string `json:"contentType,omitempty"`
	ContentURL  string `json:"contentUrl,omitempty"`
	Name        string `json:"name,omitempty"`
	Content     any    `json:"content,omitempty"`
}
```

- [ ] **Step 5: Run tests**

```
go build ./...
go test ./teams/ -run TestAdaptiveCard -count=1
go test ./teams/... -count=1
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add teams/cards.go teams/cards_test.go teams/types.go
git commit -m "feat(teams): add typed Adaptive Card payload structs"
```

---

## Task 3: `BuildModeCard`

**Goal:** Turn a `command.ModeListing` (with non-empty `Available`) into a card attachment with a dropdown of mode IDs.

**Files:**
- Modify: `teams/cards.go`
- Modify: `teams/cards_test.go`

- [ ] **Step 1: Write the failing test** — append to `teams/cards_test.go`

```go
import (
	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
)

func TestBuildModeCard_HappyPath(t *testing.T) {
	listing := command.ModeListing{
		Current: "kiro_default",
		Available: []acp.ModeInfo{
			{ID: "kiro_default", Name: "kiro_default", Description: "General agent"},
			{ID: "kiro_spec", Name: "kiro_spec", Description: "Spec planner"},
			{ID: "kiro_guide", Name: "kiro_guide"},
		},
	}

	att := BuildModeCard(listing, "teams:a:abc")
	if att.ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("ContentType = %q", att.ContentType)
	}
	card, ok := att.Content.(AdaptiveCard)
	if !ok {
		t.Fatalf("Content is not AdaptiveCard, got %T", att.Content)
	}

	// First TextBlock = title; second = current marker.
	if len(card.Body) < 3 {
		t.Fatalf("expected ≥3 body elements (title, current, choiceset), got %d", len(card.Body))
	}

	var choiceSet ChoiceSet
	for _, el := range card.Body {
		if cs, ok := el.(ChoiceSet); ok {
			choiceSet = cs
		}
	}
	if choiceSet.ID != "mode" {
		t.Errorf("ChoiceSet.ID = %q, want %q", choiceSet.ID, "mode")
	}
	if choiceSet.Style != "compact" {
		t.Errorf("ChoiceSet.Style = %q, want %q", choiceSet.Style, "compact")
	}
	if choiceSet.Value != "kiro_default" {
		t.Errorf("ChoiceSet.Value = %q (default), want %q", choiceSet.Value, "kiro_default")
	}
	if len(choiceSet.Choices) != 3 {
		t.Errorf("Choices length = %d, want 3", len(choiceSet.Choices))
	}
	// Title format: "{id} — {description}" if description present, else just id.
	wantTitles := []string{
		"kiro_default — General agent",
		"kiro_spec — Spec planner",
		"kiro_guide",
	}
	for i, c := range choiceSet.Choices {
		if c.Value != listing.Available[i].ID {
			t.Errorf("Choice[%d].Value = %q, want %q", i, c.Value, listing.Available[i].ID)
		}
		if c.Title != wantTitles[i] {
			t.Errorf("Choice[%d].Title = %q, want %q", i, c.Title, wantTitles[i])
		}
	}

	// Single Submit action, with quill.action and thread baked into data.
	if len(card.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(card.Actions))
	}
	submit, ok := card.Actions[0].(SubmitAction)
	if !ok {
		t.Fatalf("Actions[0] is not SubmitAction, got %T", card.Actions[0])
	}
	if submit.Title != "Switch" {
		t.Errorf("submit.Title = %q, want %q", submit.Title, "Switch")
	}
	if submit.Data["quill.action"] != "switch_mode" {
		t.Errorf(`Data["quill.action"] = %v, want "switch_mode"`, submit.Data["quill.action"])
	}
	if submit.Data["thread"] != "teams:a:abc" {
		t.Errorf(`Data["thread"] = %v, want "teams:a:abc"`, submit.Data["thread"])
	}
}

func TestBuildModeCard_TwelveOptions_StillCompact(t *testing.T) {
	available := make([]acp.ModeInfo, 12)
	for i := range available {
		available[i] = acp.ModeInfo{ID: fmt.Sprintf("m%d", i)}
	}
	listing := command.ModeListing{Current: "m0", Available: available}

	att := BuildModeCard(listing, "teams:a:abc")
	card := att.Content.(AdaptiveCard)
	var cs ChoiceSet
	for _, el := range card.Body {
		if c, ok := el.(ChoiceSet); ok {
			cs = c
		}
	}
	if len(cs.Choices) != 12 {
		t.Errorf("Choices = %d, want 12", len(cs.Choices))
	}
}
```

Add `"fmt"` to the imports of `cards_test.go`.

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./teams/ -run TestBuildModeCard -count=1
```

Expected: FAIL with `undefined: BuildModeCard`.

- [ ] **Step 3: Implement `BuildModeCard`** — append to `teams/cards.go`

```go
import (
	"fmt"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
)

const (
	cardSchemaURL = "http://adaptivecards.io/schemas/adaptive-card.json"
	cardVersion   = "1.5"

	// Action keys put in SubmitAction.Data["quill.action"]. The handler's
	// invoke router uses these to dispatch.
	actionSwitchMode  = "switch_mode"
	actionSwitchModel = "switch_model"
)

// BuildModeCard wraps a non-empty ModeListing into an Adaptive Card
// dropdown. Caller is responsible for checking listing.Err and Available
// before calling — this builder assumes the data is renderable.
func BuildModeCard(listing command.ModeListing, threadKey string) Attachment {
	choices := make([]Choice, len(listing.Available))
	for i, m := range listing.Available {
		choices[i] = Choice{
			Title: modeChoiceLabel(m),
			Value: m.ID,
		}
	}
	card := AdaptiveCard{
		Type:    "AdaptiveCard",
		Schema:  cardSchemaURL,
		Version: cardVersion,
		Body: []CardElement{
			TextBlock{Type: "TextBlock", Text: "Switch agent mode", Weight: "Bolder", Size: "Medium"},
			TextBlock{Type: "TextBlock", Text: fmt.Sprintf("Current: `%s`", listing.Current), IsSubtle: true, Wrap: true},
			ChoiceSet{
				Type:    "Input.ChoiceSet",
				ID:      "mode",
				Style:   "compact",
				Value:   listing.Current,
				Choices: choices,
			},
		},
		Actions: []CardAction{
			SubmitAction{
				Type:  "Action.Submit",
				Title: "Switch",
				Data: map[string]any{
					"quill.action": actionSwitchMode,
					"thread":       threadKey,
				},
			},
		},
	}
	return AdaptiveCardAttachment(card)
}

func modeChoiceLabel(m acp.ModeInfo) string {
	if m.Description != "" {
		return fmt.Sprintf("%s — %s", m.ID, m.Description)
	}
	return m.ID
}
```

- [ ] **Step 4: Run tests to verify pass**

```
go test ./teams/ -run TestBuildModeCard -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add teams/cards.go teams/cards_test.go
git commit -m "feat(teams): add BuildModeCard for Adaptive Card mode picker"
```

---

## Task 4: `BuildModelCard`

**Goal:** Symmetrical builder for `/model`, sharing the same shape as `BuildModeCard` — different ChoiceSet id (`"model"`), different action key, different title.

**Files:**
- Modify: `teams/cards.go`
- Modify: `teams/cards_test.go`

- [ ] **Step 1: Write the failing test** — append to `teams/cards_test.go`

```go
func TestBuildModelCard_HappyPath(t *testing.T) {
	listing := command.ModelListing{
		Current: "claude-sonnet-4.6",
		Available: []acp.ModelInfo{
			{ID: "auto", Name: "auto", Description: "Models chosen by task"},
			{ID: "claude-sonnet-4.6", Name: "claude-sonnet-4.6", Description: "Latest Claude Sonnet"},
			{ID: "claude-opus-4.6", Name: "claude-opus-4.6"},
		},
	}

	att := BuildModelCard(listing, "teams:a:abc")
	card := att.Content.(AdaptiveCard)

	var cs ChoiceSet
	for _, el := range card.Body {
		if c, ok := el.(ChoiceSet); ok {
			cs = c
		}
	}
	if cs.ID != "model" {
		t.Errorf("ChoiceSet.ID = %q, want %q", cs.ID, "model")
	}
	if cs.Value != "claude-sonnet-4.6" {
		t.Errorf("default = %q, want claude-sonnet-4.6", cs.Value)
	}
	if len(cs.Choices) != 3 {
		t.Errorf("Choices = %d, want 3", len(cs.Choices))
	}

	submit := card.Actions[0].(SubmitAction)
	if submit.Data["quill.action"] != "switch_model" {
		t.Errorf(`Data["quill.action"] = %v, want "switch_model"`, submit.Data["quill.action"])
	}
	if submit.Data["thread"] != "teams:a:abc" {
		t.Errorf(`Data["thread"] = %v, want "teams:a:abc"`, submit.Data["thread"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./teams/ -run TestBuildModelCard -count=1
```

Expected: FAIL.

- [ ] **Step 3: Implement `BuildModelCard`** — append to `teams/cards.go`

```go
// BuildModelCard wraps a non-empty ModelListing into an Adaptive Card
// dropdown. Symmetrical to BuildModeCard.
func BuildModelCard(listing command.ModelListing, threadKey string) Attachment {
	choices := make([]Choice, len(listing.Available))
	for i, m := range listing.Available {
		choices[i] = Choice{
			Title: modelChoiceLabel(m),
			Value: m.ID,
		}
	}
	card := AdaptiveCard{
		Type:    "AdaptiveCard",
		Schema:  cardSchemaURL,
		Version: cardVersion,
		Body: []CardElement{
			TextBlock{Type: "TextBlock", Text: "Switch LLM model", Weight: "Bolder", Size: "Medium"},
			TextBlock{Type: "TextBlock", Text: fmt.Sprintf("Current: `%s`", listing.Current), IsSubtle: true, Wrap: true},
			ChoiceSet{
				Type:    "Input.ChoiceSet",
				ID:      "model",
				Style:   "compact",
				Value:   listing.Current,
				Choices: choices,
			},
		},
		Actions: []CardAction{
			SubmitAction{
				Type:  "Action.Submit",
				Title: "Switch",
				Data: map[string]any{
					"quill.action": actionSwitchModel,
					"thread":       threadKey,
				},
			},
		},
	}
	return AdaptiveCardAttachment(card)
}

func modelChoiceLabel(m acp.ModelInfo) string {
	if m.Description != "" {
		return fmt.Sprintf("%s — %s", m.ID, m.Description)
	}
	return m.ID
}
```

- [ ] **Step 4: Run tests**

```
go test ./teams/ -run TestBuildModelCard -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add teams/cards.go teams/cards_test.go
git commit -m "feat(teams): add BuildModelCard for Adaptive Card model picker"
```

---

## Task 5: `BuildModeConfirmation` and `BuildModelConfirmation`

**Goal:** Render the post-submit "✅ switched" or "❌ failed" card that replaces the picker via `UpdateActivity`.

**Files:**
- Modify: `teams/cards.go`
- Modify: `teams/cards_test.go`

- [ ] **Step 1: Write the failing test** — append to `teams/cards_test.go`

```go
func TestBuildModeConfirmation_Success(t *testing.T) {
	att := BuildModeConfirmation("kiro_default", "kiro_spec", "")
	card := att.Content.(AdaptiveCard)

	if len(card.Actions) != 0 {
		t.Errorf("expected no actions on confirmation card, got %d", len(card.Actions))
	}
	first := card.Body[0].(TextBlock)
	if !strings.Contains(first.Text, "✅") {
		t.Errorf("title missing ✅: %q", first.Text)
	}
	all := strings.Builder{}
	for _, el := range card.Body {
		if tb, ok := el.(TextBlock); ok {
			all.WriteString(tb.Text)
			all.WriteString("\n")
		}
	}
	body := all.String()
	if !strings.Contains(body, "kiro_default") {
		t.Errorf("body missing previous mode: %s", body)
	}
	if !strings.Contains(body, "kiro_spec") {
		t.Errorf("body missing new mode: %s", body)
	}
}

func TestBuildModeConfirmation_Error(t *testing.T) {
	att := BuildModeConfirmation("kiro_default", "kiro_spec", "agent rejected the switch")
	card := att.Content.(AdaptiveCard)

	first := card.Body[0].(TextBlock)
	if !strings.Contains(first.Text, "❌") {
		t.Errorf("title missing ❌: %q", first.Text)
	}
	bodyText := ""
	for _, el := range card.Body {
		if tb, ok := el.(TextBlock); ok {
			bodyText += tb.Text + "\n"
		}
	}
	if !strings.Contains(bodyText, "agent rejected the switch") {
		t.Errorf("error message missing from card body: %s", bodyText)
	}
}

func TestBuildModelConfirmation_Success(t *testing.T) {
	att := BuildModelConfirmation("auto", "claude-opus-4.6", "")
	card := att.Content.(AdaptiveCard)
	first := card.Body[0].(TextBlock)
	if !strings.Contains(first.Text, "✅") {
		t.Errorf("title missing ✅: %q", first.Text)
	}
	bodyText := ""
	for _, el := range card.Body {
		if tb, ok := el.(TextBlock); ok {
			bodyText += tb.Text + "\n"
		}
	}
	if !strings.Contains(bodyText, "auto") || !strings.Contains(bodyText, "claude-opus-4.6") {
		t.Errorf("body missing prev/new model: %s", bodyText)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./teams/ -run TestBuildMo -count=1
```

Expected: FAIL with `undefined: BuildModeConfirmation` (the regex `BuildMo` matches both Mode and Model).

- [ ] **Step 3: Implement both confirmations** — append to `teams/cards.go`

```go
// BuildModeConfirmation renders the post-submit confirmation card. When
// errMsg is empty, the card shows ✅ Switched. When errMsg is non-empty,
// it shows ❌ with the error string and no "from→to" arrow (the switch
// did not happen).
func BuildModeConfirmation(prev, next, errMsg string) Attachment {
	return buildConfirmation("agent mode", prev, next, errMsg)
}

// BuildModelConfirmation is the /model analogue of BuildModeConfirmation.
func BuildModelConfirmation(prev, next, errMsg string) Attachment {
	return buildConfirmation("LLM model", prev, next, errMsg)
}

func buildConfirmation(label, prev, next, errMsg string) Attachment {
	body := []CardElement{}
	if errMsg == "" {
		body = append(body,
			TextBlock{Type: "TextBlock", Text: fmt.Sprintf("✅ Switched %s", label), Weight: "Bolder", Size: "Medium"},
			TextBlock{Type: "TextBlock", Text: fmt.Sprintf("`%s` → `%s`", prev, next), IsSubtle: true, Wrap: true},
		)
	} else {
		body = append(body,
			TextBlock{Type: "TextBlock", Text: fmt.Sprintf("❌ Failed to switch %s", label), Weight: "Bolder", Size: "Medium", Color: "Attention"},
			TextBlock{Type: "TextBlock", Text: errMsg, Wrap: true},
		)
	}
	return AdaptiveCardAttachment(AdaptiveCard{
		Type:    "AdaptiveCard",
		Schema:  cardSchemaURL,
		Version: cardVersion,
		Body:    body,
	})
}
```

- [ ] **Step 4: Run tests**

```
go test ./teams/ -run TestBuildMo -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add teams/cards.go teams/cards_test.go
git commit -m "feat(teams): add Mode/Model confirmation card builders"
```

---

## Task 6: `InvokeData` + `UnmarshalInvokeData`

**Goal:** Decode the `Activity.Value` JSON into a typed struct that downstream code can switch on. Anything malformed must return an error so the dispatcher can fall through to `OnMessage`.

**Files:**
- Create: `teams/invoke.go`
- Create: `teams/invoke_test.go`

- [ ] **Step 1: Write the failing test** in `teams/invoke_test.go`

```go
package teams

import (
	"encoding/json"
	"testing"
)

func TestUnmarshalInvokeData_HappyPath_Mode(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`{"quill.action":"switch_mode","thread":"teams:a:abc","mode":"kiro_spec"}`),
	}

	data, err := UnmarshalInvokeData(a)
	if err != nil {
		t.Fatalf("UnmarshalInvokeData: %v", err)
	}
	if data.Action != "switch_mode" {
		t.Errorf("Action = %q, want %q", data.Action, "switch_mode")
	}
	if data.Thread != "teams:a:abc" {
		t.Errorf("Thread = %q, want %q", data.Thread, "teams:a:abc")
	}
	if data.Mode != "kiro_spec" {
		t.Errorf("Mode = %q, want %q", data.Mode, "kiro_spec")
	}
}

func TestUnmarshalInvokeData_HappyPath_Model(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`{"quill.action":"switch_model","thread":"teams:a:abc","model":"claude-opus-4.6"}`),
	}
	data, err := UnmarshalInvokeData(a)
	if err != nil {
		t.Fatalf("UnmarshalInvokeData: %v", err)
	}
	if data.Action != "switch_model" {
		t.Errorf("Action = %q", data.Action)
	}
	if data.Model != "claude-opus-4.6" {
		t.Errorf("Model = %q", data.Model)
	}
}

func TestUnmarshalInvokeData_NoValue_NotInvoke(t *testing.T) {
	a := &Activity{Type: "message", Text: "hi"}

	_, err := UnmarshalInvokeData(a)
	if err == nil {
		t.Fatal("expected error when Value is empty")
	}
	if !errIsNotInvoke(err) {
		t.Errorf("expected ErrNotInvoke, got %v", err)
	}
}

func TestUnmarshalInvokeData_MissingAction(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`{"thread":"teams:a:abc"}`),
	}
	_, err := UnmarshalInvokeData(a)
	if err == nil {
		t.Fatal("expected error when quill.action missing")
	}
	if !errIsNotInvoke(err) {
		t.Errorf("expected ErrNotInvoke, got %v", err)
	}
}

func TestUnmarshalInvokeData_NonJSONValue(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`"some string"`),
	}
	_, err := UnmarshalInvokeData(a)
	if err == nil {
		t.Fatal("expected error when Value is not a JSON object")
	}
}

// errIsNotInvoke is a tiny helper that uses errors.Is — defined in the
// production package, but since invoke.go declares the sentinel we just
// reach for it directly.
func errIsNotInvoke(err error) bool {
	return err != nil && (err == ErrNotInvoke || err.Error() == ErrNotInvoke.Error())
}
```

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./teams/ -run TestUnmarshalInvoke -count=1
```

Expected: FAIL with `undefined: UnmarshalInvokeData` and `undefined: ErrNotInvoke`.

- [ ] **Step 3: Implement** `teams/invoke.go`

```go
package teams

import (
	"encoding/json"
	"errors"
)

// ErrNotInvoke means the activity does not carry a recognisable quill
// invoke payload — the caller should fall through to the regular
// message-handling path.
var ErrNotInvoke = errors.New("teams: activity is not a quill invoke")

// InvokeData is the decoded Action.Submit payload our cards send back.
// All optional fields use omitempty so a single struct works for both
// switch_mode and switch_model.
type InvokeData struct {
	Action string `json:"quill.action"`
	Thread string `json:"thread"`
	Mode   string `json:"mode,omitempty"`
	Model  string `json:"model,omitempty"`
}

// UnmarshalInvokeData decodes activity.Value into an InvokeData. Returns
// ErrNotInvoke (wrapped or sentinel) when the payload is missing or
// lacks a quill.action key, so the dispatcher knows to fall through.
func UnmarshalInvokeData(activity *Activity) (InvokeData, error) {
	if len(activity.Value) == 0 {
		return InvokeData{}, ErrNotInvoke
	}
	// Reject scalar values (e.g. plain string) up front — json.Unmarshal
	// into a struct from a non-object would silently produce a zero value.
	if activity.Value[0] != '{' {
		return InvokeData{}, errors.New("teams: invoke value is not a JSON object")
	}
	var d InvokeData
	if err := json.Unmarshal(activity.Value, &d); err != nil {
		return InvokeData{}, err
	}
	if d.Action == "" {
		return InvokeData{}, ErrNotInvoke
	}
	return d, nil
}
```

- [ ] **Step 4: Run tests**

```
go test ./teams/ -run TestUnmarshalInvoke -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add teams/invoke.go teams/invoke_test.go
git commit -m "feat(teams): add InvokeData decoder for messageBack payloads"
```

---

## Task 7: `OnInvokeAction` — guard paths only

**Goal:** Add the handler method that takes an invoke `Activity`, validates the thread, and (in this task) covers the failure paths that don't need a real ACP pool: thread mismatch, missing `mode`/`model` field, unknown action. The happy path that calls `command.ExecuteMode` / `ExecuteModel` is wired in Task 9.

**Files:**
- Modify: `teams/invoke.go`
- Modify: `teams/invoke_test.go`

- [ ] **Step 1: Write the failing test** — append to `teams/invoke_test.go`

```go
import (
	"context"
	"net/http"
	"net/http/httptest"
)

// captureUpdate keeps the most recent UpdateActivity payload so tests
// can assert on it. The Bot Framework stub server is set up to feed
// requests through it.
type captureUpdate struct {
	method string
	path   string
	body   Activity
}

func newBotFrameworkStub(t *testing.T, cap *captureUpdate) (*httptest.Server, *BotClient) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&cap.body)
		_, _ = w.Write([]byte(`{"id":"updated-1"}`))
	}))
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock", "expires_in": 3600})
	}))
	t.Cleanup(func() {
		ts.Close()
		tokenServer.Close()
	})
	auth := &BotAuth{appID: "app", appSecret: "sec", tenantID: "tn", tokenURL: tokenServer.URL}
	return ts, NewBotClient(auth)
}

// makeInvokeActivity returns a minimal invoke-shaped activity ready for
// OnInvokeAction. The `serviceURL` lets tests point Bot Framework calls
// at httptest stubs.
func makeInvokeActivity(serviceURL, conversationID, replyToID, action, thread, modeOrModelKey, modeOrModelVal string) *Activity {
	value := map[string]string{
		"quill.action":   action,
		"thread":         thread,
		modeOrModelKey:   modeOrModelVal,
	}
	raw, _ := json.Marshal(value)
	return &Activity{
		Type:         "message",
		ServiceURL:   serviceURL,
		Conversation: Conversation{ID: conversationID},
		ReplyToID:    replyToID,
		Value:        json.RawMessage(raw),
	}
}

// drainCtx is unused by these tests but documents that the invoke
// handler should not depend on a real context.
var _ = context.Background

func TestOnInvokeAction_ThreadMismatch_RendersErrorCard(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	h := &Handler{Client: client}
	a := makeInvokeActivity(ts.URL, "conv-different", "card-1",
		"switch_mode", "teams:a:STALE", "mode", "kiro_spec")

	h.OnInvokeAction(a)

	if cap.method != http.MethodPut {
		t.Errorf("expected UpdateActivity (PUT), got %s", cap.method)
	}
	if cap.path != "/v3/conversations/conv-different/activities/card-1" {
		t.Errorf("unexpected update path: %s", cap.path)
	}
	if len(cap.body.Attachments) == 0 {
		t.Fatalf("expected attachment with error card")
	}
	att := cap.body.Attachments[0]
	if att.ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("ContentType = %q", att.ContentType)
	}
	// Body[0].text should mention the thread-mismatch reason.
	contentJSON, _ := json.Marshal(att.Content)
	if !bytes.Contains(contentJSON, []byte("different conversation")) {
		t.Errorf("error card body missing thread-mismatch text: %s", contentJSON)
	}
}

func TestOnInvokeAction_MissingModeField_RendersErrorCard(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	h := &Handler{Client: client}
	a := makeInvokeActivity(ts.URL, "conv-X", "card-1",
		"switch_mode", "teams:conv-X", "mode", "")

	h.OnInvokeAction(a)

	if len(cap.body.Attachments) == 0 {
		t.Fatalf("expected attachment with error card")
	}
	contentJSON, _ := json.Marshal(cap.body.Attachments[0].Content)
	if !bytes.Contains(contentJSON, []byte("Selection missing")) {
		t.Errorf("error card missing selection-missing text: %s", contentJSON)
	}
}

func TestOnInvokeAction_UnknownAction_NoOp(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	h := &Handler{Client: client}
	a := makeInvokeActivity(ts.URL, "conv-X", "card-1",
		"unknown_action", "teams:conv-X", "mode", "x")

	h.OnInvokeAction(a)

	if cap.method != "" {
		t.Errorf("expected no Bot Framework call for unknown action, got %s %s", cap.method, cap.path)
	}
}
```

Add `"bytes"` to the imports of `invoke_test.go`.

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./teams/ -run TestOnInvokeAction -count=1
```

Expected: FAIL with `undefined: (*Handler).OnInvokeAction`.

- [ ] **Step 3: Implement `OnInvokeAction`** — append to `teams/invoke.go`

```go
import (
	"fmt"
	"log/slog"
)

// OnInvokeAction handles activities whose Value field carries a
// quill.action key — i.e., the messageBack from one of our Adaptive
// Cards. It validates the payload, dispatches to the right command, and
// rewrites the original card via UpdateActivity to show the result.
//
// Happy-path dispatch (calling command.ExecuteMode / ExecuteModel) is
// added in the next task. This task wires the guard paths.
func (h *Handler) OnInvokeAction(activity *Activity) {
	data, err := UnmarshalInvokeData(activity)
	if err != nil {
		slog.Debug("teams: not an invoke activity", "error", err)
		return
	}

	// Thread guard — the card's data["thread"] is set when the card is
	// built. If a stale card from another conversation gets clicked, we
	// refuse rather than mutate the wrong session.
	expectedThread := buildSessionKey(activity.Conversation.ID)
	if data.Thread != expectedThread {
		h.updateCard(activity, BuildModeConfirmation("", "", "This picker belongs to a different conversation."))
		return
	}

	switch data.Action {
	case actionSwitchMode:
		if data.Mode == "" {
			h.updateCard(activity, BuildModeConfirmation("", "", "Selection missing — please re-open the picker with /mode."))
			return
		}
		// Real switch is wired in Task 9; for now, no-op.
		// (Tests for that path live in Task 9.)
	case actionSwitchModel:
		if data.Model == "" {
			h.updateCard(activity, BuildModelConfirmation("", "", "Selection missing — please re-open the picker with /model."))
			return
		}
		// Wired in Task 9.
	default:
		slog.Debug("teams: unknown invoke action — ignoring", "action", data.Action)
		return
	}
}

// updateCard wraps the BotClient.UpdateActivity call. On failure, falls
// back to a fresh SendActivity with a plain-text warning so the user
// still sees the result.
func (h *Handler) updateCard(activity *Activity, card Attachment) {
	resp := &Activity{
		Type:        "message",
		Attachments: []Attachment{card},
	}
	err := h.Client.UpdateActivity(activity.ServiceURL, activity.Conversation.ID, activity.ReplyToID, resp)
	if err == nil {
		return
	}
	slog.Warn("teams: UpdateActivity failed, falling back to new SendActivity", "error", err)
	_, _ = h.Client.SendActivity(activity.ServiceURL, activity.Conversation.ID, &Activity{
		Type:       "message",
		Text:       fmt.Sprintf("⚠️ Card update failed: %v", err),
		TextFormat: "markdown",
	})
}
```

- [ ] **Step 4: Run tests**

```
go test ./teams/ -run TestOnInvokeAction -count=1
go test ./teams/... -count=1
```

Expected: target tests PASS; existing teams/... tests still PASS.

- [ ] **Step 5: Commit**

```bash
git add teams/invoke.go teams/invoke_test.go
git commit -m "feat(teams): add OnInvokeAction with thread-guard and missing-selection paths"
```

---

## Task 8: Wire invoke routing in `handleActivity`

**Goal:** When a `type=message` activity arrives with a `Value` carrying a quill action, send it to `OnInvokeAction` instead of `OnMessage`.

**Files:**
- Modify: `teams/adapter.go`
- Modify: `teams/adapter_test.go`

- [ ] **Step 1: Write the failing test** — append to `teams/adapter_test.go`

```go
func TestActivityDispatch_RoutesInvokeToOnInvokeAction(t *testing.T) {
	auth := &BotAuth{appID: "test", appSecret: "test", tenantID: "test"}

	called := make(chan struct{}, 1)
	handler := &Handler{
		ToolDisplay:        "compact",
		invokeForTest:      func() { called <- struct{}{} },
		messageForTestFlag: false,
	}

	mux := buildMuxSkipAuth(auth, handler)

	activity := Activity{
		Type:  "message",
		Value: json.RawMessage(`{"quill.action":"switch_mode","thread":"teams:conv","mode":"x"}`),
	}
	body, _ := json.Marshal(activity)
	req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	select {
	case <-called:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("OnInvokeAction was not called within 2s")
	}
	if handler.messageForTestFlag {
		t.Error("OnMessage should NOT have been called for invoke payload")
	}
}

func TestActivityDispatch_NonInvokeMessageFallsThroughToOnMessage(t *testing.T) {
	auth := &BotAuth{appID: "test", appSecret: "test", tenantID: "test"}

	msgCalled := make(chan struct{}, 1)
	handler := &Handler{
		ToolDisplay:   "compact",
		messageForTest: func() { msgCalled <- struct{}{} },
	}

	mux := buildMuxSkipAuth(auth, handler)

	activity := Activity{Type: "message", Text: "hello"}
	body, _ := json.Marshal(activity)
	req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	select {
	case <-msgCalled:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("OnMessage was not called within 2s")
	}
}
```

Add `"time"` to the imports of `adapter_test.go`.

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./teams/ -run TestActivityDispatch_RoutesInvoke -count=1
```

Expected: FAIL — `Handler` has no `invokeForTest` / `messageForTest` hooks yet, and the routing logic is absent.

- [ ] **Step 3: Add test hooks to `Handler`** in `teams/handler.go`

Append two unexported fields used only by tests, kept right next to `Picker`:

```go
type Handler struct {
	// ... existing fields ...

	// Test-only override hooks. When non-nil, replace the default
	// dispatch so adapter-level routing tests don't have to spin up the
	// full message pipeline.
	invokeForTest      func()
	messageForTest     func()
	messageForTestFlag bool
}
```

- [ ] **Step 4: Wire `Handler.OnInvokeAction` test hook**

Inside `teams/invoke.go`, at the very top of `OnInvokeAction`, add:

```go
func (h *Handler) OnInvokeAction(activity *Activity) {
	if h.invokeForTest != nil {
		h.invokeForTest()
		return
	}
	// ... existing body
}
```

And at the top of `OnMessage` in `teams/handler.go`:

```go
func (h *Handler) OnMessage(activity *Activity) {
	if h.messageForTest != nil {
		h.messageForTestFlag = true
		h.messageForTest()
		return
	}
	// ... existing body
}
```

- [ ] **Step 5: Wire dispatch in `teams/adapter.go`**

In `handleActivity`, replace the existing `case "message":` line with the routing logic:

```go
switch activity.Type {
case "message":
	if _, err := UnmarshalInvokeData(&activity); err == nil {
		go handler.OnInvokeAction(&activity)
	} else {
		go handler.OnMessage(&activity)
	}
case "conversationUpdate":
	slog.Info("teams conversation update", "conversation", activity.Conversation.ID)
default:
	slog.Debug("teams: ignoring activity type", "type", activity.Type)
}
```

- [ ] **Step 6: Run tests**

```
go test ./teams/ -run TestActivityDispatch -count=1
go test ./teams/... -count=1
```

Expected: target tests PASS; existing tests still PASS.

- [ ] **Step 7: Commit**

```bash
git add teams/adapter.go teams/adapter_test.go teams/handler.go teams/invoke.go
git commit -m "feat(teams): route invoke payloads to OnInvokeAction"
```

---

## Task 9: Wire `command.ExecuteMode` / `ExecuteModel` happy + error path

**Goal:** Replace the Task-7 placeholder no-ops with real calls to `command.ExecuteMode` / `ExecuteModel` and render the result via `BuildModeConfirmation` / `BuildModelConfirmation`.

`command.ExecuteMode` returns a string — empty / `"✅ "`-prefixed on success, error-prefixed on failure. We treat any string starting with `✅` or `Switched` as success; everything else is an error message we surface to the user. To stay robust to wording changes, we also fetch `conn.Modes()` after the call to confirm whether `Current` actually changed.

**Files:**
- Modify: `teams/invoke.go`
- Modify: `teams/invoke_test.go`

- [ ] **Step 1: Write the failing test** — append to `teams/invoke_test.go`

```go
import "github.com/neilkuan/quill/acp"

func TestOnInvokeAction_SwitchMode_NoActiveSession_ShowsError(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	// Real pool with no connections — ExecuteMode returns the standard
	// "no active agent session" message, which our handler surfaces.
	pool := acp.NewSessionPool("/bin/false", nil, t.TempDir(), nil, 4)

	h := &Handler{Client: client, Pool: pool}
	a := makeInvokeActivity(ts.URL, "conv-X", "card-1",
		"switch_mode", "teams:conv-X", "mode", "kiro_spec")

	h.OnInvokeAction(a)

	if cap.method != http.MethodPut {
		t.Fatalf("expected UpdateActivity, got %s", cap.method)
	}
	contentJSON, _ := json.Marshal(cap.body.Attachments[0].Content)
	if !bytes.Contains(contentJSON, []byte("❌")) {
		t.Errorf("expected ❌ confirmation, got: %s", contentJSON)
	}
	if !bytes.Contains(contentJSON, []byte("active agent session")) {
		t.Errorf("expected ExecuteMode error text passed through, got: %s", contentJSON)
	}
}

func TestOnInvokeAction_SwitchModel_NoActiveSession_ShowsError(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	pool := acp.NewSessionPool("/bin/false", nil, t.TempDir(), nil, 4)

	h := &Handler{Client: client, Pool: pool}
	a := makeInvokeActivity(ts.URL, "conv-X", "card-1",
		"switch_model", "teams:conv-X", "model", "claude-opus-4.6")

	h.OnInvokeAction(a)

	if cap.method != http.MethodPut {
		t.Fatalf("expected UpdateActivity, got %s", cap.method)
	}
	contentJSON, _ := json.Marshal(cap.body.Attachments[0].Content)
	if !bytes.Contains(contentJSON, []byte("❌")) {
		t.Errorf("expected ❌ confirmation, got: %s", contentJSON)
	}
}
```

The constructor signature was verified against `acp/pool.go` at plan-write time — `NewSessionPool(command string, args []string, workingDir string, env map[string]string, maxSessions int)`. `"/bin/false"` is a placeholder agent that never actually spawns because the test never calls `GetOrCreate`; we only need an empty pool so `ExecuteMode` short-circuits on its no-active-session check.

- [ ] **Step 2: Run tests to verify they fail**

```
go test ./teams/ -run TestOnInvokeAction_Switch -count=1
```

Expected: FAIL — the no-op switch arms in `OnInvokeAction` still don't call ExecuteMode/Model.

- [ ] **Step 3: Replace the no-ops in `OnInvokeAction`** in `teams/invoke.go`

```go
import "github.com/neilkuan/quill/command"

// ... inside OnInvokeAction, replace the placeholder no-op blocks ...

	switch data.Action {
	case actionSwitchMode:
		if data.Mode == "" {
			h.updateCard(activity, BuildModeConfirmation("", "", "Selection missing — please re-open the picker with /mode."))
			return
		}
		prev := currentModeID(h.Pool, expectedThread)
		result := command.ExecuteMode(h.Pool, expectedThread, data.Mode)
		if isSwitchSuccess(result) {
			h.updateCard(activity, BuildModeConfirmation(prev, data.Mode, ""))
		} else {
			h.updateCard(activity, BuildModeConfirmation(prev, data.Mode, result))
		}
	case actionSwitchModel:
		if data.Model == "" {
			h.updateCard(activity, BuildModelConfirmation("", "", "Selection missing — please re-open the picker with /model."))
			return
		}
		prev := currentModelID(h.Pool, expectedThread)
		result := command.ExecuteModel(h.Pool, expectedThread, data.Model)
		if isSwitchSuccess(result) {
			h.updateCard(activity, BuildModelConfirmation(prev, data.Model, ""))
		} else {
			h.updateCard(activity, BuildModelConfirmation(prev, data.Model, result))
		}
	default:
		slog.Debug("teams: unknown invoke action — ignoring", "action", data.Action)
		return
	}
```

Add the helpers at the bottom of `teams/invoke.go`:

```go
import "strings"

// isSwitchSuccess inspects the string returned by command.ExecuteMode /
// ExecuteModel. Today both functions return a "✅ Switched ..." marker
// on success and a free-form error string on failure.
func isSwitchSuccess(s string) bool {
	return strings.Contains(s, "✅")
}

// currentModeID returns the mode id active right now for the given
// thread, or "" if the connection is gone. Used to show the "from →
// to" arrow on the confirmation card.
func currentModeID(pool *acp.SessionPool, threadKey string) string {
	if pool == nil {
		return ""
	}
	conn := pool.Connection(threadKey)
	if conn == nil {
		return ""
	}
	_, current := conn.Modes()
	return current
}

func currentModelID(pool *acp.SessionPool, threadKey string) string {
	if pool == nil {
		return ""
	}
	conn := pool.Connection(threadKey)
	if conn == nil {
		return ""
	}
	_, current := conn.Models()
	return current
}
```

Be sure to add the `acp` import to invoke.go: `"github.com/neilkuan/quill/acp"`.

- [ ] **Step 4: Run tests**

```
go test ./teams/ -run TestOnInvokeAction -count=1
go test ./teams/... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add teams/invoke.go teams/invoke_test.go
git commit -m "feat(teams): wire OnInvokeAction to ExecuteMode/ExecuteModel"
```

---

## Task 10: `sendModeCard` / `sendModelCard` and `handleCommand` integration

**Goal:** When the user types `/mode` or `/model` with no argument and the listing has rows, send an Adaptive Card. When `cmd.Args` is non-empty (`/mode <id>`), keep the existing direct-switch text path.

**Files:**
- Modify: `teams/handler.go`
- Modify: `teams/handler_test.go`

- [ ] **Step 1: Write the failing test** — append to `teams/handler_test.go`

```go
import (
	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
)

// fakeSessionPool / fakeConnection scaffolding only if the existing
// codebase doesn't already give us enough — at the time of writing,
// command.ListModes works against acp.SessionPool, so we need a real
// pool with a connection that advertises modes. The simplest way to
// exercise this end-to-end without spinning a real ACP agent is to
// drive the listing path through a stubbed pool used by command tests.
//
// If a stub pool already exists in command/, prefer reusing it. If not,
// the cleanest path is to add a thin wrapper interface and inject a
// fake. For this plan we accept the test scope as: "verify
// sendModeCard's behaviour when ListModes returns Available > 0", by
// calling the helper directly with a hand-built listing.

func TestSendModeCard_BuildsAdaptiveCardAttachment(t *testing.T) {
	cap := &captureUpdate{} // reusing struct from invoke_test.go (same package).
	// SendActivity hits POST, not PUT — extend the stub to capture both.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&cap.body)
		_, _ = w.Write([]byte(`{"id":"sent-1"}`))
	}))
	defer ts.Close()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock", "expires_in": 3600})
	}))
	defer tokenServer.Close()

	auth := &BotAuth{appID: "a", appSecret: "s", tenantID: "t", tokenURL: tokenServer.URL}
	client := NewBotClient(auth)

	h := &Handler{Client: client}
	listing := command.ModeListing{
		Current:   "kiro_default",
		Available: []acp.ModeInfo{{ID: "kiro_default"}, {ID: "kiro_spec"}},
	}
	a := &Activity{ServiceURL: ts.URL, Conversation: Conversation{ID: "conv-X"}}

	h.sendModeCard(a, "teams:conv-X", listing)

	if cap.method != http.MethodPost {
		t.Fatalf("expected POST, got %s", cap.method)
	}
	if len(cap.body.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %d", len(cap.body.Attachments))
	}
	att := cap.body.Attachments[0]
	if att.ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("ContentType = %q", att.ContentType)
	}
}
```

(The `command.ModelListing` analogue is symmetrical — add the same shape under `TestSendModelCard_BuildsAdaptiveCardAttachment`.)

- [ ] **Step 2: Run test to verify it fails**

```
go test ./teams/ -run TestSendModeCard -count=1
```

Expected: FAIL — `sendModeCard` does not exist.

- [ ] **Step 3: Implement `sendModeCard` and `sendModelCard`** in `teams/handler.go` (alongside `handleCommand`)

```go
func (h *Handler) sendModeCard(activity *Activity, threadKey string, listing command.ModeListing) {
	att := BuildModeCard(listing, threadKey)
	resp := &Activity{
		Type:        "message",
		Attachments: []Attachment{att},
	}
	if _, err := h.Client.SendActivity(activity.ServiceURL, activity.Conversation.ID, resp); err != nil {
		slog.Warn("teams: failed to send mode card", "error", err)
	}
}

func (h *Handler) sendModelCard(activity *Activity, threadKey string, listing command.ModelListing) {
	att := BuildModelCard(listing, threadKey)
	resp := &Activity{
		Type:        "message",
		Attachments: []Attachment{att},
	}
	if _, err := h.Client.SendActivity(activity.ServiceURL, activity.Conversation.ID, resp); err != nil {
		slog.Warn("teams: failed to send model card", "error", err)
	}
}
```

- [ ] **Step 4: Replace the `CmdMode` and `CmdModel` cases in `handleCommand`** to prefer cards when args are empty

Current (`teams/handler.go` around line 274):

```go
case command.CmdMode:
	// Teams is text-only: no native SelectMenu / InlineKeyboard
	// hookup, so both `/mode` and `/mode <id>` fall back to the
	// plain-text listing / switch path.
	response = command.ExecuteMode(h.Pool, sessionKey, cmd.Args)
case command.CmdModel:
	response = command.ExecuteModel(h.Pool, sessionKey, cmd.Args)
```

Replace with:

```go
case command.CmdMode:
	if strings.TrimSpace(cmd.Args) == "" {
		listing := command.ListModes(h.Pool, sessionKey)
		if listing.Err == nil && len(listing.Available) > 0 {
			h.sendModeCard(activity, sessionKey, listing)
			return
		}
		response = listing.Message
		break
	}
	response = command.ExecuteMode(h.Pool, sessionKey, cmd.Args)
case command.CmdModel:
	if strings.TrimSpace(cmd.Args) == "" {
		listing := command.ListModels(h.Pool, sessionKey)
		if listing.Err == nil && len(listing.Available) > 0 {
			h.sendModelCard(activity, sessionKey, listing)
			return
		}
		response = listing.Message
		break
	}
	response = command.ExecuteModel(h.Pool, sessionKey, cmd.Args)
```

Verify the imports of `teams/handler.go` already include `"strings"` — they do (existing code uses `strings.TrimSpace` on prompts).

- [ ] **Step 5: Run tests**

```
go test ./teams/... -count=1
go test ./... -count=1
go vet ./...
go build ./...
```

Expected: all green. The cross-package tests confirm `command/` and `acp/` are unaffected.

- [ ] **Step 6: Commit**

```bash
git add teams/handler.go teams/handler_test.go
git commit -m "feat(teams): send Adaptive Card pickers on bare /mode and /model"
```

---

## Task 11: Final integration sweep + spec checkboxes

**Goal:** Cross-cutting verification before flipping the spec's DoD checkboxes.

**Files:**
- Verify: spec DoD section in `docs/superpowers/specs/2026-04-27-teams-adaptive-cards-design.md`
- Verify: full repo build / test / vet
- Verify: card payload JSON shape

- [ ] **Step 1: Build, vet, test the whole repo**

```
go build ./...
go vet ./...
go test ./... -count=1
```

Expected: no errors, no warnings, all tests PASS.

- [ ] **Step 2: Sanity-check card JSON via golden output**

Run a quick standalone Go file (or use an ad-hoc test) to dump `BuildModeCard`'s JSON, then validate it parses:

```bash
cat > /tmp/card_dump.go <<'EOF'
package main

import (
	"encoding/json"
	"fmt"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
	"github.com/neilkuan/quill/teams"
)

func main() {
	att := teams.BuildModeCard(command.ModeListing{
		Current:   "kiro_default",
		Available: []acp.ModeInfo{{ID: "kiro_default"}, {ID: "kiro_spec"}},
	}, "teams:demo")
	b, _ := json.MarshalIndent(att, "", "  ")
	fmt.Println(string(b))
}
EOF
go run /tmp/card_dump.go | python3 -m json.tool > /dev/null && echo OK
rm /tmp/card_dump.go
```

Expected: `OK`. Any JSON error means a struct field has a wrong tag or omitempty annotation.

- [ ] **Step 3: Tick the in-code DoD boxes in the spec**

Edit `docs/superpowers/specs/2026-04-27-teams-adaptive-cards-design.md` Definition of Done section and check off the first two items:

```markdown
- [x] `go build ./...`, `go vet ./...`, `go test ./... -count=1` all green.
- [x] `BuildModeCard` sample output passes `python3 -m json.tool`.
- [ ] Manual sideload verification (steps above) reproduced in personal
      chat, team channel, and private channel — all three flip the card
      to ✅ on Submit.
- [ ] After flip, sending a fresh prompt logs the new mode / model.
```

The remaining two boxes stay open — they are the manual sideload + smoke check that runs in PR review or after merge.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-04-27-teams-adaptive-cards-design.md
git commit -m "docs(specs): tick automated DoD checkboxes for teams adaptive cards"
```

- [ ] **Step 5: Push and open PR**

```bash
git push -u origin feat/teams-adaptive-cards
gh pr create --title "feat(teams): adaptive card picker for /mode and /model" --body "$(cat <<'EOF'
##### Summary

- New Adaptive Card payload structs in \`teams/cards.go\` plus builders for the mode picker, model picker, and post-submit confirmations.
- New \`teams/invoke.go\` decodes \`Activity.Value\` into typed \`InvokeData\`, guards thread mismatches, and updates the original card via \`UpdateActivity\` after dispatching to \`command.ExecuteMode\` / \`ExecuteModel\`.
- \`handleActivity\` peels invoke payloads off the regular \`type=message\` stream so existing \`OnMessage\` flow is undisturbed.
- \`/mode\` and \`/model\` with no args now render the dropdown card; \`/mode <id>\` direct-switch path is preserved.

##### Test plan

- [x] \`go build ./...\` / \`go vet ./...\` / \`go test ./... -count=1\` all green.
- [x] \`BuildModeCard\` sample output passes \`python3 -m json.tool\`.
- [ ] Repackage \`teams/appmanifest/\` (bump \`version\` to bust client cache).
- [ ] Sideload, send \`/mode\` in personal chat → expect dropdown card → switch → expect ✅ in-place update.
- [ ] Repeat for \`/model\` in personal chat.
- [ ] Repeat for both in a team channel and a private channel.
- [ ] Send a follow-up prompt and confirm \`QUILL_LOG=debug\` shows the new mode / model.

Spec: [\`docs/superpowers/specs/2026-04-27-teams-adaptive-cards-design.md\`](docs/superpowers/specs/2026-04-27-teams-adaptive-cards-design.md)
Plan: [\`docs/superpowers/plans/2026-04-27-teams-adaptive-cards.md\`](docs/superpowers/plans/2026-04-27-teams-adaptive-cards.md)
EOF
)"
```

---

## Self-Review Notes (for the implementer)

These are the gotchas I caught while writing the plan. None block the plan but knowing them up front saves debugging time.

1. **`SendActivity` already returns `(*Activity, error)`** despite the spec's wording. No signature change needed; just use `resp.ID` from the existing return value.
2. **`Attachment.Content` was missing** from the original struct — Task 2 adds it. Existing callers don't set `Content`, so the new field is backward-compatible.
3. **`acp.NewSessionPool` constructor signature** — verified at plan-write time. Task 9's test passes a `/bin/false` placeholder agent because we never call `GetOrCreate`; the test only needs an empty pool to drive `ExecuteMode`'s no-active-session early return.
4. **Test hooks (`invokeForTest`, `messageForTest`)** are pragmatic. If the team prefers an interface-based router instead, swap it in — the wiring in Task 8 is independent of the dispatch shape.
5. **`isSwitchSuccess` checks for `"✅"`** in the result string. If `command.ExecuteMode`'s wording changes, update the helper. A stronger version would compare `Current` before vs after via `conn.Modes()`, but adds an extra ACP round-trip.
