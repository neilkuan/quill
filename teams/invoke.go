package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// ErrNotInvoke means the activity does not carry a recognisable quill
// invoke payload — the caller should fall through to the regular
// message-handling path.
var ErrNotInvoke = errors.New("teams: activity is not a quill invoke")

// InvokeData is the decoded Action.Submit payload our cards send back.
// All optional fields use omitempty so a single struct works for both
// switch_mode and switch_model.
type InvokeData struct {
	Action string `json:"quill.action"`
	Thread string `json:"thread"`
	Mode   string `json:"mode,omitempty"`
	Model  string `json:"model,omitempty"`
}

// UnmarshalInvokeData decodes activity.Value into an InvokeData. Returns
// ErrNotInvoke (wrapped or sentinel) when the payload is missing or
// lacks a quill.action key, so the dispatcher knows to fall through.
func UnmarshalInvokeData(activity *Activity) (InvokeData, error) {
	if len(activity.Value) == 0 {
		return InvokeData{}, ErrNotInvoke
	}
	// Reject scalar values (e.g. plain string) up front — json.Unmarshal
	// into a struct from a non-object would silently produce a zero value.
	if activity.Value[0] != '{' {
		return InvokeData{}, errors.New("teams: invoke value is not a JSON object")
	}
	var d InvokeData
	if err := json.Unmarshal(activity.Value, &d); err != nil {
		return InvokeData{}, err
	}
	if d.Action == "" {
		return InvokeData{}, ErrNotInvoke
	}
	return d, nil
}

// OnInvokeAction handles activities whose Value field carries a
// quill.action key — i.e., the messageBack from one of our Adaptive
// Cards. It validates the payload, dispatches to the right command, and
// rewrites the original card via UpdateActivity to show the result.
//
// Happy-path dispatch (calling command.ExecuteMode / ExecuteModel) is
// added in the next task. This task wires the guard paths.
func (h *Handler) OnInvokeAction(activity *Activity) {
	data, err := UnmarshalInvokeData(activity)
	if err != nil {
		slog.Debug("teams: not an invoke activity", "error", err)
		return
	}

	// Thread guard — the card's data["thread"] is set when the card is
	// built. If a stale card from another conversation gets clicked, we
	// refuse rather than mutate the wrong session.
	expectedThread := buildSessionKey(activity.Conversation.ID)
	if data.Thread != expectedThread {
		h.updateCard(activity, BuildModeConfirmation("", "", "This picker belongs to a different conversation."))
		return
	}

	switch data.Action {
	case actionSwitchMode:
		if data.Mode == "" {
			h.updateCard(activity, BuildModeConfirmation("", "", "Selection missing — please re-open the picker with /mode."))
			return
		}
		// Real switch is wired in Task 9; for now, no-op.
		// (Tests for that path live in Task 9.)
	case actionSwitchModel:
		if data.Model == "" {
			h.updateCard(activity, BuildModelConfirmation("", "", "Selection missing — please re-open the picker with /model."))
			return
		}
		// Wired in Task 9.
	default:
		slog.Debug("teams: unknown invoke action — ignoring", "action", data.Action)
		return
	}
}

// updateCard wraps the BotClient.UpdateActivity call. On failure, falls
// back to a fresh SendActivity with a plain-text warning so the user
// still sees the result.
func (h *Handler) updateCard(activity *Activity, card Attachment) {
	resp := &Activity{
		Type:        "message",
		Attachments: []Attachment{card},
	}
	err := h.Client.UpdateActivity(activity.ServiceURL, activity.Conversation.ID, activity.ReplyToID, resp)
	if err == nil {
		return
	}
	slog.Warn("teams: UpdateActivity failed, falling back to new SendActivity", "error", err)
	_, _ = h.Client.SendActivity(activity.ServiceURL, activity.Conversation.ID, &Activity{
		Type:       "message",
		Text:       fmt.Sprintf("⚠️ Card update failed: %v", err),
		TextFormat: "markdown",
	})
}
