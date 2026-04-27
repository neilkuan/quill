package teams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neilkuan/quill/acp"
	"github.com/neilkuan/quill/command"
)

func TestStripBotMention(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		botID    string
		entities []Entity
		want     string
	}{
		{
			name:  "simple mention",
			text:  "<at>QuillBot</at> hello world",
			botID: "bot-123",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "bot-123"}, Text: "<at>QuillBot</at>"},
			},
			want: "hello world",
		},
		{
			name:  "mention in middle",
			text:  "hey <at>QuillBot</at> do something",
			botID: "bot-123",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "bot-123"}, Text: "<at>QuillBot</at>"},
			},
			want: "hey  do something",
		},
		{
			name:     "no mention",
			text:     "plain text message",
			botID:    "bot-123",
			entities: nil,
			want:     "plain text message",
		},
		{
			name:  "mention of different user",
			text:  "<at>OtherUser</at> hello",
			botID: "bot-123",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "other-456"}, Text: "<at>OtherUser</at>"},
			},
			want: "<at>OtherUser</at> hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripBotMention(tt.text, tt.botID, tt.entities)
			if got != tt.want {
				t.Errorf("stripBotMention() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsBotMentioned(t *testing.T) {
	botID := "bot-123"

	tests := []struct {
		name     string
		entities []Entity
		want     bool
	}{
		{
			name: "bot mentioned",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "bot-123"}},
			},
			want: true,
		},
		{
			name: "other user mentioned",
			entities: []Entity{
				{Type: "mention", Mentioned: &Account{ID: "other-456"}},
			},
			want: false,
		},
		{
			name:     "no entities",
			entities: nil,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBotMentioned(botID, tt.entities)
			if got != tt.want {
				t.Errorf("isBotMentioned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildSessionKey(t *testing.T) {
	got := buildSessionKey("19:abc@thread.tacv2;messageid=123")
	want := "teams:19:abc@thread.tacv2;messageid=123"
	if got != want {
		t.Errorf("buildSessionKey() = %s, want %s", got, want)
	}
}

func TestExtensionForContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        string
	}{
		{"png", "image/png", ".png"},
		{"jpeg", "image/jpeg", ".jpg"},
		{"jpg alias", "image/jpg", ".jpg"},
		{"mp3", "audio/mpeg", ".mp3"},
		{"ogg voice", "audio/ogg", ".ogg"},
		{"pdf", "application/pdf", ".pdf"},
		{"with charset suffix", "text/plain; charset=utf-8", ".txt"},
		{"upper case", "IMAGE/PNG", ".png"},
		{"csv", "text/csv", ".csv"},
		{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"},
		{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx"},
		{"pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", ".pptx"},
		{"doc legacy", "application/msword", ".doc"},
		{"xls legacy", "application/vnd.ms-excel", ".xls"},
		{"ppt legacy", "application/vnd.ms-powerpoint", ".ppt"},
		{"empty", "", ""},
		{"unknown falls through", "application/x-made-up", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extensionForContentType(tt.contentType)
			if got != tt.want {
				t.Errorf("extensionForContentType(%q) = %q, want %q", tt.contentType, got, tt.want)
			}
		})
	}
}

// TestSendModeCard_BuildsAdaptiveCardAttachment verifies sendModeCard sends
// a POST with an Adaptive Card attachment.
func TestSendModeCard_BuildsAdaptiveCardAttachment(t *testing.T) {
	cap := &captureUpdate{}
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

// TestSendModelCard_BuildsAdaptiveCardAttachment verifies sendModelCard sends
// a POST with an Adaptive Card attachment (symmetric to sendModeCard).
func TestSendModelCard_BuildsAdaptiveCardAttachment(t *testing.T) {
	cap := &captureUpdate{}
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
	listing := command.ModelListing{
		Current:   "claude-opus-4.6",
		Available: []acp.ModelInfo{{ID: "claude-opus-4.6"}, {ID: "claude-sonnet-4.5"}},
	}
	a := &Activity{ServiceURL: ts.URL, Conversation: Conversation{ID: "conv-X"}}

	h.sendModelCard(a, "teams:conv-X", listing)

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
