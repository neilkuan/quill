package acp

import (
	"encoding/json"
	"testing"
)

func TestNewJsonRpcRequest(t *testing.T) {
	req := NewJsonRpcRequest(1, "test/method", map[string]string{"key": "value"})
	if req.Jsonrpc != "2.0" {
		t.Fatalf("expected jsonrpc '2.0', got %q", req.Jsonrpc)
	}
	if req.ID != 1 {
		t.Fatalf("expected id 1, got %d", req.ID)
	}
	if req.Method != "test/method" {
		t.Fatalf("expected method 'test/method', got %q", req.Method)
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	if m["jsonrpc"] != "2.0" {
		t.Fatal("marshalled jsonrpc mismatch")
	}
}

func TestNewJsonRpcResponse(t *testing.T) {
	resp := NewJsonRpcResponse(42, "ok")
	if resp.Jsonrpc != "2.0" || resp.ID != 42 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestJsonRpcError_Error(t *testing.T) {
	e := &JsonRpcError{Code: -32600, Message: "invalid request"}
	if e.Error() != "invalid request" {
		t.Fatalf("expected 'invalid request', got %q", e.Error())
	}
}

func TestClassifyNotification_AgentMessageChunk(t *testing.T) {
	msg := makeNotification(t, "agent_message_chunk", `{"text":"hello"}`, "content")
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventText {
		t.Fatalf("expected AcpEventText, got %d", evt.Type)
	}
	if evt.Text != "hello" {
		t.Fatalf("expected text 'hello', got %q", evt.Text)
	}
}

func TestClassifyNotification_AgentThoughtChunk(t *testing.T) {
	msg := makeNotification(t, "agent_thought_chunk", "", "")
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventThinking {
		t.Fatalf("expected AcpEventThinking, got %d", evt.Type)
	}
}

func TestClassifyNotification_ToolCall(t *testing.T) {
	msg := makeNotification(t, "tool_call", `"Read"`, "title")
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventToolStart {
		t.Fatalf("expected AcpEventToolStart, got %d", evt.Type)
	}
	if evt.Title != "Read" {
		t.Fatalf("expected title 'Read', got %q", evt.Title)
	}
}

func TestClassifyNotification_ToolCallUpdateCompleted(t *testing.T) {
	msg := makeNotificationMultiField(t, "tool_call_update", map[string]string{
		"title":  "Edit",
		"status": "completed",
	})
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventToolDone {
		t.Fatalf("expected AcpEventToolDone, got %d", evt.Type)
	}
	if evt.Status != "completed" {
		t.Fatalf("expected status 'completed', got %q", evt.Status)
	}
}

func TestClassifyNotification_ToolCallUpdateFailed(t *testing.T) {
	msg := makeNotificationMultiField(t, "tool_call_update", map[string]string{
		"title":  "Bash",
		"status": "failed",
	})
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventToolDone {
		t.Fatalf("expected AcpEventToolDone, got %d", evt.Type)
	}
	if evt.Status != "failed" {
		t.Fatalf("expected status 'failed', got %q", evt.Status)
	}
}

func TestClassifyNotification_ToolCallUpdateInProgress(t *testing.T) {
	msg := makeNotificationMultiField(t, "tool_call_update", map[string]string{
		"title":  "Bash",
		"status": "running",
	})
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventToolStart {
		t.Fatalf("expected AcpEventToolStart for in-progress update, got %d", evt.Type)
	}
}

func TestClassifyNotification_Plan(t *testing.T) {
	msg := makeNotification(t, "plan", "", "")
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventStatus {
		t.Fatalf("expected AcpEventStatus, got %d", evt.Type)
	}
}

func TestClassifyNotification_UnknownType(t *testing.T) {
	msg := makeNotification(t, "some_unknown_event", "", "")
	evt := ClassifyNotification(msg)
	if evt != nil {
		t.Fatalf("expected nil for unknown event type, got %+v", evt)
	}
}

func TestClassifyNotification_NilParams(t *testing.T) {
	msg := &JsonRpcMessage{}
	evt := ClassifyNotification(msg)
	if evt != nil {
		t.Fatalf("expected nil for nil params, got %+v", evt)
	}
}

func TestClassifyNotification_MalformedParams(t *testing.T) {
	raw := json.RawMessage(`"not an object"`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt != nil {
		t.Fatalf("expected nil for malformed params, got %+v", evt)
	}
}

func TestExtractStringField(t *testing.T) {
	m := map[string]json.RawMessage{
		"title": json.RawMessage(`"hello"`),
		"count": json.RawMessage(`42`),
	}

	if v := extractStringField(m, "title"); v != "hello" {
		t.Fatalf("expected 'hello', got %q", v)
	}
	if v := extractStringField(m, "missing"); v != "" {
		t.Fatalf("expected empty for missing key, got %q", v)
	}
	if v := extractStringField(m, "count"); v != "" {
		t.Fatalf("expected empty for non-string value, got %q", v)
	}
}

func TestTextBlock(t *testing.T) {
	block := TextBlock("hello world")
	if block["type"] != "text" {
		t.Fatalf("expected type 'text', got %v", block["type"])
	}
	if block["text"] != "hello world" {
		t.Fatalf("expected text 'hello world', got %v", block["text"])
	}

	data, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	if m["type"] != "text" || m["text"] != "hello world" {
		t.Fatalf("unexpected JSON: %s", string(data))
	}
}

func TestImageBlock(t *testing.T) {
	block := ImageBlock("aGVsbG8=", "image/png")
	if block["type"] != "image" {
		t.Fatalf("expected type 'image', got %v", block["type"])
	}

	source, ok := block["source"].(map[string]string)
	if !ok {
		t.Fatalf("expected source to be map[string]string, got %T", block["source"])
	}
	if source["type"] != "base64" {
		t.Fatalf("expected source type 'base64', got %q", source["type"])
	}
	if source["media_type"] != "image/png" {
		t.Fatalf("expected media_type 'image/png', got %q", source["media_type"])
	}
	if source["data"] != "aGVsbG8=" {
		t.Fatalf("expected data 'aGVsbG8=', got %q", source["data"])
	}
}

func TestTextBlock_EmptyString(t *testing.T) {
	block := TextBlock("")
	if block["text"] != "" {
		t.Fatalf("expected empty text, got %v", block["text"])
	}
}

// --- helpers ---

func makeNotification(t *testing.T, sessionUpdate, extraValue, extraKey string) *JsonRpcMessage {
	t.Helper()
	update := map[string]interface{}{
		"sessionUpdate": sessionUpdate,
	}
	if extraKey != "" && extraValue != "" {
		update[extraKey] = json.RawMessage(extraValue)
	}

	updateBytes, _ := json.Marshal(update)
	params := map[string]interface{}{
		"update": json.RawMessage(updateBytes),
	}
	paramsBytes, _ := json.Marshal(params)
	raw := json.RawMessage(paramsBytes)
	return &JsonRpcMessage{Params: &raw}
}

func makeNotificationMultiField(t *testing.T, sessionUpdate string, fields map[string]string) *JsonRpcMessage {
	t.Helper()
	update := map[string]interface{}{
		"sessionUpdate": sessionUpdate,
	}
	for k, v := range fields {
		update[k] = v
	}

	updateBytes, _ := json.Marshal(update)
	params := map[string]interface{}{
		"update": json.RawMessage(updateBytes),
	}
	paramsBytes, _ := json.Marshal(params)
	raw := json.RawMessage(paramsBytes)
	return &JsonRpcMessage{Params: &raw}
}
