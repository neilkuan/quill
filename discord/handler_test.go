package discord

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/neilkuan/openab-go/platform"
)

// --- isImageMime ---

func TestIsImageMime_ContentType(t *testing.T) {
	tests := []struct {
		contentType string
		filename    string
		expected    bool
	}{
		{"image/png", "photo.png", true},
		{"image/jpeg", "photo.jpg", true},
		{"image/gif", "anim.gif", true},
		{"image/webp", "photo.webp", true},
		{"image/svg+xml", "icon.svg", true},
		{"application/pdf", "doc.pdf", false},
		{"text/plain", "notes.txt", false},
	}

	for _, tt := range tests {
		result := isImageMime(tt.contentType, tt.filename)
		if result != tt.expected {
			t.Errorf("isImageMime(%q, %q) = %v, want %v", tt.contentType, tt.filename, result, tt.expected)
		}
	}
}

func TestIsImageMime_FallbackToExtension(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"photo.png", true},
		{"photo.PNG", true},
		{"photo.jpg", true},
		{"photo.jpeg", true},
		{"photo.gif", true},
		{"photo.webp", true},
		{"doc.pdf", false},
		{"notes.txt", false},
		{"noext", false},
	}

	for _, tt := range tests {
		// Empty content type forces extension fallback
		result := isImageMime("", tt.filename)
		if result != tt.expected {
			t.Errorf("isImageMime(\"\", %q) = %v, want %v", tt.filename, result, tt.expected)
		}
	}
}

// --- hasImageAttachments ---

func TestHasImageAttachments(t *testing.T) {
	tests := []struct {
		name        string
		attachments []*discordgo.MessageAttachment
		expected    bool
	}{
		{
			"no attachments",
			nil,
			false,
		},
		{
			"one image",
			[]*discordgo.MessageAttachment{
				{ContentType: "image/png", Filename: "photo.png"},
			},
			true,
		},
		{
			"one non-image",
			[]*discordgo.MessageAttachment{
				{ContentType: "application/pdf", Filename: "doc.pdf"},
			},
			false,
		},
		{
			"mixed attachments",
			[]*discordgo.MessageAttachment{
				{ContentType: "application/pdf", Filename: "doc.pdf"},
				{ContentType: "image/jpeg", Filename: "photo.jpg"},
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasImageAttachments(tt.attachments)
			if result != tt.expected {
				t.Errorf("hasImageAttachments() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// --- stripMention ---

func TestStripMention(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"<@123456> hello", "hello"},
		{"<@!123456> hello world", "hello world"},
		{"<@&999> role mention", "role mention"},
		{"hello <@123> world", "hello  world"},
		{"<@111> <@222> multiple", "multiple"},
		{"no mentions here", "no mentions here"},
		{"<@123>", ""},
	}

	for _, tt := range tests {
		result := stripMention(tt.input)
		if result != tt.expected {
			t.Errorf("stripMention(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// --- shortenThreadName ---

func TestShortenThreadName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			"short text",
			"fix the bug",
			"fix the bug",
		},
		{
			"long text truncated",
			"this is a very long prompt that exceeds the forty character limit for thread names",
			"this is a very long prompt that exceeds ...",
		},
		{
			"exactly 40 chars",
			strings.Repeat("a", 40),
			strings.Repeat("a", 40),
		},
		{
			"41 chars truncated",
			strings.Repeat("b", 41),
			strings.Repeat("b", 40) + "...",
		},
		{
			"github URL shortened",
			"check https://github.com/neilkuan/openab-go/issues/42 please",
			"check neilkuan/openab-go#42 please",
		},
		{
			"github PR URL shortened",
			"review https://github.com/neilkuan/openab-go/pull/10",
			"review neilkuan/openab-go#10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shortenThreadName(tt.input)
			if result != tt.expected {
				t.Errorf("shortenThreadName() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// --- composeDisplay ---

func TestComposeDisplay(t *testing.T) {
	tests := []struct {
		name      string
		toolLines []string
		text      string
		expected  string
	}{
		{
			"text only",
			nil,
			"hello world",
			"hello world",
		},
		{
			"text with trailing whitespace",
			nil,
			"hello world  \n\n",
			"hello world",
		},
		{
			"tools and text",
			[]string{"🔧 `Read`...", "✅ `Edit`"},
			"done!",
			"🔧 `Read`...\n✅ `Edit`\n\ndone!",
		},
		{
			"tools only no text",
			[]string{"🔧 `Read`..."},
			"",
			"🔧 `Read`...\n\n",
		},
		{
			"empty",
			nil,
			"",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := composeDisplay(tt.toolLines, tt.text)
			if result != tt.expected {
				t.Errorf("composeDisplay() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// --- downloadImageToFile ---

func TestDownloadImageToFile_Success(t *testing.T) {
	imageData := []byte("fake-png-data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(imageData)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	path, err := downloadImageToFile(server.URL, "test.png", tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasSuffix(path, "_test.png") {
		t.Errorf("expected path ending with '_test.png', got %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(data) != string(imageData) {
		t.Errorf("file content mismatch: got %q, want %q", string(data), string(imageData))
	}
}

func TestDownloadImageToFile_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	_, err := downloadImageToFile(server.URL, "test.png", tmpDir)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected error to mention 404, got %q", err.Error())
	}
}

func TestDownloadImageToFile_TooLarge(t *testing.T) {
	// Serve slightly over 10MB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// Write 10MB + 2 bytes
		chunk := make([]byte, 1024*1024)
		for i := 0; i < 10; i++ {
			w.Write(chunk)
		}
		w.Write([]byte("xx"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	_, err := downloadImageToFile(server.URL, "huge.png", tmpDir)
	if err == nil {
		t.Fatal("expected error for oversized image")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' in error, got %q", err.Error())
	}

	// Verify no leftover file
	files, _ := os.ReadDir(tmpDir)
	if len(files) != 0 {
		t.Errorf("expected cleanup of temp file, found %d files", len(files))
	}
}

func TestDownloadImageToFile_PathTraversal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	path, err := downloadImageToFile(server.URL, "../../etc/passwd", tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// File must be inside tmpDir, not escaped
	if !strings.HasPrefix(path, tmpDir) {
		t.Errorf("path %q escaped tmpDir %q", path, tmpDir)
	}
	// filename should be sanitized to just "passwd"
	if !strings.HasSuffix(path, "_passwd") {
		t.Errorf("expected sanitized filename ending with '_passwd', got %q", path)
	}
}

func TestDownloadImageToFile_InvalidURL(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := downloadImageToFile("http://127.0.0.1:1/invalid", "test.png", tmpDir)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestDownloadImageToFile_DifferentNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	path1, err := downloadImageToFile(server.URL, "photo.png", tmpDir)
	if err != nil {
		t.Fatalf("first download failed: %v", err)
	}
	path2, err := downloadImageToFile(server.URL, "screenshot.jpg", tmpDir)
	if err != nil {
		t.Fatalf("second download failed: %v", err)
	}

	if path1 == path2 {
		t.Errorf("expected different paths, both are %q", path1)
	}

	for _, p := range []string{path1, path2} {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			t.Errorf("expected file to exist: %s", p)
		}
	}
}

// --- imageExtensions map ---

func TestImageExtensionsMap(t *testing.T) {
	expected := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
	}

	for ext, mime := range expected {
		got, ok := imageExtensions[ext]
		if !ok {
			t.Errorf("missing extension %q in imageExtensions", ext)
			continue
		}
		if got != mime {
			t.Errorf("imageExtensions[%q] = %q, want %q", ext, got, mime)
		}
	}

	if len(imageExtensions) != len(expected) {
		t.Errorf("imageExtensions has %d entries, want %d", len(imageExtensions), len(expected))
	}
}

// --- mentionRe / githubURLRe regex ---

func TestMentionRegex(t *testing.T) {
	tests := []struct {
		input string
		match bool
	}{
		{"<@123456>", true},
		{"<@!123456>", true},
		{"<@&123456>", true},
		{"<@abc>", false},
		{"@123456", false},
	}

	for _, tt := range tests {
		matched := mentionRe.MatchString(tt.input)
		if matched != tt.match {
			t.Errorf("mentionRe.MatchString(%q) = %v, want %v", tt.input, matched, tt.match)
		}
	}
}

func TestGithubURLRegex(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			"https://github.com/neilkuan/openab-go/issues/42",
			"neilkuan/openab-go#42",
		},
		{
			"https://github.com/neilkuan/openab-go/pull/10",
			"neilkuan/openab-go#10",
		},
		{
			"http://github.com/org/repo/issues/1",
			"org/repo#1",
		},
	}

	for _, tt := range tests {
		result := githubURLRe.ReplaceAllString(tt.input, "$1#$3")
		if result != tt.expected {
			t.Errorf("githubURLRe replace %q = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// --- buildPromptContent ---

func TestBuildPromptContent_TextOnly(t *testing.T) {
	result := buildPromptContent("hello", nil, nil, nil)
	if result != "hello" {
		t.Errorf("expected 'hello', got %q", result)
	}
}

func TestBuildPromptContent_WithImages(t *testing.T) {
	result := buildPromptContent("hello", []string{"/tmp/img.png"}, nil, nil)
	if !strings.Contains(result, "<attached_images>") {
		t.Error("expected <attached_images> tag")
	}
	if !strings.Contains(result, "/tmp/img.png") {
		t.Error("expected image path in output")
	}
	if strings.Contains(result, "<voice_transcription>") {
		t.Error("unexpected <voice_transcription> tag")
	}
}

func TestBuildPromptContent_WithTranscriptions(t *testing.T) {
	result := buildPromptContent("hello", nil, []string{"這是一段測試語音"}, nil)
	if !strings.Contains(result, "<voice_transcription>") {
		t.Error("expected <voice_transcription> tag")
	}
	if !strings.Contains(result, "這是一段測試語音") {
		t.Error("expected transcription text in output")
	}
	if !strings.Contains(result, "transcription of the user's voice message") {
		t.Error("expected voice message instruction")
	}
}

func TestBuildPromptContent_ImagesAndTranscriptions(t *testing.T) {
	result := buildPromptContent("hello",
		[]string{"/tmp/img.png"},
		[]string{"這是語音內容"},
		nil,
	)
	if !strings.Contains(result, "<attached_images>") {
		t.Error("expected <attached_images> tag")
	}
	if !strings.Contains(result, "<voice_transcription>") {
		t.Error("expected <voice_transcription> tag")
	}
}

func TestBuildPromptContent_MultipleTranscriptions(t *testing.T) {
	result := buildPromptContent("base", nil, []string{"第一段", "第二段"}, nil)
	if !strings.Contains(result, "第一段") || !strings.Contains(result, "第二段") {
		t.Error("expected both transcriptions in output")
	}
}

func TestBuildPromptContent_WithFiles(t *testing.T) {
	files := []platform.FileAttachment{
		{Filename: "script.py", ContentType: "text/x-python", Size: 1234, LocalPath: "/tmp/123_script.py"},
	}
	result := buildPromptContent("hello", nil, nil, files)
	if !strings.Contains(result, "<attached_files>") {
		t.Error("expected <attached_files> tag")
	}
	if !strings.Contains(result, "script.py") {
		t.Error("expected filename in output")
	}
	if !strings.Contains(result, "text/x-python") {
		t.Error("expected content type in output")
	}
	if !strings.Contains(result, "1234 bytes") {
		t.Error("expected file size in output")
	}
	if !strings.Contains(result, "/tmp/123_script.py") {
		t.Error("expected local path in output")
	}
}

func TestBuildPromptContent_AllTypes(t *testing.T) {
	files := []platform.FileAttachment{
		{Filename: "data.json", ContentType: "application/json", Size: 500, LocalPath: "/tmp/456_data.json"},
	}
	result := buildPromptContent("check this",
		[]string{"/tmp/img.png"},
		[]string{"語音內容"},
		files,
	)
	if !strings.Contains(result, "<attached_images>") {
		t.Error("expected <attached_images> tag")
	}
	if !strings.Contains(result, "<voice_transcription>") {
		t.Error("expected <voice_transcription> tag")
	}
	if !strings.Contains(result, "<attached_files>") {
		t.Error("expected <attached_files> tag")
	}
}

// --- isAudioMime ---

func TestIsAudioMime_ContentType(t *testing.T) {
	tests := []struct {
		contentType string
		filename    string
		expected    bool
	}{
		{"audio/ogg", "voice.ogg", true},
		{"audio/mpeg", "voice.mp3", true},
		{"audio/wav", "voice.wav", true},
		{"audio/flac", "voice.flac", true},
		{"audio/mp4", "voice.m4a", true},
		{"audio/webm", "voice.webm", true},
		{"video/webm", "voice.webm", true},   // Discord voice messages
		{"video/ogg", "voice.ogg", true},      // Discord voice messages
		{"image/png", "photo.png", false},
		{"application/pdf", "doc.pdf", false},
		{"text/plain", "notes.txt", false},
	}

	for _, tt := range tests {
		result := isAudioMime(tt.contentType, tt.filename)
		if result != tt.expected {
			t.Errorf("isAudioMime(%q, %q) = %v, want %v", tt.contentType, tt.filename, result, tt.expected)
		}
	}
}

func TestIsAudioMime_FallbackToExtension(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"voice.ogg", true},
		{"voice.OGG", true},
		{"voice.oga", true},
		{"voice.mp3", true},
		{"voice.wav", true},
		{"voice.flac", true},
		{"voice.m4a", true},
		{"voice.webm", true},
		{"voice.mp4", true},
		{"photo.png", false},
		{"doc.pdf", false},
		{"notes.txt", false},
		{"noext", false},
	}

	for _, tt := range tests {
		result := isAudioMime("", tt.filename)
		if result != tt.expected {
			t.Errorf("isAudioMime(\"\", %q) = %v, want %v", tt.filename, result, tt.expected)
		}
	}
}

// --- hasAudioAttachments ---

func TestHasAudioAttachments(t *testing.T) {
	tests := []struct {
		name        string
		attachments []*discordgo.MessageAttachment
		expected    bool
	}{
		{
			"no attachments",
			nil,
			false,
		},
		{
			"one audio",
			[]*discordgo.MessageAttachment{
				{ContentType: "audio/ogg", Filename: "voice.ogg"},
			},
			true,
		},
		{
			"one non-audio",
			[]*discordgo.MessageAttachment{
				{ContentType: "image/png", Filename: "photo.png"},
			},
			false,
		},
		{
			"mixed attachments with audio",
			[]*discordgo.MessageAttachment{
				{ContentType: "image/png", Filename: "photo.png"},
				{ContentType: "audio/ogg", Filename: "voice.ogg"},
			},
			true,
		},
		{
			"discord voice message (video/webm)",
			[]*discordgo.MessageAttachment{
				{ContentType: "video/webm", Filename: "voice-message.webm"},
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasAudioAttachments(tt.attachments)
			if result != tt.expected {
				t.Errorf("hasAudioAttachments() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// --- audioExtensions map ---

func TestAudioExtensionsMap(t *testing.T) {
	expected := map[string]string{
		".ogg":  "audio/ogg",
		".oga":  "audio/ogg",
		".mp3":  "audio/mpeg",
		".wav":  "audio/wav",
		".flac": "audio/flac",
		".m4a":  "audio/mp4",
		".webm": "audio/webm",
		".mp4":  "audio/mp4",
	}

	for ext, mime := range expected {
		got, ok := audioExtensions[ext]
		if !ok {
			t.Errorf("missing extension %q in audioExtensions", ext)
			continue
		}
		if got != mime {
			t.Errorf("audioExtensions[%q] = %q, want %q", ext, got, mime)
		}
	}

	if len(audioExtensions) != len(expected) {
		t.Errorf("audioExtensions has %d entries, want %d", len(audioExtensions), len(expected))
	}
}

// --- downloadAudioToFile ---

func TestDownloadAudioToFile_Success(t *testing.T) {
	audioData := []byte("fake-ogg-data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/ogg")
		w.Write(audioData)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	path, err := downloadAudioToFile(server.URL, "voice.ogg", tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasSuffix(path, "_voice.ogg") {
		t.Errorf("expected path ending with '_voice.ogg', got %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(data) != string(audioData) {
		t.Errorf("file content mismatch: got %q, want %q", string(data), string(audioData))
	}
}

func TestDownloadAudioToFile_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	_, err := downloadAudioToFile(server.URL, "voice.ogg", tmpDir)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("expected error to mention 404, got %q", err.Error())
	}
}

func TestDownloadAudioToFile_TooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/ogg")
		// Write 25MB + 2 bytes
		chunk := make([]byte, 1024*1024)
		for i := 0; i < 25; i++ {
			w.Write(chunk)
		}
		w.Write([]byte("xx"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	_, err := downloadAudioToFile(server.URL, "huge.ogg", tmpDir)
	if err == nil {
		t.Fatal("expected error for oversized audio")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' in error, got %q", err.Error())
	}

	// Verify no leftover file
	files, _ := os.ReadDir(tmpDir)
	if len(files) != 0 {
		t.Errorf("expected cleanup of temp file, found %d files", len(files))
	}
}

func TestDownloadAudioToFile_PathTraversal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	path, err := downloadAudioToFile(server.URL, "../../etc/passwd", tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(path, tmpDir) {
		t.Errorf("path %q escaped tmpDir %q", path, tmpDir)
	}
	if !strings.HasSuffix(path, "_passwd") {
		t.Errorf("expected sanitized filename ending with '_passwd', got %q", path)
	}
}

func TestDownloadAudioToFile_InvalidURL(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := downloadAudioToFile("http://127.0.0.1:1/invalid", "voice.ogg", tmpDir)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

// --- hasFileAttachments ---

func TestHasFileAttachments(t *testing.T) {
	tests := []struct {
		name        string
		attachments []*discordgo.MessageAttachment
		expected    bool
	}{
		{
			"no attachments",
			nil,
			false,
		},
		{
			"only images",
			[]*discordgo.MessageAttachment{
				{ContentType: "image/png", Filename: "photo.png"},
			},
			false,
		},
		{
			"only audio",
			[]*discordgo.MessageAttachment{
				{ContentType: "audio/ogg", Filename: "voice.ogg"},
			},
			false,
		},
		{
			"one file",
			[]*discordgo.MessageAttachment{
				{ContentType: "application/pdf", Filename: "doc.pdf"},
			},
			true,
		},
		{
			"text file",
			[]*discordgo.MessageAttachment{
				{ContentType: "text/plain", Filename: "notes.txt"},
			},
			true,
		},
		{
			"mixed with image and file",
			[]*discordgo.MessageAttachment{
				{ContentType: "image/png", Filename: "photo.png"},
				{ContentType: "application/json", Filename: "data.json"},
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasFileAttachments(tt.attachments)
			if result != tt.expected {
				t.Errorf("hasFileAttachments() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// --- downloadFileToDisk ---

func TestDownloadFileToDisk_Success(t *testing.T) {
	fileData := []byte(`{"key": "value"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(fileData)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	path, err := downloadFileToDisk(server.URL, "data.json", tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasSuffix(path, "_data.json") {
		t.Errorf("expected path ending with '_data.json', got %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(data) != string(fileData) {
		t.Errorf("file content mismatch: got %q, want %q", string(data), string(fileData))
	}
}

func TestDownloadFileToDisk_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	_, err := downloadFileToDisk(server.URL, "data.json", tmpDir)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestDownloadFileToDisk_PathTraversal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	path, err := downloadFileToDisk(server.URL, "../../etc/shadow", tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(path, tmpDir) {
		t.Errorf("path %q escaped tmpDir %q", path, tmpDir)
	}
	if !strings.HasSuffix(path, "_shadow") {
		t.Errorf("expected sanitized filename ending with '_shadow', got %q", path)
	}
}

// --- integration-style: compose with tool progress ---

func TestComposeDisplay_ToolProgress(t *testing.T) {
	var toolLines []string

	// Simulate tool start
	toolLines = append(toolLines, fmt.Sprintf("🔧 `%s`...", "Read"))
	display := composeDisplay(toolLines, "")
	if !strings.Contains(display, "🔧 `Read`...") {
		t.Errorf("expected tool start in display, got %q", display)
	}

	// Simulate tool done
	toolLines[0] = fmt.Sprintf("✅ `%s`", "Read")
	display = composeDisplay(toolLines, "response text")
	if !strings.Contains(display, "✅ `Read`") {
		t.Errorf("expected tool done in display, got %q", display)
	}
	if !strings.Contains(display, "response text") {
		t.Errorf("expected response text in display, got %q", display)
	}
}
