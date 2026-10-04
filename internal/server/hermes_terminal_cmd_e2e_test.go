package server_test

import (
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

// Hermes agent (Nous Research) registers tool "terminal" with required
// parameter "command". Codex-oriented default projection to "cmd" must not
// rewrite Hermes outbound tool args.
func TestHermesChatCompletionsKeepsCommandLive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			`data: {"id":"chatcmpl_h","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_t","type":"function","function":{"name":"terminal","arguments":"{\"command\":"}}]}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
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

	// Real Hermes terminal schema: required "command", no "cmd".
	body := `{
		"model":"grok-4.5",
		"stream":true,
		"tools":[{"type":"function","function":{"name":"terminal","parameters":{"type":"object","properties":{"command":{"type":"string"},"background":{"type":"boolean"}},"required":["command"]}}}],
		"messages":[{"role":"user","content":"list files"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "hermes-agent/1.0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	// Must keep OpenAI-style "command", not Codex "cmd".
	if strings.Contains(out, `"cmd"`) || strings.Contains(out, `\"cmd\"`) {
		// Only fail when the shell payload was rewritten to cmd without command.
		if !strings.Contains(out, "command") {
			t.Fatalf("Hermes terminal projected to cmd without command: %s", out)
		}
		// Fail if completed args contain cmd key for the shell payload.
		if (strings.Contains(out, `"cmd":"ls`) || strings.Contains(out, `\"cmd\":\"ls`)) &&
			!(strings.Contains(out, `"command":"ls`) || strings.Contains(out, `\"command\":\"ls`)) {
			t.Fatalf("Hermes terminal args became cmd: %s", out)
		}
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "ls -la") && (strings.Contains(line, `"command"`) || strings.Contains(line, `\"command\"`)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected command+ls -la in Hermes chat stream, body=\n%s", out)
	}
}
