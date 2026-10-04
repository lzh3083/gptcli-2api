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

func TestCodexChatCompletionsProjectsCmdLive(t *testing.T) {
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
		Ready:       func() bool { return true },
		ChatEnabled: true,
		APIKeys:     auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
		Candidates:  []pool.Candidate{{ID: "acc", Token: "tok", Enabled: true}},
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
	// Reject command-only completed tool args without cmd.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "tool_calls") {
			continue
		}
		if strings.Contains(line, `"arguments"`) && strings.Contains(line, `"command"`) && !strings.Contains(line, `"cmd"`) {
			// allow incomplete partials? assembler should emit complete only.
			if strings.Contains(line, `pwd`) {
				t.Fatalf("command leaked without cmd: %s", line)
			}
		}
	}
	// Soft assert: some frame has cmd+pwd
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "pwd") && (strings.Contains(line, `"cmd"`) || strings.Contains(line, `\"cmd\"`)) {
			found = true
			break
		}
	}
	if !found {
		// parse last tool frame if present
		for _, line := range strings.Split(out, "\n") {
			if !strings.HasPrefix(line, "data: {") {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line[len("data: "):]), &obj) != nil {
				continue
			}
			b, _ := json.Marshal(obj)
			if strings.Contains(string(b), "cmd") && strings.Contains(string(b), "pwd") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no chat tool frame with cmd+pwd; body=\n%s", out)
	}
}
