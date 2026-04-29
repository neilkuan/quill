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
	conn.alive.Store(true)

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
	conn.alive.Store(true)

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

func TestAcpConnection_SessionCancel_NotAlive(t *testing.T) {
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		SessionID: "sess_dead",
	}
	// alive defaults to false
	err := conn.SessionCancel()
	if err == nil {
		t.Fatal("expected error when connection is not alive")
	}
	if !strings.Contains(err.Error(), "not alive") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestAcpConnection_SessionCancel_WatchdogFires verifies that if the agent
// ignores session/cancel, the watchdog synthesizes a stopReason=cancelled
// response so the streaming loop does not hang forever.
func TestAcpConnection_SessionCancel_WatchdogFires(t *testing.T) {
	// Shrink the watchdog timeout for the test.
	origTimeout := cancelWatchdogTimeout
	cancelWatchdogTimeout = 50 * time.Millisecond
	defer func() { cancelWatchdogTimeout = origTimeout }()

	w := &fakeWriteCloser{}
	notifyCh := make(chan *JsonRpcMessage, 16)
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_stuck",
		notifyCh:  notifyCh,
	}
	conn.alive.Store(true)

	// Register a pending prompt ID (simulates an in-flight session/prompt).
	respCh := make(chan *JsonRpcMessage, 1)
	const pendingID uint64 = 7
	conn.pendingMu.Lock()
	conn.pending[pendingID] = respCh
	conn.pendingMu.Unlock()

	if err := conn.SessionCancel(); err != nil {
		t.Fatalf("SessionCancel returned error: %v", err)
	}

	// Both the pending channel and the notification channel should
	// receive the synthetic cancelled response within the timeout.
	select {
	case msg := <-respCh:
		if StopReason(msg) != "cancelled" {
			t.Errorf("expected stopReason=cancelled on pending channel, got %q", StopReason(msg))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog did not resolve pending channel in time")
	}

	select {
	case msg := <-notifyCh:
		if StopReason(msg) != "cancelled" {
			t.Errorf("expected stopReason=cancelled on notify channel, got %q", StopReason(msg))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watchdog did not forward to notify channel")
	}

	// Pending map should be cleared.
	conn.pendingMu.Lock()
	_, stillPending := conn.pending[pendingID]
	conn.pendingMu.Unlock()
	if stillPending {
		t.Error("watchdog did not remove pending entry")
	}
}

// TestAcpConnection_SessionCancel_WatchdogBlocksBriefly verifies the
// watchdog's bounded blocking send: when notifyCh is full, it waits for
// the consumer to drain one slot instead of silently dropping the
// synthetic cancelled response. Without this behavior the streaming
// loop (which reads from notifyCh, not the pending channel) would hang.
//
// Synchronization is done via watchdogPreSendHook (fires exactly when
// the watchdog is about to enter its blocking send) instead of a wall
// sleep — makes the test independent of CI scheduler timing. Go channel
// FIFO guarantees the filler is delivered before the subsequently-sent
// cancelled response.
func TestAcpConnection_SessionCancel_WatchdogBlocksBriefly(t *testing.T) {
	origTimeout := cancelWatchdogTimeout
	cancelWatchdogTimeout = 10 * time.Millisecond
	defer func() { cancelWatchdogTimeout = origTimeout }()

	blocked := make(chan struct{})
	origHook := watchdogPreSendHook
	watchdogPreSendHook = func() { close(blocked) }
	defer func() { watchdogPreSendHook = origHook }()

	w := &fakeWriteCloser{}
	// Notification channel with a tiny buffer, filled to capacity so the
	// first send from the watchdog will block.
	notifyCh := make(chan *JsonRpcMessage, 1)
	filler := json.RawMessage(`{"filler":true}`)
	notifyCh <- &JsonRpcMessage{Result: &filler}

	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_full",
		notifyCh:  notifyCh,
	}
	conn.alive.Store(true)

	respCh := make(chan *JsonRpcMessage, 1)
	const pendingID uint64 = 11
	conn.pendingMu.Lock()
	conn.pending[pendingID] = respCh
	conn.pendingMu.Unlock()

	if err := conn.SessionCancel(); err != nil {
		t.Fatalf("SessionCancel returned error: %v", err)
	}

	// Wait deterministically until the watchdog is about to enter its
	// blocking send — no clock-based sleeps, no scheduler flakiness.
	select {
	case <-blocked:
	case <-time.After(1 * time.Second):
		t.Fatal("watchdog did not reach blocking send in time")
	}

	// First receive: the filler (inserted before the watchdog's send;
	// Go channels are FIFO per the spec, "Channels act as first-in-
	// first-out queues").
	first := <-notifyCh
	if StopReason(first) == "cancelled" {
		t.Fatal("got cancelled before filler — channel FIFO broken")
	}

	// Second receive: the synthetic cancelled response, now unblocked.
	select {
	case m := <-notifyCh:
		if StopReason(m) != "cancelled" {
			t.Fatalf("expected cancelled on notifyCh after drain, got %+v", m)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("watchdog did not deliver cancelled to notifyCh despite room appearing")
	}
}

// TestAcpConnection_SessionCancel_WatchdogSkipsResolvedIDs verifies the
// watchdog is a no-op when the agent already responded normally before
// the timeout fires — no double-resolve, no spurious cancelled messages.
func TestAcpConnection_SessionCancel_WatchdogSkipsResolvedIDs(t *testing.T) {
	origTimeout := cancelWatchdogTimeout
	cancelWatchdogTimeout = 50 * time.Millisecond
	defer func() { cancelWatchdogTimeout = origTimeout }()

	w := &fakeWriteCloser{}
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_honored",
	}
	conn.alive.Store(true)

	respCh := make(chan *JsonRpcMessage, 1)
	const pendingID uint64 = 3
	conn.pendingMu.Lock()
	conn.pending[pendingID] = respCh
	conn.pendingMu.Unlock()

	if err := conn.SessionCancel(); err != nil {
		t.Fatalf("SessionCancel returned error: %v", err)
	}

	// Simulate the agent honoring cancel — readLoop would normally do
	// this: remove from pending and deliver the real response.
	realResult := json.RawMessage(`{"stopReason":"cancelled","source":"agent"}`)
	conn.pendingMu.Lock()
	delete(conn.pending, pendingID)
	conn.pendingMu.Unlock()
	id := pendingID
	respCh <- &JsonRpcMessage{ID: &id, Result: &realResult}

	// Drain respCh to get the real response.
	got := <-respCh
	var body map[string]string
	_ = json.Unmarshal(*got.Result, &body)
	if body["source"] != "agent" {
		t.Errorf("expected agent-sourced response, got %v", body)
	}

	// Wait for watchdog to fire — it should find nothing to resolve.
	time.Sleep(100 * time.Millisecond)

	// No second message should appear on respCh.
	select {
	case extra := <-respCh:
		t.Fatalf("watchdog fired for already-resolved id, got %+v", extra)
	default:
	}
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

func TestConnectionModes_RoundTrip(t *testing.T) {
	c := &AcpConnection{}
	// Fresh conn has no modes.
	avail, cur := c.Modes()
	if len(avail) != 0 || cur != "" {
		t.Fatalf("fresh conn should have no modes, got avail=%v cur=%q", avail, cur)
	}

	c.setModeState("ask", []ModeInfo{{ID: "ask", Name: "Ask"}, {ID: "code", Name: "Code"}})
	avail, cur = c.Modes()
	if cur != "ask" || len(avail) != 2 {
		t.Fatalf("post-setModeState state wrong: cur=%q avail=%v", cur, avail)
	}

	// SetCurrentMode simulates a current_mode_update notification
	// arriving mid-session — only the active id changes.
	c.SetCurrentMode("code")
	_, cur = c.Modes()
	if cur != "code" {
		t.Errorf("SetCurrentMode didn't stick: %q", cur)
	}

	// applyModeSet with nil is a no-op: older agents that omit the
	// modes field from session/new must not clobber prior state.
	c.applyModeSet(nil)
	avail, cur = c.Modes()
	if cur != "code" || len(avail) != 2 {
		t.Errorf("nil applyModeSet should preserve state; got cur=%q avail=%v", cur, avail)
	}

	// Modes() must return a defensive copy — mutating the returned
	// slice cannot reach the connection's internal state.
	avail[0].ID = "mutated"
	avail2, _ := c.Modes()
	if avail2[0].ID != "ask" {
		t.Errorf("Modes() must return a defensive copy; got %q", avail2[0].ID)
	}
}

func TestConnectionModels_RoundTrip(t *testing.T) {
	c := &AcpConnection{}
	avail, cur := c.Models()
	if len(avail) != 0 || cur != "" {
		t.Fatalf("fresh conn should have no models, got avail=%v cur=%q", avail, cur)
	}

	c.setModelState("haiku", []ModelInfo{{ID: "haiku", Name: "Haiku"}, {ID: "sonnet", Name: "Sonnet"}})
	avail, cur = c.Models()
	if cur != "haiku" || len(avail) != 2 {
		t.Fatalf("post-setModelState state wrong: cur=%q avail=%v", cur, avail)
	}

	c.SetCurrentModel("sonnet")
	_, cur = c.Models()
	if cur != "sonnet" {
		t.Errorf("SetCurrentModel didn't stick: %q", cur)
	}

	// Nil applyModelSet must preserve state (older agents may omit).
	c.applyModelSet(nil)
	avail, cur = c.Models()
	if cur != "sonnet" || len(avail) != 2 {
		t.Errorf("nil applyModelSet should preserve state; got cur=%q avail=%v", cur, avail)
	}

	avail[0].ID = "mutated"
	avail2, _ := c.Models()
	if avail2[0].ID != "haiku" {
		t.Errorf("Models() must return a defensive copy; got %q", avail2[0].ID)
	}
}

// TestAcpConnection_ResolvePendingWithError_ForwardsToNotifyCh verifies
// that when the agent process dies, every in-flight pending request gets
// resolved AND the same error is forwarded to the notification subscriber.
//
// Without forwarding to notifyCh, streamPrompt (which reads from notifyCh,
// not from the pending channel) hangs forever — the user sees the
// thinking/restoring placeholder stuck on screen until idle cleanup,
// which is exactly the silent-death symptom we observed with kiro-cli
// crashing on base64 ImageBlock prompts.
func TestAcpConnection_ResolvePendingWithError_ForwardsToNotifyCh(t *testing.T) {
	w := &fakeWriteCloser{}
	notifyCh := make(chan *JsonRpcMessage, 16)
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_dead",
		ThreadKey: "teams:test",
		notifyCh:  notifyCh,
	}
	conn.alive.Store(true)

	respCh := make(chan *JsonRpcMessage, 1)
	const pendingID uint64 = 42
	conn.pendingMu.Lock()
	conn.pending[pendingID] = respCh
	conn.pendingMu.Unlock()

	conn.resolvePendingWithError("agent exited (code 1): segfault")

	// notifyCh: streaming consumer wakes up here.
	select {
	case msg := <-notifyCh:
		if msg.ID == nil || *msg.ID != pendingID {
			t.Errorf("expected forwarded msg to carry id=%d, got %+v", pendingID, msg)
		}
		if msg.Error == nil || !strings.Contains(msg.Error.Message, "segfault") {
			t.Errorf("expected error message with stderr tail, got %+v", msg.Error)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("resolvePendingWithError did not forward to notifyCh")
	}

	// pending channel: sendRequest path also wakes up.
	select {
	case msg := <-respCh:
		if msg.Error == nil {
			t.Errorf("expected error on pending channel, got %+v", msg)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("resolvePendingWithError did not resolve pending channel")
	}

	// pending map cleared.
	conn.pendingMu.Lock()
	leftover := len(conn.pending)
	conn.pendingMu.Unlock()
	if leftover != 0 {
		t.Errorf("expected pending map cleared, still has %d entries", leftover)
	}
}

// TestAcpConnection_ResolvePendingWithError_NoSubscriber covers the
// no-streaming-consumer branch — sendRequest still gets its error even
// when notifyCh is nil (e.g. agent dies before any prompt was issued, or
// streamPrompt already called PromptDone). Must not panic / block.
func TestAcpConnection_ResolvePendingWithError_NoSubscriber(t *testing.T) {
	w := &fakeWriteCloser{}
	conn := &AcpConnection{
		pending:   make(map[uint64]chan *JsonRpcMessage),
		stdin:     w,
		SessionID: "sess_dead2",
		notifyCh:  nil,
	}
	conn.alive.Store(true)

	respCh := make(chan *JsonRpcMessage, 1)
	conn.pendingMu.Lock()
	conn.pending[1] = respCh
	conn.pendingMu.Unlock()

	done := make(chan struct{})
	go func() {
		conn.resolvePendingWithError("connection closed")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("resolvePendingWithError blocked when notifyCh was nil")
	}

	select {
	case msg := <-respCh:
		if msg.Error == nil || msg.Error.Message != "connection closed" {
			t.Errorf("expected pending channel to receive 'connection closed' error, got %+v", msg)
		}
	default:
		t.Fatal("pending channel did not receive error")
	}
}
