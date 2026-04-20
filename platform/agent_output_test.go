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

func TestIsCopilotReasoningEffortError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "", want: false},
		{name: "generic copilot error", in: "Error: Execution failed: CAPIError: 500 internal error", want: false},
		{name: "reasoning_effort mismatch", in: `Error: Execution failed: CAPIError: 400 reasoning_effort "medium" was provided, but model claude-haiku-4.5 does not support reasoning effort`, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCopilotReasoningEffortError(tc.in); got != tc.want {
				t.Fatalf("IsCopilotReasoningEffortError(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPickFallbackModel(t *testing.T) {
	tests := []struct {
		name      string
		available []string
		current   string
		wantID    string
		wantOK    bool
	}{
		{
			name:      "prefer non-haiku over haiku",
			available: []string{"claude-haiku-4.5", "gpt-5-mini", "gpt-4.1"},
			current:   "claude-haiku-4.5",
			wantID:    "gpt-5-mini",
			wantOK:    true,
		},
		{
			name:      "skip current, pick next non-haiku",
			available: []string{"gpt-5-mini", "claude-haiku-4.5", "gpt-4.1"},
			current:   "gpt-5-mini",
			wantID:    "gpt-4.1",
			wantOK:    true,
		},
		{
			name:      "fallback to haiku only if nothing else left",
			available: []string{"claude-haiku-4.5", "claude-haiku-3"},
			current:   "claude-haiku-4.5",
			wantID:    "claude-haiku-3",
			wantOK:    true,
		},
		{
			name:      "case-insensitive haiku filter",
			available: []string{"Claude-Haiku-Next", "gpt-4.1"},
			current:   "claude-haiku-4.5",
			wantID:    "gpt-4.1",
			wantOK:    true,
		},
		{
			name:      "no alternative",
			available: []string{"claude-haiku-4.5"},
			current:   "claude-haiku-4.5",
			wantID:    "",
			wantOK:    false,
		},
		{
			name:      "empty list",
			available: nil,
			current:   "anything",
			wantID:    "",
			wantOK:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := PickFallbackModel(tc.available, tc.current)
			if gotID != tc.wantID || gotOK != tc.wantOK {
				t.Fatalf("PickFallbackModel(%v, %q) = (%q, %v), want (%q, %v)",
					tc.available, tc.current, gotID, gotOK, tc.wantID, tc.wantOK)
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
