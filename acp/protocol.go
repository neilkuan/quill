package acp

import (
	"encoding/json"
)

// --- Content blocks for prompts ---

// ContentBlock represents a single content element in an ACP prompt.
// Uses a flexible map representation to support different JSON shapes for text and image blocks.
type ContentBlock map[string]interface{}

func TextBlock(text string) ContentBlock {
	return ContentBlock{
		"type": "text",
		"text": text,
	}
}

// ImageBlock creates an ACP image content block with nested source structure.
// Schema: {"type":"image","source":{"type":"base64","media_type":"...","data":"..."}}
func ImageBlock(base64Data, mimeType string) ContentBlock {
	return ContentBlock{
		"type": "image",
		"source": map[string]string{
			"type":       "base64",
			"media_type": mimeType,
			"data":       base64Data,
		},
	}
}

// --- Outgoing ---

type JsonRpcRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      uint64      `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

func NewJsonRpcRequest(id uint64, method string, params interface{}) *JsonRpcRequest {
	return &JsonRpcRequest{
		Jsonrpc: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
}

// JsonRpcNotification is a JSON-RPC 2.0 notification (no id, no response expected).
type JsonRpcNotification struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

func NewJsonRpcNotification(method string, params interface{}) *JsonRpcNotification {
	return &JsonRpcNotification{
		Jsonrpc: "2.0",
		Method:  method,
		Params:  params,
	}
}

type JsonRpcResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      uint64      `json:"id"`
	Result  interface{} `json:"result"`
}

func NewJsonRpcResponse(id uint64, result interface{}) *JsonRpcResponse {
	return &JsonRpcResponse{
		Jsonrpc: "2.0",
		ID:      id,
		Result:  result,
	}
}

// --- Incoming ---

type JsonRpcMessage struct {
	ID     *uint64          `json:"id,omitempty"`
	Method *string          `json:"method,omitempty"`
	Result *json.RawMessage `json:"result,omitempty"`
	Error  *JsonRpcError    `json:"error,omitempty"`
	Params *json.RawMessage `json:"params,omitempty"`
}

type JsonRpcError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

func (e *JsonRpcError) Error() string {
	return e.Message
}

// --- ACP notification classification ---

type AcpEventType int

const (
	AcpEventText AcpEventType = iota
	AcpEventThinking
	AcpEventToolStart
	AcpEventToolDone
	AcpEventStatus
)

type AcpEvent struct {
	Type   AcpEventType
	Text   string
	Title  string
	Status string
}

func ClassifyNotification(msg *JsonRpcMessage) *AcpEvent {
	if msg.Params == nil {
		return nil
	}

	var params struct {
		Update json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(*msg.Params, &params); err != nil {
		return nil
	}

	var update map[string]json.RawMessage
	if err := json.Unmarshal(params.Update, &update); err != nil {
		return nil
	}

	sessionUpdateRaw, ok := update["sessionUpdate"]
	if !ok {
		return nil
	}

	var sessionUpdate string
	if err := json.Unmarshal(sessionUpdateRaw, &sessionUpdate); err != nil {
		return nil
	}

	switch sessionUpdate {
	case "agent_message_chunk":
		contentRaw, ok := update["content"]
		if !ok {
			return nil
		}
		var content struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(contentRaw, &content); err != nil {
			return nil
		}
		return &AcpEvent{Type: AcpEventText, Text: content.Text}

	case "agent_thought_chunk":
		return &AcpEvent{Type: AcpEventThinking}

	case "tool_call":
		title := extractStringField(update, "title")
		return &AcpEvent{Type: AcpEventToolStart, Title: title}

	case "tool_call_update":
		title := extractStringField(update, "title")
		status := extractStringField(update, "status")
		if status == "completed" || status == "failed" {
			return &AcpEvent{Type: AcpEventToolDone, Title: title, Status: status}
		}
		return &AcpEvent{Type: AcpEventToolStart, Title: title}

	case "plan":
		return &AcpEvent{Type: AcpEventStatus}

	default:
		return nil
	}
}

// StopReason returns the stopReason field from a session/prompt response.
// Returns "" when the message is not a response, has no result, or has
// no stopReason field. Common values: "end_turn", "cancelled",
// "max_tokens", "refusal".
func StopReason(msg *JsonRpcMessage) string {
	if msg == nil || msg.ID == nil || msg.Result == nil {
		return ""
	}
	var r struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(*msg.Result, &r); err != nil {
		return ""
	}
	return r.StopReason
}

func extractStringField(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
