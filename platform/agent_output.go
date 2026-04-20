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
