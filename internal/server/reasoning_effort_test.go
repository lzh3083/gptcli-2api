package server

import "testing"

func TestExtractReasoningEffort(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want string
	}{
		// Client-facing labels (usage detail / admin UI)
		{map[string]any{"reasoning_effort": "high"}, "high"},
		{map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 8000}}, "medium"},
		{map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 50000}}, "xhigh"},
		{map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 200000}}, "max"},
		{map[string]any{"reasoning": map[string]any{"effort": "low"}}, "low"},
		{map[string]any{"effort": "MAX"}, "max"},
		// Claude Code / Anthropic output_config.effort
		{map[string]any{"output_config": map[string]any{"effort": "low"}}, "low"},
		{map[string]any{"output_config": map[string]any{"effort": "medium"}}, "medium"},
		{map[string]any{"output_config": map[string]any{"effort": "high"}}, "high"},
		{map[string]any{"output_config": map[string]any{"effort": "xhigh"}}, "xhigh"},
		{map[string]any{"output_config": map[string]any{"effort": "max"}}, "max"},
		{map[string]any{"output_config": map[string]any{"effort": "ultracode"}}, "ultracode"},
		// Codex thinking modes → client labels
		// Low / Base / High / Ultra / Proactive (+ legacy aliases)
		{map[string]any{"reasoning_effort": "auto"}, "low"},
		{map[string]any{"reasoning_effort": "low"}, "low"},
		{map[string]any{"reasoning_effort": "base"}, "medium"},
		{map[string]any{"reasoning_effort": "default"}, "medium"},
		{map[string]any{"reasoning_effort": "high"}, "high"},
		{map[string]any{"reasoning_effort": "proactive"}, "high"},
		{map[string]any{"reasoning_effort": "standard"}, "high"},
		{map[string]any{"reasoning_effort": "ultra"}, "ultracode"},
		{map[string]any{"reasoning_effort": "extra-high"}, "xhigh"},
		{map[string]any{"thinking": "xhigh"}, "xhigh"},
		{map[string]any{"reasoning": map[string]any{"effort": "extra_high"}}, "xhigh"},
		{map[string]any{"reasoning": map[string]any{"effort": "proactive"}}, "high"},
		{map[string]any{"effort": "ultracode"}, "ultracode"},
		{map[string]any{}, ""},
	}
	for i, tc := range cases {
		got := extractReasoningEffort(tc.in)
		if got != tc.want {
			t.Fatalf("case %d: got %q want %q (in=%v)", i, got, tc.want, tc.in)
		}
	}
}
