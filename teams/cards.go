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
