package discord

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
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
