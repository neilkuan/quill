package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionPool_CancelSession_NoConnection(t *testing.T) {
	p := &SessionPool{connections: make(map[string]*AcpConnection)}

	err := p.CancelSession("discord:missing")
	if err == nil {
		t.Fatal("expected error for missing thread")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSessionPool_CancelSession_SendsNotification(t *testing.T) {
	w := &fakeWriteCloser{}
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_pool",
		ThreadKey: "discord:42",
	}
	conn.alive.Store(true)
	p := &SessionPool{connections: map[string]*AcpConnection{
		"discord:42": conn,
	}}

	if err := p.CancelSession("discord:42"); err != nil {
		t.Fatalf("CancelSession returned error: %v", err)
	}

	line := strings.TrimSpace(w.buf.String())
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("invalid json written to stdin: %v (line=%q)", err, line)
	}
	if msg["method"] != "session/cancel" {
		t.Errorf("expected method session/cancel, got %v", msg["method"])
	}
	params, _ := msg["params"].(map[string]any)
	if params["sessionId"] != "sess_pool" {
		t.Errorf("expected sessionId 'sess_pool', got %v", params["sessionId"])
	}
}
