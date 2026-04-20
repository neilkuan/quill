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

// Tool display modes for ReactionsConfig.ToolDisplay.
const (
	ToolDisplayFull    = "full"
	ToolDisplayCompact = "compact"
	ToolDisplayNone    = "none"
)

// genericStatusVerbs are leading tokens that describe *state*, not the tool.
// Some agents (notably kiro-cli) format every tool title as
// "Running: <cmd> <args>", which makes compact mode collapse every tool line
// to the literal string "Running" and the user can't tell calls apart.
// Strip these before picking a label.
var genericStatusVerbs = map[string]bool{
	"running":   true,
	"executing": true,
	"invoking":  true,
	"calling":   true,
}

// FormatToolTitle returns the title to render in the streamed chat message
// for an ACP tool-call event, based on the configured display mode.
//
// The boolean return is false when the caller should skip rendering a tool
// line entirely (mode = "none", or the title is empty). The string return
// is empty in that case.
//
// Empty titles always return (false) regardless of mode — otherwise a
// "match-by-substring" update loop could match every existing tool line
// (since strings.Contains(s, "") is always true) and silently overwrite
// the wrong entry.
//
// Modes:
//   - "full"    — return the title unchanged.
//   - "compact" — strip a leading generic status verb (e.g. "Running:"),
//     then return the first whitespace-delimited token with trailing
//     punctuation (":") stripped. Keeps callers informed *which* tool is
//     running (e.g. "gh", "bash", "curl") without leaking long/sensitive
//     argument lists. Falls back to the full title when it has no whitespace.
//   - "none"    — skip the tool line.
//
// Unrecognised modes fall back to "full" (safest — preserves existing
// behaviour for users who typo the config).
func FormatToolTitle(title, mode string) (string, bool) {
	if mode == ToolDisplayNone {
		return "", false
	}
	if strings.TrimSpace(title) == "" {
		return "", false
	}
	switch mode {
	case ToolDisplayCompact:
		trimmed := strings.TrimSpace(title)
		// Drop a leading generic status verb — this is the sole reason
		// "Running: gh ..." / "Running: curl ..." used to collapse into a
		// single indistinguishable "Running" label.
		if rest, ok := stripGenericStatusVerb(trimmed); ok {
			trimmed = rest
		}
		first := trimmed
		if idx := strings.IndexAny(trimmed, " \t\n"); idx > 0 {
			first = trimmed[:idx]
		}
		first = strings.TrimRight(first, ":,;")
		if first == "" {
			return trimmed, true
		}
		return first, true
	default: // "full" or empty/unknown
		return title, true
	}
}

// stripGenericStatusVerb removes a leading "Running:" / "Executing:" /
// "Invoking:" / "Calling:" (case-insensitive, trailing ":" optional) and
// returns the remainder trimmed. Returns (title, false) if no such prefix.
func stripGenericStatusVerb(title string) (string, bool) {
	idx := strings.IndexAny(title, " \t\n")
	if idx <= 0 {
		return title, false
	}
	firstRaw := title[:idx]
	first := strings.ToLower(strings.TrimRight(firstRaw, ":,;"))
	if !genericStatusVerbs[first] {
		return title, false
	}
	return strings.TrimSpace(title[idx:]), true
}

// FormatSessionFooter returns a small footer showing the session's
// current mode / model for appending to the agent's reply. Returns
// empty string when both are blank, so callers can unconditionally
// concatenate it.
//
// Deliberately avoids italic wrapping around inline code: Telegram's
// markdown-to-HTML conversion closes italic spans when it hits a
// backtick, so `_mode: \`x\` · model: \`y\`_` renders with broken
// spacing. Plain text + inline code works cleanly on Discord,
// Telegram-HTML and Teams alike.
func FormatSessionFooter(mode, model string) string {
	var parts []string
	if mode != "" {
		parts = append(parts, "mode: `"+mode+"`")
	}
	if model != "" {
		parts = append(parts, "model: `"+model+"`")
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\n— " + strings.Join(parts, " · ")
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
