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
			SubmitAction{Type: "Action.Submit", Title: "OK", Data: map[string]any{"k": "v"}},
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
