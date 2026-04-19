package acp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpandEnv(t *testing.T) {
	t.Setenv("QUILL_TEST_VAR", "expanded_value")

	tests := []struct {
		input    string
		expected string
	}{
		{"${QUILL_TEST_VAR}", "expanded_value"},
		{"plain_value", "plain_value"},
		{"${QUILL_NONEXISTENT_VAR_12345}", ""},
		{"partial${VAR}", "partial${VAR}"},
		{"", ""},
	}

	for _, tt := range tests {
		result := expandEnv(tt.input)
		if result != tt.expected {
			t.Errorf("expandEnv(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestSpawnConnection_InvalidCommand(t *testing.T) {
	_, err := SpawnConnection("/nonexistent/binary", nil, os.TempDir(), nil, "test")
	if err == nil {
		t.Fatal("expected error for invalid command")
	}
}

func TestAcpConnection_DefaultFields(t *testing.T) {
	// Verify that a freshly constructed AcpConnection has the expected defaults
	// for the new session resume fields.
	conn := &AcpConnection{
		pending: make(map[uint64]chan *JsonRpcMessage),
	}

	if conn.CanLoadSession {
		t.Error("expected CanLoadSession to be false by default")
	}
	if conn.SessionResumed {
		t.Error("expected SessionResumed to be false by default")
	}
	if conn.SessionReset {
		t.Error("expected SessionReset to be false by default")
	}
	if conn.SessionID != "" {
		t.Errorf("expected empty SessionID, got %q", conn.SessionID)
	}
}

func TestAcpConnection_SessionLoadWithoutCapability(t *testing.T) {
	conn := &AcpConnection{
		pending:        make(map[uint64]chan *JsonRpcMessage),
		CanLoadSession: false,
	}

	err := conn.SessionLoad("sess_123", "/tmp")
	if err == nil {
		t.Fatal("expected error when CanLoadSession is false")
	}
	if err.Error() != "agent does not support session/load" {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}

func TestAcpConnection_SessionCancel_NoSession(t *testing.T) {
	conn := &AcpConnection{
		pending: make(map[uint64]chan *JsonRpcMessage),
	}
	if err := conn.SessionCancel(); err == nil {
		t.Fatal("expected error when SessionID is empty")
	}
}

// fakeWriteCloser captures everything written to it — used to verify the
// exact JSON-RPC line SessionCancel emits without spawning a real agent.
type fakeWriteCloser struct {
	buf    strings.Builder
	closed atomic.Bool
}

func (w *fakeWriteCloser) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *fakeWriteCloser) Close() error {
	w.closed.Store(true)
	return nil
}

func TestAcpConnection_SessionCancel_SendsNotification(t *testing.T) {
	w := &fakeWriteCloser{}
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_abc",
		ThreadKey: "discord:123",
	}

	if err := conn.SessionCancel(); err != nil {
		t.Fatalf("SessionCancel returned error: %v", err)
	}

	line := strings.TrimSpace(w.buf.String())
	if line == "" {
		t.Fatal("expected bytes written to stdin")
	}

	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("not valid json: %v (line=%q)", err, line)
	}

	if msg["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc 2.0, got %v", msg["jsonrpc"])
	}
	if msg["method"] != "session/cancel" {
		t.Errorf("expected method session/cancel, got %v", msg["method"])
	}
	// Notifications must not carry an id field.
	if _, hasID := msg["id"]; hasID {
		t.Errorf("notification must not carry id, got %v", msg["id"])
	}
	params, ok := msg["params"].(map[string]any)
	if !ok {
		t.Fatalf("expected params object, got %T", msg["params"])
	}
	if params["sessionId"] != "sess_abc" {
		t.Errorf("expected sessionId 'sess_abc', got %v", params["sessionId"])
	}
}

// TestAcpConnection_SessionCancel_DoesNotBlock guards against a regression
// where the cancel path might try to acquire promptMu. If another goroutine
// is holding promptMu (as SessionPrompt does), cancel must still return
// promptly so the UI stays responsive.
func TestAcpConnection_SessionCancel_DoesNotBlock(t *testing.T) {
	w := &fakeWriteCloser{}
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_xyz",
	}

	// Simulate an in-flight prompt holding promptMu.
	conn.promptMu.Lock()
	defer conn.promptMu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- conn.SessionCancel()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SessionCancel returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SessionCancel blocked waiting for promptMu — cancel must not take the lock")
	}
}

// readJsonLine is a small helper used by the pool test below.
func readJsonLine(r io.Reader) (string, error) {
	s := bufio.NewScanner(r)
	if !s.Scan() {
		return "", s.Err()
	}
	return s.Text(), nil
}

func TestStopReason(t *testing.T) {
	tests := []struct {
		name string
		msg  *JsonRpcMessage
		want string
	}{
		{"nil message", nil, ""},
		{"notification (no id)", &JsonRpcMessage{}, ""},
		{"response with no result", func() *JsonRpcMessage {
			id := uint64(1)
			return &JsonRpcMessage{ID: &id}
		}(), ""},
		{"response with stopReason cancelled", func() *JsonRpcMessage {
			id := uint64(2)
			raw := json.RawMessage(`{"stopReason":"cancelled"}`)
			return &JsonRpcMessage{ID: &id, Result: &raw}
		}(), "cancelled"},
		{"response with stopReason end_turn", func() *JsonRpcMessage {
			id := uint64(3)
			raw := json.RawMessage(`{"stopReason":"end_turn"}`)
			return &JsonRpcMessage{ID: &id, Result: &raw}
		}(), "end_turn"},
		{"response without stopReason", func() *JsonRpcMessage {
			id := uint64(4)
			raw := json.RawMessage(`{}`)
			return &JsonRpcMessage{ID: &id, Result: &raw}
		}(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StopReason(tt.msg); got != tt.want {
				t.Errorf("StopReason() = %q, want %q", got, tt.want)
			}
		})
	}
}
