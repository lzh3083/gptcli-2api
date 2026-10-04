package grok

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessagesToResponsesPreservesImageURL(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "what is in this image?"},
				map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url":    "data:image/png;base64,iVBORw0KGgo=",
						"detail": "high",
					},
				},
			},
		},
	}
	input := chatMessagesToResponsesInput(messages)
	raw, _ := json.Marshal(input)
	s := string(raw)
	if !strings.Contains(s, "input_image") {
		t.Fatalf("expected input_image in responses input: %s", s)
	}
	if !strings.Contains(s, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("expected image data url preserved: %s", s)
	}
	if !strings.Contains(s, "what is in this image?") {
		t.Fatalf("expected text preserved: %s", s)
	}
	// Must not stringify multimodal to a single text field that drops the image.
	if strings.Contains(s, `"type":"input_text"`) && !strings.Contains(s, "input_image") {
		t.Fatalf("image dropped: %s", s)
	}
}

func TestImagePartURLNestedOpenAI(t *testing.T) {
	url := imagePartURL(map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "https://example.com/a.png"},
	})
	if url != "https://example.com/a.png" {
		t.Fatalf("url=%q", url)
	}
}

func TestImagePartURLAnthropicSource(t *testing.T) {
	url := imagePartURL(map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": "image/jpeg",
			"data":       "abc",
		},
	})
	if url != "data:image/jpeg;base64,abc" {
		t.Fatalf("url=%q", url)
	}
}
