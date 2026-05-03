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
	AcpEventModeUpdate
	AcpEventModelUpdate
	AcpEventImage
)

type AcpEvent struct {
	Type   AcpEventType
	Text   string
	Title  string
	Status string
	// ModeID is the new current mode id carried by a
	// current_mode_update session notification.
	ModeID string
	// ModelID is the new current model id carried by a
	// current_model_update session notification.
	ModelID string
	// ImageBase64 / ImageMimeType carry an inline image emitted by the
	// agent inside an agent_message_chunk with content.type == "image".
	// Only populated when Type == AcpEventImage; bytes are kept
	// base64-encoded so platform code can decide whether to decode in
	// memory or stream straight to disk.
	ImageBase64   string
	ImageMimeType string
}

// ModeInfo describes one entry of the `availableModes` array in an ACP
// session setup response. `Description` is optional per spec.
type ModeInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ModeSet mirrors the `modes` object returned by session/new and
// session/load: which mode is active now, and what else is available.
type ModeSet struct {
	CurrentModeID  string     `json:"currentModeId"`
	AvailableModes []ModeInfo `json:"availableModes"`
}

// ModelInfo describes one entry of the `availableModels` array in an
// ACP session setup response. Shape parallels ModeInfo — but note the
// asymmetric field naming: per ACP spec (and observed with Kiro), the
// canonical key is `modelId`, not `id`. The custom UnmarshalJSON
// accepts either so we stay robust across agents that follow older
// drafts or that reuse the mode shape.
type ModelInfo struct {
	ID          string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (m *ModelInfo) UnmarshalJSON(data []byte) error {
	var raw struct {
		ModelID     string `json:"modelId,omitempty"`
		ID          string `json:"id,omitempty"`
		Name        string `json:"name,omitempty"`
		Description string `json:"description,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.ModelID != "" {
		m.ID = raw.ModelID
	} else {
		m.ID = raw.ID
	}
	m.Name = raw.Name
	m.Description = raw.Description
	return nil
}

// ModelSet mirrors the `models` object returned by session/new and
// session/load: which model is active now, and what else is available.
type ModelSet struct {
	CurrentModelID  string      `json:"currentModelId"`
	AvailableModels []ModelInfo `json:"availableModels"`
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
		// Dispatch on the content block's `type` field. ACP defines text /
		// image / audio / resource_link / resource; we surface text and
		// image today and silently ignore the others rather than treat
		// them as text.
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(contentRaw, &head); err != nil {
			return nil
		}
		switch head.Type {
		case "", "text":
			var content struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(contentRaw, &content); err != nil {
				return nil
			}
			return &AcpEvent{Type: AcpEventText, Text: content.Text}
		case "image":
			data, mime := parseImageContent(contentRaw)
			if data == "" {
				return nil
			}
			return &AcpEvent{Type: AcpEventImage, ImageBase64: data, ImageMimeType: mime}
		default:
			return nil
		}

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

	case "current_mode_update":
		// Agent tells the client the session's active mode changed.
		// The new id is wrapped in a nested object named either
		// `currentMode` (per ACP spec) or flattened — accept both to
		// stay robust across agents.
		if raw, ok := update["currentModeId"]; ok {
			var id string
			if err := json.Unmarshal(raw, &id); err == nil && id != "" {
				return &AcpEvent{Type: AcpEventModeUpdate, ModeID: id}
			}
		}
		if raw, ok := update["currentMode"]; ok {
			var inner struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &inner); err == nil && inner.ID != "" {
				return &AcpEvent{Type: AcpEventModeUpdate, ModeID: inner.ID}
			}
		}
		return nil

	case "current_model_update":
		// Mirror of current_mode_update for the model axis, but the
		// model side has more shape variance in the wild: flat
		// `currentModelId`, flat `modelId` (some Kiro builds), or
		// nested `currentModel` object. Accept any form so the read
		// loop stays in sync with the agent regardless.
		if raw, ok := update["currentModelId"]; ok {
			var id string
			if err := json.Unmarshal(raw, &id); err == nil && id != "" {
				return &AcpEvent{Type: AcpEventModelUpdate, ModelID: id}
			}
		}
		if raw, ok := update["modelId"]; ok {
			var id string
			if err := json.Unmarshal(raw, &id); err == nil && id != "" {
				return &AcpEvent{Type: AcpEventModelUpdate, ModelID: id}
			}
		}
		if raw, ok := update["currentModel"]; ok {
			var inner struct {
				ModelID string `json:"modelId"`
				ID      string `json:"id"`
			}
			if err := json.Unmarshal(raw, &inner); err == nil {
				if inner.ModelID != "" {
					return &AcpEvent{Type: AcpEventModelUpdate, ModelID: inner.ModelID}
				}
				if inner.ID != "" {
					return &AcpEvent{Type: AcpEventModelUpdate, ModelID: inner.ID}
				}
			}
		}
		return nil

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

// parseImageContent extracts the base64 payload + mime type from an
// `agent_message_chunk` image content block. Accepts both shapes seen
// in the wild:
//
//   - Flat (per ACP spec):  {"type":"image","data":"<base64>","mimeType":"image/png"}
//   - Nested (Anthropic-style): {"type":"image","source":{"type":"base64","media_type":"image/png","data":"<base64>"}}
//
// Returns ("", "") when neither shape carries a usable payload.
func parseImageContent(raw json.RawMessage) (data, mime string) {
	var flat struct {
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil && flat.Data != "" {
		return flat.Data, flat.MimeType
	}
	var nested struct {
		Source struct {
			Data      string `json:"data"`
			MediaType string `json:"media_type"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil && nested.Source.Data != "" {
		return nested.Source.Data, nested.Source.MediaType
	}
	return "", ""
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
