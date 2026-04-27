package teams

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
