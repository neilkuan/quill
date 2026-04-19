// Package sessionpicker lists historical sessions from supported ACP agent
// backends so a user can pick one and resume it.
//
// Pickers read the agent's on-disk session store directly — they do not
// require the agent process to be running. Each backend has its own
// location and format (see individual implementations).
package sessionpicker

import (
	"path/filepath"
	"time"
)

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
