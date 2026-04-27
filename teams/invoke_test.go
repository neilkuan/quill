package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnmarshalInvokeData_HappyPath_Mode(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`{"quill.action":"switch_mode","thread":"teams:a:abc","mode":"kiro_spec"}`),
	}

	data, err := UnmarshalInvokeData(a)
	if err != nil {
		t.Fatalf("UnmarshalInvokeData: %v", err)
	}
	if data.Action != "switch_mode" {
		t.Errorf("Action = %q, want %q", data.Action, "switch_mode")
	}
	if data.Thread != "teams:a:abc" {
		t.Errorf("Thread = %q, want %q", data.Thread, "teams:a:abc")
	}
	if data.Mode != "kiro_spec" {
		t.Errorf("Mode = %q, want %q", data.Mode, "kiro_spec")
	}
}

func TestUnmarshalInvokeData_HappyPath_Model(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`{"quill.action":"switch_model","thread":"teams:a:abc","model":"claude-opus-4.6"}`),
	}
	data, err := UnmarshalInvokeData(a)
	if err != nil {
		t.Fatalf("UnmarshalInvokeData: %v", err)
	}
	if data.Action != "switch_model" {
		t.Errorf("Action = %q", data.Action)
	}
	if data.Model != "claude-opus-4.6" {
		t.Errorf("Model = %q", data.Model)
	}
}

func TestUnmarshalInvokeData_NoValue_NotInvoke(t *testing.T) {
	a := &Activity{Type: "message", Text: "hi"}

	_, err := UnmarshalInvokeData(a)
	if err == nil {
		t.Fatal("expected error when Value is empty")
	}
	if !errIsNotInvoke(err) {
		t.Errorf("expected ErrNotInvoke, got %v", err)
	}
}

func TestUnmarshalInvokeData_MissingAction(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`{"thread":"teams:a:abc"}`),
	}
	_, err := UnmarshalInvokeData(a)
	if err == nil {
		t.Fatal("expected error when quill.action missing")
	}
	if !errIsNotInvoke(err) {
		t.Errorf("expected ErrNotInvoke, got %v", err)
	}
}

func TestUnmarshalInvokeData_NonJSONValue(t *testing.T) {
	a := &Activity{
		Type:  "message",
		Value: json.RawMessage(`"some string"`),
	}
	_, err := UnmarshalInvokeData(a)
	if err == nil {
		t.Fatal("expected error when Value is not a JSON object")
	}
}

// errIsNotInvoke is a tiny helper that uses errors.Is — defined in the
// production package, but since invoke.go declares the sentinel we just
// reach for it directly.
func errIsNotInvoke(err error) bool {
	return err != nil && (err == ErrNotInvoke || err.Error() == ErrNotInvoke.Error())
}

// captureUpdate keeps the most recent UpdateActivity payload so tests
// can assert on it. The Bot Framework stub server is set up to feed
// requests through it.
type captureUpdate struct {
	method string
	path   string
	body   Activity
}

func newBotFrameworkStub(t *testing.T, cap *captureUpdate) (*httptest.Server, *BotClient) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&cap.body)
		_, _ = w.Write([]byte(`{"id":"updated-1"}`))
	}))
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock", "expires_in": 3600})
	}))
	t.Cleanup(func() {
		ts.Close()
		tokenServer.Close()
	})
	auth := &BotAuth{appID: "app", appSecret: "sec", tenantID: "tn", tokenURL: tokenServer.URL}
	return ts, NewBotClient(auth)
}

// makeInvokeActivity returns a minimal invoke-shaped activity ready for
// OnInvokeAction. The `serviceURL` lets tests point Bot Framework calls
// at httptest stubs.
func makeInvokeActivity(serviceURL, conversationID, replyToID, action, thread, modeOrModelKey, modeOrModelVal string) *Activity {
	value := map[string]string{
		"quill.action": action,
		"thread":       thread,
		modeOrModelKey: modeOrModelVal,
	}
	raw, _ := json.Marshal(value)
	return &Activity{
		Type:         "message",
		ServiceURL:   serviceURL,
		Conversation: Conversation{ID: conversationID},
		ReplyToID:    replyToID,
		Value:        json.RawMessage(raw),
	}
}

// drainCtx is unused by these tests but documents that the invoke
// handler should not depend on a real context.
var _ = context.Background

func TestOnInvokeAction_ThreadMismatch_RendersErrorCard(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	h := &Handler{Client: client}
	a := makeInvokeActivity(ts.URL, "conv-different", "card-1",
		"switch_mode", "teams:a:STALE", "mode", "kiro_spec")

	h.OnInvokeAction(a)

	if cap.method != http.MethodPut {
		t.Errorf("expected UpdateActivity (PUT), got %s", cap.method)
	}
	if cap.path != "/v3/conversations/conv-different/activities/card-1" {
		t.Errorf("unexpected update path: %s", cap.path)
	}
	if len(cap.body.Attachments) == 0 {
		t.Fatalf("expected attachment with error card")
	}
	att := cap.body.Attachments[0]
	if att.ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("ContentType = %q", att.ContentType)
	}
	// Body[0].text should mention the thread-mismatch reason.
	contentJSON, _ := json.Marshal(att.Content)
	if !bytes.Contains(contentJSON, []byte("different conversation")) {
		t.Errorf("error card body missing thread-mismatch text: %s", contentJSON)
	}
}

func TestOnInvokeAction_MissingModeField_RendersErrorCard(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	h := &Handler{Client: client}
	a := makeInvokeActivity(ts.URL, "conv-X", "card-1",
		"switch_mode", "teams:conv-X", "mode", "")

	h.OnInvokeAction(a)

	if len(cap.body.Attachments) == 0 {
		t.Fatalf("expected attachment with error card")
	}
	contentJSON, _ := json.Marshal(cap.body.Attachments[0].Content)
	if !bytes.Contains(contentJSON, []byte("Selection missing")) {
		t.Errorf("error card missing selection-missing text: %s", contentJSON)
	}
}

func TestOnInvokeAction_UnknownAction_NoOp(t *testing.T) {
	cap := &captureUpdate{}
	ts, client := newBotFrameworkStub(t, cap)

	h := &Handler{Client: client}
	a := makeInvokeActivity(ts.URL, "conv-X", "card-1",
		"unknown_action", "teams:conv-X", "mode", "x")

	h.OnInvokeAction(a)

	if cap.method != "" {
		t.Errorf("expected no Bot Framework call for unknown action, got %s %s", cap.method, cap.path)
	}
}
