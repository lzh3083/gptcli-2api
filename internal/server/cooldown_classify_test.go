package server

import (
	"net/http"
	"testing"
	"time"
)

func TestChatFailureCooldownFreeUsageBody(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 2042591/2000000."}`
	until, code, model, a, b := chatFailureCooldown(429, body)
	if until == nil {
		t.Fatal("expected cooldown until")
	}
	if until.Before(time.Now().Add(90 * time.Minute)) {
		t.Fatalf("free-usage cool too short: %v", until)
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

func TestChatFailureCooldownChineseQuota(t *testing.T) {
	until, code, _, _, _ := chatFailureCooldown(429, "免费额度耗尽，请稍后再试")
	if until == nil || code != "subscription:free-usage-exhausted" {
		t.Fatalf("until=%v code=%q", until, code)
	}
}

func TestChatFailureCooldownBareRateLimit(t *testing.T) {
	until, code, _, _, _ := chatFailureCooldown(http.StatusTooManyRequests, "rate limit exceeded")
	if until == nil || code != "rate_limit" {
		t.Fatalf("until=%v code=%q", until, code)
	}
	// shorter than free-usage
	if until.After(time.Now().Add(20 * time.Minute)) {
		t.Fatalf("rate limit cool too long: %v", until)
	}
}

func TestChatFailureCooldownBadRequestNoCool(t *testing.T) {
	until, _, _, _, _ := chatFailureCooldown(400, "invalid_request_error: max_tokens required")
	if until != nil {
		t.Fatalf("must not cool validation errors: %v", until)
	}
}
