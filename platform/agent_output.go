package platform

import (
	"regexp"
	"strings"
)

// copilotRetryNoiseRE matches Copilot CLI's internal retry notices, which it
// streams as agent_message_chunk text instead of keeping them inside its
// own diagnostics channel. A single prompt often produces several of these
// back-to-back with no separator, so the regex deliberately swallows any
// trailing whitespace.
//
// Example input:
//
//	Info: Request failed (transient_bad_request). Retrying...
var copilotRetryNoiseRE = regexp.MustCompile(`Info: Request failed \([^)]*\)\.\s*Retrying\.\.\.\s*`)

// copilotExecutionErrorPrefix is Copilot CLI's self-reported execution
// failure marker (e.g. an upstream CAPIError surfaced as chat text). The
// stopReason on the RPC response is still "end_turn", so without this
// check Quill would silently publish the error string as if it were the
// agent's reply.
const copilotExecutionErrorPrefix = "Error: Execution failed:"

// StripAgentRetryNoise removes Copilot CLI's transient retry notices from
// an accumulated agent reply. The output keeps any real content (including
// genuine error messages) untouched.
func StripAgentRetryNoise(text string) string {
	if !strings.Contains(text, "Retrying...") {
		return text
	}
	return copilotRetryNoiseRE.ReplaceAllString(text, "")
}

// DetectAgentError reports whether `text` (already stripped of retry noise)
// looks like an agent-reported execution error that should be surfaced as
// a failure rather than a normal reply. Currently recognises Copilot CLI's
// "Error: Execution failed:" pattern.
func DetectAgentError(text string) bool {
	return strings.Contains(text, copilotExecutionErrorPrefix)
}

// copilotReasoningEffortMismatch is the tail of Copilot CLI's CAPIError
// when the active model can't accept the configured `reasoning_effort`
// value. Observed in practice when the session resumes with model
// `claude-haiku-4.5` while Copilot's global reasoning_effort stays at
// `"medium"`.
const copilotReasoningEffortMismatch = "does not support reasoning effort"

// IsCopilotReasoningEffortError reports whether `text` matches Copilot's
// "model … does not support reasoning effort" CAPIError. When true,
// switching the session's model to one that accepts reasoning effort
// (e.g. GPT-5 mini / GPT-4.1) is a reliable recovery path — the session
// state otherwise does not expose a way to downgrade reasoning_effort.
func IsCopilotReasoningEffortError(text string) bool {
	return strings.Contains(text, copilotReasoningEffortMismatch)
}

// PickFallbackModel chooses a non-current model id from `available` to
// use when the session's current model has been rejected by the agent.
// Preference order:
//  1. a non-current id that does not look like a Claude Haiku variant
//     (Haiku is the usual culprit of the Copilot reasoning_effort clash),
//  2. any non-current id,
//  3. ("", false) when the list has no alternative.
//
// The comparison is case-insensitive on the id to catch "claude-haiku-4.5"
// / "Claude-Haiku" / future suffix variations without a hard-coded list.
func PickFallbackModel(available []string, current string) (string, bool) {
	for _, id := range available {
		if id == "" || id == current {
			continue
		}
		if strings.Contains(strings.ToLower(id), "haiku") {
			continue
		}
		return id, true
	}
	for _, id := range available {
		if id == "" || id == current {
			continue
		}
		return id, true
	}
	return "", false
}
