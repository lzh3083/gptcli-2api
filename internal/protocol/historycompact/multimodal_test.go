package historycompact

import (
	"encoding/json"
	"testing"
)

func TestIsMultimodalContentDetectsImageURL(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "see"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,xx"}},
	}
	if !isMultimodalContent(content) {
		t.Fatal("expected multimodal")
	}
	if isMultimodalContent("plain") {
		t.Fatal("string is not multimodal")
	}
}

func TestApplyTextCompactSkipsMultimodal(t *testing.T) {
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,xx"}},
		},
	}
	if applyTextCompactIfSafe(msg, "flattened") {
		t.Fatal("must not flatten multimodal content")
	}
	// content still array
	if _, ok := msg["content"].([]any); !ok {
		t.Fatalf("content changed type: %#v", msg["content"])
	}
	raw, _ := json.Marshal(msg["content"])
	if string(raw) == `"flattened"` {
		t.Fatal("image content was replaced")
	}
}

func TestApplyTextCompactAllowsPlain(t *testing.T) {
	msg := map[string]any{"role": "user", "content": "long text here"}
	if !applyTextCompactIfSafe(msg, "short") {
		t.Fatal("plain text should compact")
	}
	if msg["content"] != "short" {
		t.Fatalf("content=%v", msg["content"])
	}
}
