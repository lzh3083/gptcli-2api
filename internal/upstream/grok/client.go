package grok

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL          string
	CLIversion       string
	ClientSurface    string
	ClientIdentifier string
	HTTP             *http.Client
}

type Account struct {
	ID    string
	Token string
}

type Event struct {
	Data []byte
	Done bool
}

// SSE scan constants (package-level to avoid per-call []byte("data:") allocs).
var (
	dataPrefix = []byte("data:")
	doneMarker = []byte("[DONE]")
)

type UpstreamError struct {
	Status     int
	Body       string
	RetryAfter string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.Status, e.Body)
}

// defaultHTTPClient returns a properly configured HTTP client with connection pooling
func defaultHTTPClient() *http.Client {
	// No overall Client.Timeout: streaming responses can run for minutes.
	// Bound connect + response headers only via Transport.
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        200,               // 增加全局空闲连接数
			MaxIdleConnsPerHost: 100,               // 增加每个 host 的空闲连接数，支持高并发
			MaxConnsPerHost:     200,               // 增加每个 host 的最大连接数
			IdleConnTimeout:     120 * time.Second, // 延长空闲连接保持时间
			TLSHandshakeTimeout: 8 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,  // fail-fast dial for failover TTFT
				KeepAlive: 60 * time.Second, // 延长 TCP keepalive
			}).DialContext,
			ForceAttemptHTTP2:     true,
			DisableCompression:    false,
			ExpectContinueTimeout: 1 * time.Second,
			// Failover TTFT: do not sit 20s on a hung first-byte account.
			// Long streams keep the body open after headers arrive.
			ResponseHeaderTimeout: 12 * time.Second,
			DisableKeepAlives:     false,
			WriteBufferSize:       32 * 1024, // 增加写缓冲，提高大请求性能
			ReadBufferSize:        32 * 1024, // 增加读缓冲，提高大响应性能
		},
	}
}

func (c *Client) Open(ctx context.Context, account Account, model string, body map[string]any) (*http.Response, error) {
	if c.HTTP == nil {
		c.HTTP = defaultHTTPClient()
	}
	// CPA / cli-chat-proxy cache path: always call /responses (not /chat/completions).
	// Convert chat-style body → responses body, then bridge SSE back to chat chunks
	// so internal/proxy can keep parsing chat.completion.chunk frames.
	src := cloneMap(body)
	convID := extractConvID(src)
	if convID != "" {
		if raw, _ := src["prompt_cache_key"].(string); strings.TrimSpace(raw) == "" {
			src["prompt_cache_key"] = convID
		}
	}
	payload := chatToResponsesPayload(src, model)
	if convID != "" {
		if raw, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(raw) == "" {
			payload["prompt_cache_key"] = convID
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/responses", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	for name, value := range c.Headers(account.Token, model, convID) {
		request.Header.Set(name, value)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return nil, &UpstreamError{
			Status: response.StatusCode, Body: string(errBody),
			RetryAfter: response.Header.Get("Retry-After"),
		}
	}
	// Translate Responses SSE → chat.completion.chunk SSE for the proxy stack.
	response.Body = responsesToChatStream(response.Body)
	if response.Header != nil {
		response.Header.Set("Content-Type", "text/event-stream")
		response.Header.Set("X-Grok2API-Upstream-Protocol", "responses")
	}
	return response, nil
}

func (c *Client) Headers(token, model string, convID ...string) map[string]string {
	version := c.CLIversion
	if version == "" {
		version = "1.0.13"
	}
	// Defaults match CPA/xAI workspace shell headers that hit prompt cache.
	// Do not force model override by default: CPA cache-hit traffic often omits it.
	identifier := c.ClientIdentifier
	if identifier == "" {
		identifier = "grok-shell"
	}
	out := map[string]string{
		"Content-Type":             "application/json",
		"Authorization":            "Bearer " + token,
		"X-XAI-Token-Auth":         "xai-grok-cli",
		"x-grok-client-version":    version,
		"x-grok-client-identifier": identifier,
		"User-Agent":               "xai-grok-workspace/" + version,
		"Accept":                   "text/event-stream",
		"Connection":               "Keep-Alive",
	}
	// Keep optional surface/model override only when explicitly configured.
	if surface := strings.TrimSpace(c.ClientSurface); surface != "" {
		out["x-grok-client-surface"] = surface
	}
	// Model override is optional. Prefer body model; only set override when ClientSurface
	// is configured for legacy grok-cli compatibility, or model is non-empty AND
	// explicitly requested via env later. For cache parity with CPA, leave unset.
	_ = model
	id := ""
	if len(convID) > 0 {
		id = strings.TrimSpace(convID[0])
	}
	if id != "" {
		// CPA sets both casings; Go Header.Set canonicalizes keys.
		out["x-grok-conv-id"] = id
	}
	return out
}

// extractConvID picks a stable session/cache key from the upstream body for
// x-grok-conv-id. Preference matches CPA: prompt_cache_key first.
func extractConvID(body map[string]any) string {
	if body == nil {
		return ""
	}
	for _, key := range []string{
		"prompt_cache_key",
		"conversation_id",
		"conversation",
		"thread_id",
		"session_id",
	} {
		if value, _ := body[key].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	// Claude Code often only has the conversation id inside metadata.user_id /
	// top-level user (user_…__session_<uuid>). Prefer the session_ token so
	// x-grok-conv-id stays stable across multi-turn tool loops.
	if meta, _ := body["metadata"].(map[string]any); meta != nil {
		for _, key := range []string{"prompt_cache_key", "session_id", "sessionId", "thread_id", "conversation_id"} {
			if value, _ := meta[key].(string); strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		if sid := claudeCodeSessionFromUser(stringField(meta, "user_id")); sid != "" {
			return sid
		}
	}
	if sid := claudeCodeSessionFromUser(stringField(body, "user")); sid != "" {
		return sid
	}
	if sid := claudeCodeSessionFromUser(stringField(body, "user_id")); sid != "" {
		return sid
	}
	return ""
}

// claudeCodeSessionFromUser extracts session_<uuid> from Claude Code user ids.
// Mirrors anthropic.ExtractClaudeCodeSessionID without importing that package
// (upstream/grok must stay free of protocol cycles).
func claudeCodeSessionFromUser(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	low := strings.ToLower(userID)
	if strings.HasPrefix(low, "session_") {
		return userID
	}
	// user_xxx_account__session_01234567-89ab-cdef-0123-456789abcdef
	idx := strings.Index(low, "session_")
	if idx < 0 {
		return ""
	}
	// Return original-cased slice from the same offset.
	return strings.TrimSpace(userID[idx:])
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func ReadSSE(reader io.Reader, emit func(Event) error) error {
	scanner := bufio.NewScanner(reader)
	// Dense thinking / multi-tool turns produce large SSE lines (tool args).
	// 128KiB initial / 8MiB max reduces re-allocation on long payloads.
	scanner.Buffer(make([]byte, 128<<10), 8<<20)

	// Common case: one "data: {...}" line per event. Avoid strings.Join + Text()
	// by scanning bytes and copying only the payload we keep until flush.
	var parts [][]byte
	flush := func() error {
		if len(parts) == 0 {
			return nil
		}
		var joined []byte
		if len(parts) == 1 {
			joined = parts[0]
		} else {
			n := 0
			for _, p := range parts {
				n += len(p) + 1
			}
			joined = make([]byte, 0, n-1)
			for i, p := range parts {
				if i > 0 {
					joined = append(joined, '\n')
				}
				joined = append(joined, p...)
			}
		}
		parts = parts[:0]
		if bytes.Equal(joined, doneMarker) {
			return emit(Event{Done: true})
		}
		return emit(Event{Data: joined})
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, dataPrefix) {
			payload := bytes.TrimSpace(line[len(dataPrefix):])
			// Copy: scanner.Bytes() is invalidated by the next Scan.
			cp := make([]byte, len(payload))
			copy(cp, payload)
			parts = append(parts, cp)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// ReadSSEWithIdle is like ReadSSE but invokes onIdle whenever no complete SSE
// frame arrives for idle duration. Used for Anthropic/OpenAI stream keepalives.
//
// On early consumer exit (emit/onIdle error): closes the reader (if possible) so
// the scanner unblocks, then drains residual channel events until done so the
// background goroutine always exits (no leak under soft-disconnect storms).
func ReadSSEWithIdle(reader io.Reader, idle time.Duration, emit func(Event) error, onIdle func() error) error {
	if idle <= 0 || onIdle == nil {
		return ReadSSE(reader, emit)
	}
	type closer interface{ Close() error }
	closeReader := func() {
		if c, ok := reader.(closer); ok {
			_ = c.Close()
		}
	}

	type result struct {
		event Event
		err   error
		done  bool
	}
	ch := make(chan result, 32)
	go func() {
		err := ReadSSE(reader, func(event Event) error {
			ch <- result{event: event}
			return nil
		})
		ch <- result{err: err, done: true}
	}()

	// drainUntilDone consumes residual events after early return so the scanner
	// goroutine is never stuck forever on a full channel send.
	drainUntilDone := func() {
		for item := range ch {
			if item.done {
				return
			}
		}
	}

	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case item := <-ch:
			if item.done {
				return item.err
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
			if err := emit(item.event); err != nil {
				closeReader()
				go drainUntilDone()
				return err
			}
		case <-timer.C:
			if err := onIdle(); err != nil {
				closeReader()
				go drainUntilDone()
				return err
			}
			timer.Reset(idle)
		}
	}
}
func Retryable(err error) bool {
	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	switch upstream.Status {
	case 401, 403, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input)+2)
	for key, value := range input {
		switch v := value.(type) {
		case map[string]any:
			out[key] = cloneMap(v)
		case []any:
			out[key] = cloneSlice(v)
		default:
			out[key] = value
		}
	}
	return out
}

func cloneSlice(input []any) []any {
	if input == nil {
		return nil
	}
	out := make([]any, len(input))
	for i, value := range input {
		switch v := value.(type) {
		case map[string]any:
			out[i] = cloneMap(v)
		case []any:
			out[i] = cloneSlice(v)
		default:
			out[i] = value
		}
	}
	return out
}
