package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/neilkuan/quill/acp"
)

const (
	CmdSessions   = "sessions"
	CmdReset      = "reset"
	CmdResume     = "resume"
	CmdInfo       = "info"
	CmdSetVoice   = "setvoice"
	CmdVoiceClear = "voice-clear"
	CmdVoiceMode  = "voicemode"
)

type Command struct {
	Name string
	Args string
}

// ParseCommand checks if text is a known bot command.
func ParseCommand(text string) (*Command, bool) {
	text = strings.TrimSpace(text)
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return nil, false
	}

	name := strings.ToLower(parts[0])
	known := map[string]bool{
		CmdSessions: true, CmdReset: true, CmdResume: true, CmdInfo: true,
		CmdSetVoice: true, CmdVoiceClear: true, CmdVoiceMode: true,
	}
	if !known[name] {
		return nil, false
	}

	args := ""
	if len(parts) > 1 {
		args = strings.Join(parts[1:], " ")
	}
	return &Command{Name: name, Args: args}, true
}

// ExecuteSessions returns a formatted list of all active sessions.
func ExecuteSessions(pool *acp.SessionPool) string {
	sessions := pool.ListSessions()
	active, max := pool.Stats()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Active Sessions: %d/%d**\n\n", active, max))

	if len(sessions) == 0 {
		sb.WriteString("No active sessions.")
		return sb.String()
	}

	for i, s := range sessions {
		status := "alive"
		if !s.Alive {
			status = "dead"
		}
		sb.WriteString(fmt.Sprintf(
			"`%s` [%s]\n"+
				"  created %s ago | active %s ago | msgs: %d",
			s.ThreadKey,
			status,
			formatDuration(time.Since(s.CreatedAt)),
			formatDuration(time.Since(s.LastActive)),
			s.MessageCount,
		))
		if i < len(sessions)-1 {
			sb.WriteString("\n\n")
		} else {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// VoiceInfo holds voice feature status for display.
type VoiceInfo struct {
	STTEnabled  bool
	STTProvider string
	STTModel    string
	TTSEnabled  bool
	TTSModel    string
	TTSVoice    string
	CustomVoice string // per-user custom voice ID, if set
}

// ExecuteInfo returns detailed info for a specific session.
func ExecuteInfo(pool *acp.SessionPool, threadKey string, voice *VoiceInfo) string {
	info, err := pool.GetSessionInfo(threadKey)
	if err != nil {
		return "No active session for this thread."
	}

	status := "Running"
	if !info.Alive {
		status = "Dead"
	}
	if info.Resumed {
		status += " (restored)"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"**Session Info**\n"+
			"Thread: `%s`\n"+
			"Session ID: `%s`\n"+
			"Status: %s\n"+
			"Created: %s ago\n"+
			"Last Active: %s ago\n"+
			"Messages: %d",
		info.ThreadKey,
		info.SessionID,
		status,
		formatDuration(time.Since(info.CreatedAt)),
		formatDuration(time.Since(info.LastActive)),
		info.MessageCount,
	))

	if voice != nil {
		sb.WriteString("\n\n**Voice**\n")
		if voice.STTEnabled {
			sb.WriteString(fmt.Sprintf("STT: `%s` / `%s`\n", voice.STTProvider, voice.STTModel))
		} else {
			sb.WriteString("STT: disabled\n")
		}
		if voice.TTSEnabled {
			voiceDisplay := voice.TTSVoice
			if voice.CustomVoice != "" {
				voiceDisplay = fmt.Sprintf("%s (custom: `%s`)", voice.TTSVoice, voice.CustomVoice)
			}
			sb.WriteString(fmt.Sprintf("TTS: `%s` / %s", voice.TTSModel, voiceDisplay))
		} else {
			sb.WriteString("TTS: disabled")
		}
	}

	return sb.String()
}

// ExecuteReset kills the current session and returns a status message.
func ExecuteReset(pool *acp.SessionPool, threadKey string) string {
	if err := pool.ResetSession(threadKey); err != nil {
		return "No active session to reset."
	}
	return "Session reset. A new session will be created on the next message."
}

// ExecuteResume attempts to restore a previous session for this thread.
func ExecuteResume(pool *acp.SessionPool, threadKey string) string {
	_, msg := pool.ResumeSession(threadKey)
	return msg
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
