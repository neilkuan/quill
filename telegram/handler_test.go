package telegram

import (
	"fmt"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/neilkuan/quill/platform"
)

func TestBuildSessionKey(t *testing.T) {
	tests := []struct {
		name string
		msg  *models.Message
		want string
	}{
		{
			name: "private chat",
			msg: &models.Message{
				Chat: models.Chat{ID: 12345, Type: models.ChatTypePrivate},
			},
			want: "tg:12345",
		},
		{
			name: "group chat (non-forum)",
			msg: &models.Message{
				Chat: models.Chat{ID: -100123456789, Type: models.ChatTypeSupergroup},
			},
			want: "tg:-100123456789",
		},
		{
			name: "forum supergroup with topic",
			msg: &models.Message{
				MessageThreadID: 42,
				IsTopicMessage:  true,
				Chat:            models.Chat{ID: -100123456789, Type: models.ChatTypeSupergroup, IsForum: true},
			},
			want: "tg:-100123456789:42",
		},
		{
			name: "forum supergroup General topic (thread_id=1)",
			msg: &models.Message{
				MessageThreadID: 1,
				IsTopicMessage:  true,
				Chat:            models.Chat{ID: -100123456789, Type: models.ChatTypeSupergroup, IsForum: true},
			},
			want: "tg:-100123456789:1",
		},
		{
			name: "forum chat but message not in topic",
			msg: &models.Message{
				Chat: models.Chat{ID: -100123456789, Type: models.ChatTypeSupergroup, IsForum: true},
			},
			want: "tg:-100123456789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSessionKey(tt.msg)
			if got != tt.want {
				t.Errorf("buildSessionKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildSessionKeyFromChat(t *testing.T) {
	tests := []struct {
		name     string
		chatID   int64
		threadID int
		want     string
	}{
		{
			name:     "non-forum",
			chatID:   -100123456789,
			threadID: 0,
			want:     "tg:-100123456789",
		},
		{
			name:     "forum topic",
			chatID:   -100123456789,
			threadID: 42,
			want:     "tg:-100123456789:42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSessionKeyFromChat(tt.chatID, tt.threadID)
			if got != tt.want {
				t.Errorf("buildSessionKeyFromChat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTopicThreadID(t *testing.T) {
	tests := []struct {
		name string
		msg  *models.Message
		want int
	}{
		{"non-topic message", &models.Message{MessageThreadID: 5, IsTopicMessage: false}, 0},
		{"topic message", &models.Message{MessageThreadID: 42, IsTopicMessage: true, Chat: models.Chat{IsForum: true}}, 42},
		{"no thread id", &models.Message{}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := topicThreadID(tt.msg)
			if got != tt.want {
				t.Errorf("topicThreadID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsForumTopic(t *testing.T) {
	tests := []struct {
		name string
		msg  *models.Message
		want bool
	}{
		{"non-forum", &models.Message{Chat: models.Chat{IsForum: false}}, false},
		{"forum but not topic msg", &models.Message{Chat: models.Chat{IsForum: true}, IsTopicMessage: false}, false},
		{"forum topic msg", &models.Message{Chat: models.Chat{IsForum: true}, IsTopicMessage: true, MessageThreadID: 42}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isForumTopic(tt.msg)
			if got != tt.want {
				t.Errorf("isForumTopic() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractCommand(t *testing.T) {
	tests := []struct {
		name string
		msg  *models.Message
		want string
	}{
		{
			name: "simple command",
			msg: &models.Message{
				Text: "/sessions",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 9},
				},
			},
			want: "sessions",
		},
		{
			name: "command with bot name",
			msg: &models.Message{
				Text: "/reset@mybot",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 12},
				},
			},
			want: "reset",
		},
		{
			name: "command not at start",
			msg: &models.Message{
				Text: "hey /info",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 4, Length: 5},
				},
			},
			want: "",
		},
		{
			name: "resume command",
			msg: &models.Message{
				Text: "/resume",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 7},
				},
			},
			want: "resume",
		},
		{
			name: "resume command with bot name",
			msg: &models.Message{
				Text: "/resume@mybot",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 13},
				},
			},
			want: "resume",
		},
		{
			name: "no command",
			msg: &models.Message{
				Text: "hello world",
			},
			want: "",
		},
		{
			name: "command with numeric arg",
			msg: &models.Message{
				Text: "/pick 3",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 5},
				},
			},
			want: "pick 3",
		},
		{
			name: "command with multi-word args",
			msg: &models.Message{
				Text: "/pick load abc-123",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 5},
				},
			},
			want: "pick load abc-123",
		},
		{
			name: "command with bot name and args",
			msg: &models.Message{
				Text: "/pick@mybot all",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 11},
				},
			},
			want: "pick all",
		},
		{
			// Fallback path: some clients occasionally deliver a /command
			// without the bot_command entity. We still want the command
			// to dispatch, otherwise /mode would silently become a
			// prompt and the agent would treat it as natural language.
			name: "no entity, command text fallback",
			msg: &models.Message{
				Text: "/mode",
			},
			want: "mode",
		},
		{
			name: "no entity, command with args fallback",
			msg: &models.Message{
				Text: "/mode ask",
			},
			want: "mode ask",
		},
		{
			name: "no entity, command with @botname fallback",
			msg: &models.Message{
				Text: "/mode@mybot ask",
			},
			want: "mode ask",
		},
		{
			name: "no entity, just a slash",
			msg: &models.Message{
				Text: "/",
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCommand(tt.msg)
			if got != tt.want {
				t.Errorf("extractCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsBotMentioned(t *testing.T) {
	tests := []struct {
		name        string
		msg         *models.Message
		botUsername string
		want        bool
	}{
		{
			name: "mentioned in text",
			msg: &models.Message{
				Text: "@testbot hello",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeMention, Offset: 0, Length: 8},
				},
			},
			botUsername: "testbot",
			want:        true,
		},
		{
			name: "mentioned case insensitive",
			msg: &models.Message{
				Text: "@TestBot hello",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeMention, Offset: 0, Length: 8},
				},
			},
			botUsername: "testbot",
			want:        true,
		},
		{
			name: "not mentioned",
			msg: &models.Message{
				Text:     "hello world",
				Entities: []models.MessageEntity{},
			},
			botUsername: "testbot",
			want:        false,
		},
		{
			name: "different bot mentioned",
			msg: &models.Message{
				Text: "@otherbot hello",
				Entities: []models.MessageEntity{
					{Type: models.MessageEntityTypeMention, Offset: 0, Length: 9},
				},
			},
			botUsername: "testbot",
			want:        false,
		},
		{
			name: "mentioned in caption",
			msg: &models.Message{
				Text:    "",
				Caption: "@testbot check this",
				CaptionEntities: []models.MessageEntity{
					{Type: models.MessageEntityTypeMention, Offset: 0, Length: 8},
				},
			},
			botUsername: "testbot",
			want:        true,
		},
		{
			name: "no entities",
			msg: &models.Message{
				Text: "@testbot hello",
			},
			botUsername: "testbot",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBotMentioned(tt.msg, tt.botUsername)
			if got != tt.want {
				t.Errorf("isBotMentioned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStripBotMention(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		botUsername string
		want        string
	}{
		{"mention at start", "@testbot hello world", "testbot", "hello world"},
		{"mention at end", "hello @testbot", "testbot", "hello"},
		{"mention in middle", "hey @testbot how are you", "testbot", "hey  how are you"},
		{"case insensitive", "@TestBot hello", "testbot", "hello"},
		{"no mention", "hello world", "testbot", "hello world"},
		{"empty text", "", "testbot", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripBotMention(tt.text, tt.botUsername)
			if got != tt.want {
				t.Errorf("stripBotMention() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestComposeDisplay_Telegram(t *testing.T) {
	tests := []struct {
		name      string
		toolLines []string
		text      string
		want      string
	}{
		{"text only", nil, "Hello world", "Hello world"},
		{"text with trailing whitespace", nil, "Hello world  \n\n", "Hello world"},
		{"tools and text", []string{"🔧 `Read`...", "✅ `Write`"}, "Done!", "🔧 `Read`...\n✅ `Write`\n\nDone!"},
		{"tools only", []string{"🔧 `Read`..."}, "", "🔧 `Read`...\n\n"},
		{"empty", nil, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := composeDisplay(tt.toolLines, tt.text)
			if got != tt.want {
				t.Errorf("composeDisplay() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildPromptContent_Telegram(t *testing.T) {
	tests := []struct {
		name           string
		base           string
		imagePaths     []string
		transcriptions []string
		files          []platform.FileAttachment
		wantContains   []string
	}{
		{
			name:         "text only",
			base:         "hello",
			wantContains: []string{"hello"},
		},
		{
			name:       "with images",
			base:       "check this",
			imagePaths: []string{"/tmp/photo.jpg"},
			wantContains: []string{
				"check this",
				"<attached_images>",
				"/tmp/photo.jpg",
				"</attached_images>",
			},
		},
		{
			name:           "with transcriptions",
			base:           "",
			transcriptions: []string{"hello from voice"},
			wantContains: []string{
				"<voice_transcription>",
				"hello from voice",
				"</voice_transcription>",
			},
		},
		{
			name: "with file attachments",
			base: "check this file",
			files: []platform.FileAttachment{
				{Filename: "notes.txt", ContentType: "text/plain", Size: 256, LocalPath: "/tmp/789_notes.txt"},
			},
			wantContains: []string{
				"<attached_files>",
				"notes.txt",
				"text/plain",
				"256 bytes",
				"/tmp/789_notes.txt",
				"</attached_files>",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPromptContent(tt.base, tt.imagePaths, tt.transcriptions, tt.files)
			for _, want := range tt.wantContains {
				if !contains(got, want) {
					t.Errorf("buildPromptContent() missing %q in:\n%s", want, got)
				}
			}
		})
	}
}

func TestImageFilenameFromMime(t *testing.T) {
	cases := map[string]string{
		"image/png":   "image.png",
		"image/jpeg":  "image.jpg",
		"image/jpg":   "image.jpg",
		"image/gif":   "image.gif",
		"image/webp":  "image.webp",
		"":            "image.png",
		"image/heic":  "image.heic",
		"image/avif ": "image.avif",
		// Junk that contains separators must not become a filename.
		"image/foo bar": "image.bin",
		"weird":         "image.bin",
	}
	for mime, want := range cases {
		if got := imageFilenameFromMime(mime); got != want {
			t.Errorf("imageFilenameFromMime(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestFormatImageMarker(t *testing.T) {
	got := formatImageMarker("image/png", 42, nil)
	if !contains(got, "png") || !contains(got, "msg #42") {
		t.Errorf("happy-path marker missing fields: %q", got)
	}
	got = formatImageMarker("image/jpeg", 0, nil)
	if !contains(got, "jpeg") || contains(got, "msg #") {
		t.Errorf("zero-id marker should omit msg #, got: %q", got)
	}
	got = formatImageMarker("image/png", 0, fmt.Errorf("boom"))
	if !contains(got, "failed") || !contains(got, "boom") {
		t.Errorf("error marker missing surface text: %q", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
