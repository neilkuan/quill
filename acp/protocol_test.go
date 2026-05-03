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

func TestClassifyNotification_AgentMessageChunk_TypedText(t *testing.T) {
	// Spec-compliant text chunks include "type":"text". Must classify
	// the same way as the legacy untyped form above.
	msg := makeNotification(t, "agent_message_chunk", `{"type":"text","text":"hi"}`, "content")
	evt := ClassifyNotification(msg)
	if evt == nil || evt.Type != AcpEventText || evt.Text != "hi" {
		t.Fatalf("expected text event with 'hi', got %+v", evt)
	}
}

func TestClassifyNotification_AgentMessageChunk_ImageFlat(t *testing.T) {
	msg := makeNotification(t, "agent_message_chunk",
		`{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}`, "content")
	evt := ClassifyNotification(msg)
	if evt == nil {
		t.Fatal("expected non-nil event")
	}
	if evt.Type != AcpEventImage {
		t.Fatalf("expected AcpEventImage, got %d", evt.Type)
	}
	if evt.ImageBase64 != "iVBORw0KGgo=" {
		t.Errorf("ImageBase64 = %q", evt.ImageBase64)
	}
	if evt.ImageMimeType != "image/png" {
		t.Errorf("ImageMimeType = %q", evt.ImageMimeType)
	}
}

func TestClassifyNotification_AgentMessageChunk_ImageNested(t *testing.T) {
	// Anthropic-style nested source — must still classify as image.
	msg := makeNotification(t, "agent_message_chunk",
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/AA=="}}`,
		"content")
	evt := ClassifyNotification(msg)
	if evt == nil || evt.Type != AcpEventImage {
		t.Fatalf("expected image event, got %+v", evt)
	}
	if evt.ImageBase64 != "/9j/AA==" {
		t.Errorf("ImageBase64 = %q", evt.ImageBase64)
	}
	if evt.ImageMimeType != "image/jpeg" {
		t.Errorf("ImageMimeType = %q", evt.ImageMimeType)
	}
}

func TestClassifyNotification_AgentMessageChunk_ImageEmptyDataIgnored(t *testing.T) {
	// Image block with no data is unusable — return nil so the read
	// loop doesn't try to send a zero-byte attachment.
	msg := makeNotification(t, "agent_message_chunk",
		`{"type":"image","data":"","mimeType":"image/png"}`, "content")
	if evt := ClassifyNotification(msg); evt != nil {
		t.Fatalf("expected nil event for empty image data, got %+v", evt)
	}
}

func TestClassifyNotification_AgentMessageChunk_UnknownType(t *testing.T) {
	// audio / resource_link etc are not surfaced today — skip silently
	// rather than misclassify as text.
	msg := makeNotification(t, "agent_message_chunk",
		`{"type":"audio","data":"AAAA","mimeType":"audio/wav"}`, "content")
	if evt := ClassifyNotification(msg); evt != nil {
		t.Fatalf("expected nil event for audio, got %+v", evt)
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

func TestClassifyNotification_CurrentModeUpdate_FlatID(t *testing.T) {
	// sessionUpdate == current_mode_update, with the new id flat at
	// the same level (form used by some agents).
	raw := json.RawMessage(`{"update":{"sessionUpdate":"current_mode_update","currentModeId":"code"}}`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt == nil || evt.Type != AcpEventModeUpdate {
		t.Fatalf("expected AcpEventModeUpdate, got %+v", evt)
	}
	if evt.ModeID != "code" {
		t.Errorf("ModeID = %q, want 'code'", evt.ModeID)
	}
}

func TestClassifyNotification_CurrentModeUpdate_NestedID(t *testing.T) {
	// Some agents wrap the id in a currentMode object. The classifier
	// accepts either.
	raw := json.RawMessage(`{"update":{"sessionUpdate":"current_mode_update","currentMode":{"id":"ask","name":"Ask"}}}`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt == nil || evt.Type != AcpEventModeUpdate {
		t.Fatalf("expected AcpEventModeUpdate, got %+v", evt)
	}
	if evt.ModeID != "ask" {
		t.Errorf("ModeID = %q, want 'ask'", evt.ModeID)
	}
}

func TestClassifyNotification_CurrentModeUpdate_MissingID(t *testing.T) {
	raw := json.RawMessage(`{"update":{"sessionUpdate":"current_mode_update"}}`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt != nil {
		t.Fatalf("expected nil when id is absent, got %+v", evt)
	}
}

func TestClassifyNotification_CurrentModelUpdate_FlatID(t *testing.T) {
	raw := json.RawMessage(`{"update":{"sessionUpdate":"current_model_update","currentModelId":"haiku"}}`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt == nil || evt.Type != AcpEventModelUpdate {
		t.Fatalf("expected AcpEventModelUpdate, got %+v", evt)
	}
	if evt.ModelID != "haiku" {
		t.Errorf("ModelID = %q, want 'haiku'", evt.ModelID)
	}
}

func TestClassifyNotification_CurrentModelUpdate_NestedID(t *testing.T) {
	raw := json.RawMessage(`{"update":{"sessionUpdate":"current_model_update","currentModel":{"id":"sonnet","name":"Sonnet"}}}`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt == nil || evt.Type != AcpEventModelUpdate {
		t.Fatalf("expected AcpEventModelUpdate, got %+v", evt)
	}
	if evt.ModelID != "sonnet" {
		t.Errorf("ModelID = %q, want 'sonnet'", evt.ModelID)
	}
}

func TestClassifyNotification_CurrentModelUpdate_MissingID(t *testing.T) {
	raw := json.RawMessage(`{"update":{"sessionUpdate":"current_model_update"}}`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt != nil {
		t.Fatalf("expected nil when model id is absent, got %+v", evt)
	}
}

func TestClassifyNotification_CurrentModelUpdate_FlatModelId(t *testing.T) {
	// Kiro-shape flat `modelId` (not `currentModelId`).
	raw := json.RawMessage(`{"update":{"sessionUpdate":"current_model_update","modelId":"claude-haiku-4.5"}}`)
	msg := &JsonRpcMessage{Params: &raw}
	evt := ClassifyNotification(msg)
	if evt == nil || evt.Type != AcpEventModelUpdate {
		t.Fatalf("expected AcpEventModelUpdate, got %+v", evt)
	}
	if evt.ModelID != "claude-haiku-4.5" {
		t.Errorf("ModelID = %q, want 'claude-haiku-4.5'", evt.ModelID)
	}
}

func TestModelInfo_UnmarshalAcceptsBothKeys(t *testing.T) {
	// Kiro (and the current ACP spec) uses `modelId`.
	var kiro ModelInfo
	if err := json.Unmarshal([]byte(`{"modelId":"claude-sonnet-4.6","name":"Sonnet 4.6","description":"d"}`), &kiro); err != nil {
		t.Fatalf("unmarshal Kiro shape: %v", err)
	}
	if kiro.ID != "claude-sonnet-4.6" || kiro.Name != "Sonnet 4.6" || kiro.Description != "d" {
		t.Errorf("Kiro shape parsed wrong: %+v", kiro)
	}

	// Older shape using `id` must still work.
	var legacy ModelInfo
	if err := json.Unmarshal([]byte(`{"id":"haiku","name":"Haiku"}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy shape: %v", err)
	}
	if legacy.ID != "haiku" || legacy.Name != "Haiku" {
		t.Errorf("legacy shape parsed wrong: %+v", legacy)
	}

	// When both are present, modelId wins — it's the canonical key.
	var both ModelInfo
	if err := json.Unmarshal([]byte(`{"modelId":"A","id":"B","name":"x"}`), &both); err != nil {
		t.Fatalf("unmarshal both shapes: %v", err)
	}
	if both.ID != "A" {
		t.Errorf("modelId should win over id: got %q", both.ID)
	}
}

func TestModelSet_UnmarshalKiroShape(t *testing.T) {
	// Verifies the availableModels array parses `modelId` end-to-end.
	data := []byte(`{
		"currentModelId": "claude-sonnet-4.6",
		"availableModels": [
			{"modelId": "auto", "name": "auto", "description": "pick best"},
			{"modelId": "claude-sonnet-4.6", "name": "Claude Sonnet 4.6", "description": "latest"}
		]
	}`)
	var ms ModelSet
	if err := json.Unmarshal(data, &ms); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ms.CurrentModelID != "claude-sonnet-4.6" {
		t.Errorf("CurrentModelID = %q", ms.CurrentModelID)
	}
	if len(ms.AvailableModels) != 2 {
		t.Fatalf("len = %d, want 2", len(ms.AvailableModels))
	}
	if ms.AvailableModels[0].ID != "auto" {
		t.Errorf("[0].ID = %q, want 'auto'", ms.AvailableModels[0].ID)
	}
	if ms.AvailableModels[1].ID != "claude-sonnet-4.6" {
		t.Errorf("[1].ID = %q, want 'claude-sonnet-4.6'", ms.AvailableModels[1].ID)
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
