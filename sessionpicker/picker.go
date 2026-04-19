// Package sessionpicker lists historical sessions from supported ACP agent
// backends so a user can pick one and resume it.
//
// Pickers read the agent's on-disk session store directly — they do not
// require the agent process to be running. Each backend has its own
// location and format (see individual implementations).
package sessionpicker

import (
	"path/filepath"
	"strings"
	"time"
)

// quillMetadataTags are pure metadata wrappers quill prepends to user
// prompts. Their contents are never what a human typed — strip
// them whole when cleaning up a title candidate.
var quillMetadataTags = []string{"sender_context", "attached_files"}

// stripQuillEnvelope extracts the text a human would recognise as
// the "real" message from a prompt quill injected metadata into.
//
// Algorithm:
//  1. Peel off any leading metadata envelopes (sender_context,
//     attached_files) and their enclosing whitespace.
//  2. If the resulting text starts with <voice_transcription>,
//     unwrap it and return just the transcribed utterance — discard
//     quill's trailing "The above is a transcription…" hint, which
//     is prompt guidance for the agent, not what the user said.
//  3. Otherwise return the stripped remainder verbatim.
//
// Envelopes that appear mid-string are left untouched, so a user
// message that merely mentions <sender_context> in prose survives.
func stripQuillEnvelope(s string) string {
	// Repeatedly peel leading metadata envelopes in case they stack.
	for {
		next := stripOneMetadataEnvelope(s)
		if next == s {
			break
		}
		s = next
	}

	if inner, ok := unwrapVoiceTranscription(s); ok {
		return inner
	}
	return s
}

func stripOneMetadataEnvelope(s string) string {
	t := strings.TrimLeft(s, " \t\r\n")
	for _, tag := range quillMetadataTags {
		open := "<" + tag + ">"
		closeTag := "</" + tag + ">"
		if !strings.HasPrefix(t, open) {
			continue
		}
		end := strings.Index(t, closeTag)
		if end < 0 {
			continue
		}
		return strings.TrimLeft(t[end+len(closeTag):], " \t\r\n")
	}
	return s
}

// unwrapVoiceTranscription returns the inner transcript when s starts
// with a <voice_transcription>...</voice_transcription> block. The
// trailing "The above is a transcription of the user's voice message"
// hint quill appends after the closing tag is dropped because it is
// agent guidance, not user speech.
func unwrapVoiceTranscription(s string) (string, bool) {
	const open = "<voice_transcription>"
	const closeTag = "</voice_transcription>"
	t := strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(t, open) {
		return "", false
	}
	rest := t[len(open):]
	end := strings.Index(rest, closeTag)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:end]), true
}

// Session is the minimal metadata needed to render a picker row and
// later resume via AcpConnection.SessionLoad.
type Session struct {
	ID           string
	Title        string
	CWD          string
	UpdatedAt    time.Time
	MessageCount int
}

// Picker lists sessions for one agent backend.
type Picker interface {
	// AgentType returns a stable identifier for the backend, matching
	// the agent binary name (e.g. "kiro-cli", "claude-agent-acp").
	AgentType() string

	// List returns sessions newest first. If cwd is non-empty, only
	// sessions matching that working directory are returned. If limit
	// is > 0, at most that many results are returned.
	List(cwd string, limit int) ([]Session, error)
}

// Detect picks a Picker based on the agent binary path or name.
// Returns false when the binary is not recognised or no picker is
// implemented yet for that backend.
func Detect(agentCommand string) (Picker, bool) {
	switch filepath.Base(agentCommand) {
	case "kiro-cli":
		return NewKiroPicker(""), true
	case "claude-agent-acp":
		return NewClaudePicker(""), true
	case "copilot":
		return NewCopilotPicker(""), true
	case "codex-acp", "codex":
		return NewCodexPicker(""), true
	}
	return nil, false
}
