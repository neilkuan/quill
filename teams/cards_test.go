package teams

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
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
