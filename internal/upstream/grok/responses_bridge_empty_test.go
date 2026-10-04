package grok

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessagesSkipEmptyContentBlocks(t *testing.T) {
	input := chatMessagesToResponsesInput([]any{
		map[string]any{"role": "system", "content": ""},
		map[string]any{"role": "user", "content": ""},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": ""},
		}},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "shell", "arguments": `{"cmd":"ls"}`}},
		}},
		map[string]any{"role": "user", "content": "hi"},
	})
	// system empty + empty user + empty parts dropped; tool call + user hi remain
	if len(input) < 2 {
		t.Fatalf("expected tool + user, got %#v", input)
	}
	raw, _ := json.Marshal(input)
	s := string(raw)
	if strings.Contains(s, `"text":""`) {
		t.Fatalf("empty text content block leaked: %s", s)
	}
	// user hi present
	foundUser := false
	for _, item := range input {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if m["type"] == "message" && m["role"] == "user" {
			foundUser = true
		}
		if m["type"] == "function_call" {
			// ok
		}
	}
	if !foundUser {
		t.Fatalf("missing user message: %#v", input)
	}
}

func TestResponsesMessageOrNilEmpty(t *testing.T) {
	if responsesMessageOrNil("user", "") != nil {
		t.Fatal("empty string should be nil")
	}
	if responsesMessageOrNil("user", []any{map[string]any{"type": "input_text", "text": ""}}) != nil {
		t.Fatal("empty input_text parts should be nil")
	}
	item := responsesMessageOrNil("user", "hello")
	if item == nil {
		t.Fatal("expected message")
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%#v", content)
	}
	part, _ := content[0].(map[string]any)
	if part["text"] != "hello" {
		t.Fatalf("part=%#v", part)
	}
}

func TestChatToResponsesPayloadNoEmptyBlocks(t *testing.T) {
	body := chatToResponsesPayload(map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "  "},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": ""},
		},
		"_skip_x_search": true,
	}, "grok-4.5")
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), `"text":""`) {
		t.Fatalf("empty content block in payload: %s", raw)
	}
	input, _ := body["input"].([]any)
	// only user hi (system whitespace + empty assistant dropped)
	if len(input) != 1 {
		t.Fatalf("input=%#v", input)
	}
}
