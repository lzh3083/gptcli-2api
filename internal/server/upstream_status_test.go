package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/config"
)

func TestUpstreamHostPort(t *testing.T) {
	hp, scheme, err := upstreamHostPort("https://cli-chat-proxy.grok.com/v1")
	if err != nil {
		t.Fatal(err)
	}
	if scheme != "https" {
		t.Fatalf("scheme=%q", scheme)
	}
	if hp != "cli-chat-proxy.grok.com:443" {
		t.Fatalf("hostPort=%q", hp)
	}
	hp, scheme, err = upstreamHostPort("http://127.0.0.1:8080/v1")
	if err != nil {
		t.Fatal(err)
	}
	if scheme != "http" || hp != "127.0.0.1:8080" {
		t.Fatalf("got %s %s", scheme, hp)
	}
}

func TestProbeUpstreamStatusOK(t *testing.T) {
	// Reset process cache so this test is isolated.
	upstreamStatusMu.Lock()
	upstreamStatusCache = nil
	upstreamStatusAt = time.Time{}
	upstreamStatusInFly = false
	upstreamStatusMu.Unlock()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" && r.URL.Path != "/models" {
			// Accept either BaseURL ends with /v1 or not.
			if !strings.HasSuffix(r.URL.Path, "/models") {
				http.NotFound(w, r)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "grok-4.5", "object": "model"},
				{"id": "grok-3", "object": "model"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	opts := Options{
		Config:           config.Config{UpstreamBase: srv.URL + "/v1", DefaultModel: "grok-4.5"},
		AdminReadEnabled: true,
		AdminSessions:    nil,
	}
	// Bypass auth for unit test by calling probe directly.
	result := probeUpstreamStatus(context.Background(), opts, true)
	if result["ok"] != true {
		t.Fatalf("expected ok=true, got %#v", result)
	}
	if result["reachable"] != true {
		t.Fatalf("expected reachable, got %#v", result)
	}
	if n, _ := result["models_count"].(int); n < 1 {
		// models_count may be int or float64 depending on clone — accept both.
		if f, ok := result["models_count"].(float64); !ok || f < 1 {
			if n == 0 {
				// parseUpstreamModels may add local extras — still need >=1 from upstream
				// or from extras. Fail only if completely empty.
				t.Fatalf("models_count missing/empty: %#v", result)
			}
		}
	}
	if result["latency_ms"] == nil {
		t.Fatalf("missing latency_ms: %#v", result)
	}

	// Second call should hit cache.
	cached := probeUpstreamStatus(context.Background(), opts, false)
	if cached["cached"] != true {
		t.Fatalf("expected cached=true, got %#v", cached)
	}
}

func TestProbeUpstreamStatusDialFail(t *testing.T) {
	upstreamStatusMu.Lock()
	upstreamStatusCache = nil
	upstreamStatusAt = time.Time{}
	upstreamStatusInFly = false
	upstreamStatusMu.Unlock()

	opts := Options{
		Config: config.Config{UpstreamBase: "http://127.0.0.1:1", DefaultModel: "grok-4.5"},
	}
	result := probeUpstreamStatus(context.Background(), opts, true)
	if result["ok"] == true {
		t.Fatalf("expected ok=false, got %#v", result)
	}
	if result["reachable"] == true {
		t.Fatalf("expected unreachable, got %#v", result)
	}
	if result["error"] == nil || result["error"] == "" {
		t.Fatalf("expected error, got %#v", result)
	}
}

func TestServeUpstreamStatusUnauthorizedWithoutSession(t *testing.T) {
	mux := NewMux(Options{
		Config:           config.Config{UpstreamBase: "https://example.invalid/v1"},
		AdminReadEnabled: true,
		Ready:            func() bool { return true },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/upstream-status", nil)
	mux.ServeHTTP(rec, req)
	// Without session → 401 (or 503 if write flags off — accept either auth gate).
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCachedUpstreamStatusStale(t *testing.T) {
	upstreamStatusMu.Lock()
	upstreamStatusCache = map[string]any{"ok": true, "base_url": "x"}
	upstreamStatusAt = time.Now().Add(-2 * time.Minute)
	upstreamStatusMu.Unlock()
	if got := cachedUpstreamStatus(); got != nil {
		t.Fatalf("expected nil stale cache, got %#v", got)
	}
	upstreamStatusMu.Lock()
	upstreamStatusCache = map[string]any{"ok": true, "base_url": "x"}
	upstreamStatusAt = time.Now()
	upstreamStatusMu.Unlock()
	got := cachedUpstreamStatus()
	if got == nil || got["ok"] != true {
		t.Fatalf("expected fresh cache, got %#v", got)
	}
}
