package server

import (
	"net/http"
	"testing"
	"time"
)

func TestChatFailureCooldownFreeUsage(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. tokens (actual/limit): 2042591/2000000"}`
	until, code, model, a, b := chatFailureCooldown(429, body)
	if until == nil {
		t.Fatal("expected until")
	}
	if until.Before(time.Now().Add(90 * time.Minute)) {
		t.Fatalf("until too short: %v", until)
	}
	if code != "subscription:free-usage-exhausted" {
		t.Fatalf("code=%q", code)
	}
	if model != "grok-4.5" {
		t.Fatalf("model=%q", model)
	}
	if a == nil || b == nil || *a != 2042591 || *b != 2000000 {
		t.Fatalf("tokens a=%v b=%v", a, b)
	}
}

func TestChatFailureCooldownRateLimit(t *testing.T) {
	until, code, _, _, _ := chatFailureCooldown(http.StatusTooManyRequests, "rate limited")
	if until == nil || code != "rate_limit" {
		t.Fatalf("until=%v code=%q", until, code)
	}
	until, code, _, _, _ = chatFailureCooldown(503, "upstream 503")
	if until == nil || code != "server_error" {
		t.Fatalf("until=%v code=%q", until, code)
	}
	until, code, _, _, _ = chatFailureCooldown(400, "bad request")
	if until != nil {
		t.Fatalf("unexpected cooldown for 400: %v %q", until, code)
	}
}

func TestExtractGrokModelFromError(t *testing.T) {
	if got := extractGrokModelFromError("for model grok-4.5-build-free for now"); got != "grok-4.5" {
		t.Fatalf("got %q", got)
	}
	if got := extractGrokModelFromError("no model here"); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := extractGrokModelFromError("model claude-sonnet-4"); got != "claude-sonnet-4" {
		t.Fatalf("got %q", got)
	}
}

func TestParseTokenPair(t *testing.T) {
	a, b, ok := parseTokenPair("tokens (actual/limit): 12/34")
	if !ok || a != 12 || b != 34 {
		t.Fatalf("%v %v %v", a, b, ok)
	}
}
