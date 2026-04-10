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
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdinMu      sync.Mutex
	stderrBuf    *bytes.Buffer
	nextID       atomic.Uint64
	pending      map[uint64]chan *JsonRpcMessage
	pendingMu    sync.Mutex
	notifyCh     chan *JsonRpcMessage
	notifyMu     sync.Mutex
	promptMu     sync.Mutex // serialize prompts — only one at a time per connection
	SessionID    string
	LastActive   time.Time
	SessionReset bool
	alive        atomic.Bool
}

func expandEnv(val string) string {
	if strings.HasPrefix(val, "${") && strings.HasSuffix(val, "}") {
		key := val[2 : len(val)-1]
		return os.Getenv(key)
	}
	return val
}

func SpawnConnection(command string, args []string, workingDir string, env map[string]string) (*AcpConnection, error) {
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

	conn := &AcpConnection{
		cmd:        cmd,
		stdin:      stdinPipe,
		stderrBuf:  &stderrBuf,
		pending:    make(map[uint64]chan *JsonRpcMessage),
		LastActive: time.Now(),
	}
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

	c.pendingMu.Lock()
	for id, ch := range c.pending {
		ch <- &JsonRpcMessage{
			Error: &JsonRpcError{Code: -1, Message: errMsg},
		}
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
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
		"clientInfo":         map[string]string{"name": "openab-go", "version": "0.1.0"},
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
		}
		if err := json.Unmarshal(*resp.Result, &result); err == nil && result.AgentInfo.Name != "" {
			agentName = result.AgentInfo.Name
		}
	}
	slog.Info("initialized", "agent", agentName)
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
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(*resp.Result, &result); err != nil {
		return "", err
	}
	if result.SessionID == "" {
		return "", fmt.Errorf("no sessionId in session/new response")
	}

	slog.Info("session created", "session_id", result.SessionID)
	c.SessionID = result.SessionID
	return result.SessionID, nil
}

// SessionPrompt sends a prompt and returns a channel for streaming notifications.
// The final message on the channel will have ID set (the prompt response).
// Only one prompt may be active at a time — concurrent callers block until PromptDone.
func (c *AcpConnection) SessionPrompt(prompt string) (<-chan *JsonRpcMessage, uint64, error) {
	c.promptMu.Lock() // released by PromptDone

	c.LastActive = time.Now()

	if c.SessionID == "" {
		c.promptMu.Unlock()
		return nil, 0, fmt.Errorf("no session")
	}

	notifyCh := make(chan *JsonRpcMessage, 256)
	c.notifyMu.Lock()
	c.notifyCh = notifyCh
	c.notifyMu.Unlock()

	id := c.nextID.Add(1) - 1
	req := NewJsonRpcRequest(id, "session/prompt", map[string]interface{}{
		"sessionId": c.SessionID,
		"prompt":    []map[string]string{{"type": "text", "text": prompt}},
	})
	data, err := json.Marshal(req)
	if err != nil {
		c.promptMu.Unlock()
		return nil, 0, err
	}

	respCh := make(chan *JsonRpcMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	if err := c.sendRaw(string(data)); err != nil {
		c.promptMu.Unlock()
		return nil, 0, err
	}

	return notifyCh, id, nil
}

// PromptDone cleans up the notification subscriber after prompt streaming is done.
// It releases the prompt lock so the next queued prompt can proceed.
func (c *AcpConnection) PromptDone() {
	c.notifyMu.Lock()
	c.notifyCh = nil
	c.notifyMu.Unlock()
	c.LastActive = time.Now()
	c.promptMu.Unlock()
}

func (c *AcpConnection) Alive() bool {
	return c.alive.Load()
}

func (c *AcpConnection) Kill() {
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
}
