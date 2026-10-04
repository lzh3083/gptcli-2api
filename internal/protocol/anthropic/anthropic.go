package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type Usage struct {
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	CacheReadTokens     int
	CacheCreationTokens int
}

func StopReason(finish string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_use"
	}
	switch finish {
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func Completion(messageID, model, content, reasoning, finish string, calls []ToolCall, usage Usage, allowed []string) map[string]any {
	blocks := make([]any, 0, 2+len(calls))
	if reasoning != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning})
	}
	if content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	}
	emittedTools := 0
	for _, call := range calls {
		name := toolcall.CanonicalName(call.Name, allowed)
		// Non-stream end-of-turn: force-finish like stream Finish(). Without
		// CoerceCompleteJSON, path+old omit of new_string (true delete-match)
		// is dropped and Claude Code sees end_turn instead of tool_use.
		arguments := toolcall.CoerceCompleteJSON(call.Arguments, name)
		if name == "" || !toolcall.CompleteJSON(arguments, name) {
			// Retry under Edit after Update→Edit rename race / alias recovery.
			for _, tryName := range []string{
				toolcall.CanonicalName("Edit", allowed),
				"Edit",
			} {
				tryName = strings.TrimSpace(tryName)
				if tryName == "" || tryName == name {
					continue
				}
				if coerced := toolcall.CoerceCompleteJSON(call.Arguments, tryName); toolcall.CompleteJSON(coerced, tryName) {
					name = tryName
					arguments = coerced
					break
				}
			}
		}
		if name == "" || !toolcall.CompleteJSON(arguments, name) {
			continue
		}
		var input any
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			continue
		}
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("toolu_go_%d", emittedTools)
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": input,
		})
		emittedTools++
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}

	outputTokens := usage.CompletionTokens
	if outputTokens <= 0 {
		chars := len([]rune(content)) + len([]rune(reasoning))
		if chars > 0 {
			outputTokens = (chars + 3) / 4
		}
	}
	inputTokens := usage.PromptTokens
	if inputTokens <= 0 && outputTokens <= 0 && usage.TotalTokens > 0 {
		inputTokens = usage.TotalTokens
	}

	return map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"content":       blocks,
		"model":         model,
		"stop_reason":   StopReason(finish, emittedTools > 0),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": usage.CacheCreationTokens,
			"cache_read_input_tokens":     usage.CacheReadTokens,
		},
	}
}

func event(name string, payload any) string {
	encoded, _ := json.Marshal(payload)
	// Hot path: avoid fmt.Sprintf on every Anthropic SSE frame.
	var b strings.Builder
	b.Grow(16 + len(name) + len(encoded))
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteString("\ndata: ")
	b.Write(encoded)
	b.WriteString("\n\n")
	return b.String()
}
func TerminalError(message, errorType string) []string {
	if errorType == "" {
		errorType = "api_error"
	}
	if message == "" {
		message = "request failed"
	}
	// Always close the Anthropic SSE envelope so Claude Code leaves "running"
	// and tool loops can surface the failure instead of hanging on an open stream.
	return []string{
		event("error", map[string]any{
			"type": "error", "error": map[string]any{"type": errorType, "message": message},
		}),
		event("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 0},
		}),
		messageStopFrame,
	}
}

// Cached static SSE frames — Ping/CommentKeepalive are hot on every idle tick
// and pending-tool drip; re-marshaling the same JSON every call was pure waste.
var (
	cachedPing             = event("ping", map[string]any{"type": "ping"})
	cachedCommentKeepalive = ": keepalive\n\n"
	// messageStopFrame is used by Finish / TerminalError hot paths.
	messageStopFrame = event("message_stop", map[string]any{"type": "message_stop"})
)

// Ping is the Anthropic SSE keepalive used during long thinking / tool-prep gaps.
func Ping() string { return cachedPing }

// CommentKeepalive is a pure SSE comment for reverse proxies that ignore named
// ping events but still need idle traffic.
func CommentKeepalive() string { return cachedCommentKeepalive }
