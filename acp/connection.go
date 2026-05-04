package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type AcpConnection struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdinMu        sync.Mutex
	stderrBuf      *bytes.Buffer
	nextID         atomic.Uint64
	pending        map[uint64]chan *JsonRpcMessage
	pendingMu      sync.Mutex
	notifyCh       chan *JsonRpcMessage
	notifyMu       sync.Mutex
	promptMu       sync.Mutex // serialize prompts — only one at a time per connection
	SessionID      string
	lastActive     atomic.Int64 // unix nano — use GetLastActive/touchLastActive
	CreatedAt      time.Time
	MessageCount   atomic.Uint64
	ThreadKey      string
	SessionReset   bool
	SessionResumed bool // true when session was restored via session/load (cleared after first prompt)
	WasResumed     bool // true when session was restored via session/load (persistent, for /info)
	CanLoadSession bool // true when agent advertises loadSession capability
	alive          atomic.Bool

	// modeMu guards AvailableModes and CurrentModeID so the read loop
	// (which reacts to `current_mode_update` notifications) and the
	// command/user goroutines (which set and render modes) do not race.
	modeMu         sync.RWMutex
	AvailableModes []ModeInfo
	CurrentModeID  string

	// modelMu guards AvailableModels and CurrentModelID — same pattern
	// as modeMu but for the model axis (session/set_model).
	modelMu         sync.RWMutex
	AvailableModels []ModelInfo
	CurrentModelID  string

	// ownerMu guards currentOwner so handlers can peek at "who is
	// holding promptMu right now" without acquiring promptMu itself.
	// The owner string is set when SessionPrompt acquires promptMu
	// and cleared by PromptDone before promptMu releases.
	ownerMu      sync.RWMutex
	currentOwner string // "" when idle; "user msg @neil" / "cron a3f5b201" etc when busy
}

// IsBusy returns whether the connection currently has a prompt running
// and a human-readable description of who started it. Cheap; safe to
// call from any goroutine. Used by message handlers to render a
// "queued behind X" placeholder when a new prompt is about to wait on
// promptMu.
//
// Note: there is a small window between SessionPrompt acquiring
// promptMu and writing the owner field. During that window IsBusy
// returns (false, ""). The worst-case effect on UX is a "thinking…"
// placeholder briefly shown instead of "queued behind X" — benign.
func (c *AcpConnection) IsBusy() (bool, string) {
	c.ownerMu.RLock()
	defer c.ownerMu.RUnlock()
	return c.currentOwner != "", c.currentOwner
}

func (c *AcpConnection) setCurrentOwner(owner string) {
	c.ownerMu.Lock()
	c.currentOwner = owner
	c.ownerMu.Unlock()
}

// Modes returns a copy of the session's currently advertised modes
// and the id that is active right now. Safe for concurrent use.
func (c *AcpConnection) Modes() (available []ModeInfo, current string) {
	c.modeMu.RLock()
	defer c.modeMu.RUnlock()
	out := make([]ModeInfo, len(c.AvailableModes))
	copy(out, c.AvailableModes)
	return out, c.CurrentModeID
}

func (c *AcpConnection) setModeState(current string, available []ModeInfo) {
	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	if current != "" {
		c.CurrentModeID = current
	}
	if available != nil {
		c.AvailableModes = available
	}
}

// SetCurrentMode records a mode change that arrived via a
// `current_mode_update` notification. The available-modes list is
// untouched because the notification payload only carries the new id.
func (c *AcpConnection) SetCurrentMode(id string) {
	c.modeMu.Lock()
	defer c.modeMu.Unlock()
	c.CurrentModeID = id
}

// Models returns a copy of the session's currently advertised models
// and the id that is active right now. Safe for concurrent use.
func (c *AcpConnection) Models() (available []ModelInfo, current string) {
	c.modelMu.RLock()
	defer c.modelMu.RUnlock()
	out := make([]ModelInfo, len(c.AvailableModels))
	copy(out, c.AvailableModels)
	return out, c.CurrentModelID
}

func (c *AcpConnection) setModelState(current string, available []ModelInfo) {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if current != "" {
		c.CurrentModelID = current
	}
	if available != nil {
		c.AvailableModels = available
	}
}

// SetCurrentModel records a model change that arrived via a
// `current_model_update` notification.
func (c *AcpConnection) SetCurrentModel(id string) {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	c.CurrentModelID = id
}

// GetLastActive returns the last activity time (safe for concurrent reads).
func (c *AcpConnection) GetLastActive() time.Time {
	return time.Unix(0, c.lastActive.Load())
}

// touchLastActive updates the last activity timestamp (safe for concurrent writes).
func (c *AcpConnection) touchLastActive() {
	c.lastActive.Store(time.Now().UnixNano())
}

func expandEnv(val string) string {
	if strings.HasPrefix(val, "${") && strings.HasSuffix(val, "}") {
		key := val[2 : len(val)-1]
		return os.Getenv(key)
	}
	return val
}

func SpawnConnection(command string, args []string, workingDir string, env map[string]string, threadKey string) (*AcpConnection, error) {
	slog.Info("spawning agent", "cmd", command, "args", args, "cwd", workingDir)

	cmd := exec.Command(command, args...)
	cmd.Dir = workingDir

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	for k, v := range env {
		cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s", k, expandEnv(v)))
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn %s: %w", command, err)
	}

	now := time.Now()
	conn := &AcpConnection{
		cmd:       cmd,
		stdin:     stdinPipe,
		stderrBuf: &stderrBuf,
		pending:   make(map[uint64]chan *JsonRpcMessage),
		CreatedAt: now,
		ThreadKey: threadKey,
	}
	conn.lastActive.Store(now.UnixNano())
	conn.nextID.Store(1)
	conn.alive.Store(true)

	go conn.readLoop(stdoutPipe)

	return conn, nil
}

func (c *AcpConnection) readLoop(stdout io.Reader) {
	defer c.alive.Store(false)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB max line

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg JsonRpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		slog.Debug("acp_recv", "line", line)

		// Auto-reply session/request_permission
		if msg.Method != nil && *msg.Method == "session/request_permission" {
			if msg.ID != nil {
				title := "?"
				if msg.Params != nil {
					var params struct {
						ToolCall struct {
							Title string `json:"title"`
						} `json:"toolCall"`
					}
					if err := json.Unmarshal(*msg.Params, &params); err == nil && params.ToolCall.Title != "" {
						title = params.ToolCall.Title
					}
				}
				slog.Info("auto-allow permission", "title", title)
				resp := NewJsonRpcResponse(*msg.ID, map[string]string{"optionId": "allow_always"})
				data, err := json.Marshal(resp)
				if err == nil {
					c.sendRaw(string(data))
				}
			}
			continue
		}

		// Response (has id) → resolve pending AND forward to subscriber
		if msg.ID != nil {
			c.pendingMu.Lock()
			ch, ok := c.pending[*msg.ID]
			if ok {
				delete(c.pending, *msg.ID)
			}
			c.pendingMu.Unlock()

			if ok {
				// Also forward to notification subscriber so they see the completion
				c.notifyMu.Lock()
				if c.notifyCh != nil {
					// Send a copy with id set to signal completion
					select {
					case c.notifyCh <- &msg:
					default:
					}
				}
				c.notifyMu.Unlock()

				ch <- &msg
				continue
			}
		}

		// Track session-scoped mode / model changes before forwarding —
		// the subscriber may or may not act on the notification, but
		// the connection itself needs its state kept in sync so /info,
		// /mode and /model reflect the latest values regardless.
		if msg.Method != nil && *msg.Method == "session/update" {
			if ev := ClassifyNotification(&msg); ev != nil {
				switch ev.Type {
				case AcpEventModeUpdate:
					if ev.ModeID != "" {
						c.SetCurrentMode(ev.ModeID)
					}
				case AcpEventModelUpdate:
					if ev.ModelID != "" {
						c.SetCurrentModel(ev.ModelID)
					}
				}
			}
		}

		// Surface Kiro's silent agent fallback. Kiro emits
		// `_kiro.dev/agent/not_found` when `--agent <name>` does not
		// match any installed agent; without this log, users see
		// wrong-persona responses and wonder why, with no hint that
		// Kiro substituted another agent behind their back.
		if msg.Method != nil && *msg.Method == "_kiro.dev/agent/not_found" && msg.Params != nil {
			var p struct {
				RequestedAgent string `json:"requestedAgent"`
				FallbackAgent  string `json:"fallbackAgent"`
			}
			if err := json.Unmarshal(*msg.Params, &p); err == nil {
				slog.Warn("🚨 kiro agent not found, fell back",
					"requested", p.RequestedAgent,
					"fallback", p.FallbackAgent,
					"hint", "check the --agent arg in [agent].args against the agent name in ~/.kiro/agents/*.json")
			}
		}

		// Notification → forward to subscriber
		c.notifyMu.Lock()
		if c.notifyCh != nil {
			select {
			case c.notifyCh <- &msg:
			default:
			}
		}
		c.notifyMu.Unlock()
	}

	// Connection closed — build descriptive error from exit code + stderr
	errMsg := "connection closed"
	if c.cmd.ProcessState != nil {
		exitCode := c.cmd.ProcessState.ExitCode()
		stderr := strings.TrimSpace(c.stderrBuf.String())
		if stderr != "" {
			// Take last line of stderr (most relevant)
			lines := strings.Split(stderr, "\n")
			lastLine := strings.TrimSpace(lines[len(lines)-1])
			errMsg = fmt.Sprintf("agent exited (code %d): %s", exitCode, lastLine)
		} else {
			errMsg = fmt.Sprintf("agent exited (code %d)", exitCode)
		}
		slog.Error("agent process exited", "exit_code", exitCode, "stderr", stderr)
	}

	c.resolvePendingWithError(errMsg)
}

// resolvePendingWithError force-resolves every in-flight request with the
// given error message AND forwards the same error to the notification
// subscriber (if any) so streaming consumers (streamPrompt) wake up and
// surface the failure to the user.
//
// Mirrors cancelWatchdog's dual-channel delivery, but for "agent died"
// rather than "agent ignored cancel". Without forwarding to notifyCh,
// streamPrompt — which reads from notifyCh, not from the pending
// response channel — would block forever, leaving the user staring at a
// thinking/restoring placeholder until idle cleanup (which itself does
// not unblock the goroutine, only removes the connection from the pool).
//
// The notifyCh send is bounded by 2s so a stuck consumer doesn't keep
// readLoop's goroutine alive past process exit.
func (c *AcpConnection) resolvePendingWithError(errMsg string) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[uint64]chan *JsonRpcMessage)
	c.pendingMu.Unlock()

	for id, ch := range pending {
		idCopy := id
		msg := &JsonRpcMessage{
			ID:    &idCopy,
			Error: &JsonRpcError{Code: -1, Message: errMsg},
		}

		// Forward to the notification subscriber first — that's the
		// channel streamPrompt reads. Without this, streamPrompt hangs.
		c.notifyMu.Lock()
		target := c.notifyCh
		if target != nil {
			select {
			case target <- msg:
			case <-time.After(2 * time.Second):
				slog.Error("acp: connection died but could not forward error to notifyCh — stream may hang",
					"session_id", c.SessionID, "thread_key", c.ThreadKey, "request_id", id)
			}
		}
		c.notifyMu.Unlock()

		// Pending channel is buffered size 1 — non-blocking send is safe.
		select {
		case ch <- msg:
		default:
		}
	}
}

func (c *AcpConnection) sendRaw(data string) error {
	slog.Debug("acp_send", "data", data)
	c.stdinMu.Lock()
	defer c.stdinMu.Unlock()
	_, err := fmt.Fprintf(c.stdin, "%s\n", data)
	return err
}

func (c *AcpConnection) sendRequest(method string, params interface{}) (*JsonRpcMessage, error) {
	id := c.nextID.Add(1) - 1
	req := NewJsonRpcRequest(id, method, params)
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	ch := make(chan *JsonRpcMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	if err := c.sendRaw(string(data)); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	timeoutSecs := 30
	if method == "session/new" {
		timeoutSecs = 120
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp, nil
	case <-time.After(time.Duration(timeoutSecs) * time.Second):
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("timeout waiting for %s response", method)
	}
}

func (c *AcpConnection) Initialize() error {
	resp, err := c.sendRequest("initialize", map[string]interface{}{
		"protocolVersion":    1,
		"clientCapabilities": map[string]interface{}{},
		"clientInfo":         map[string]string{"name": "quill", "version": "0.1.0"},
	})
	if err != nil {
		return err
	}

	agentName := "unknown"
	if resp.Result != nil {
		var result struct {
			AgentInfo struct {
				Name string `json:"name"`
			} `json:"agentInfo"`
			AgentCapabilities struct {
				LoadSession bool `json:"loadSession"`
			} `json:"agentCapabilities"`
		}
		if err := json.Unmarshal(*resp.Result, &result); err == nil {
			if result.AgentInfo.Name != "" {
				agentName = result.AgentInfo.Name
			}
			c.CanLoadSession = result.AgentCapabilities.LoadSession
		}
	}
	slog.Info("initialized", "agent", agentName, "load_session", c.CanLoadSession)
	return nil
}

func (c *AcpConnection) SessionNew(cwd string) (string, error) {
	resp, err := c.sendRequest("session/new", map[string]interface{}{
		"cwd":        cwd,
		"mcpServers": []interface{}{},
	})
	if err != nil {
		return "", err
	}

	if resp.Result == nil {
		return "", fmt.Errorf("no result in session/new response")
	}

	var result struct {
		SessionID string    `json:"sessionId"`
		Modes     *ModeSet  `json:"modes,omitempty"`
		Models    *ModelSet `json:"models,omitempty"`
	}
	if err := json.Unmarshal(*resp.Result, &result); err != nil {
		return "", err
	}
	if result.SessionID == "" {
		return "", fmt.Errorf("no sessionId in session/new response")
	}

	slog.Info("session created", "session_id", result.SessionID)
	c.SessionID = result.SessionID
	c.applyModeSet(result.Modes)
	c.applyModelSet(result.Models)
	return result.SessionID, nil
}

// applyModeSet stashes whatever `modes` object the agent returned from
// session/new or session/load. When the response omits the field (older
// agents), the previous state is preserved.
func (c *AcpConnection) applyModeSet(ms *ModeSet) {
	if ms == nil {
		return
	}
	c.setModeState(ms.CurrentModeID, ms.AvailableModes)
	slog.Info("session modes advertised",
		"current", ms.CurrentModeID,
		"available_count", len(ms.AvailableModes))
}

// applyModelSet mirrors applyModeSet for the model axis.
func (c *AcpConnection) applyModelSet(ms *ModelSet) {
	if ms == nil {
		return
	}
	c.setModelState(ms.CurrentModelID, ms.AvailableModels)
	slog.Info("session models advertised",
		"current", ms.CurrentModelID,
		"available_count", len(ms.AvailableModels))
}

// SessionLoad attempts to resume a previous session by ID.
// The agent replays conversation history as session/update notifications,
// then responds to signal load is complete.
// Returns nil on success; the caller can then use SessionPrompt as normal.
func (c *AcpConnection) SessionLoad(sessionID string, cwd string) error {
	if !c.CanLoadSession {
		return fmt.Errorf("agent does not support session/load")
	}

	resp, err := c.sendRequest("session/load", map[string]interface{}{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []interface{}{},
	})
	if err != nil {
		return fmt.Errorf("session/load failed: %w", err)
	}

	// session/load may return null result on success
	if resp.Error != nil {
		return fmt.Errorf("session/load error: %s", resp.Error.Message)
	}

	slog.Info("session loaded", "session_id", sessionID)
	c.SessionID = sessionID
	c.SessionResumed = true
	c.WasResumed = true

	if resp.Result != nil {
		var result struct {
			Modes  *ModeSet  `json:"modes,omitempty"`
			Models *ModelSet `json:"models,omitempty"`
		}
		if err := json.Unmarshal(*resp.Result, &result); err == nil {
			c.applyModeSet(result.Modes)
			c.applyModelSet(result.Models)
		}
	}
	return nil
}

// SessionSetMode asks the agent to switch the current session to the
// given mode. The agent is expected to emit a `current_mode_update`
// session notification on success, which the read loop uses to keep
// CurrentModeID in sync.
func (c *AcpConnection) SessionSetMode(modeID string) error {
	if c.SessionID == "" {
		return fmt.Errorf("no active session")
	}
	if modeID == "" {
		return fmt.Errorf("modeId is required")
	}
	resp, err := c.sendRequest("session/set_mode", map[string]interface{}{
		"sessionId": c.SessionID,
		"modeId":    modeID,
	})
	if err != nil {
		return fmt.Errorf("session/set_mode failed: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("session/set_mode error: %s", resp.Error.Message)
	}
	// Optimistically reflect the change locally — if the agent also
	// emits a `current_mode_update` notification, SetCurrentMode will
	// run with the same id and be a no-op.
	c.SetCurrentMode(modeID)
	slog.Info("session mode switched", "session_id", c.SessionID, "mode_id", modeID)
	return nil
}

// SessionSetModel asks the agent to switch the current session to the
// given model id. Parallels SessionSetMode: same error shape, same
// optimistic local update, expects a `current_model_update`
// notification to reconcile.
func (c *AcpConnection) SessionSetModel(modelID string) error {
	if c.SessionID == "" {
		return fmt.Errorf("no active session")
	}
	if modelID == "" {
		return fmt.Errorf("modelId is required")
	}
	resp, err := c.sendRequest("session/set_model", map[string]interface{}{
		"sessionId": c.SessionID,
		"modelId":   modelID,
	})
	if err != nil {
		return fmt.Errorf("session/set_model failed: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("session/set_model error: %s", resp.Error.Message)
	}
	c.SetCurrentModel(modelID)
	slog.Info("session model switched", "session_id", c.SessionID, "model_id", modelID)
	return nil
}

// SessionPrompt sends a prompt and returns a channel for streaming notifications.
// The final message on the channel will have ID set (the prompt response).
// Only one prompt may be active at a time — concurrent callers block until PromptDone.
// Returns the notification channel, request ID, whether this is a reset, whether
// this is a resumed session, and any error.
func (c *AcpConnection) SessionPrompt(content []ContentBlock, owner string) (<-chan *JsonRpcMessage, uint64, bool, bool, error) {
	c.promptMu.Lock() // released by PromptDone
	c.setCurrentOwner(owner)

	// Consume one-shot flags under promptMu to avoid races
	reset := c.SessionReset
	c.SessionReset = false
	resumed := c.SessionResumed
	c.SessionResumed = false

	c.touchLastActive()
	c.MessageCount.Add(1)

	if c.SessionID == "" {
		c.setCurrentOwner("")
		c.promptMu.Unlock()
		return nil, 0, false, false, fmt.Errorf("no session")
	}

	notifyCh := make(chan *JsonRpcMessage, 256)
	c.notifyMu.Lock()
	c.notifyCh = notifyCh
	c.notifyMu.Unlock()

	id := c.nextID.Add(1) - 1
	req := NewJsonRpcRequest(id, "session/prompt", map[string]interface{}{
		"sessionId": c.SessionID,
		"prompt":    content,
	})
	data, err := json.Marshal(req)
	if err != nil {
		c.setCurrentOwner("")
		c.promptMu.Unlock()
		return nil, 0, false, false, err
	}

	respCh := make(chan *JsonRpcMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	if err := c.sendRaw(string(data)); err != nil {
		c.setCurrentOwner("")
		c.promptMu.Unlock()
		return nil, 0, false, false, err
	}

	return notifyCh, id, reset, resumed, nil
}

// PromptDone cleans up the notification subscriber after prompt streaming is done.
// It releases the prompt lock so the next queued prompt can proceed.
func (c *AcpConnection) PromptDone() {
	c.notifyMu.Lock()
	c.notifyCh = nil
	c.notifyMu.Unlock()
	c.touchLastActive()
	c.setCurrentOwner("")
	c.promptMu.Unlock()
}

// cancelWatchdogTimeout is how long SessionCancel waits for the agent to
// honor session/cancel (i.e. return its pending session/prompt response
// with stopReason="cancelled") before synthesizing that response itself.
// Some ACP agents may not implement session/cancel; without this fallback
// the prompt goroutine would block indefinitely on the pending channel.
var cancelWatchdogTimeout = 10 * time.Second

// watchdogPreSendHook is called (when non-nil) immediately before the
// watchdog enters its blocking send on notifyCh. Intended for tests
// that need to synchronize with the exact moment the watchdog is about
// to block, instead of relying on wall-clock sleeps. Nil in production.
var watchdogPreSendHook func()

// SessionCancel sends a session/cancel notification to the agent.
// This is a JSON-RPC notification (no id, no response); the agent stops
// producing output for the active prompt and the pending session/prompt
// response returns with stopReason="cancelled".
//
// Must be called from a goroutine distinct from the one blocked inside
// SessionPrompt — it does NOT attempt to acquire promptMu, since the
// prompt goroutine already holds it and releases it via PromptDone after
// the cancelled response arrives.
//
// A watchdog goroutine force-resolves any still-pending session/prompt
// requests with a synthetic stopReason="cancelled" response after
// cancelWatchdogTimeout, guarding against agents that ignore
// session/cancel or have died mid-prompt.
func (c *AcpConnection) SessionCancel() error {
	if c.SessionID == "" {
		return fmt.Errorf("no session")
	}
	if !c.Alive() {
		return fmt.Errorf("connection not alive")
	}

	// Snapshot pending request IDs before sending cancel. Only these IDs
	// will be force-resolved by the watchdog — IDs created *after* cancel
	// (e.g. a new prompt on the same connection) are left alone.
	c.pendingMu.Lock()
	pendingIDs := make([]uint64, 0, len(c.pending))
	for id := range c.pending {
		pendingIDs = append(pendingIDs, id)
	}
	c.pendingMu.Unlock()

	notif := NewJsonRpcNotification("session/cancel", map[string]interface{}{
		"sessionId": c.SessionID,
	})
	data, err := json.Marshal(notif)
	if err != nil {
		return err
	}
	slog.Info("acp: sending session/cancel", "session_id", c.SessionID, "thread_key", c.ThreadKey, "pending", len(pendingIDs))
	if err := c.sendRaw(string(data)); err != nil {
		return err
	}

	if len(pendingIDs) > 0 {
		go c.cancelWatchdog(pendingIDs, cancelWatchdogTimeout)
	}
	return nil
}

// cancelWatchdog waits for the given timeout then force-resolves any of
// the specified pending request IDs that are still outstanding with a
// synthetic response carrying stopReason="cancelled". The prompt
// goroutine then returns normally and PromptDone releases promptMu.
// The synthetic response is also forwarded to the notification
// subscriber (if any) so the streaming loop actually wakes up to observe
// it — the rx channel in streamPrompt is the subscriber, not the
// pending map.
func (c *AcpConnection) cancelWatchdog(pendingIDs []uint64, timeout time.Duration) {
	time.Sleep(timeout)

	synthetic := json.RawMessage(`{"stopReason":"cancelled"}`)
	for _, id := range pendingIDs {
		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		if !ok {
			continue
		}
		slog.Warn("acp: session/cancel watchdog firing — agent did not honor cancel",
			"session_id", c.SessionID, "thread_key", c.ThreadKey, "request_id", id,
			"timeout", timeout)
		reqID := id
		msg := &JsonRpcMessage{ID: &reqID, Result: &synthetic}

		// Forward to notification subscriber first (the rx channel the
		// streaming loop reads from) — if we only resolved the pending
		// channel, the loop would miss the completion signal.
		//
		// Hold notifyMu across the entire send. Together with the
		// invariant that pending[id] still existed one line above
		// (i.e. streamPrompt for this id hasn't seen a response yet,
		// so PromptDone cannot have cleared notifyCh), this guarantees
		// the channel we send to is the one the live streamPrompt is
		// reading — not a stale pointer left over from a prior prompt
		// on the same connection. A 2s timeout bounds the watchdog
		// goroutine's lifetime if the consumer has stopped reading
		// (e.g. handler panicked).
		c.notifyMu.Lock()
		target := c.notifyCh
		if target != nil {
			if watchdogPreSendHook != nil {
				watchdogPreSendHook()
			}
			select {
			case target <- msg:
			case <-time.After(2 * time.Second):
				slog.Error("acp: watchdog could not forward synthetic cancelled to notifyCh — stream may hang",
					"session_id", c.SessionID, "thread_key", c.ThreadKey, "request_id", id)
			}
		}
		c.notifyMu.Unlock()

		ch <- msg
	}
}

func (c *AcpConnection) Alive() bool {
	return c.alive.Load()
}

func (c *AcpConnection) Kill() {
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
}
