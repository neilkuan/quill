package platform

import (
	"fmt"
	"strings"
)

// FileAttachment describes a non-image, non-audio file downloaded from a chat platform.
type FileAttachment struct {
	Filename    string
	ContentType string
	Size        int
	LocalPath   string
}

// FormatFileBlock renders a list of file attachments into the <attached_files> prompt block.
func FormatFileBlock(files []FileAttachment) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<attached_files>\n")
	for _, f := range files {
		b.WriteString(fmt.Sprintf("[Attached file: %s (%s, %d bytes) — saved to %s]\n", f.Filename, f.ContentType, f.Size, f.LocalPath))
	}
	b.WriteString("</attached_files>")
	return b.String()
}

// Platform is the interface every chat adapter (Discord, Telegram, Teams …) must implement.
type Platform interface {
	Start() error
	Stop() error
}

// SplitMessage splits text into chunks at line boundaries, each <= limit bytes.
// Every chat platform has a message-size ceiling, so this lives in the shared package.
// Hard-splits for long lines are UTF-8 safe (never cuts mid-character).
func SplitMessage(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}

	var chunks []string
	var current strings.Builder

	for _, line := range strings.Split(text, "\n") {
		// +1 for the newline
		if current.Len() > 0 && current.Len()+len(line)+1 > limit {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		// If a single line exceeds limit, hard-split on rune boundaries
		if len(line) > limit {
			for _, r := range line {
				if current.Len()+len(string(r)) > limit {
					chunks = append(chunks, current.String())
					current.Reset()
				}
				current.WriteRune(r)
			}
		} else {
			current.WriteString(line)
		}
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

// TruncateUTF8 truncates text to at most limit bytes without cutting multi-byte characters.
// If truncated, appends the suffix (e.g. "…").
func TruncateUTF8(text string, limit int, suffix string) string {
	if len(text) <= limit {
		return text
	}
	targetLen := limit - len(suffix)
	if targetLen <= 0 {
		return suffix
	}
	var b strings.Builder
	for _, r := range text {
		if b.Len()+len(string(r)) > targetLen {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString(suffix)
	return b.String()
}
