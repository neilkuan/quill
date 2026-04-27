package teams

import (
	"encoding/json"
	"testing"
)

func TestActivityUnmarshal(t *testing.T) {
	raw := `{
		"type": "message",
		"id": "abc123",
		"timestamp": "2026-04-16T10:00:00Z",
		"serviceUrl": "https://smba.trafficmanager.net/teams/",
		"channelId": "msteams",
		"from": {"id": "user-123", "name": "Test User"},
		"conversation": {"id": "conv-456"},
		"recipient": {"id": "bot-789", "name": "QuillBot"},
		"text": "<at>QuillBot</at> hello world",
		"entities": [
			{
				"type": "mention",
				"mentioned": {"id": "bot-789", "name": "QuillBot"},
				"text": "<at>QuillBot</at>"
			}
		],
		"attachments": [
			{
				"contentType": "image/png",
				"contentUrl": "https://example.com/image.png",
				"name": "screenshot.png"
			}
		]
	}`

	var activity Activity
	if err := json.Unmarshal([]byte(raw), &activity); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if activity.Type != "message" {
		t.Errorf("type: got %s, want message", activity.Type)
	}
	if activity.From.ID != "user-123" {
		t.Errorf("from.id: got %s, want user-123", activity.From.ID)
	}
	if activity.Conversation.ID != "conv-456" {
		t.Errorf("conversation.id: got %s, want conv-456", activity.Conversation.ID)
	}
	if activity.Recipient.ID != "bot-789" {
		t.Errorf("recipient.id: got %s, want bot-789", activity.Recipient.ID)
	}
	if len(activity.Entities) != 1 {
		t.Fatalf("entities: got %d, want 1", len(activity.Entities))
	}
	if activity.Entities[0].Mentioned == nil || activity.Entities[0].Mentioned.ID != "bot-789" {
		t.Error("expected mention entity with bot-789")
	}
	if len(activity.Attachments) != 1 {
		t.Fatalf("attachments: got %d, want 1", len(activity.Attachments))
	}
	if activity.Attachments[0].ContentURL != "https://example.com/image.png" {
		t.Errorf("attachment contentUrl: got %s", activity.Attachments[0].ContentURL)
	}
}

func TestActivityMarshal(t *testing.T) {
	activity := Activity{
		Type:         "message",
		Text:         "Hello from bot",
		Conversation: Conversation{ID: "conv-456"},
		ReplyToID:    "msg-123",
	}

	data, err := json.Marshal(activity)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]interface{}
	json.Unmarshal(data, &decoded)

	if decoded["type"] != "message" {
		t.Errorf("type: got %v", decoded["type"])
	}
	if decoded["replyToId"] != "msg-123" {
		t.Errorf("replyToId: got %v", decoded["replyToId"])
	}
}

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
