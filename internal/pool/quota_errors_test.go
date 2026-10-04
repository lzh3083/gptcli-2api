package pool

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassifyFreeUsageEnglish(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 2042591/2000000."}`
	d := ClassifyUpstreamFailure(429, body)
	if !d.ShouldCooldown || d.Class != ClassFreeUsage {
		t.Fatalf("decision=%+v", d)
	}
	if d.Model != "grok-4.5" {
		t.Fatalf("model=%q", d.Model)
	}
	if d.BlockModel {
		t.Fatal("free-usage must cool only, not soft-block model")
	}
	if d.Until == nil || d.Until.Before(time.Now().Add(90*time.Minute)) {
		t.Fatalf("until=%v", d.Until)
	}
	if d.TokensActual == nil || *d.TokensActual != 2042591 || d.TokensLimit == nil || *d.TokensLimit != 2000000 {
		t.Fatalf("tokens a=%v b=%v", d.TokensActual, d.TokensLimit)
	}
}

func TestClassifyFreeUsageChinese(t *testing.T) {
	d := ClassifyUpstreamFailure(429, "账号免费额度耗尽，请稍后再试")
	if d.Class != ClassFreeUsage || !d.ShouldCooldown {
		t.Fatalf("%+v", d)
	}
}

func TestClassifyRateLimitWithoutQuota(t *testing.T) {
	d := ClassifyUpstreamFailure(429, "rate limit exceeded, try again later")
	if d.Class != ClassRateLimit {
		t.Fatalf("%+v", d)
	}
	if d.Until == nil || d.Until.After(time.Now().Add(15*time.Minute)) {
		t.Fatalf("rate limit cool too long: %v", d.Until)
	}
}

func TestClassifyBadRequestNoCooldown(t *testing.T) {
	d := ClassifyUpstreamFailure(400, "invalid_request_error: messages required")
	if d.ShouldCooldown {
		t.Fatalf("must not cool: %+v", d)
	}
}

func TestClassifyAuth(t *testing.T) {
	d := ClassifyUpstreamFailure(401, "Invalid or missing API key")
	if d.Class != ClassAuth || !d.ShouldCooldown {
		t.Fatalf("%+v", d)
	}
}

func TestIsFreeUsageExhaustedHelper(t *testing.T) {
	if !IsFreeUsageExhausted("subscription:free-usage-exhausted") {
		t.Fatal("expected true")
	}
	if IsFreeUsageExhausted("hello world") {
		t.Fatal("expected false")
	}
}

func TestExtractModelAndTokens(t *testing.T) {
	if got := ExtractModelName("for model grok-4.5-build-free for now"); got != "grok-4.5" {
		t.Fatalf("model=%q", got)
	}
	a, b, ok := ParseTokenPair("tokens (actual/limit): 12/34")
	if !ok || a != 12 || b != 34 {
		t.Fatalf("%v %v %v", a, b, ok)
	}
}

func TestBillingVsFree(t *testing.T) {
	d := ClassifyUpstreamFailure(402, "insufficient_quota: billing hard limit")
	if d.Class != ClassBilling {
		t.Fatalf("%+v", d)
	}
}

func TestServerError(t *testing.T) {
	d := ClassifyUpstreamFailure(503, "upstream unavailable")
	if d.Class != ClassServer || !d.ShouldCooldown {
		t.Fatalf("%+v", d)
	}
	if d.Until != nil && d.Until.After(time.Now().Add(10*time.Minute)) {
		t.Fatalf("5xx cool too long")
	}
}

func TestCodeOnlyJSON(t *testing.T) {
	d := ClassifyUpstreamFailure(http.StatusTooManyRequests, `{"code":"subscription:free-usage-exhausted"}`)
	if d.Class != ClassFreeUsage {
		t.Fatalf("%+v", d)
	}
	if !strings.Contains(d.Code, "free-usage") {
		t.Fatalf("code=%q", d.Code)
	}
}

func TestClassifyQuotaTextEntersCooldownPool(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		class  FailureClass
	}{
		{"chinese_quota_used_up", 429, "模型额度用完，请稍后再试", ClassFreeUsage},
		{"chinese_no_quota", 0, "账号没额度了", ClassFreeUsage},
		{"english_out_of_quota", 429, "out of quota for this model", ClassFreeUsage},
		{"wrapped_upstream_error", 0, `upstream status 429: {"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. tokens (actual/limit): 10/10."}`, ClassFreeUsage},
		{"rate_limit_no_quota", 429, "rate limit exceeded, try again later", ClassRateLimit},
		{"validation_no_cool", 400, "invalid_request_error: messages required", ClassNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ClassifyUpstreamFailure(tc.status, tc.body, "grok-4.5")
			if tc.class == ClassNone {
				if d.ShouldCooldown {
					t.Fatalf("must not cool: %+v", d)
				}
				return
			}
			if !d.ShouldCooldown {
				t.Fatalf("expected cooldown for %q: %+v", tc.body, d)
			}
			if d.Class != tc.class {
				t.Fatalf("class=%q want=%q body=%q", d.Class, tc.class, tc.body)
			}
			if tc.class == ClassFreeUsage {
				if d.BlockModel {
					t.Fatalf("free-usage must not block model: %+v", d)
				}
				if d.Model == "" {
					t.Fatalf("expected model fallback: %+v", d)
				}
			}
		})
	}
}

func TestShouldEnterCooldownPoolTextFirst(t *testing.T) {
	if !ShouldEnterCooldownPool(0, "额度用完") {
		t.Fatal("额度用完 must enter cooldown pool")
	}
	if !ShouldEnterCooldownPool(200, `{"code":"subscription:free-usage-exhausted"}`) {
		t.Fatal("code-only free-usage must enter cooldown pool even on status 200")
	}
	if ShouldEnterCooldownPool(400, "invalid_request_error: max_tokens required") {
		t.Fatal("validation must not enter cooldown pool")
	}
}

func TestFreeUsageGrok45BuildFreeCoolOnly(t *testing.T) {
	body := `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1056720/1000000. Upgrade to a Grok subscription for higher limits: https://grok.com/supergrok"}`
	d := ClassifyUpstreamFailure(429, body, "grok-4.5")
	if !d.ShouldCooldown || d.Class != ClassFreeUsage {
		t.Fatalf("decision=%+v", d)
	}
	if d.BlockModel {
		t.Fatalf("free-usage must not soft-block model: %+v", d)
	}
	if d.Until == nil {
		t.Fatal("expected cooldown until")
	}
}

func TestClassifyEmptyModelOutputShortCool(t *testing.T) {
	// Proxy rewrites empty HTTP 200 as synthetic 502; body text must win over 5xx cool.
	// Empty output now soft-blocks the requested model (admin 模型封禁) for a short window.
	t.Setenv("GROK2API_EMPTY_OUTPUT_BLOCK_SEC", "600") // 10m for deterministic assertion
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"synthetic_502_body", 502, "Upstream returned HTTP 200 with empty model output (no content/tool_calls)"},
		{"status_zero_body", 0, "empty model output"},
		{"code_empty_upstream", 502, `{"code":"empty_upstream","error":"no content/tool_calls"}`},
		{"no_client_visible", 502, "no client-visible content after stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ClassifyUpstreamFailure(tc.status, tc.body, "grok-4.5")
			if d.ShouldCooldown {
				t.Fatalf("empty output must NOT sticky-cool whole account: %+v", d)
			}
			if d.Class != ClassEmptyUpstream {
				t.Fatalf("class=%q want empty_upstream body=%q decision=%+v", d.Class, tc.body, d)
			}
			if !d.BlockModel {
				t.Fatalf("empty upstream must soft-block model (模型封禁): %+v", d)
			}
			if d.Model != "grok-4.5" {
				t.Fatalf("model=%q want grok-4.5", d.Model)
			}
			if d.Until == nil {
				t.Fatal("expected until")
			}
			// 10m block (±30s slack). Must be longer than old 12s ultra-short cool.
			if d.Until.Before(time.Now().Add(8 * time.Minute)) {
				t.Fatalf("empty block too short: until=%v", d.Until)
			}
			if d.Until.After(time.Now().Add(15 * time.Minute)) {
				t.Fatalf("empty block too long: until=%v", d.Until)
			}
		})
	}

	// Bare 5xx without empty phrasing still uses server cool (minutes), no model block.
	d := ClassifyUpstreamFailure(502, "bad gateway from reverse proxy")
	if d.Class != ClassServer {
		t.Fatalf("bare 502 should be server class: %+v", d)
	}
	if d.Until == nil || d.Until.Before(time.Now().Add(2*time.Minute)) {
		t.Fatalf("bare 5xx cool should be ~3m: %+v", d)
	}
	if d.BlockModel {
		t.Fatalf("bare 5xx must not soft-block model: %+v", d)
	}
}

func TestClassifyForbiddenEdgeBlock(t *testing.T) {
	d := ClassifyUpstreamFailure(403, "error code: 1020\nAccess denied\ncloudflare", "grok-4.5")
	if !d.ShouldCooldown {
		t.Fatalf("expected cool: %+v", d)
	}
	if d.Class != ClassAuth {
		t.Fatalf("class=%v", d.Class)
	}
	if d.Until == nil || d.Until.Before(time.Now().Add(10*time.Minute)) {
		t.Fatalf("403 edge cool too short: until=%v", d.Until)
	}
}

func TestClassifyUnauthorizedShorterThanForbidden(t *testing.T) {
	u := ClassifyUpstreamFailure(401, "invalid api key", "grok-4.5")
	f := ClassifyUpstreamFailure(403, "cloudflare access denied", "grok-4.5")
	if u.Until == nil || f.Until == nil {
		t.Fatal("missing until")
	}
	if !f.Until.After(*u.Until) {
		t.Fatalf("forbidden cool should be longer: 401=%v 403=%v", u.Until, f.Until)
	}
}
