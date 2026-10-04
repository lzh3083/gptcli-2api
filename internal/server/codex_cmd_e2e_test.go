package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/auth"
	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/server"
)

func TestCodexExecCommandProjectsCmdLive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			`data: {"id":"chatcmpl_x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"command\":"}}]}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	h := server.NewMux(server.Options{
		Ready:            func() bool { return true },
		ResponsesEnabled: true,
		ChatEnabled:      true,
		APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
		Candidates:       []pool.Candidate{{ID: "acc", Token: "tok", Enabled: true}},
		Config: config.Config{
			UpstreamBase: upstream.URL + "/v1",
			DefaultModel: "grok-4.5",
			SSEKeepalive: 2 * time.Second,
		},
	})

	// Codex-like request: tools schema prefers cmd
	body := `{
		"model":"grok-4.5",
		"stream":true,
		"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}],
		"input":[{"role":"user","content":"run pwd"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "codex-cli/0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	// Must project to cmd for Codex.
	if !strings.Contains(out, `"cmd"`) && !strings.Contains(out, `\"cmd\"`) {
		t.Fatalf("expected cmd in stream, body=%s", out)
	}
	// Reject command-only completed tool args.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "function_call_arguments.done") && !strings.Contains(line, `"status":"completed"`) {
			continue
		}
		if strings.Contains(line, `"command"`) && !strings.Contains(line, `"cmd"`) {
			t.Fatalf("command leaked without cmd: %s", line)
		}
	}
	// Soft assert: completed args should include cmd
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "function_call_arguments.done") {
			// data line JSON
			idx := strings.Index(line, "{")
			if idx < 0 {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line[idx:]), &obj) != nil {
				continue
			}
			args := fmtSprint(obj["arguments"])
			if strings.Contains(args, "cmd") {
				found = true
			}
		}
	}
	if !found {
		// print for debug
		t.Fatalf("no function_call_arguments.done with cmd; body=\n%s", out)
	}
}

func fmtSprint(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCodexChatCompletionsProjectsCmd(t *testing.T) {
	// Non-stream client request; upstream is always SSE and collector aggregates.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			`data: {"id":"chatcmpl_x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"command\":"}}]}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	h := server.NewMux(server.Options{
		Ready:            func() bool { return true },
		ResponsesEnabled: true,
		ChatEnabled:      true,
		APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
		Candidates:       []pool.Candidate{{ID: "acc", Token: "tok", Enabled: true}},
		Config: config.Config{
			UpstreamBase: upstream.URL + "/v1",
			DefaultModel: "grok-4.5",
			SSEKeepalive: 2 * time.Second,
		},
	})

	body := `{
		"model":"grok-4.5",
		"stream":false,
		"tools":[{"type":"function","function":{"name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}}],
		"messages":[{"role":"user","content":"run pwd"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "codex-cli/0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	if !strings.Contains(out, `"cmd"`) && !strings.Contains(out, `\"cmd\"`) && !strings.Contains(out, `"cmd"`) {
		t.Fatalf("expected cmd projection for Codex chat, body=%s", out)
	}
	// Reject command-only without cmd for shell tools.
	hasCmd := strings.Contains(out, `"cmd":"pwd"`) || strings.Contains(out, `\"cmd\":\"pwd\"`) || strings.Contains(out, `"cmd":"pwd"`)
	hasCommandOnly := strings.Contains(out, `"command":"pwd"`) || strings.Contains(out, `"command":"pwd"`)
	if hasCommandOnly && !hasCmd {
		t.Fatalf("command leaked without cmd: %s", out)
	}
	if !strings.Contains(out, "pwd") {
		t.Fatalf("expected pwd in args, body=%s", out)
	}
}

func TestCodexChatStreamProjectsCmd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			`data: {"id":"chatcmpl_x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"command\":"}}]}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	defer upstream.Close()

	h := server.NewMux(server.Options{
		Ready:            func() bool { return true },
		ResponsesEnabled: true,
		ChatEnabled:      true,
		APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
		Candidates:       []pool.Candidate{{ID: "acc", Token: "tok", Enabled: true}},
		Config: config.Config{
			UpstreamBase: upstream.URL + "/v1",
			DefaultModel: "grok-4.5",
			SSEKeepalive: 2 * time.Second,
		},
	})

	body := `{
		"model":"grok-4.5",
		"stream":true,
		"tools":[{"type":"function","function":{"name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}}],
		"messages":[{"role":"user","content":"run pwd"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "codex-cli/0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	if !strings.Contains(out, `"cmd"`) && !strings.Contains(out, `\"cmd\"`) {
		t.Fatalf("expected cmd in chat stream, body=%s", out)
	}
	if strings.Contains(out, `"command":"pwd"`) && !strings.Contains(out, `"cmd"`) {
		t.Fatalf("command leaked without cmd: %s", out)
	}
}
