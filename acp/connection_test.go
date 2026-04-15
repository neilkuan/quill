package acp

import (
	"os"
	"testing"
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
