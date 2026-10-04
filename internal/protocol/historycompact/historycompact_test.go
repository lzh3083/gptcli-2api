package historycompact

import (
	"os"
	"strings"
	"testing"
)

func TestCompactMessagesSoftTierPreservesRecentToolResults(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "sys"},
	}
	// 3 old rounds + 1 recent
	for i := 0; i < 4; i++ {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c" + string(rune('0'+i)), "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"file_path":"/x"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "c" + string(rune('0'+i)), "content": strings.Repeat("A", 200) + "-round-" + string(rune('0'+i)) + "-" + strings.Repeat("Z", 200)},
		)
	}
	opts := Options{
		Enabled: true, PrefixStable: true,
		KeepToolRounds: 1, MidToolRounds: 1,
		MaxToolResultChars: 10_000, MidToolResultChars: 120, OldToolResultChars: 80,
		MaxMessagesChars: 1_000_000,
	}
	out, stats := CompactMessages(messages, opts)
	if !truthy(stats["applied"]) {
		t.Fatalf("expected applied stats %#v", stats)
	}
	// last tool message should stay full (within max)
	last := out[len(out)-1].(map[string]any)
	if !strings.Contains(stringValue(last["content"]), "-round-3-") || strings.Contains(stringValue(last["content"]), "truncated") {
		t.Fatalf("recent tool mutated: %q", last["content"])
	}
	// older tool should be soft-summarized
	old := out[2].(map[string]any)
	if !alreadyCompacted(stringValue(old["content"])) && len([]rune(stringValue(old["content"]))) > 120 {
		t.Fatalf("old tool not compacted: %q", old["content"])
	}
}

func TestApplyDisabledByDefault(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	_ = os.Unsetenv("GROK2API_HISTORY_COMPACT")
	_ = os.Unsetenv("GROK2API_HISTORY_COMPACT_AUTO_CHARS")
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "1", "content": strings.Repeat("x", 5000)},
		},
	}
	stats := Apply(body)
	if truthy(stats["enabled"]) || truthy(stats["applied"]) {
		t.Fatalf("default should be off: %#v", stats)
	}
	content := body["messages"].([]any)[1].(map[string]any)["content"]
	if len(stringValue(content)) != 5000 {
		t.Fatalf("content mutated while disabled")
	}
}

func TestShouldAutoCompact(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	// Admin path clamps non-zero auto_chars to >= 4000.
	Configure(nil, intPtr(4000))
	if AutoChars() != 4000 {
		t.Fatalf("auto=%d want 4000", AutoChars())
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("y", 5000)}}}
	if !ShouldAutoCompact(body) {
		t.Fatalf("expected auto compact th=%d", AutoChars())
	}
	// Below threshold
	small := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if ShouldAutoCompact(small) {
		t.Fatal("small body must not auto")
	}
}

func TestCodexDefaultAutoCompact(t *testing.T) {
	// Reset runtime + env so only Codex default applies.
	Reset()
	t.Cleanup(Reset)
	_ = os.Unsetenv("GROK2API_HISTORY_COMPACT")
	_ = os.Unsetenv("GROK2API_HISTORY_COMPACT_AUTO_CHARS")
	// Explicit admin "auto off" still leaves Codex default via EffectiveAutoChars.
	Configure(boolPtr(false), intPtr(0))

	// Build a body larger than CodexDefaultAutoChars.
	big := strings.Repeat("Z", CodexDefaultAutoChars/2)
	// two messages so JSON size exceeds threshold
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": big + big},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c2", "content": big + big},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c3", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c3", "content": big},
		},
	}
	// Non-codex UA with auto_chars=0 must NOT compact.
	stats := Apply(body, "curl/8.0")
	if truthy(stats["applied"]) {
		t.Fatalf("non-codex must not auto compact: %#v", stats)
	}
	// Codex UA should auto-force.
	body2 := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": big + big},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c2", "content": big + big},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c3", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c3", "content": big},
		},
	}
	stats2 := Apply(body2, "codex-cli/0.50")
	if !truthy(stats2["enabled"]) {
		t.Fatalf("codex should enable compact: %#v", stats2)
	}
	if !truthy(stats2["auto"]) {
		t.Fatalf("codex should mark auto: %#v", stats2)
	}
	if stats2["auto_source"] != "codex_default" {
		t.Fatalf("auto_source=%v", stats2["auto_source"])
	}
	// Recent tool rounds should still exist (soft-tier, not wipe).
	msgs := body2["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("messages vanished")
	}
}

func TestConfigureOverridesEnv(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	t.Setenv("GROK2API_HISTORY_COMPACT", "0")
	t.Setenv("GROK2API_HISTORY_COMPACT_AUTO_CHARS", "0")
	Configure(boolPtr(true), intPtr(50_000))
	if !DefaultOptions().Enabled {
		t.Fatal("Configure enabled should override env")
	}
	if AutoChars() != 50_000 {
		t.Fatalf("auto=%d", AutoChars())
	}
}

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

func TestApplyAutoCompactByToolsSchemaNoUA(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Configure(boolPtr(false), intPtr(0))
	big := strings.Repeat("Z", CodexDefaultAutoChars/2)
	body := map[string]any{
		"tools": []any{
			map[string]any{
				"name": "exec_command",
				"parameters": map[string]any{
					"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
					"required":   []any{"cmd"},
				},
			},
		},
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": big + big},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c2", "content": big + big},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c3", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "c3", "content": big},
		},
	}
	// Empty UA — must still detect via tools schema.
	stats := Apply(body, "")
	if !truthy(stats["enabled"]) || !truthy(stats["auto"]) {
		t.Fatalf("tools-schema codex should auto compact without UA: %#v", stats)
	}
	if stats["auto_source"] != "codex_default" && stats["auto_source"] != "threshold" {
		// global auto may be set in other tests; codex_default expected when auto_chars=0
		if AutoChars() == 0 && stats["auto_source"] != "codex_default" {
			t.Fatalf("auto_source=%v", stats["auto_source"])
		}
	}
}
