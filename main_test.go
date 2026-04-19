package main

import "testing"

func TestExtractKiroAgentName(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"flag with value", []string{"acp", "--agent", "openab-go"}, "openab-go"},
		{"equals form", []string{"acp", "--agent=openab-go"}, "openab-go"},
		{"flag among others", []string{"acp", "--trust-all-tools", "--agent", "foo"}, "foo"},
		{"trailing --agent with no value falls back", []string{"acp", "--agent"}, "kiro_default"},
		{"no flag falls back to kiro_default", []string{"acp", "--trust-all-tools"}, "kiro_default"},
		{"empty args", []string{}, "kiro_default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractKiroAgentName(c.args); got != c.want {
				t.Errorf("extractKiroAgentName(%v) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}
