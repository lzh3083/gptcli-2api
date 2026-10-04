package server

import (
	"testing"

	"github.com/hm2899/grokcli-2api/internal/protocol/reasoning"
)

func TestClampCodexReasoningHonorsExplicitEffort(t *testing.T) {
	ua := "codex-cli/0.1"
	tools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "exec_command",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
			},
		},
	}}

	cases := []struct {
		name       string
		raw        map[string]any
		wantUp     string
		wantClient string
	}{
		{
			name: "ultra",
			raw: map[string]any{
				"tools":     tools,
				"reasoning": map[string]any{"effort": "ultra"},
			},
			wantUp:     reasoning.High,
			wantClient: reasoning.ClientUltracode,
		},
		{
			name: "proactive",
			raw: map[string]any{
				"tools":            tools,
				"reasoning_effort": "proactive",
			},
			wantUp:     reasoning.High,
			wantClient: reasoning.ClientHigh,
		},
		{
			name: "base",
			raw: map[string]any{
				"tools":            tools,
				"reasoning_effort": "base",
			},
			wantUp:     reasoning.Medium,
			wantClient: reasoning.ClientMedium,
		},
		{
			name: "high",
			raw: map[string]any{
				"tools":     tools,
				"reasoning": map[string]any{"effort": "high"},
			},
			wantUp:     reasoning.High,
			wantClient: reasoning.ClientHigh,
		},
		{
			name: "omitted defaults low",
			raw: map[string]any{
				"tools": tools,
			},
			wantUp:     reasoning.Low,
			wantClient: reasoning.ClientLow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"tools": tools}
			// Simulate BuildChatBody having copied reasoning_effort when present.
			if v := reasoning.FromRequestUpstream(tc.raw); v != "" {
				body["reasoning_effort"] = v
			}
			raw := cloneAnyMap(tc.raw)
			clampCodexReasoning(raw, body, ua, true)
			if body["reasoning_effort"] != tc.wantUp {
				t.Fatalf("body upstream effort=%v want %q", body["reasoning_effort"], tc.wantUp)
			}
			rm, _ := body["reasoning"].(map[string]any)
			if rm == nil || rm["effort"] != tc.wantUp {
				t.Fatalf("body.reasoning=%v want effort %q", body["reasoning"], tc.wantUp)
			}
			// Usage extract should still see client tier (not forced low).
			gotClient := extractReasoningEffort(raw)
			if gotClient == "" {
				gotClient = extractReasoningEffort(body)
			}
			// For omitted case raw may now hold client low.
			if gotClient != tc.wantClient && reasoning.ToUpstream(gotClient) != tc.wantUp {
				t.Fatalf("client effort=%q want %q (or upstream %q)", gotClient, tc.wantClient, tc.wantUp)
			}
			if tc.name != "omitted defaults low" && gotClient == reasoning.ClientLow && tc.wantClient != reasoning.ClientLow {
				t.Fatalf("explicit %s was rewritten to low", tc.name)
			}
		})
	}
}

func TestClampCodexReasoningDisabledNoop(t *testing.T) {
	raw := map[string]any{"reasoning": map[string]any{"effort": "ultra"}}
	body := map[string]any{"reasoning_effort": "high"}
	clampCodexReasoning(raw, body, "codex-cli/1", false)
	if body["reasoning_effort"] != "high" {
		t.Fatalf("disabled clamp must not rewrite, got %v", body["reasoning_effort"])
	}
	if extractReasoningEffort(raw) != reasoning.ClientUltracode {
		t.Fatalf("raw ultra corrupted: %q", extractReasoningEffort(raw))
	}
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if m, ok := v.(map[string]any); ok {
			out[k] = cloneAnyMap(m)
			continue
		}
		out[k] = v
	}
	return out
}
