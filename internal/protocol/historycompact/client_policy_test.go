package historycompact

import (
	"testing"
	"time"
)

func TestIsOpenAINativeClient(t *testing.T) {
	if IsOpenAINativeClient("") {
		t.Fatal("empty UA must be conservative")
	}
	if IsOpenAINativeClient("claude-cli/1.0") {
		t.Fatal("claude-cli is not native")
	}
	if IsOpenAINativeClient("anthropic-sdk") {
		t.Fatal("anthropic is not native")
	}
	if !IsOpenAINativeClient("codex-cli/0.1") {
		t.Fatal("codex should be native")
	}
	if !IsOpenAINativeClient("openai-python/1.0") {
		t.Fatal("openai-python should be native")
	}
}

func TestResolveOutboundMaxTools(t *testing.T) {
	if got := ResolveOutboundMaxTools("openai", "anything", 1, 0, 0); got != 0 {
		t.Fatalf("chat max=%d", got)
	}
	if got := ResolveOutboundMaxTools("openai_responses", "codex", 1, 0, 0); got != 0 {
		t.Fatalf("native responses max=%d", got)
	}
	if got := ResolveOutboundMaxTools("openai_responses", "claude-cli", 1, 0, 0); got != 1 {
		t.Fatalf("claude responses max=%d", got)
	}
	if got := ResolveOutboundMaxTools("anthropic", "", 1, 0, 0); got != 1 {
		t.Fatalf("anthropic max=%d", got)
	}
}

func TestResolveOutboundToolGap(t *testing.T) {
	claude := 80 * time.Millisecond
	native := time.Duration(0)
	if got := ResolveOutboundToolGap("openai", "x", claude, native); got != native {
		t.Fatalf("chat gap=%v", got)
	}
	if got := ResolveOutboundToolGap("openai_responses", "codex", claude, native); got != native {
		t.Fatalf("native gap=%v", got)
	}
	if got := ResolveOutboundToolGap("anthropic", "claude-cli", claude, native); got != claude {
		t.Fatalf("claude gap=%v", got)
	}
}

func TestIsCodexClient(t *testing.T) {
	if !IsCodexClient("codex-tui/0.144.1") {
		t.Fatal("codex-tui")
	}
	if IsCodexClient("OpenAI/Python 2.24.0") {
		t.Fatal("generic openai python should not be codex")
	}
	if IsCodexClient("Go-http-client/1.1") {
		t.Fatal("go client")
	}
}

func TestLooksLikeCodexRequestByTools(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"name": "exec_command",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
				"required":   []any{"cmd"},
			},
		},
	}
	if !LooksLikeCodexRequest("", tools, nil) {
		t.Fatal("exec_command+cmd should look like Codex without UA")
	}
	if LooksLikeCodexRequest("", []any{map[string]any{"name": "Read", "parameters": map[string]any{"properties": map[string]any{"file_path": map[string]any{"type": "string"}}}}}, nil) {
		t.Fatal("Read tool must not look like Codex")
	}
	if !LooksLikeCodexRequest("codex-cli/1", nil, nil) {
		t.Fatal("UA still works")
	}
}

func TestEffectiveAutoCharsToolsSchema(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Configure(boolPtr(false), intPtr(0))
	tools := []any{map[string]any{
		"name":       "exec_command",
		"parameters": map[string]any{"properties": map[string]any{"cmd": map[string]any{"type": "string"}}},
	}}
	if got := EffectiveAutoCharsFor("", tools, nil); got != CodexDefaultAutoChars {
		t.Fatalf("got %d", got)
	}
	if got := EffectiveAutoCharsFor("curl/8", nil, nil); got != 0 {
		t.Fatalf("curl should be 0, got %d", got)
	}
}
