package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/auth"
	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/server"
)

// memRespAffinity implements proxy.AffinityStore + responseAffinityStore.
type memRespAffinity struct {
	mu      sync.Mutex
	bound   map[string]string
	respAcc map[string]string
	respPCK map[string]string
}

func (m *memRespAffinity) GetAffinity(_ context.Context, fingerprint string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bound == nil {
		return "", nil
	}
	return m.bound[fingerprint], nil
}

func (m *memRespAffinity) BindAffinity(_ context.Context, fingerprint, accountID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bound == nil {
		m.bound = map[string]string{}
	}
	m.bound[fingerprint] = accountID
	return nil
}

func (m *memRespAffinity) ClearAffinity(_ context.Context, fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bound != nil {
		delete(m.bound, fingerprint)
	}
	return nil
}

func (m *memRespAffinity) BindResponseAccount(_ context.Context, responseID, accountID, promptCacheKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.respAcc == nil {
		m.respAcc = map[string]string{}
		m.respPCK = map[string]string{}
	}
	m.respAcc[responseID] = accountID
	m.respPCK[responseID] = promptCacheKey
	return nil
}

func (m *memRespAffinity) GetResponseAccount(_ context.Context, responseID string) (accountID, promptCacheKey string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.respAcc == nil {
		return "", "", nil
	}
	return m.respAcc[responseID], m.respPCK[responseID], nil
}

func TestCodexPreviousResponseIDSticky(t *testing.T) {
	store := &memRespAffinity{bound: map[string]string{}, respAcc: map[string]string{}, respPCK: map[string]string{}}
	var seenAuth []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_x","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	h := server.NewMux(server.Options{
		Ready:            func() bool { return true },
		ResponsesEnabled: true,
		ChatEnabled:      true,
		APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
		Candidates: []pool.Candidate{
			// Least-used would pick acc-b (RequestCount 0) without sticky.
			{ID: "acc-a", Token: "token-a", Enabled: true, RequestCount: 50},
			{ID: "acc-b", Token: "token-b", Enabled: true, RequestCount: 0},
		},
		AffinityStore: store,
		Config: config.Config{
			UpstreamBase: upstream.URL + "/v1",
			DefaultModel: "grok-4.5",
			SSEKeepalive: 2 * time.Second,
		},
	})

	// Turn 1: fresh conversation with explicit prompt_cache_key (Codex multi-turn stable key).
	body1 := `{
		"model":"grok-4.5",
		"stream":true,
		"prompt_cache_key":"codex-session-1",
		"input":[{"role":"user","content":"hi"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body1))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "codex-cli/0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out1 := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("turn1 status=%d body=%s", rec.Code, out1)
	}
	acc1 := rec.Header().Get("X-Grok2API-Account")
	if acc1 == "" {
		t.Fatalf("turn1 missing account header: %v body=%s", rec.Header(), out1)
	}
	// Extract response id from stream.
	respID := ""
	for _, line := range strings.Split(out1, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line[len("data: "):]), &obj) != nil {
			continue
		}
		if typ, _ := obj["type"].(string); typ == "response.created" || typ == "response.completed" {
			if resp, ok := obj["response"].(map[string]any); ok {
				if id, _ := resp["id"].(string); strings.TrimSpace(id) != "" {
					respID = id
				}
			}
		}
		if id, _ := obj["id"].(string); strings.HasPrefix(id, "resp_") {
			respID = id
		}
	}
	if respID == "" {
		// Fallback: scan for resp_ substring
		if i := strings.Index(out1, "resp_"); i >= 0 {
			j := i
			for j < len(out1) && (out1[j] == '_' || (out1[j] >= 'a' && out1[j] <= 'z') || (out1[j] >= '0' && out1[j] <= '9') || (out1[j] >= 'A' && out1[j] <= 'Z')) {
				j++
			}
			respID = out1[i:j]
		}
	}
	if respID == "" {
		t.Fatalf("could not find response id in body=%s", out1)
	}

	// Turn 2: only previous_response_id (no prompt_cache_key) — must stick to same account.
	body2 := `{
		"model":"grok-4.5",
		"stream":true,
		"previous_response_id":` + jsonString(respID) + `,
		"input":[{"role":"user","content":"again"}]
	}`
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body2))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "codex-cli/0.1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out2 := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("turn2 status=%d body=%s", rec.Code, out2)
	}
	acc2 := rec.Header().Get("X-Grok2API-Account")
	if acc2 != acc1 {
		t.Fatalf("sticky broken: turn1=%s turn2=%s affinity=%s body=%s", acc1, acc2, rec.Header().Get("X-Grok2API-Affinity"), out2)
	}
	if rec.Header().Get("X-Grok2API-Affinity") != "1" {
		// PreferAccount may still set affinity=1; if not, same account is enough when sticky recovered.
		t.Logf("note: affinity header=%q (account still matched)", rec.Header().Get("X-Grok2API-Affinity"))
	}

	// Turn 3: prompt_cache_key alone should also stick.
	body3 := `{
		"model":"grok-4.5",
		"stream":true,
		"prompt_cache_key":"codex-session-1",
		"input":[{"role":"user","content":"third"}]
	}`
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body3))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("User-Agent", "codex-cli/0.1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	acc3 := rec.Header().Get("X-Grok2API-Account")
	if acc3 != acc1 {
		t.Fatalf("pck sticky broken: turn1=%s turn3=%s body=%s", acc1, acc3, rec.Body.String())
	}
}

func TestCodexApplyPatchProjectsInput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Upstream emits patch alias; client should see input after normalize.
		frames := []string{
			`data: {"id":"chatcmpl_x","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"apply_patch","arguments":"{\"patch\":"}}]}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"*** Begin Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
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
		"tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}],
		"input":[{"role":"user","content":"apply a patch"}]
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
	// Must project to input for apply_patch.
	if !strings.Contains(out, `"input"`) && !strings.Contains(out, `\"input\"`) {
		t.Fatalf("expected input key in stream, body=%s", out)
	}
	// Completed args should not leave only patch alias without input.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "function_call_arguments.done") && !strings.Contains(line, `"status":"completed"`) {
			continue
		}
		if strings.Contains(line, `"patch"`) && !strings.Contains(line, `"input"`) {
			t.Fatalf("patch leaked without input: %s", line)
		}
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "function_call_arguments.done") {
			continue
		}
		idx := strings.Index(line, "{")
		if idx < 0 {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line[idx:]), &obj) != nil {
			continue
		}
		args := fmtSprint(obj["arguments"])
		if strings.Contains(args, "input") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no function_call_arguments.done with input; body=\n%s", out)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
