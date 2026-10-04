package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/auth"
	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/protocol/anthropic"
	"github.com/hm2899/grokcli-2api/internal/proxy"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
)

func TestLive(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/live", nil)
	NewMigrationMux(func() bool { return true }).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["implementation"] != "go" || body["ok"] != true {
		t.Fatalf("unexpected body %#v", body)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("unexpected content type %q", got)
	}
}

func TestReadyFailClosed(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ready", nil)
	NewMux(Options{
		Ready:  func() bool { return false },
		Reason: func() string { return "migration pending" },
	}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status %d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["reason"] != "migration pending" {
		t.Fatalf("unexpected body %#v", body)
	}
}

func TestMethodAndUnknownRoute(t *testing.T) {
	handler := NewMux(Options{Ready: func() bool { return true }, StaticDir: t.TempDir()})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/live", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /live status = %d", recorder.Code)
	}

	// Root is exact-match only; unknown paths must not fall through to index.html.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /unknown status = %d body=%q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	// Missing index still 404, but route itself must match exactly "/".
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET / with empty static dir status = %d", recorder.Code)
	}
}

func TestHealthAndMetricsAreReadOnlyShells(t *testing.T) {
	handler := NewMigrationMux(func() bool { return false })

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/health status = %d", recorder.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["implementation"] != "go" || health["ready"] != false {
		t.Fatalf("unexpected health %#v", health)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "g2a_runtime_ready") {
		t.Fatalf("unexpected metrics %q", body)
	}
	// Stream hot-path counters (always present even when zero).
	for _, marker := range []string{
		"g2a_stream_writes_total",
		"g2a_stream_bytes_total",
		"g2a_stream_keepalives_total",
		"g2a_stream_soft_gone_total",
		"g2a_stream_coalesce_flush_total",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing stream metric %q in %q", marker, body)
		}
	}
}

func TestModelsRouteFlagAndAuth(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled models route status = %d", recorder.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder = httptest.NewRecorder()
	NewMux(Options{
		Ready:             func() bool { return true },
		PublicReadEnabled: true,
		APIKeys:           auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
	}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	recorder = httptest.NewRecorder()
	NewMux(Options{
		Ready:             func() bool { return true },
		PublicReadEnabled: true,
		APIKeys:           auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
	}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized models status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["object"] != "list" {
		t.Fatalf("unexpected models body %#v", body)
	}
}

type fakeAdminSessions struct{ ok bool }

func (f fakeAdminSessions) VerifyAdminSession(string) bool { return f.ok }

func TestAdminReadRoutesRequireFlagReadinessAndSession(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled status route = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return false }, AdminReadEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status route = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public admin status = %d body=%q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true, AdminSessions: fakeAdminSessions{ok: false}}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/dashboard", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard without session = %d", recorder.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/models", nil)
	req.Header.Set("X-Admin-Token", "token")
	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true, AdminSessions: fakeAdminSessions{ok: true}}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin models with session = %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestMessagesRouteGates(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/messages"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("disabled messages route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return false }, MessagesEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("not-ready messages route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("missing auth messages route = %d", recorder.Code)
			}

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer secret")
			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("store-unavailable messages route = %d", recorder.Code)
			}
		})
	}

	for _, path := range []string{"/v1/messages/count_tokens", "/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("disabled count_tokens route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return false }, MessagesEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("not-ready count_tokens route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("missing auth count_tokens route = %d", recorder.Code)
			}

			// count_tokens is a pure local heuristic: no store required.
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer secret")
			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("empty count_tokens without store = %d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestMessagesCountTokensMatchesPythonHeuristic(t *testing.T) {
	body := `{"system":"abcd","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"tool_use","name":"Edit","input":{"file_path":"/x","old_string":"a","new_string":""}}]}],"tools":[{"name":"Edit","description":"edit files","input_schema":{"type":"object"}}]}`
	recorder := httptest.NewRecorder()
	NewMux(Options{
		Ready:           func() bool { return true },
		MessagesEnabled: true,
		Store:           &postgres.Connector{},
	}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	var payload map[string]int
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["input_tokens"] != 27 {
		t.Fatalf("input_tokens = %d", payload["input_tokens"])
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, MessagesEnabled: true, Store: &postgres.Connector{}}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/messages/count_tokens", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty count_tokens status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid_request_error") {
		t.Fatalf("unexpected empty count_tokens body=%q", recorder.Body.String())
	}
}

func TestResponsesRouteGates(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("disabled responses route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return false }, ResponsesEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("not-ready responses route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:            func() bool { return true },
				ResponsesEnabled: true,
				APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("missing auth responses route = %d", recorder.Code)
			}

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer secret")
			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:            func() bool { return true },
				ResponsesEnabled: true,
				APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("store-unavailable responses route = %d", recorder.Code)
			}
		})
	}
}

func TestStreamAnthropicMessagesWritesSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	usage, _, err := streamAnthropicMessages(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\",\"reasoning_content\":\"plan\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"), "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage["total_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, marker := range []string{"event: message_start", "event: content_block_start", "\"type\":\"thinking\"", "plan", "hi", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamAnthropicMessagesWritesToolUse(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, _, err := streamAnthropicMessages(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), "msg_test", "grok", true, []string{"Edit"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"\"type\":\"tool_use\"", "\"name\":\"Edit\"", "input_json_delta", "\"stop_reason\":\"tool_use\""} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamAnthropicMessagesEmitsThinkingDelta(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, _, err := streamAnthropicMessages(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"\"type\":\"thinking\"", "thinking_delta", "think", "event: message_stop"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamAnthropicThinkingCoalescesFlushes(t *testing.T) {
	// Many tiny thinking deltas must not produce one Flush per delta after the first.
	rec := &countingFlusher{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	var b strings.Builder
	// 40 micro thinking chunks (~1 char each of payload growth)
	for i := 0; i < 40; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"reasoning_content":"x"}}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	_, _, err := streamAnthropicMessages(rec, req, strings.NewReader(b.String()), "msg_coal", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "thinking") && !strings.Contains(out, "thinking_delta") {
		// assembler may emit thinking_delta event name
		if !strings.Contains(out, "content_block") {
			t.Fatalf("expected thinking stream, body=%q", out[:min(200, len(out))])
		}
	}
	// 40 deltas would be ≥40 flushes without coalesce; allow headroom for start/stop.
	if rec.flushCount >= 40 {
		t.Fatalf("thinking deltas not coalesced: flushCount=%d bodyLen=%d", rec.flushCount, len(out))
	}
	if rec.flushCount < 1 {
		t.Fatal("expected some flushes")
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("missing message_stop, flushes=%d", rec.flushCount)
	}
}

type countingFlusher struct {
	*httptest.ResponseRecorder
	flushCount int
}

func (c *countingFlusher) Flush() {
	c.flushCount++
	// ResponseRecorder has no Flush; satisfy http.Flusher only.
}

func TestStreamOpenAIResponsesWritesSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	usage, _, err := streamOpenAIResponses(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"), "resp_test", "grok", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage["total_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", usage)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"event: response.created", "event: response.output_item.added", "event: response.output_text.delta", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamOpenAIResponsesWritesFunctionCall(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, _, err := streamOpenAIResponses(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), "resp_test", "grok", []string{"Edit"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"event: response.function_call_arguments.delta", "event: response.function_call_arguments.done", "event: response.output_item.done", "\"name\":\"Edit\"", "event: response.completed"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamChatCompletionsWritesSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stats, err := streamChatCompletions(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Usage != nil {
		t.Fatalf("unexpected usage stats %#v", stats.Usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("unexpected content-type %q", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"choices"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected stream body %q", body)
	}
}

func TestAdminAndStaticFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.html"), "home")
	mustWrite(t, filepath.Join(dir, "favicon.ico"), "ico")
	mustWrite(t, filepath.Join(dir, "admin", "index.html"), "admin")
	mustWrite(t, filepath.Join(dir, "admin", "keys.html"), "keys")
	mustWrite(t, filepath.Join(dir, "admin", "usage.html"), "usage-page")
	mustWrite(t, filepath.Join(dir, "admin", "logs.html"), "logs-page")
	mustWrite(t, filepath.Join(dir, "js", "app.js"), "console.log(1)")

	handler := NewMux(Options{Ready: func() bool { return true }, StaticDir: dir})

	for _, path := range []string{"/", "/favicon.ico", "/admin", "/admin/keys", "/admin/usage", "/admin/logs", "/static/js/app.js"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%q", path, recorder.Code, recorder.Body.String())
		}
	}

	// overview alias must serve index.html (not 404 looking for overview.html).
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/overview", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "admin" {
		t.Fatalf("/admin/overview status=%d body=%q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/usage", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "usage-page" {
		t.Fatalf("/admin/usage status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("admin cache-control = %q", got)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/keys", nil))
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("admin cache-control = %q", got)
	}

	// Mobile nav regression: missing PAGE_HREF entry produced href="undefined".
	for _, bad := range []string{"/admin/undefined", "/admin/null", "/admin/nope"} {
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, bad, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d want 404", bad, recorder.Code)
		}
	}
}

func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

type slowReader struct {
	chunks []string
	idx    int
	delay  time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.idx >= len(s.chunks) {
		return 0, io.EOF
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	n := copy(p, s.chunks[s.idx])
	s.idx++
	if n < len(s.chunks[s.idx-1]) {
		// should not happen with full chunk sizes in tests
	}
	return n, nil
}

func TestStreamAnthropicKeepaliveEmitsPing(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(withAnthropicKeepalive(req.Context(), 30*time.Millisecond))
	body := &slowReader{
		delay: 80 * time.Millisecond,
		chunks: []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n",
			"data: [DONE]\n\n",
		},
	}
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "event: ping") && !strings.Contains(out, ": keepalive") {
		t.Fatalf("expected keepalive/ping in %q", out)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("missing content/terminal in %q", out)
	}
}

func TestStreamAnthropicSoftDisconnectStillClosesEnvelope(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	// Deliver content first, then cancel before [DONE] so finish path sees client_gone.
	body := &cancelAfterChunk{
		chunks: []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n",
			"data: [DONE]\n\n",
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	for _, marker := range []string{"event: message_start", "hi", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in soft-disconnect body %q", marker, out)
		}
	}
}

func TestStreamAnthropicToolUseAtomicOnSoftDisconnect(t *testing.T) {
	// tool_use start/delta/stop must land as a complete group even when the client
	// soft-disconnects right after the upstream tool chunk (Claude Code "Tool use interrupted").
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	body := &cancelAfterChunk{
		chunks: []string{
			toolChunk,
			"data: [DONE]\n\n",
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_tool", "grok", true, []string{"Edit"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	for _, marker := range []string{
		"event: message_start",
		`"type":"tool_use"`,
		`"name":"Edit"`,
		"content_block_delta",
		"content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in soft-disconnect tool body %q", marker, out)
		}
	}
	// start must not be the sole tool frame before stop (half-open tool_use).
	startIdx := strings.Index(out, `"type":"tool_use"`)
	stopIdx := strings.Index(out, "content_block_stop")
	if startIdx < 0 || stopIdx < 0 || stopIdx < startIdx {
		t.Fatalf("tool_use start/stop order broken in %q", out)
	}
}

func TestStreamAnthropicSoftWriteRequeuesToolUse(t *testing.T) {
	// Soft write failure after tool frames are produced must requeue unacked tools
	// so Finish re-emits a complete start+delta+stop group (not half-open tool_use).
	// Uses a ResponseWriter that fails the first tool_use Write, then succeeds.
	// No ctx cancel — pure soft write pressure (Claude Code intermittent blip).
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	body := strings.NewReader(toolChunk + "data: [DONE]\n\n")
	// softFailRecorder fails first tool_use Write once, then works.
	rec := &softFailRecorder{ResponseRecorder: httptest.NewRecorder(), failWrites: 1}
	_, _, err := streamAnthropicMessages(rec, req, body, "msg_soft_tool", "grok", true, []string{"Edit"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, marker := range []string{
		"event: message_start",
		`"type":"tool_use"`,
		`"name":"Edit"`,
		"content_block_delta",
		"content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in soft-write recovery body (len=%d):\n%s", marker, len(out), out)
		}
	}
	// start must precede stop for tool_use.
	startIdx := strings.Index(out, `"type":"tool_use"`)
	stopIdx := strings.Index(out, "content_block_stop")
	if startIdx < 0 || stopIdx < 0 || stopIdx < startIdx {
		t.Fatalf("tool_use start/stop order broken in %q", out)
	}
	// Only one message_stop (no duplicate terminal after requeue recovery).
	if n := strings.Count(out, "event: message_stop"); n != 1 {
		t.Fatalf("want 1 message_stop, got %d", n)
	}
	// Tool group must appear (re-emitted after soft fail); failWrites exhausted.
	if rec.failWrites != 0 {
		t.Fatalf("expected softFailRecorder to have consumed failWrites, left=%d writes=%d", rec.failWrites, rec.writes)
	}
}

// softFailRecorder fails the first N Write calls that contain tool_use with a soft
// client error, then delegates to ResponseRecorder. Models intermittent write
// pressure under Claude Code without killing the early message_start envelope.
// When shortWrite is true, returns n=len/2 with nil error once (short write path).
// When failAny is true, fails any Write (including message_stop) N times.
type softFailRecorder struct {
	*httptest.ResponseRecorder
	failWrites int
	writes     int
	shortWrite bool
	failAny    bool
	failMatch  string // if non-empty, only fail Writes containing this substring
}

func (s *softFailRecorder) Write(p []byte) (int, error) {
	s.writes++
	body := string(p)
	shouldFail := false
	if s.failWrites > 0 {
		if s.failAny {
			shouldFail = true
		} else if s.failMatch != "" && strings.Contains(body, s.failMatch) {
			shouldFail = true
		} else if s.failMatch == "" && strings.Contains(body, `"type":"tool_use"`) {
			// Default: fail tool_use batches only (not message_start / keepalive).
			shouldFail = true
		}
	}
	if shouldFail {
		s.failWrites--
		if s.shortWrite {
			// Short write: accept prefix, no err — server must treat as soft fail.
			half := len(p) / 2
			if half < 1 {
				half = 0
			}
			if half > 0 {
				_, _ = s.ResponseRecorder.Write(p[:half])
			}
			return half, nil
		}
		return 0, errors.New("write: connection reset by peer")
	}
	return s.ResponseRecorder.Write(p)
}

func (s *softFailRecorder) Flush() {
	if s.ResponseRecorder != nil {
		// httptest.ResponseRecorder has no Flush; no-op is fine for tests.
	}
}

// softFailNthToolRecorder fails the Nth Write that contains tool_use (1-based),
// then succeeds. Models multi-tool Finish soft-fail of one tool group.
// softFailNthToolRecorder fails the Nth Write that contains tool_use (1-based), then succeeds.
// Used to model multi-tool Finish where only one tool group soft-fails.
type softFailNthToolRecorder struct {
	*httptest.ResponseRecorder
	failNth    int
	toolWrites int
	writes     int
}

func (s *softFailNthToolRecorder) Write(p []byte) (int, error) {
	s.writes++
	if strings.Contains(string(p), `"type":"tool_use"`) {
		s.toolWrites++
		if s.failNth > 0 && s.toolWrites == s.failNth {
			return 0, errors.New("write: connection reset by peer")
		}
	}
	return s.ResponseRecorder.Write(p)
}

func (s *softFailNthToolRecorder) Flush() {}

// shortWriteToolRecorder returns a short write (n<len, err=nil) once on tool_use.
// writeFrames must treat that as soft fail and not Ack the half-open group.
type shortWriteToolRecorder struct {
	*httptest.ResponseRecorder
	shorted bool
	writes  int
}

func (s *shortWriteToolRecorder) Write(p []byte) (int, error) {
	s.writes++
	if !s.shorted && strings.Contains(string(p), `"type":"tool_use"`) {
		s.shorted = true
		half := len(p) / 2
		if half < 1 {
			half = 1
		}
		// Real short-write semantics: first half is accepted by the peer.
		// (Returning n without Write left the body missing content_block_start
		// while the resumable path continued with only the tail.)
		n, err := s.ResponseRecorder.Write(p[:half])
		if err != nil {
			return n, err
		}
		return half, nil
	}
	return s.ResponseRecorder.Write(p)
}

func (s *shortWriteToolRecorder) Flush() {}

// softFailMessageStopRecorder fails the first Write containing message_stop.
type softFailMessageStopRecorder struct {
	*httptest.ResponseRecorder
	failLeft int
}

func (s *softFailMessageStopRecorder) Write(p []byte) (int, error) {
	if s.failLeft > 0 && (strings.Contains(string(p), "message_stop") || strings.Contains(string(p), `"type":"message_stop"`)) {
		s.failLeft--
		return 0, errors.New("short write: connection reset by peer")
	}
	return s.ResponseRecorder.Write(p)
}

func (s *softFailMessageStopRecorder) Flush() {}

func TestStreamAnthropicSoftWriteMultiToolPartial(t *testing.T) {
	// Two tools: fail the first tool_use Write, succeed the second + terminal.
	// Must still deliver both tools (re-emit failed) and exactly one message_stop.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/a\\\",\\\"old_string\\\":\\\"x\\\",\\\"new_string\\\":\\\"y\\\"}\"}}," +
		"{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"file_path\\\":\\\"/b\\\"}\"}}" +
		"]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	body := strings.NewReader(toolChunk + "data: [DONE]\n\n")
	rec := &softFailNthToolRecorder{ResponseRecorder: httptest.NewRecorder(), failNth: 1}
	_, _, err := streamAnthropicMessages(rec, req, body, "msg_mt", "grok", true, []string{"Edit", "Read"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, marker := range []string{`"type":"tool_use"`, "Edit", "Read", "message_delta", "message_stop"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in multi-tool soft recovery (len=%d):\n%s", marker, len(out), out)
		}
	}
	if n := strings.Count(out, "event: message_stop"); n != 1 {
		t.Fatalf("want 1 message_stop, got %d", n)
	}
	if !strings.Contains(out, "call_a") || !strings.Contains(out, "call_b") {
		t.Fatalf("both tools must land after recovery:\n%s", out)
	}
}

func TestStreamAnthropicShortWriteToolRequeues(t *testing.T) {
	// Short write (n<len, err=nil) on first tool_use must not Ack half-open group.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	body := strings.NewReader(toolChunk + "data: [DONE]\n\n")
	rec := &shortWriteToolRecorder{ResponseRecorder: httptest.NewRecorder()}
	_, _, err := streamAnthropicMessages(rec, req, body, "msg_short", "grok", true, []string{"Edit"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, marker := range []string{`"type":"tool_use"`, "Edit", "content_block_stop", "message_stop"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q after short-write recovery:\n%s", marker, out)
		}
	}
	if n := strings.Count(out, "event: message_stop"); n != 1 {
		t.Fatalf("want 1 message_stop, got %d", n)
	}
	if !rec.shorted {
		t.Fatal("expected short write to fire")
	}
}

func TestStreamAnthropicTerminalSoftWriteRetry(t *testing.T) {
	// Fail first Write that contains message_stop; recovery must re-emit terminal once.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	body := strings.NewReader(toolChunk + "data: [DONE]\n\n")
	rec := &softFailMessageStopRecorder{ResponseRecorder: httptest.NewRecorder(), failLeft: 1}
	_, _, err := streamAnthropicMessages(rec, req, body, "msg_term_soft", "grok", true, []string{"Edit"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, marker := range []string{`"type":"tool_use"`, "message_stop", "content_block_stop"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q after terminal soft recovery:\n%s", marker, out)
		}
	}
	if n := strings.Count(out, "event: message_stop"); n != 1 {
		t.Fatalf("want 1 message_stop after recovery, got %d\n%s", n, out)
	}
}

func TestStreamChatCompletionsForceFinishOnSoftDisconnect(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	// Complete tool args but no finish_reason before cancel (soft disconnect mid-stream).
	toolChunk := "data: {\"id\":\"chatcmpl_t\",\"model\":\"grok\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]},\"finish_reason\":null}]}\n\n"
	body := &cancelAfterChunk{
		chunks: []string{
			toolChunk,
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	stats, err := streamChatCompletions(recorder, req, body, 50*time.Millisecond)
	_ = stats
	if err != nil {
		t.Fatalf("streamChatCompletions err=%v body=%q", err, recorder.Body.String())
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "tool_calls") {
		t.Fatalf("missing tool_calls in %q", out)
	}
	if !strings.Contains(out, "finish_reason") {
		t.Fatalf("missing finish_reason in %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("missing [DONE] in %q", out)
	}
	if strings.Count(out, "data: [DONE]") != 1 {
		t.Fatalf("want single [DONE], got %d in %q", strings.Count(out, "data: [DONE]"), out)
	}
}

func TestStreamAnthropicMidStreamUpstreamErrorSoftCloses(t *testing.T) {
	// Regression: upstream read error AFTER model text/tools must soft-finish
	// (message_delta/stop) and must NOT emit event:error — that is what Claude Code
	// surfaces as "API Error: Server error mid-response / response may be incomplete".
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := &errAfterChunks{
		chunks: []string{
			`data: {"choices":[{"delta":{"content":"partial answer already streamed"}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"content":" continues."}}]}` + "\n\n",
		},
		err: io.ErrUnexpectedEOF,
	}
	usage, ttft, err := streamAnthropicMessages(recorder, req, body, "msg_mid", "grok-4.5", false, nil, 0)
	_ = usage
	if err != nil {
		t.Fatalf("expected soft-close nil err after payload, got %v", err)
	}
	if ttft <= 0 {
		t.Fatalf("expected TTFT > 0 after model content, got %d", ttft)
	}
	out := recorder.Body.String()
	if strings.Contains(out, "event: error") {
		t.Fatalf("must not emit event:error mid-response after payload:\n%s", out)
	}
	for _, marker := range []string{"partial answer", "message_delta", "message_stop"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in soft mid-stream close body (len=%d)", marker, len(out))
		}
	}
}

func TestStreamChatCompletionsMidStreamUpstreamErrorSoftCloses(t *testing.T) {
	// OpenAI chat: upstream drop after content/tools should finish_reason+[DONE],
	// not a trailing error JSON (Claude Code / relays treat that as mid-response).
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := &errAfterChunks{
		chunks: []string{
			`data: {"id":"c1","model":"grok","choices":[{"index":0,"delta":{"content":"hello mid"},"finish_reason":null}]}` + "\n\n",
		},
		err: io.ErrUnexpectedEOF,
	}
	stats, err := streamChatCompletions(recorder, req, body, 50*time.Millisecond)
	_ = stats
	if err != nil {
		t.Fatalf("expected soft-close nil err after payload, got %v body=%q", err, recorder.Body.String())
	}
	out := recorder.Body.String()
	if strings.Contains(out, `"type":"server_error"`) || strings.Contains(out, `"type": "server_error"`) {
		t.Fatalf("must not emit error JSON after payload:\n%s", out)
	}
	if !strings.Contains(out, "finish_reason") {
		t.Fatalf("missing finish_reason in %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("missing [DONE] in %q", out)
	}
}

func TestStreamChatCompletionsHollowSSENotSoftSuccess(t *testing.T) {
	// Regression: role-only / empty-choice SSE must NOT count as payload. Upstream
	// drop after hollow frames should surface as error, not soft-success empty.
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := &errAfterChunks{
		chunks: []string{
			`data: {"id":"c0","model":"grok","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"c0","model":"grok","choices":[{"index":0,"delta":{},"finish_reason":null}]}` + "\n\n",
		},
		err: io.ErrUnexpectedEOF,
	}
	stats, err := streamChatCompletions(recorder, req, body, 50*time.Millisecond)
	_ = stats
	if err == nil {
		t.Fatalf("expected error after hollow-only stream, got nil body=%q", recorder.Body.String())
	}
	// Headers stay uncommitted on pure hollow so serveChatCompletions multi-open can
	// rotate accounts. Error text must still classify as empty model output.
	if !strings.Contains(err.Error(), "empty model output") && !strings.Contains(err.Error(), "no content/tool_calls") {
		t.Fatalf("expected empty model output error, got %v body=%q", err, recorder.Body.String())
	}
	if stats.FirstTokenMS > 0 {
		t.Fatalf("hollow SSE must not set TTFT, got %d", stats.FirstTokenMS)
	}
}

func TestStreamOpenAIResponsesMidStreamUpstreamErrorSoftCloses(t *testing.T) {
	// Responses: upstream drop after content must Complete (not response.failed mid-turn).
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := &errAfterChunks{
		chunks: []string{
			`data: {"choices":[{"delta":{"content":"partial responses text"}}]}` + "\n\n",
		},
		err: io.ErrUnexpectedEOF,
	}
	usage, ttft, err := streamOpenAIResponses(recorder, req, body, "resp_mid", "grok-4.5", nil, 50*time.Millisecond, 0)
	_ = usage
	if err != nil {
		t.Fatalf("expected soft-close nil err after payload, got %v", err)
	}
	if ttft <= 0 {
		t.Fatalf("expected TTFT > 0 after content, got %d", ttft)
	}
	out := recorder.Body.String()
	if strings.Contains(out, "response.failed") {
		t.Fatalf("must not emit response.failed mid-response after payload:\n%s", out)
	}
	for _, marker := range []string{"partial responses text", "response.completed", "data: [DONE]"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in soft mid-stream responses body (len=%d)", marker, len(out))
		}
	}
}

func TestStreamAnthropicToolRequeueOnSoftWrite(t *testing.T) {
	// Regression: soft write failure after tool frames are produced must requeue
	// unacked tools so Finish re-emits a complete start+delta+stop group.
	// Without Ack/Requeue, Claude Code sees half-open tool_use → "Tool use interrupted".
	a := anthropic.NewStreamAssembler("msg_rq", "grok", true, 0, []string{"Edit"})
	_ = a.Start(0)
	frames := a.Feed("", "", []anthropic.ToolDelta{{
		Index: 0, ID: "call_1", Name: "Edit",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":"b"}`,
	}})
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, `"type":"tool_use"`) {
		t.Fatalf("expected tool_use frames before soft fail:\n%s", joined)
	}
	if !a.HasUnackedTools() {
		t.Fatal("tools must be unacked until AckEmittedTools")
	}
	// Soft write "failed": requeue instead of Ack.
	a.RequeueUnackedTools()
	if a.HasUnackedTools() {
		t.Fatal("after Requeue, pending acks must clear")
	}
	// Finish must re-emit complete tool group + terminal.
	fin := a.Finish("tool_calls", anthropic.Usage{})
	out := strings.Join(fin, "")
	for _, marker := range []string{`"type":"tool_use"`, "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q after requeue Finish:\n%s", marker, out)
		}
	}
	// Ack after successful write.
	if a.HasUnackedTools() {
		a.AckEmittedTools()
	}
	if a.HasUnackedTools() {
		t.Fatal("expected no unacked tools after Ack")
	}
}

type errAfterChunks struct {
	chunks []string
	idx    int
	buf    []byte
	err    error
}

func (e *errAfterChunks) Read(p []byte) (int, error) {
	if len(e.buf) == 0 {
		if e.idx >= len(e.chunks) {
			if e.err != nil {
				return 0, e.err
			}
			return 0, io.EOF
		}
		e.buf = []byte(e.chunks[e.idx])
		e.idx++
	}
	n := copy(p, e.buf)
	e.buf = e.buf[n:]
	return n, nil
}

type cancelAfterChunk struct {
	chunks      []string
	idx         int
	cancelAfter int
	cancel      context.CancelFunc
}

func (c *cancelAfterChunk) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.idx])
	c.idx++
	if c.idx == c.cancelAfter && c.cancel != nil {
		c.cancel()
	}
	return n, nil
}

type cancelAfterRead struct {
	io.Reader
	cancel context.CancelFunc
	after  int
	n      int
}

func (c *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n++
	if c.n >= c.after && c.cancel != nil {
		c.cancel()
	}
	return n, err
}

func TestStreamAnthropicSilentUpstreamStillKeepalives(t *testing.T) {
	// Upstream emits continuous incomplete tool deltas (no client frames).
	// Server must still write keepalive pings so ~60s idle cutoffs cannot fire.
	// After force-finish drops incomplete tools, must not soft-succeed (ok=true tokens=0).
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(withAnthropicKeepalive(req.Context(), 20*time.Millisecond))

	// Chunked incomplete tool stream with delays so idle timer fires.
	body := &slowChunks{
		chunks: []string{
			// message with tools requested path is controlled by toolsRequested arg
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ls"}}]}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
			"data: [DONE]\n\n",
		},
		delay: 30 * time.Millisecond,
	}
	// toolsRequested=true so text would be held; incomplete Bash should not emit tool block mid-stream.
	// Force-finish drops incomplete tools → must return empty error (not soft-ok).
	_, _, err := streamAnthropicMessagesWithOptions(recorder, req, body, "msg_k", "grok", true, []string{"Bash"}, 1, anthropicStreamOptions{Keepalive: 20 * time.Millisecond})
	out := recorder.Body.String()
	if !strings.Contains(out, "event: ping") && !strings.Contains(out, ": keepalive") {
		// Keepalive may race if total < idle; accept terminal/error frames as stream activity.
		if !strings.Contains(out, "event: message_stop") && !strings.Contains(out, "event: error") && !strings.Contains(out, "event: message_start") {
			t.Fatalf("expected keepalive or terminal, got %q", out)
		}
	}
	// Incomplete-only tool stream must NOT soft-succeed (admin ok=true tokens=0 leak).
	if err == nil {
		t.Fatalf("incomplete-only tool stream must not soft-succeed, body=%q", out)
	}
	if !strings.Contains(err.Error(), "empty model output") && !strings.Contains(err.Error(), "no content/tool_calls") {
		t.Fatalf("expected empty-output error, got %v body=%q", err, out)
	}
}

type slowChunks struct {
	chunks []string
	idx    int
	delay  time.Duration
	buf    []byte
}

func (s *slowChunks) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		if s.idx >= len(s.chunks) {
			return 0, io.EOF
		}
		if s.delay > 0 && s.idx > 0 {
			time.Sleep(s.delay)
		}
		s.buf = []byte(s.chunks[s.idx])
		s.idx++
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func TestMessagesCountTokensWithoutStore(t *testing.T) {
	body := `{"system":"abcd","messages":[{"role":"user","content":"hello"}]}`
	recorder := httptest.NewRecorder()
	NewMux(Options{
		Ready:           func() bool { return true },
		MessagesEnabled: true,
		// no Store — count_tokens is a pure local heuristic
	}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("count_tokens without store = %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestStreamAnthropicLongThinkingMultiToolCloses(t *testing.T) {
	// Claude Code shape: toolsRequested=true, long reasoning stream, then multi-tool
	// (one incomplete early index + complete later indexes). Expect:
	//  - live thinking_delta (SSE keepalive)
	//  - complete tools emitted (not blocked by incomplete index 0)
	//  - always message_stop so task leaves "running"
	var chunks []string
	for i := 0; i < 30; i++ {
		chunks = append(chunks, `data: {"choices":[{"delta":{"reasoning_content":"think-"}}]}`+"\n\n")
	}
	// incomplete Bash@0, complete Read@1, complete Edit@2
	chunks = append(chunks,
		`data: {"choices":[{"delta":{"tool_calls":[`+
			`{"index":0,"id":"t0","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}} ,`+
			`{"index":1,"id":"t1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/a.go\"}"}} ,`+
			`{"index":2,"id":"t2","type":"function","function":{"name":"Edit","arguments":"{\"file_path\":\"/b.go\",\"old_string\":\"x\",\"new_string\":\"y\"}"}}`+
			`]},"finish_reason":"tool_calls"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	body := strings.NewReader(strings.Join(chunks, ""))
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_cc", "grok-4.5", true, []string{"Bash", "Read", "Edit"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	for _, marker := range []string{
		"thinking_delta",
		"think-",
		`"name":"Read"`,
		"message_delta",
		"message_stop",
	} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in long-thinking multi-tool stream (len=%d)", marker, len(out))
		}
	}
	// Incomplete Bash must not block Read; and should not appear as tool_use.
	if strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("incomplete Bash should not emit tool_use")
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected tool_use stop_reason, body head=%q", out[:min(400, len(out))])
	}
}

func TestStreamResponsesLongReasoningMultiToolCloses(t *testing.T) {
	var chunks []string
	for i := 0; i < 20; i++ {
		chunks = append(chunks, `data: {"choices":[{"delta":{"reasoning_content":"plan-"}}]}`+"\n\n")
	}
	chunks = append(chunks,
		`data: {"choices":[{"delta":{"tool_calls":[`+
			`{"index":0,"id":"c0","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}} ,`+
			`{"index":1,"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/a.go\"}"}}`+
			`]},"finish_reason":"tool_calls"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, _, err := streamOpenAIResponses(recorder, req, strings.NewReader(strings.Join(chunks, "")), "resp_cc", "grok-4.5", []string{"Bash", "Read"}, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	for _, marker := range []string{
		"reasoning_summary_text.delta",
		"plan-",
		"function_call",
		"Read",
		"response.completed",
		"data: [DONE]",
	} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in responses long-reasoning multi-tool (len=%d)", marker, len(out))
		}
	}
	if strings.Contains(out, `"name":"Bash"`) || (strings.Contains(out, "Bash") && strings.Contains(out, "function_call") && strings.Count(out, "Bash") > 0 && strings.Contains(out, `"call_id":"c0"`)) {
		// Soft check: incomplete Bash at c0 must not be emitted.
		if strings.Contains(out, `"call_id":"c0"`) {
			t.Fatalf("incomplete Bash c0 should not emit function_call")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestClaudeCodeLongThinkingMultiToolEventTrace(t *testing.T) {
	// End-to-end SSE shape for Claude Code:
	// 1) early message_start
	// 2) live thinking_delta x N (keepalive)
	// 3) multi-tool with incomplete early index + complete later indexes
	// 4) message_delta + message_stop (task can leave "running")
	var upstream strings.Builder
	for i := 0; i < 25; i++ {
		upstream.WriteString(`data: {"choices":[{"delta":{"reasoning_content":"think-"}}]}` + "\n\n")
	}
	// incomplete Bash@0 should not block Read@1 / Edit@2
	upstream.WriteString(`data: {"choices":[{"delta":{"tool_calls":[`)
	upstream.WriteString(`{"index":0,"id":"t0","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}},`)
	upstream.WriteString(`{"index":1,"id":"t1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/a.go\"}"}},`)
	upstream.WriteString(`{"index":2,"id":"t2","type":"function","function":{"name":"Edit","arguments":"{\"file_path\":\"/tmp/b.go\",\"old_string\":\"x\",\"new_string\":\"y\"}"}}`)
	upstream.WriteString(`]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}` + "\n\n")
	upstream.WriteString("data: [DONE]\n\n")

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// toolsRequested=true, maxTools=2 (Claude-compatible default path)
	usage, firstTokenMS, err := streamAnthropicMessages(
		recorder, req, strings.NewReader(upstream.String()),
		"msg_claude_code", "grok-4.5", true,
		[]string{"Bash", "Read", "Edit"}, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()

	// Parse event types in order.
	var types []string
	thinkingN, toolStarts := 0, 0
	toolNames := []string{}
	for _, block := range strings.Split(out, "\n\n") {
		var ev string
		var data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			}
		}
		if ev == "" && data == "" {
			continue
		}
		if ev != "" {
			types = append(types, ev)
		}
		if strings.Contains(data, `"type":"thinking_delta"`) || strings.Contains(data, `"thinking_delta"`) {
			thinkingN++
		}
		if strings.Contains(data, `"type":"tool_use"`) {
			toolStarts++
			// rough name extract
			for _, name := range []string{"Bash", "Read", "Edit"} {
				if strings.Contains(data, `"name":"`+name+`"`) {
					toolNames = append(toolNames, name)
				}
			}
		}
	}

	t.Logf("firstTokenMS=%d usage=%v thinking_deltas~%d tool_starts=%d tools=%v events=%d",
		firstTokenMS, usage, thinkingN, toolStarts, toolNames, len(types))
	t.Logf("event head: %v", types[:min(12, len(types))])
	t.Logf("event tail: %v", types[max(0, len(types)-8):])

	// Early envelope
	if len(types) == 0 {
		t.Fatalf("expected stream events, got none")
	}
	// message_start may be deferred until first model payload (empty-stream fix).
	hasStart := false
	for _, ty := range types {
		if ty == "message_start" {
			hasStart = true
			break
		}
	}
	if !hasStart {
		t.Fatalf("expected message_start somewhere, got head=%v", types[:min(8, len(types))])
	}
	// Live thinking
	if thinkingN < 10 {
		t.Fatalf("expected live thinking_delta stream, got ~%d", thinkingN)
	}
	// Read must appear; incomplete Bash must not
	hasRead, hasBash, hasEdit := false, false, false
	for _, n := range toolNames {
		switch n {
		case "Read":
			hasRead = true
		case "Bash":
			hasBash = true
		case "Edit":
			hasEdit = true
		}
	}
	if !hasRead {
		t.Fatalf("Read tool missing (blocked by incomplete Bash?); tools=%v", toolNames)
	}
	if hasBash {
		t.Fatalf("incomplete Bash must not emit; tools=%v", toolNames)
	}
	// maxTools=2: Read + Edit both complete and should fit
	if !hasEdit {
		// with maxTools=2 and Bash skipped, Edit should still emit
		t.Fatalf("Edit tool missing; tools=%v", toolNames)
	}
	// Terminal
	if !strings.Contains(out, "event: message_delta") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("missing terminal frames")
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("expected stop_reason=tool_use")
	}
	// Envelope opened before model tokens: firstTokenMS may be tiny but stream must be OK.
	_ = firstTokenMS
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestAllowedAnthropicToolNamesTopLevelAndFunction(t *testing.T) {
	// Anthropic Messages tools use top-level name (Claude Code).
	names := allowedAnthropicToolNames(map[string]any{
		"tools": []any{
			map[string]any{"name": "Edit", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "Read", "description": "read"},
			map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
		},
	})
	if len(names) != 3 {
		t.Fatalf("names=%v", names)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"Edit", "Read", "Bash"} {
		if !got[want] {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
	// Empty / nil
	if out := allowedAnthropicToolNames(nil); len(out) != 0 {
		t.Fatalf("nil body: %v", out)
	}
	if out := allowedAnthropicToolNames(map[string]any{}); len(out) != 0 {
		t.Fatalf("empty: %v", out)
	}
}

func TestStreamOpenAIResponsesSoftWriteRequeuesTool(t *testing.T) {
	// Soft write of a function_call group must re-emit full added+delta+done + completed.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"
	body := strings.NewReader(toolChunk + "data: [DONE]\n\n")
	rec := &softFailRecorder{ResponseRecorder: httptest.NewRecorder(), failWrites: 1, failMatch: "function_call"}
	_, _, err := streamOpenAIResponses(rec, req, body, "resp_soft", "grok", []string{"Edit"}, 50*time.Millisecond, 2)
	if err != nil {
		t.Fatalf("expected soft recovery nil err, got %v", err)
	}
	out := rec.Body.String()
	for _, marker := range []string{
		"function_call",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
		"[DONE]",
	} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q after Responses soft-write recovery:\n%s", marker, out)
		}
	}
	if n := strings.Count(out, "response.completed"); n < 1 {
		t.Fatalf("want completed terminal, got %d", n)
	}
}

func TestStreamChatCompletionsSoftWriteRequeuesTool(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	toolChunk := "data: {\"id\":\"chatcmpl_s\",\"model\":\"grok\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"b\\\"}\"}}]},\"finish_reason\":null}]}\n\n"
	body := strings.NewReader(toolChunk + "data: [DONE]\n\n")
	rec := &softFailRecorder{ResponseRecorder: httptest.NewRecorder(), failWrites: 1, failMatch: "tool_calls"}
	_, err := streamChatCompletions(rec, req, body, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("expected soft recovery nil err, got %v body=%q", err, rec.Body.String())
	}
	out := rec.Body.String()
	for _, marker := range []string{"tool_calls", "finish_reason", "[DONE]"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q after Chat soft-write recovery:\n%s", marker, out)
		}
	}
}

func TestStreamOpenAIResponsesHollowSSENotSoftSuccess(t *testing.T) {
	// Role-only / empty SSE must Fail and return error (usage ok=false).
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := &errAfterChunks{
		chunks: []string{
			`data: {"id":"r0","model":"grok","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"r0","model":"grok","choices":[{"index":0,"delta":{},"finish_reason":null}]}` + "\n\n",
		},
		err: io.ErrUnexpectedEOF,
	}
	usage, ttft, err := streamOpenAIResponses(recorder, req, body, "resp_hollow", "grok-4.5", nil, 50*time.Millisecond, 0)
	_ = usage
	if err == nil {
		t.Fatalf("expected error after hollow-only responses stream, got nil body=%q", recorder.Body.String())
	}
	if ttft > 0 {
		t.Fatalf("hollow responses SSE must not set TTFT, got %d", ttft)
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "response.failed") && !strings.Contains(out, "empty model output") {
		t.Fatalf("expected response.failed after hollow stream, body=%q", out)
	}
}

func TestStreamOpenAIResponsesIncompleteToolNotSoftSuccess(t *testing.T) {
	// Incomplete tool args that never emit a function_call must Fail (not soft-success).
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// Intentionally truncated JSON arguments — live holds, force-finish drops.
	body := strings.NewReader(
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Bash","arguments":"{\"cmd\""}}]},"finish_reason":null}]}` + "\n\n" +
			"data: [DONE]\n\n",
	)
	usage, ttft, err := streamOpenAIResponses(recorder, req, body, "resp_incomp", "grok-4.5", []string{"Bash"}, 50*time.Millisecond, 1)
	_ = usage
	_ = ttft
	if err == nil {
		t.Fatalf("expected empty/incomplete-tool error, got nil body=%q", recorder.Body.String())
	}
	out := recorder.Body.String()
	if strings.Contains(out, "response.completed") && !strings.Contains(out, "response.failed") && !strings.Contains(out, "function_call") {
		t.Fatalf("hollow completed without tools/failed is a soft-success leak: %q", out)
	}
}

func TestStreamOpenAIResponsesIncompleteToolNotSoftSuccessOnClientGone(t *testing.T) {
	// clientGone must not soft-succeed an incomplete-only tool stream.
	// Admin pattern: ok=true tokens=0 ttft>0 with Claude Code "Tool use interrupted".
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	body := &cancelAfterChunk{
		chunks: []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Bash","arguments":"{\"cmd\""}}]},"finish_reason":null}]}` + "\n\n",
			"data: [DONE]\n\n",
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	usage, ttft, err := streamOpenAIResponses(recorder, req, body, "resp_gone", "grok-4.5", []string{"Bash"}, 50*time.Millisecond, 1)
	_ = usage
	_ = ttft
	if err == nil {
		t.Fatalf("clientGone + incomplete tool must not soft-succeed, body=%q", recorder.Body.String())
	}
	if !strings.Contains(err.Error(), "empty model output") && !strings.Contains(err.Error(), "no content/tool_calls") {
		t.Fatalf("expected empty-output error, got %v body=%q", err, recorder.Body.String())
	}
}

func TestStreamAnthropicIncompleteToolNotSoftSuccessOnClientGone(t *testing.T) {
	// Same hollow-success leak on Anthropic path when Claude Code soft-disconnects.
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	body := &cancelAfterChunk{
		chunks: []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
			"data: [DONE]\n\n",
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_gone", "grok", true, []string{"Bash"}, 1)
	if err == nil {
		t.Fatalf("clientGone + incomplete tool must not soft-succeed, body=%q", recorder.Body.String())
	}
	if !strings.Contains(err.Error(), "empty model output") && !strings.Contains(err.Error(), "no content/tool_calls") {
		t.Fatalf("expected empty-output error, got %v body=%q", err, recorder.Body.String())
	}
}

func TestStreamOpenAIResponsesClientGoneHalfOpenToolFails(t *testing.T) {
	// Soft client cancel after incomplete tool args must NOT soft-succeed.
	// Admin previously logged ok=true tokens=0 TTFT>0 for this pattern
	// (Claude Code intermittent "Tool use interrupted").
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	body := &cancelAfterChunk{
		chunks: []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Bash","arguments":"{\"cmd\""}}]},"finish_reason":null}]}` + "\n\n",
			"data: [DONE]\n\n",
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	recorder := httptest.NewRecorder()
	_, ttft, err := streamOpenAIResponses(recorder, req, body, "resp_half", "grok-4.5", []string{"Bash"}, 50*time.Millisecond, 1)
	_ = ttft
	if err == nil {
		t.Fatalf("clientGone + incomplete tool must not soft-succeed, body=%q", recorder.Body.String())
	}
	if !strings.Contains(err.Error(), "empty model output") && !strings.Contains(err.Error(), "no content/tool_calls") {
		t.Fatalf("expected empty-output error, got %v body=%q", err, recorder.Body.String())
	}
}

func TestStreamAnthropicClientGoneIncompleteToolFails(t *testing.T) {
	// Soft client cancel after incomplete tool-only deltas must fail (not soft-ok).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	body := &cancelAfterChunk{
		chunks: []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}` + "\n\n",
			"data: [DONE]\n\n",
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	recorder := httptest.NewRecorder()
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_half", "grok", true, []string{"Bash"}, 1)
	if err == nil {
		t.Fatalf("clientGone + incomplete tool must not soft-succeed, body=%q", recorder.Body.String())
	}
	if !strings.Contains(err.Error(), "empty model output") && !strings.Contains(err.Error(), "no content/tool_calls") {
		t.Fatalf("expected empty-output error, got %v body=%q", err, recorder.Body.String())
	}
}

func TestResponsesSoftCloseWhenToolAckedTerminalMissing(t *testing.T) {
	// Regression: tool was fully written+Ack'd but response.completed soft-failed /
	// never Ack'd. Must soft-close (nil err) instead of Fail("empty model output")
	// which Claude Code reports as intermittent "Tool use interrupted".
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"file_path\\\":\\\"/a.go\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	body := strings.NewReader(sse)
	fw := &failOnCompletedWriter{ResponseRecorder: recorder}
	_, _, err := streamOpenAIResponses(fw, req, body, "resp_soft", "grok-4.5", []string{"Read"}, 20*time.Millisecond, 1)
	out := recorder.Body.String()
	if !strings.Contains(out, "function_call") {
		t.Fatalf("expected function_call in body, got %q", out)
	}
	if err != nil && strings.Contains(err.Error(), "empty model output") {
		t.Fatalf("must soft-close after acked tool (not empty fail): err=%v body=%q", err, out)
	}
	if strings.Contains(out, "empty model output") {
		t.Fatalf("must not Fail empty over delivered tool: body=%q", out)
	}
}

// failOnCompletedWriter soft-fails only response.completed / pure [DONE] writes.
type failOnCompletedWriter struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *failOnCompletedWriter) Flush() { f.flushed++ }

func (f *failOnCompletedWriter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.Contains(s, "response.completed") {
		return 0, errors.New("broken pipe")
	}
	// Terminal [DONE] only (not mixed with function_call payload).
	if strings.Contains(s, "[DONE]") && !strings.Contains(s, "function_call") && !strings.Contains(s, "output_text") && !strings.Contains(s, "reasoning") {
		return 0, errors.New("broken pipe")
	}
	return f.ResponseRecorder.Write(p)
}

// Soft-fail during text coalesce must not drop buffered text_delta frames.
// Regression: flushPendingSSE used to clear pendingSSE before LastOK, so a single
// connection-reset blip truncated assistant text mid-turn (Claude Code incomplete output).
func TestAnthropicTextCoalesceSurvivesSoftWriteBlip(t *testing.T) {
	soft := &softFailRecorder{ResponseRecorder: httptest.NewRecorder(), failWrites: 1, failAny: true}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	var b strings.Builder
	// First token lands immediately (TTFT); subsequent tiny deltas coalesce.
	b.WriteString(`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n")
	for i := 0; i < 20; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"content":" world"}}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":10,"total_tokens":11}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	_, _, err := streamAnthropicMessages(soft, req, strings.NewReader(b.String()), "msg_text_soft", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := soft.Body.String()
	if !strings.Contains(out, "Hello") {
		t.Fatalf("missing first text, body=%q", out[:min(400, len(out))])
	}
	// Coalesced " world" repeats must survive the soft blip (not permanently dropped).
	if n := strings.Count(out, " world"); n < 10 {
		t.Fatalf("coalesced text lost after soft write: world_count=%d bodyLen=%d softGone_writes=%d", n, len(out), soft.writes)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("missing message_stop after soft text blip, body=%q", out[:min(300, len(out))])
	}
}

// Responses path: soft-fail mid-coalesce must still deliver full output_text.
func TestResponsesTextCoalesceSurvivesSoftWriteBlip(t *testing.T) {
	soft := &softFailRecorder{ResponseRecorder: httptest.NewRecorder(), failWrites: 1, failAny: true}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	var b strings.Builder
	b.WriteString(`data: {"choices":[{"delta":{"content":"Alpha"}}]}` + "\n\n")
	for i := 0; i < 20; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"content":"Beta"}}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":10,"total_tokens":11}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	_, _, err := streamOpenAIResponses(soft, req, strings.NewReader(b.String()), "resp_text_soft", "grok", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := soft.Body.String()
	if !strings.Contains(out, "Alpha") {
		t.Fatalf("missing first text, body=%q", out[:min(400, len(out))])
	}
	if n := strings.Count(out, "Beta"); n < 10 {
		t.Fatalf("coalesced text lost after soft write: beta_count=%d bodyLen=%d", n, len(out))
	}
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("missing response.completed, body=%q", out[:min(300, len(out))])
	}
}

// Soft short-write mid text coalesce must not leave the client with truncated
// assistant text. The unsent tail stays buffered and is force-drained before
// message_stop (Finish does not re-emit lost text_delta frames).
func TestStreamAnthropicTextSoftShortWriteNotTruncated(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	var b strings.Builder
	// First delta flushes immediately (TTFT); subsequent ones coalesce.
	b.WriteString(`data: {"choices":[{"delta":{"content":"START-"}}]}` + "\n\n")
	for i := 0; i < 40; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"content":"chunk"}}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{"content":"-END"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":10,"total_tokens":11}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")

	rec := &softFailRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		failWrites:       1,
		failMatch:        "text_delta",
		shortWrite:       true,
	}
	_, _, err := streamAnthropicMessages(rec, req, strings.NewReader(b.String()), "msg_text_soft_sw", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "START-") {
		t.Fatalf("missing START- in body (len=%d): %s", len(out), out[:min(500, len(out))])
	}
	if !strings.Contains(out, "-END") {
		t.Fatalf("missing -END (truncated text on soft short-write), body tail: %s", out[max(0, len(out)-800):])
	}
	if n := strings.Count(out, "chunk"); n < 30 {
		t.Fatalf("expected ~40 chunk deltas, got raw chunk count=%d (truncated?)", n)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("missing message_stop after soft short-write recovery")
	}
}

// Chat completions path: soft short-write of coalesced pure text must re-buffer
// the unsent tail and drain it before [DONE] (not drop mid-turn content).
func TestStreamChatTextSoftShortWriteNotTruncated(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	var b strings.Builder
	b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"HELLO-"}}]}` + "\n\n")
	for i := 0; i < 30; i++ {
		b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"xx"}}]}` + "\n\n")
	}
	b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"-BYE"},"finish_reason":"stop"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")

	rec := &softFailRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		failWrites:       1,
		failMatch:        `"content":"xx"`,
		shortWrite:       true,
	}
	_, err := streamChatCompletions(rec, req, strings.NewReader(b.String()), 0)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "HELLO-") {
		t.Fatalf("missing HELLO-: %s", out[:min(400, len(out))])
	}
	if !strings.Contains(out, "-BYE") {
		t.Fatalf("missing -BYE after soft short-write (truncated): tail=%s", out[max(0, len(out)-500):])
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatal("missing [DONE]")
	}
}

// Responses path: soft short-write of coalesced text must not drop mid-turn output.
func TestStreamOpenAIResponsesTextSoftShortWriteNotTruncated(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	var b strings.Builder
	b.WriteString(`data: {"choices":[{"delta":{"content":"ALPHA-"}}]}` + "\n\n")
	for i := 0; i < 30; i++ {
		b.WriteString(`data: {"choices":[{"delta":{"content":"yy"}}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[{"delta":{"content":"-OMEGA"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":8,"total_tokens":10}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")

	rec := &softFailRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		failWrites:       1,
		failMatch:        "output_text",
		shortWrite:       true,
	}
	_, _, err := streamOpenAIResponses(rec, req, strings.NewReader(b.String()), "resp_text_soft_sw", "grok", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "ALPHA-") {
		t.Fatalf("missing ALPHA-: %s", out[:min(400, len(out))])
	}
	if !strings.Contains(out, "-OMEGA") {
		t.Fatalf("missing -OMEGA (truncated text): tail=%s", out[max(0, len(out)-600):])
	}
	if !strings.Contains(out, "response.completed") {
		t.Fatal("missing response.completed")
	}
}


func TestEnsureOpenAIChatPromptCacheKeyStable(t *testing.T) {
	// Turn 1: no explicit pck → mint from first user seed.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	chat1 := proxy.ChatRequest{Model: "grok", Raw: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "plan a trip to tokyo"}},
	}}
	pck1 := ensureOpenAIChatPromptCacheKey(req1, &chat1)
	if pck1 == "" {
		t.Fatal("expected minted pck")
	}
	if chat1.Raw["prompt_cache_key"] != pck1 {
		t.Fatalf("raw not updated: %#v", chat1.Raw["prompt_cache_key"])
	}
	// Turn 2: growing history, same seed → same pck.
	chat2 := proxy.ChatRequest{Model: "grok", Raw: map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "plan a trip to tokyo"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": "add food"},
		},
	}}
	pck2 := ensureOpenAIChatPromptCacheKey(req1, &chat2)
	if pck1 != pck2 {
		t.Fatalf("pck changed across turns: %q vs %q", pck1, pck2)
	}
	// Explicit header wins.
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req3.Header.Set("X-Prompt-Cache-Key", "client-stable-key")
	chat3 := proxy.ChatRequest{Model: "grok", Raw: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}}
	if got := ensureOpenAIChatPromptCacheKey(req3, &chat3); got != "client-stable-key" {
		t.Fatalf("header pck=%q", got)
	}
}
