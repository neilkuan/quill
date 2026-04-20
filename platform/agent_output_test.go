package platform

import "testing"

func TestStripAgentRetryNoise(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no retry noise passes through",
			in:   "Hello world",
			want: "Hello world",
		},
		{
			name: "single retry notice removed",
			in:   "Info: Request failed (transient_bad_request). Retrying...",
			want: "",
		},
		{
			name: "double retry notice collapses (copilot chains them without separators)",
			in:   "Info: Request failed (transient_bad_request). Retrying...Info: Request failed (transient_bad_request). Retrying...",
			want: "",
		},
		{
			name: "retry notices stripped, error body preserved verbatim",
			in:   `Info: Request failed (transient_bad_request). Retrying...Info: Request failed (transient_bad_request). Retrying...Error: Execution failed: CAPIError: 400 reasoning_effort "medium" was provided, but model claude-haiku-4.5 does not support reasoning effort (Request ID: C888:1B4D03:4A5199:507A46:69E6242C)`,
			want: `Error: Execution failed: CAPIError: 400 reasoning_effort "medium" was provided, but model claude-haiku-4.5 does not support reasoning effort (Request ID: C888:1B4D03:4A5199:507A46:69E6242C)`,
		},
		{
			name: "retry notice preceded by real content keeps the content",
			in:   "Here is the answer.\nInfo: Request failed (transient_bad_request). Retrying...",
			want: "Here is the answer.\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StripAgentRetryNoise(tc.in)
			if got != tc.want {
				t.Fatalf("StripAgentRetryNoise()\n  got:  %q\n  want: %q", got, tc.want)
			}
		})
	}
}

func TestDetectAgentError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "", want: false},
		{name: "normal reply", in: "Here is the answer.", want: false},
		{name: "copilot execution error", in: `Error: Execution failed: CAPIError: 400 reasoning_effort "medium" was provided`, want: true},
		{name: "error mid-stream still detected", in: "A partial reply.\nError: Execution failed: oops", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectAgentError(tc.in); got != tc.want {
				t.Fatalf("DetectAgentError(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
