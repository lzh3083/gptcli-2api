package grok

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/protocol/reasoning"
)

// chatToResponsesPayload converts an OpenAI chat/completions-style body into a
// Responses API body for cli-chat-proxy /responses (CPA-compatible path).
func chatToResponsesPayload(body map[string]any, model string) map[string]any {
	out := map[string]any{
		"model":  model,
		"stream": true,
		// CPA always sets these for cli-chat-proxy prompt cache compatibility.
		"store":   false,
		"include": []any{"reasoning.encrypted_content"},
	}
	if messages, ok := body["messages"].([]any); ok {
		out["input"] = chatMessagesToResponsesInput(messages)
	} else if input := body["input"]; input != nil {
		out["input"] = input
	}
	tools := convertChatToolsToResponses(body["tools"])
	// Critical for free-account prompt cache on cli-chat-proxy:
	// CPA injects built-in x_search so traffic lands on the cacheable route.
	// Keep that default for sub2api/new-api/codex parity. Allow explicit opt-out.
	injectSearch := true
	if v, ok := body["_skip_x_search"]; ok {
		switch x := v.(type) {
		case bool:
			injectSearch = !x
		case string:
			s := strings.TrimSpace(strings.ToLower(x))
			injectSearch = s != "1" && s != "true" && s != "yes"
		}
	}
	if injectSearch && !hasToolType(tools, "x_search") {
		tools = append([]any{map[string]any{"type": "x_search"}}, tools...)
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	for _, key := range []string{
		"temperature", "top_p", "tool_choice", "parallel_tool_calls",
		"prompt_cache_key", "user", "instructions",
	} {
		if value, ok := body[key]; ok && value != nil {
			out[key] = value
		}
	}
	// Responses uses max_output_tokens; accept either name from chat body.
	if value := firstNonNil(body["max_output_tokens"], body["max_tokens"]); value != nil {
		out["max_output_tokens"] = value
	}
	if effort, _ := body["reasoning_effort"].(string); strings.TrimSpace(effort) != "" {
		out["reasoning"] = map[string]any{"effort": clampGrokEffort(effort), "summary": "auto"}
	} else if reasoning, ok := body["reasoning"].(map[string]any); ok {
		// Preserve client reasoning object; ensure effort exists for cli-chat-proxy.
		r := cloneMap(reasoning)
		effort := strings.TrimSpace(firstString(r, "effort"))
		if effort == "" {
			effort = "low"
		}
		r["effort"] = clampGrokEffort(effort)
		if _, ok := r["summary"]; !ok {
			r["summary"] = "auto"
		}
		out["reasoning"] = r
	} else {
		// Default low for TTFT. Codex/OpenAI often omit effort; high is Grok's top tier.
		out["reasoning"] = map[string]any{"effort": "low", "summary": "auto"}
	}
	if _, ok := out["instructions"]; !ok {
		out["instructions"] = ""
	}
	if _, ok := out["parallel_tool_calls"]; !ok {
		out["parallel_tool_calls"] = true
	}
	// CPA deletes these chat-only / non-cache fields before upstream.
	delete(out, "stream_options")
	delete(out, "presence_penalty")
	delete(out, "frequency_penalty")
	delete(out, "logit_bias")
	delete(out, "n")
	delete(out, "logprobs")
	delete(out, "top_logprobs")
	delete(out, "prompt_cache_retention")
	delete(out, "previous_response_id")
	delete(out, "safety_identifier")
	// Drop chat-only fields that responses rejects (CPA also deletes these).
	// stream_options / presence_penalty / etc. are intentionally omitted.
	return out
}

func chatMessagesToResponsesInput(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		switch role {
		case "tool":
			callID := firstString(msg, "tool_call_id", "call_id", "id")
			if callID == "" {
				callID = "call_go"
			}
			// Tool outputs may be empty strings; that is valid (not a content block).
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  stringifyContent(msg["content"]),
			})
		case "assistant":
			// Prefer structured tool_calls when present.
			if calls := toolCallsFromMessage(msg); len(calls) > 0 {
				for _, call := range calls {
					out = append(out, call)
				}
				// Also keep assistant text if any (skip empty — Empty content block).
				if item := responsesMessageOrNil("assistant", msg["content"]); item != nil {
					out = append(out, item)
				}
				continue
			}
			if item := responsesMessageOrNil("assistant", msg["content"]); item != nil {
				out = append(out, item)
			}
		case "system":
			// CPA maps system → developer for cli-chat-proxy cache prefix stability.
			// Never emit empty developer content blocks (upstream 400 Empty content block).
			if item := responsesMessageOrNil("developer", msg["content"]); item != nil {
				out = append(out, item)
			}
		case "user", "developer":
			if item := responsesMessageOrNil(role, msg["content"]); item != nil {
				out = append(out, item)
			}
		default:
			// Pass through unknown roles as user text when possible.
			if item := responsesMessageOrNil("user", msg["content"]); item != nil {
				out = append(out, item)
			}
		}
	}
	return out
}

// responsesMessage builds CPA-compatible Responses input message items:
// {type:message, role, content:[{type:input_text|output_text, text}]}.
// Deprecated for empty text — use responsesMessageOrNil.
func responsesMessage(role string, content any) map[string]any {
	if item := responsesMessageOrNil(role, content); item != nil {
		return item
	}
	// Fallback for callers that still require a map: use a single space only when
	// forced. Prefer responsesMessageOrNil and skip empty messages.
	role = strings.ToLower(strings.TrimSpace(role))
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	return map[string]any{
		"type": "message",
		"role": role,
		"content": []any{
			map[string]any{"type": partType, "text": " "},
		},
	}
}

// responsesMessageOrNil returns nil when the message would have an empty content
// block. cli-chat-proxy rejects {type:input_text|output_text, text:""} with
// HTTP 400 "Empty content block" (Codex multi-turn / tool-only history).
func responsesMessageOrNil(role string, content any) map[string]any {
	role = strings.ToLower(strings.TrimSpace(role))
	partType := "input_text"
	if role == "assistant" {
		partType = "output_text"
	}
	// Multi-part image content: preserve non-empty structured parts.
	if parts, ok := content.([]any); ok && len(parts) > 0 {
		normalized := normalizeResponsesContentParts(parts, partType)
		if len(normalized) == 0 {
			return nil
		}
		return map[string]any{
			"type":    "message",
			"role":    role,
			"content": normalized,
		}
	}
	text := strings.TrimSpace(stringifyContent(content))
	if text == "" {
		return nil
	}
	return map[string]any{
		"type": "message",
		"role": role,
		"content": []any{
			map[string]any{"type": partType, "text": text},
		},
	}
}

// normalizeResponsesContentParts drops empty text blocks and unknown empty
// objects that trigger upstream "Empty content block".
func normalizeResponsesContentParts(parts []any, defaultPartType string) []any {
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch p := part.(type) {
		case string:
			if strings.TrimSpace(p) == "" {
				continue
			}
			out = append(out, map[string]any{"type": defaultPartType, "text": p})
		case map[string]any:
			typeName := strings.ToLower(strings.TrimSpace(firstString(p, "type")))
			switch typeName {
			case "", "text", "input_text", "output_text":
				text := strings.TrimSpace(firstString(p, "text", "content"))
				if text == "" {
					continue
				}
				pt := defaultPartType
				if typeName == "output_text" {
					pt = "output_text"
				} else if typeName == "input_text" || typeName == "text" || typeName == "" {
					pt = defaultPartType
				}
				out = append(out, map[string]any{"type": pt, "text": text})
			case "input_image", "image", "image_url":
				// Preserve multimodal images for vision models. Normalize OpenAI
				// chat {type:image_url,image_url:{url}} → Responses input_image.
				url := imagePartURL(p)
				if url == "" {
					continue
				}
				part := map[string]any{
					"type":      "input_image",
					"image_url": url,
				}
				if img, ok := p["image_url"].(map[string]any); ok {
					if d := firstString(img, "detail"); d != "" {
						part["detail"] = d
					}
				}
				if d := firstString(p, "detail"); d != "" {
					part["detail"] = d
				}
				out = append(out, part)
			default:
				// Unknown part with no text: drop (common empty placeholder blocks).
				if strings.TrimSpace(firstString(p, "text", "content")) == "" {
					continue
				}
				out = append(out, p)
			}
		}
	}
	return out
}

func toolCallsFromMessage(msg map[string]any) []map[string]any {
	rawCalls, ok := msg["tool_calls"].([]any)
	if !ok || len(rawCalls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(rawCalls))
	for _, raw := range rawCalls {
		call, _ := raw.(map[string]any)
		if call == nil {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		name := firstString(fn, "name")
		if name == "" {
			name = firstString(call, "name")
		}
		args := firstString(fn, "arguments")
		if args == "" {
			args = firstString(call, "arguments")
			if args == "" {
				args = "{}"
			}
		}
		callID := firstString(call, "id", "call_id")
		if callID == "" {
			callID = "call_go"
		}
		item := map[string]any{
			"type":      "function_call",
			"call_id":   callID,
			"name":      name,
			"arguments": args,
		}
		if id := firstString(call, "id"); id != "" {
			item["id"] = id
		}
		out = append(out, item)
	}
	return out
}

func hasToolType(tools []any, typeName string) bool {
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(firstString(tool, "type"))) == typeName {
			return true
		}
	}
	return false
}

func convertChatToolsToResponses(raw any) []any {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		typeName, _ := tool["type"].(string)
		typeName = strings.ToLower(strings.TrimSpace(typeName))
		// OpenAI chat shape: {type:function, function:{name,description,parameters}}
		if fn, _ := tool["function"].(map[string]any); fn != nil {
			name := firstString(fn, "name")
			if name == "" {
				continue
			}
			entry := map[string]any{
				"type":        "function",
				"name":        name,
				"parameters":  firstNonNil(fn["parameters"], map[string]any{"type": "object", "properties": map[string]any{}}),
				"description": firstString(fn, "description"),
			}
			if entry["description"] == "" {
				delete(entry, "description")
			}
			out = append(out, entry)
			continue
		}
		// Already responses-ish: {type:function,name,parameters}
		if typeName == "function" || typeName == "" {
			name := firstString(tool, "name")
			if name == "" {
				continue
			}
			entry := map[string]any{
				"type":        "function",
				"name":        name,
				"parameters":  firstNonNil(tool["parameters"], map[string]any{"type": "object", "properties": map[string]any{}}),
				"description": firstString(tool, "description"),
			}
			if entry["description"] == "" {
				delete(entry, "description")
			}
			out = append(out, entry)
			continue
		}
		// Pass through server-side tools (web_search etc.) when already flat.
		out = append(out, tool)
	}
	return out
}

func normalizeMessageContent(value any) any {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		// Keep multi-part content as-is when it already looks responses-compatible.
		return v
	default:
		return stringifyContent(value)
	}
}

func stringifyContent(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			switch p := part.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				if text, _ := p["text"].(string); text != "" {
					b.WriteString(text)
					continue
				}
				if text, _ := p["content"].(string); text != "" {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}

// imagePartURL extracts a usable image URL from OpenAI/Anthropic/Responses shapes.
func imagePartURL(p map[string]any) string {
	if p == nil {
		return ""
	}
	if u := firstString(p, "url", "image"); u != "" {
		return u
	}
	switch v := p["image_url"].(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	case map[string]any:
		if u := firstString(v, "url"); u != "" {
			return u
		}
	}
	if src, ok := p["source"].(map[string]any); ok {
		if strings.EqualFold(firstString(src, "type"), "base64") {
			media := firstString(src, "media_type")
			if media == "" {
				media = "image/png"
			}
			data := firstString(src, "data")
			if data != "" {
				return "data:" + media + ";base64," + data
			}
		}
		if u := firstString(src, "url"); u != "" {
			return u
		}
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, key := range keys {
		if value, _ := m[key].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// responsesToChatStream rewrites cli-chat-proxy /responses SSE into
// chat.completion.chunk frames so the rest of the proxy stack stays unchanged.
// If the upstream body is already chat.completion.chunk SSE (unit tests /
// legacy fixtures), frames are passed through unchanged.
func responsesToChatStream(body io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := translateOrPassthroughSSE(body, pw)
		_ = body.Close()
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	return pr
}

type responsesBridge struct {
	id        string
	model     string
	created   int64
	toolIndex map[string]int // item_id / call_id -> tool index
	nextTool  int
	usage     map[string]any
	finish    string
	// hasPayload tracks whether streaming events already emitted client-visible
	// text/reasoning/tool output. Some upstreams only include final output on
	// response.completed; bridge that once, but do not duplicate streamed deltas.
	hasPayload bool
	mu         sync.Mutex
}

func translateOrPassthroughSSE(reader io.Reader, writer io.Writer) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)

	var (
		mode string // "", "chat", "responses"
		data []string
		// rawLines holds original lines for the current SSE event while mode is unknown.
		rawLines []string
		bridge   = &responsesBridge{created: time.Now().Unix(), toolIndex: map[string]int{}}
	)

	emitRawEvent := func() error {
		for _, line := range rawLines {
			if _, err := io.WriteString(writer, line+"\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(writer, "\n"); err != nil {
			return err
		}
		rawLines = rawLines[:0]
		return nil
	}

	handleResponsesData := func(joined string) error {
		if joined == "[DONE]" {
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(joined), &event); err != nil {
			return nil
		}
		frames := bridge.handle(event)
		for _, frame := range frames {
			if _, err := io.WriteString(writer, "data: "+frame+"\n\n"); err != nil {
				return err
			}
		}
		return nil
	}

	detectMode := func(joined string) string {
		if joined == "[DONE]" {
			return "chat"
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(joined), &probe); err != nil {
			return "chat"
		}
		if obj, _ := probe["object"].(string); strings.Contains(obj, "chat.completion") {
			return "chat"
		}
		if _, ok := probe["choices"].([]any); ok {
			return "chat"
		}
		if typeName, _ := probe["type"].(string); strings.HasPrefix(typeName, "response.") {
			return "responses"
		}
		if _, ok := probe["response"].(map[string]any); ok {
			return "responses"
		}
		return "responses"
	}

	flush := func() error {
		if len(data) == 0 {
			// blank event separator with no data — ignore
			rawLines = rawLines[:0]
			return nil
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		if mode == "" {
			mode = detectMode(joined)
		}
		if mode == "chat" {
			// Write original event exactly once.
			if err := emitRawEvent(); err != nil {
				return err
			}
			return nil
		}
		rawLines = rawLines[:0]
		return handleResponsesData(joined)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if mode == "chat" {
			if _, err := io.WriteString(writer, line+"\n"); err != nil {
				return err
			}
			continue
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		rawLines = append(rawLines, line)
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if mode == "chat" {
		return nil
	}
	if mode == "" {
		if _, err := io.WriteString(writer, "data: [DONE]\n\n"); err != nil {
			return err
		}
		return nil
	}
	// Terminal chat frames for responses mode.
	if bridge.usage != nil || bridge.finish != "" {
		if bridge.usage != nil {
			usageOnly := bridge.baseChunk()
			usageOnly["choices"] = []any{}
			usageOnly["usage"] = bridge.usage
			encodedUsage, _ := json.Marshal(usageOnly)
			if _, err := io.WriteString(writer, "data: "+string(encodedUsage)+"\n\n"); err != nil {
				return err
			}
		} else {
			term := bridge.baseChunk()
			choice := map[string]any{"index": 0, "delta": map[string]any{}}
			if bridge.finish != "" {
				choice["finish_reason"] = bridge.finish
			} else {
				choice["finish_reason"] = "stop"
			}
			term["choices"] = []any{choice}
			encoded, _ := json.Marshal(term)
			if _, err := io.WriteString(writer, "data: "+string(encoded)+"\n\n"); err != nil {
				return err
			}
		}
	}
	if _, err := io.WriteString(writer, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return nil
}

// deprecated name kept as thin wrapper for clarity in call sites / tests.
func translateResponsesSSE(reader io.Reader, writer io.Writer) error {
	return translateOrPassthroughSSE(reader, writer)
}

func (b *responsesBridge) handle(event map[string]any) []string {
	typeName, _ := event["type"].(string)
	switch typeName {
	case "response.created", "response.in_progress":
		if resp, _ := event["response"].(map[string]any); resp != nil {
			b.captureMeta(resp)
		}
		return nil
	case "response.output_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		b.hasPayload = true
		return []string{b.encodeDelta(map[string]any{"content": delta}, nil)}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		b.hasPayload = true
		return []string{b.encodeDelta(map[string]any{"reasoning_content": delta}, nil)}
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item == nil {
			return nil
		}
		itemType, _ := item["type"].(string)
		if itemType != "function_call" {
			return nil
		}
		idx := b.toolIdx(item)
		callID := firstString(item, "call_id", "id")
		name := firstString(item, "name")
		args, _ := item["arguments"].(string)
		toolCall := map[string]any{
			"index": idx,
			"id":    callID,
			"type":  "function",
			"function": map[string]any{
				"name":      name,
				"arguments": args,
			},
		}
		b.hasPayload = true
		return []string{b.encodeDelta(map[string]any{"tool_calls": []any{toolCall}}, nil)}
	case "response.function_call_arguments.delta":
		delta, _ := event["delta"].(string)
		if delta == "" {
			return nil
		}
		itemID := firstString(event, "item_id", "call_id")
		idx := b.toolIdxByID(itemID)
		toolCall := map[string]any{
			"index": idx,
			"type":  "function",
			"function": map[string]any{
				"arguments": delta,
			},
		}
		b.hasPayload = true
		return []string{b.encodeDelta(map[string]any{"tool_calls": []any{toolCall}}, nil)}
	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		if item == nil {
			return nil
		}
		if firstString(item, "type") == "function_call" {
			b.finish = "tool_calls"
			// Ensure name/id are present even if only done event carried them.
			idx := b.toolIdx(item)
			callID := firstString(item, "call_id", "id")
			name := firstString(item, "name")
			args, _ := item["arguments"].(string)
			if callID == "" && name == "" && args == "" {
				return nil
			}
			toolCall := map[string]any{
				"index": idx,
				"id":    callID,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": "",
				},
			}
			// Don't re-emit full arguments on done (already streamed via deltas);
			// only fill name/id if missing earlier.
			return []string{b.encodeDelta(map[string]any{"tool_calls": []any{toolCall}}, nil)}
		}
		return nil
	case "response.completed":
		if resp, _ := event["response"].(map[string]any); resp != nil {
			b.captureMeta(resp)
			if usage, _ := resp["usage"].(map[string]any); usage != nil {
				b.usage = responsesUsageToChat(usage)
			}
			if b.finish == "" {
				b.finish = finishFromResponsesOutput(resp["output"])
			}
			if !b.hasPayload {
				return b.completedOutputFrames(resp["output"])
			}
		}
		return nil
	default:
		return nil
	}
}

func (b *responsesBridge) captureMeta(resp map[string]any) {
	if id, _ := resp["id"].(string); id != "" && b.id == "" {
		b.id = id
	}
	if model, _ := resp["model"].(string); model != "" {
		b.model = model
	}
	if created, ok := asInt64(resp["created_at"]); ok && created > 0 {
		b.created = created
	}
}

func (b *responsesBridge) toolIdx(item map[string]any) int {
	id := firstString(item, "id", "call_id")
	return b.toolIdxByID(id)
}

func (b *responsesBridge) toolIdxByID(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	id = strings.TrimSpace(id)
	if id != "" {
		if idx, ok := b.toolIndex[id]; ok {
			return idx
		}
		idx := b.nextTool
		b.toolIndex[id] = idx
		b.nextTool++
		return idx
	}
	idx := b.nextTool
	b.nextTool++
	return idx
}

func (b *responsesBridge) baseChunk() map[string]any {
	id := b.id
	if id == "" {
		id = fmt.Sprintf("chatcmpl-go-%d", b.created)
	}
	model := b.model
	if model == "" {
		model = "grok-4.5"
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": b.created,
		"model":   model,
	}
}

func (b *responsesBridge) encodeDelta(delta map[string]any, finish any) string {
	chunk := b.baseChunk()
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = finish
	}
	chunk["choices"] = []any{choice}
	encoded, _ := json.Marshal(chunk)
	return string(encoded)
}

func responsesUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	prompt := firstNumber(usage, "input_tokens", "prompt_tokens")
	completion := firstNumber(usage, "output_tokens", "completion_tokens")
	total := firstNumber(usage, "total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	// Preserve cost/extra fields when present.
	for _, key := range []string{"num_sources_used", "cost_in_usd_ticks", "num_server_side_tools_used"} {
		if value, ok := usage[key]; ok {
			out[key] = value
		}
	}
	cached := 0.0
	if details, _ := usage["input_tokens_details"].(map[string]any); details != nil {
		cached = firstNumber(details, "cached_tokens", "cache_read_tokens")
		textTokens := firstNumber(details, "text_tokens")
		if textTokens == 0 {
			textTokens = prompt
		}
		out["prompt_tokens_details"] = map[string]any{
			"cached_tokens": cached,
			"text_tokens":   textTokens,
			"audio_tokens":  firstNumber(details, "audio_tokens"),
			"image_tokens":  firstNumber(details, "image_tokens"),
		}
	} else if details, _ := usage["prompt_tokens_details"].(map[string]any); details != nil {
		out["prompt_tokens_details"] = details
		cached = firstNumber(details, "cached_tokens")
	} else {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": 0, "text_tokens": prompt}
	}
	if details, _ := usage["output_tokens_details"].(map[string]any); details != nil {
		out["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": firstNumber(details, "reasoning_tokens", "thinking_tokens"),
			"audio_tokens":     firstNumber(details, "audio_tokens"),
		}
	} else if details, _ := usage["completion_tokens_details"].(map[string]any); details != nil {
		out["completion_tokens_details"] = details
	}
	// Top-level aliases used by UsageFromOpenAI.
	out["cached_tokens"] = cached
	out["input_tokens"] = prompt
	out["output_tokens"] = completion
	out["cache_read_input_tokens"] = cached
	return out
}

func (b *responsesBridge) completedOutputFrames(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	frames := make([]string, 0, len(items))
	for _, item := range items {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		switch firstString(m, "type") {
		case "message":
			for _, text := range outputTextParts(m["content"]) {
				b.hasPayload = true
				frames = append(frames, b.encodeDelta(map[string]any{"content": text}, nil))
			}
		case "reasoning":
			for _, text := range outputTextParts(firstNonNil(m["summary"], m["content"])) {
				b.hasPayload = true
				frames = append(frames, b.encodeDelta(map[string]any{"reasoning_content": text}, nil))
			}
		case "function_call":
			idx := b.toolIdx(m)
			callID := firstString(m, "call_id", "id")
			name := firstString(m, "name")
			args, _ := m["arguments"].(string)
			if callID == "" && name == "" && args == "" {
				continue
			}
			toolCall := map[string]any{
				"index": idx,
				"id":    callID,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}
			b.hasPayload = true
			frames = append(frames, b.encodeDelta(map[string]any{"tool_calls": []any{toolCall}}, nil))
		}
	}
	return frames
}

func outputTextParts(raw any) []string {
	var out []string
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				out = append(out, v)
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			if text, _ := v["text"].(string); strings.TrimSpace(text) != "" {
				out = append(out, text)
				return
			}
			if text, _ := v["content"].(string); strings.TrimSpace(text) != "" {
				out = append(out, text)
			}
		}
	}
	walk(raw)
	return out
}

func finishFromResponsesOutput(raw any) string {
	items, ok := raw.([]any)
	if !ok {
		return "stop"
	}
	for _, item := range items {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		if firstString(m, "type") == "function_call" {
			return "tool_calls"
		}
	}
	return "stop"
}

func firstNumber(m map[string]any, keys ...string) float64 {
	if m == nil {
		return 0
	}
	for _, key := range keys {
		if value, ok := m[key]; ok {
			if n, ok := asFloat(value); ok {
				return n
			}
		}
	}
	return 0
}

func asFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func asInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

// Ensure unused import guard for bytes in case of future binary ops.
var _ = bytes.MinRead

// clampGrokEffort folds Claude Code / Anthropic client labels onto Grok's three
// levels (low|medium|high). xhigh / max / ultracode / Ultra / Proactive / extra-high → high.
// Empty / disabled defaults to low for TTFT-friendly outbound.
func clampGrokEffort(effort string) string {
	if up := reasoning.ToUpstream(effort); up != "" {
		return up
	}
	s := strings.ToLower(strings.TrimSpace(effort))
	if s == "" || s == "none" || s == "null" || s == "false" || s == "off" || s == "disabled" {
		return "low"
	}
	// Unknown non-empty → medium (safe middle) rather than pass-through garbage.
	return "medium"
}
