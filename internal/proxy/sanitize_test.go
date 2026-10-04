package proxy

import (
	"fmt"
	"strings"
	"testing"
)

func TestSanitizeUpstreamBodyNormalizesToolsAndToolChoice(t *testing.T) {
	body := map[string]any{
		"model":            "grok",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"presence_penalty": 0,
		"temperature":      3,
		"top_p":            -1,
		"max_tokens":       0,
		"tool_choice":      map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
		"tools": []any{
			map[string]any{"type": "web_search_preview"},
			map[string]any{"name": "Zed", "description": "last", "input_schema": map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string"}}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "Bash", "parameters": `{"type":"object"}`}},
		},
	}
	got := SanitizeUpstreamBody(body)
	if _, ok := got["presence_penalty"]; ok {
		t.Fatalf("unsupported field leaked: %#v", got)
	}
	if got["temperature"] != float64(2) || got["top_p"] != float64(0) {
		t.Fatalf("unexpected clamps %#v", got)
	}
	if _, ok := got["max_tokens"]; ok {
		t.Fatalf("invalid max_tokens leaked: %#v", got)
	}
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v", got["tool_choice"])
	}
	tools := got["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	first := tools[0].(map[string]any)["function"].(map[string]any)
	second := tools[1].(map[string]any)["function"].(map[string]any)
	if first["name"] != "Bash" || second["name"] != "Zed" {
		t.Fatalf("tools not sorted/normalized: %#v", tools)
	}
	if _, ok := first["input_schema"]; ok {
		t.Fatalf("input_schema leaked in function: %#v", first)
	}
	params := second["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("missing object type: %#v", params)
	}
}

func TestSanitizeUpstreamBodyDropsToolChoiceWithoutTools(t *testing.T) {
	got := SanitizeUpstreamBody(map[string]any{
		"messages":            []any{map[string]any{"role": "user", "content": "hi"}},
		"tool_choice":         "required",
		"parallel_tool_calls": true,
		"function_call":       map[string]any{"name": "legacy"},
		"tools":               []any{map[string]any{"type": "web_search"}},
	})
	if got["tools"] != nil || got["tool_choice"] != nil || got["parallel_tool_calls"] != nil || got["function_call"] != nil {
		t.Fatalf("tool-only fields leaked without tools: %#v", got)
	}
}

func TestSanitizeUpstreamBodyNormalizesLegacyFunctions(t *testing.T) {
	got := SanitizeUpstreamBody(map[string]any{
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"functions":   []any{map[string]any{"name": "legacy", "input_schema": map[string]any{}}},
		"tool_choice": map[string]any{"type": "any"},
	})
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v", got["tool_choice"])
	}
	functions := got["functions"].([]any)
	fn := functions[0].(map[string]any)
	if fn["input_schema"] != nil {
		t.Fatalf("input_schema leaked: %#v", fn)
	}
	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" || params["properties"] == nil {
		t.Fatalf("parameters not repaired: %#v", params)
	}
}

func TestSanitizeUpstreamBodyDoesNotMutateInput(t *testing.T) {
	input := map[string]any{"temperature": 3, "tools": []any{map[string]any{"name": "Bash"}}}
	got := SanitizeUpstreamBody(input)
	if input["temperature"] != 3 {
		t.Fatalf("input mutated: %#v", input)
	}
	if got["temperature"] != float64(2) {
		t.Fatalf("output not sanitized: %#v", got)
	}
}

func TestPrepareUpstreamBodyStripsPrivateKeys(t *testing.T) {
	out := PrepareUpstreamBody(map[string]any{
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":            []any{map[string]any{"name": "Edit", "input_schema": map[string]any{"type": "object"}}},
		"_history_compact": map[string]any{"applied": true},
		"prompt_cache_key": "x",
	})
	if out["_history_compact"] != nil || out["_prompt_stabilize"] != nil {
		t.Fatalf("private keys leaked: %#v", out)
	}
	if out["prompt_cache_key"] != "x" {
		t.Fatalf("prompt_cache_key should be forwarded to upstream: %#v", out)
	}
	tools := out["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["parameters"] == nil {
		t.Fatalf("parameters missing: %#v", tools)
	}
}

func TestSanitizeUpstreamBodyKeepsPromptCacheKey(t *testing.T) {
	out := SanitizeUpstreamBody(map[string]any{
		"messages":               []any{map[string]any{"role": "user", "content": "hi"}},
		"prompt_cache_key":       "019f668b-9052-7842-ae62-12580fdf5005",
		"prompt_cache_retention": "session",
		"presence_penalty":       0.5,
		"_prompt_cache_key":      "private",
	})
	if out["prompt_cache_key"] != "019f668b-9052-7842-ae62-12580fdf5005" {
		t.Fatalf("prompt_cache_key should be kept for upstream: %#v", out)
	}
	if out["prompt_cache_retention"] != "session" {
		t.Fatalf("prompt_cache_retention should be kept for upstream: %#v", out)
	}
	if out["_prompt_cache_key"] != nil {
		t.Fatalf("private prompt_cache key leaked: %#v", out)
	}
	if out["presence_penalty"] != nil {
		t.Fatalf("unsupported field leaked: %#v", out)
	}
}

func TestSanitizeUpstreamBodyPromptCacheKeyAbsent(t *testing.T) {
	out := SanitizeUpstreamBody(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, ok := out["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should not be present: %#v", out)
	}
	if _, ok := out["prompt_cache_retention"]; ok {
		t.Fatalf("prompt_cache_retention should not be present: %#v", out)
	}
}

func TestNormalizeFunctionToolHardensShellSchema(t *testing.T) {
	body := map[string]any{
		"model":    "grok-4.5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "shell",
					"description": "Run command",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd":     map[string]any{"type": "string", "description": "legacy"},
							"command": map[string]any{"type": "string"},
							"argv":    map[string]any{"type": "array"},
							"workdir": map[string]any{"type": "string"},
						},
						"required": []any{"cmd"},
					},
				},
			},
		},
	}
	got := SanitizeUpstreamBody(body)
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", got["tools"])
	}
	tool := tools[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if _, ok := props["command"]; !ok {
		t.Fatalf("command property missing: %#v", props)
	}
	for _, bad := range []string{"cmd", "argv", "args"} {
		if _, ok := props[bad]; ok {
			t.Fatalf("alias %s must be removed from schema: %#v", bad, props)
		}
	}
	if _, ok := props["workdir"]; !ok {
		t.Fatalf("workdir should remain: %#v", props)
	}
	req, _ := params["required"].([]any)
	joined := fmt.Sprint(req)
	if !strings.Contains(joined, "command") {
		t.Fatalf("required must include command: %#v", req)
	}
	if strings.Contains(joined, "cmd") {
		t.Fatalf("required must not include cmd: %#v", req)
	}
	desc := fmt.Sprint(fn["description"])
	if !strings.Contains(strings.ToLower(desc), "command") {
		t.Fatalf("description should mention command: %q", desc)
	}
}

func TestNormalizeFunctionToolHardensExecCommand(t *testing.T) {
	body := map[string]any{
		"model":    "grok-4.5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "exec_command",
					"description": "Run a shell command",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"cmd": map[string]any{"type": "string"},
						},
						"required": []any{"cmd"},
					},
				},
			},
		},
	}
	got := SanitizeUpstreamBody(body)
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", got["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	if _, ok := props["command"]; !ok {
		t.Fatalf("exec_command must harden to command: %#v", props)
	}
	if _, ok := props["cmd"]; ok {
		t.Fatalf("cmd alias must be removed for exec_command: %#v", props)
	}
	req := fmt.Sprint(params["required"])
	if !strings.Contains(req, "command") || strings.Contains(req, "cmd") {
		t.Fatalf("required=%#v", params["required"])
	}
}

func TestSanitizeHistoryRewritesFunctionCallCmd(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5",
		"messages": []any{
			map[string]any{"role": "user", "content": "weather"},
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"function_call": map[string]any{
					"name":      "exec_command",
					"arguments": `{"cmd":"curl wttr.in/Changsha"}`,
				},
			},
		},
	}
	// History alias rewrite runs in StabilizePromptBody (via PrepareUpstreamBody),
	// not SanitizeUpstreamBody alone.
	got := PrepareUpstreamBody(body)
	msgs, _ := got["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("messages=%#v", got["messages"])
	}
	asst := msgs[len(msgs)-1].(map[string]any)
	fc := asst["function_call"].(map[string]any)
	args := fmt.Sprint(fc["arguments"])
	if !strings.Contains(args, `"command"`) {
		t.Fatalf("function_call history must rewrite cmd→command: %s", args)
	}
	if strings.Contains(args, `"cmd"`) {
		t.Fatalf("cmd leftover in history: %s", args)
	}
}
