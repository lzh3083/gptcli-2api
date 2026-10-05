package pool

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FailureClass categorizes upstream/account errors for cooldown decisions.
type FailureClass string

const (
	ClassNone          FailureClass = ""
	ClassFreeUsage     FailureClass = "subscription:free-usage-exhausted"
	ClassRateLimit     FailureClass = "rate_limit"
	ClassAuth          FailureClass = "auth_error"
	ClassFatalAuth     FailureClass = "fatal_auth_invalid"
	ClassServer        FailureClass = "server_error"
	ClassModelCapacity FailureClass = "model_capacity"
	ClassBilling       FailureClass = "billing_quota"
	// ClassEmptyUpstream is a transient HTTP 200 with no content/tool payload.
	// Matches Python empty_upstream short cool (8–20s) — must NOT use 5xx 3m cool.
	ClassEmptyUpstream FailureClass = "empty_upstream"
)

// CooldownDecision is the structured action for pool/picker after a failure.
type CooldownDecision struct {
	Class        FailureClass
	Code         string
	Model        string
	Until        *time.Time
	TokensActual *int64
	TokensLimit  *int64
	// BlockModel soft-blocks CooldownDecision.Model for the same window.
	// Free-usage exhaustion must NOT set this: the account is already cooled
	// account-wide, and other models (e.g. grok-4.5) should stay pickable once cool ends.
	BlockModel bool
	// ShouldCooldown is false for transient/client errors that must not cool the account.
	ShouldCooldown bool
	// DisableAccount permanently removes the account from rotation on fatal auth/token death.
	DisableAccount bool
	Reason         string
}

var (
	reTokenPair = regexp.MustCompile(`(?i)tokens?\s*(?:\(actual\s*/\s*limit\))?\s*[:=]?\s*(\d+)\s*/\s*(\d+)`)
	reModelFor  = regexp.MustCompile(`(?i)(?:for\s+model|model|模型)\s*[:：]?\s*([a-z0-9][a-z0-9._-]{2,80})`)
	// Rolling window hints from Grok free-usage bodies.
	reResetsWindow = regexp.MustCompile(`(?i)resets?\s+over\s+a\s+rolling\s+(\d+)\s*-\s*hour`)
)

// ClassifyUpstreamFailure decides whether an account should enter the cooldown
// pool based on HTTP status + upstream error body text (English/Chinese).
//
// Decision priority is body/code text first, status second:
//  1. free-usage / 额度用完 phrasing → hard account cooldown only (no model block)
//  2. billing hard-quota → long cooldown
//  3. empty model output / no content/tool_calls → model soft-block (模型封禁) + cool
//  4. model capacity / overloaded → short cool
//  5. auth 401/403 → short cool
//  6. bare rate-limit / 429 without quota language → shorter cool
//  7. bare 5xx → brief cool
//  8. validation / client errors → do NOT cool
//
// requestedModel is the outbound model for this request; used as a fallback when
// the upstream body does not name a model (common for Chinese/admin paraphrases).
func ClassifyUpstreamFailure(status int, errText string, requestedModel ...string) CooldownDecision {
	// Unwrap "upstream status 429: {...}" wrappers so free-usage bodies still match
	// even when the only available signal is error.Error() instead of Body.
	if unwrappedStatus, unwrappedBody, ok := unwrapUpstreamErrorText(errText); ok {
		if status <= 0 {
			status = unwrappedStatus
		}
		errText = unwrappedBody
	}
	text := strings.TrimSpace(errText)
	low := strings.ToLower(text)
	// Unwrap nested JSON {"error":"...","code":"..."} if present.
	codeFromJSON, msgFromJSON := parseUpstreamErrorJSON(text)
	if msgFromJSON != "" {
		// Prefer the nested human/message body for token/model extraction while
		// still keeping the outer JSON code in low for free-usage matching.
		if text == "" || len(msgFromJSON) > len(text)/2 || looksLikeQuotaMessage(msgFromJSON) {
			text = msgFromJSON
			low = strings.ToLower(text)
		}
	}
	if codeFromJSON != "" && !strings.Contains(low, strings.ToLower(codeFromJSON)) {
		low = strings.ToLower(codeFromJSON) + " " + low
	}
	// When status is missing (0) but body is pure free-usage, still cool.

	fallbackModel := ""
	if len(requestedModel) > 0 {
		fallbackModel = strings.TrimSpace(requestedModel[0])
	}
	model := ExtractModelName(text)
	if model == "" {
		// Nested JSON model field is also common.
		if m := extractModelFromJSONField(errText); m != "" {
			model = m
		}
	}
	if model == "" {
		model = fallbackModel
	}
	actual, limit, hasTokens := ParseTokenPair(text)
	if !hasTokens {
		// Some wrappers only keep tokens in the outer JSON string.
		actual, limit, hasTokens = ParseTokenPair(errText)
	}

	// --- Free usage / quota exhausted (model or account) ---
	// Always kick into cooldown when body/code indicates model/account quota
	// exhaustion ("额度用完" / free-usage-exhausted / tokens actual>=limit, etc.).
	// Text-first: even HTTP 200/400 with free-usage wording should cool the account.
	// Do NOT soft-block the model: rolling free-usage is account-window scoped;
	// writing blocked_models would keep the account tagged "模型封禁" even when
	// the operator only cares that the account is cooling and other models remain usable.
	if isFreeUsageExhaustedText(low) || isFreeUsageCode(codeFromJSON) || isFreeUsageCode(text) {
		d := CooldownDecision{
			Class:          ClassFreeUsage,
			Code:           ClassFreeUsage.String(),
			Model:          defaultModel(model),
			ShouldCooldown: true,
			BlockModel:     false,
			Reason:         firstNonEmpty(text, codeFromJSON, "free usage exhausted"),
		}
		if hasTokens {
			d.TokensActual = &actual
			d.TokensLimit = &limit
		}
		// Prefer rolling-window hint; default 2h cool (matches probe path).
		until := time.Now().Add(freeUsageCooldownDuration(low))
		d.Until = &until
		return d
	}

	// Billing / hard quota (not free-tier rolling) — longer cool, still soft-block model if known.
	if isBillingQuotaText(low) {
		until := time.Now().Add(6 * time.Hour)
		d := CooldownDecision{
			Class:          ClassBilling,
			Code:           string(ClassBilling),
			Model:          model,
			Until:          &until,
			ShouldCooldown: true,
			BlockModel:     model != "",
			Reason:         firstNonEmpty(text, "billing quota"),
		}
		if hasTokens {
			d.TokensActual = &actual
			d.TokensLimit = &limit
		}
		return d
	}

	// Empty HTTP 200 / empty model output: soft-block the requested model so the
	// account shows under admin「模型封禁」and is skipped for that model only.
	// Do NOT sticky-cool the whole account (other models stay pickable).
	// Status is often rewritten to 502 by the proxy path; body text must win over
	// bare 5xx classification.
	if isEmptyModelOutputText(low) || isEmptyModelOutputCode(codeFromJSON) {
		// Default 4 minutes — long enough for admin chips, short enough that empty-storms do not shrink the pool.
		// Override via GROK2API_EMPTY_OUTPUT_BLOCK_SEC (30–86400).
		blockSec := 4 * 60
		if v := strings.TrimSpace(os.Getenv("GROK2API_EMPTY_OUTPUT_BLOCK_SEC")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 30 && n <= 24*3600 {
				blockSec = n
			}
		}
		until := time.Now().Add(time.Duration(blockSec) * time.Second)
		bm := defaultModel(model)
		return CooldownDecision{
			Class: ClassEmptyUpstream,
			Code:  string(ClassEmptyUpstream),
			Model: bm,
			Until: &until,
			// Account-wide cool would tag the row as 冷却 and hide it from 模型封禁.
			ShouldCooldown: false,
			// Soft-block the model → pool_status=model_blocked + picker skip.
			BlockModel: bm != "",
			Reason:     firstNonEmpty(text, "empty model output"),
		}
	}

	// Model capacity / overloaded (not account free-usage) — short cool only.
	if isModelCapacityText(low) {
		until := time.Now().Add(3 * time.Minute)
		return CooldownDecision{
			Class:          ClassModelCapacity,
			Code:           string(ClassModelCapacity),
			Model:          model,
			Until:          &until,
			ShouldCooldown: true,
			BlockModel:     false,
			Reason:         firstNonEmpty(text, "model capacity"),
		}
	}

	// Auth / WAF / edge block — cool so the picker stops re-using the same token
	// against a 403 wall (Cloudflare / edge bans heat up with rapid retries).
	if status == http.StatusUnauthorized || status == http.StatusForbidden || isAuthText(low) || isEdgeBlockText(low) || IsFatalAuthText(low) {
		if IsFatalAuthText(low) {
			return CooldownDecision{
				Class:          ClassFatalAuth,
				Code:           string(ClassFatalAuth),
				ShouldCooldown: true,
				DisableAccount: true,
				Reason:         firstNonEmpty(text, "authentication token invalidated/revoked"),
			}
		}
		// 403 / Cloudflare-like: longer cool. 401 token issues: moderate cool.
		cool := 8 * time.Minute
		if status == http.StatusUnauthorized && !isEdgeBlockText(low) {
			cool = 3 * time.Minute
		} else if status == http.StatusForbidden || isEdgeBlockText(low) {
			cool = 15 * time.Minute
		}
		until := time.Now().Add(cool)
		reason := "auth error"
		if status == http.StatusForbidden || isEdgeBlockText(low) {
			reason = "edge/waf blocked (403)"
		}
		return CooldownDecision{
			Class:          ClassAuth,
			Code:           string(ClassAuth),
			Until:          &until,
			ShouldCooldown: true,
			Reason:         firstNonEmpty(text, reason),
		}
	}

	// Rate limit without free-usage language.
	// Text-first: Chinese/English rate-limit phrasing cools even if status is missing.
	if status == http.StatusTooManyRequests || isRateLimitText(low) {
		until := time.Now().Add(10 * time.Minute)
		return CooldownDecision{
			Class:          ClassRateLimit,
			Code:           string(ClassRateLimit),
			Model:          model,
			Until:          &until,
			ShouldCooldown: true,
			// Soft-block model only when body points at a specific model.
			BlockModel: model != "" && strings.Contains(low, "model"),
			Reason:     firstNonEmpty(text, "rate limit"),
		}
	}

	// Upstream 5xx — brief cool.
	// Note: empty-model-output paths often surface as synthetic 502; those are
	// already handled above via body text before this branch.
	if status >= 500 && status <= 599 {
		until := time.Now().Add(3 * time.Minute)
		return CooldownDecision{
			Class:          ClassServer,
			Code:           string(ClassServer),
			Until:          &until,
			ShouldCooldown: true,
			Reason:         firstNonEmpty(text, "server error"),
		}
	}

	// Everything else (400 validation, cancel, etc.): do NOT cool.
	return CooldownDecision{ShouldCooldown: false, Reason: text}
}

// unwrapUpstreamErrorText peels "upstream status 429: <body>" (and similar)
// wrappers produced by UpstreamError.Error() so body classifiers still match.
func unwrapUpstreamErrorText(errText string) (status int, body string, ok bool) {
	text := strings.TrimSpace(errText)
	if text == "" {
		return 0, "", false
	}
	// Common shapes:
	//   upstream status 429: {...}
	//   Upstream status 429: rate limited
	//   status 429: {...}
	lower := strings.ToLower(text)
	prefixes := []string{"upstream status ", "status "}
	for _, p := range prefixes {
		if !strings.HasPrefix(lower, p) {
			continue
		}
		rest := strings.TrimSpace(text[len(p):])
		// rest starts with digits then optional ":" and body.
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			status = status*10 + int(rest[i]-'0')
			i++
		}
		if status <= 0 || i == 0 {
			return 0, "", false
		}
		rest = strings.TrimSpace(rest[i:])
		if strings.HasPrefix(rest, ":") {
			rest = strings.TrimSpace(rest[1:])
		}
		if rest == "" {
			return status, "", true
		}
		return status, rest, true
	}
	return 0, "", false
}

func looksLikeQuotaMessage(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	if low == "" {
		return false
	}
	return isFreeUsageExhaustedText(low) || isBillingQuotaText(low) || isRateLimitText(low)
}

func extractModelFromJSONField(errText string) string {
	errText = strings.TrimSpace(errText)
	if errText == "" || errText[0] != '{' {
		return ""
	}
	var raw map[string]any
	if json.Unmarshal([]byte(errText), &raw) != nil {
		return ""
	}
	for _, key := range []string{"model", "model_id", "modelId"} {
		if v, ok := raw[key].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return ExtractModelName("model " + v)
			}
		}
	}
	return ""
}

func (c FailureClass) String() string { return string(c) }

func isFreeUsageCode(code string) bool {
	c := strings.ToLower(strings.TrimSpace(code))
	if c == "" {
		return false
	}
	// Accept code embedded in longer strings ("code: subscription:free-usage-exhausted").
	if strings.Contains(c, "subscription:free-usage-exhausted") ||
		strings.Contains(c, "free-usage-exhausted") ||
		strings.Contains(c, "free_usage_exhausted") ||
		strings.Contains(c, "usage-limit-exceeded") ||
		strings.Contains(c, "usage_limit_exceeded") {
		return true
	}
	return (strings.Contains(c, "free-usage") || strings.Contains(c, "free_usage")) &&
		(strings.Contains(c, "exhaust") || strings.Contains(c, "exceed") || strings.Contains(c, "limit"))
}

func isFreeUsageExhaustedText(low string) bool {
	if low == "" {
		return false
	}
	// Canonical Grok / xAI bodies
	if strings.Contains(low, "free-usage-exhausted") ||
		strings.Contains(low, "free_usage_exhausted") ||
		strings.Contains(low, "subscription:free-usage") ||
		strings.Contains(low, "usage-limit-exceeded") ||
		strings.Contains(low, "usage_limit_exceeded") ||
		strings.Contains(low, "free-tier-limit") ||
		strings.Contains(low, "free_tier_limit") {
		return true
	}
	// English free-tier phrasing
	if strings.Contains(low, "free usage") ||
		strings.Contains(low, "included free usage") ||
		strings.Contains(low, "used all the included free") ||
		strings.Contains(low, "you've used all the included free") ||
		strings.Contains(low, "you have used all the included free") ||
		strings.Contains(low, "free quota") ||
		strings.Contains(low, "no remaining quota") ||
		strings.Contains(low, "no remaining free") ||
		strings.Contains(low, "out of free") ||
		strings.Contains(low, "out of quota") ||
		strings.Contains(low, "out of credits") ||
		strings.Contains(low, "run out of credits") ||
		strings.Contains(low, "quota exceeded") ||
		strings.Contains(low, "usage limit exceeded") ||
		strings.Contains(low, "usage limit reached") ||
		strings.Contains(low, "usage resets over a rolling") ||
		(strings.Contains(low, "free tier") && (strings.Contains(low, "exhaust") || strings.Contains(low, "limit") || strings.Contains(low, "exceed"))) {
		return true
	}
	// Chinese admin/proxy phrasing (model/account quota exhausted → cooldown pool).
	// Keep broad so live traffic with paraphrased bodies still hard-kicks.
	// Note: low is strings.ToLower of the original; Chinese is case-insensitive so
	// these match both original and lowercased text.
	chineseHits := []string{
		"额度耗尽", "额度用完", "额度不足", "额度已用尽", "额度已耗尽",
		"免费额度", "免费用量", "用量用完", "用量耗尽", "用量超限", "用量已用尽",
		"配额耗尽", "配额已用尽", "配额不足", "配额超限", "配额用完",
		"没有额度", "没额度", "无额度", "可用额度不足", "模型额度",
		"临时额度", "额度已满", "额度超限", "额度达到上限",
		"模型额度用完", "模型额度耗尽", "账号额度用完", "账号额度耗尽",
		"额度不够", "没额度了", "额度没了", "用完额度", "耗尽额度",
	}
	for _, p := range chineseHits {
		if strings.Contains(low, p) {
			return true
		}
	}
	// Generic quota exhausted (rolling window often present in free-usage body)
	if (strings.Contains(low, "quota") && (strings.Contains(low, "exhaust") || strings.Contains(low, "exceed") || strings.Contains(low, "limit") || strings.Contains(low, "remain"))) ||
		(strings.Contains(low, "usage") && (strings.Contains(low, "exhaust") || strings.Contains(low, "exceed")) && (strings.Contains(low, "limit") || strings.Contains(low, "free") || strings.Contains(low, "model"))) {
		// Prefer free-usage if free/rolling/model markers nearby.
		if strings.Contains(low, "free") || strings.Contains(low, "rolling") ||
			strings.Contains(low, "24-hour") || strings.Contains(low, "24 hour") ||
			strings.Contains(low, "model") || strings.Contains(low, "subscription") ||
			strings.Contains(low, "included") || strings.Contains(low, "tokens") {
			return true
		}
	}
	// tokens (actual/limit) with actual>=limit — strong free-usage signal even
	// without the word "free" (some bodies only expose the pair + model name).
	if a, b, ok := ParseTokenPair(low); ok && b > 0 && a >= b {
		if strings.Contains(low, "free") || strings.Contains(low, "subscription") ||
			strings.Contains(low, "included") || strings.Contains(low, "model") ||
			strings.Contains(low, "usage") || strings.Contains(low, "quota") ||
			strings.Contains(low, "rolling") {
			return true
		}
	}
	return false
}

func isBillingQuotaText(low string) bool {
	if low == "" {
		return false
	}
	if strings.Contains(low, "insufficient_quota") || strings.Contains(low, "billing") && strings.Contains(low, "quota") {
		return true
	}
	if strings.Contains(low, "payment") && (strings.Contains(low, "required") || strings.Contains(low, "fail")) {
		return true
	}
	// Chinese
	if strings.Contains(low, "余额不足") || strings.Contains(low, "欠费") || strings.Contains(low, "需要付费") {
		return true
	}
	return false
}

func isModelCapacityText(low string) bool {
	return strings.Contains(low, "capacity") ||
		strings.Contains(low, "overloaded") ||
		strings.Contains(low, "server_busy") ||
		strings.Contains(low, "too many concurrent") ||
		strings.Contains(low, "engine_overloaded")
}

func isRateLimitText(low string) bool {
	return strings.Contains(low, "rate limit") ||
		strings.Contains(low, "rate_limit") ||
		strings.Contains(low, "too many requests") ||
		strings.Contains(low, "请求过于频繁") ||
		strings.Contains(low, "速率限制")
}

// isEmptyModelOutputText detects synthetic empty-HTTP-200 failures produced by
// the Go proxy/server when upstream returns 200 with no content/tool_calls.
// Must be classified before bare 5xx: FailureReporter surfaces these as 502.
func isEmptyModelOutputText(low string) bool {
	if low == "" {
		return false
	}
	return strings.Contains(low, "empty model output") ||
		strings.Contains(low, "no content/tool_calls") ||
		strings.Contains(low, "no client-visible content") ||
		strings.Contains(low, "empty_upstream") ||
		strings.Contains(low, "empty upstream")
}

func isEmptyModelOutputCode(code string) bool {
	c := strings.ToLower(strings.TrimSpace(code))
	if c == "" {
		return false
	}
	return c == "empty_upstream" ||
		c == "empty-model-output" ||
		c == "empty_model_output" ||
		strings.Contains(c, "empty_upstream") ||
		strings.Contains(c, "empty-model-output")
}

func isAuthText(low string) bool {
	return strings.Contains(low, "invalid api key") ||
		strings.Contains(low, "invalid token") ||
		strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "authentication") ||
		strings.Contains(low, "未授权") ||
		strings.Contains(low, "鉴权失败")
}

// IsFatalAuthText detects irrecoverable token revocation or account deactivation.
func IsFatalAuthText(text string) bool {
	low := strings.ToLower(text)
	if low == "" {
		return false
	}
	return strings.Contains(low, "token_invalidated") ||
		strings.Contains(low, "token_revoked") ||
		strings.Contains(low, "refresh_token_invalidated") ||
		strings.Contains(low, "invalidated oauth token") ||
		strings.Contains(low, "authentication token has been invalidated") ||
		strings.Contains(low, "your session has ended") ||
		strings.Contains(low, "account_deactivated") ||
		strings.Contains(low, "account_blocked") ||
		strings.Contains(low, "user_deactivated") ||
		strings.Contains(low, "invalid_grant") ||
		strings.Contains(low, "revoked-tokens") ||
		strings.Contains(low, "token has been revoked")
}

// isEdgeBlockText detects Cloudflare / reverse-proxy WAF style blocks that often
// surface as HTTP 403 (or HTML bodies) when a single token is hammered.
func isEdgeBlockText(low string) bool {
	if low == "" {
		return false
	}
	return strings.Contains(low, "cloudflare") ||
		strings.Contains(low, "cf-ray") ||
		strings.Contains(low, "cf-mitigated") ||
		strings.Contains(low, "attention required") ||
		strings.Contains(low, "just a moment") ||
		strings.Contains(low, "access denied") ||
		strings.Contains(low, "request blocked") ||
		strings.Contains(low, "web application firewall") ||
		strings.Contains(low, "waf") ||
		strings.Contains(low, "captcha") && strings.Contains(low, "challenge") ||
		strings.Contains(low, "error 1015") || // CF rate limited
		strings.Contains(low, "error 1020") || // CF access denied
		strings.Contains(low, "error 1006") ||
		strings.Contains(low, "403 forbidden") ||
		strings.Contains(low, "http 403")
}

func freeUsageCooldownDuration(low string) time.Duration {
	// "resets over a rolling 24-hour window"
	if m := reResetsWindow.FindStringSubmatch(low); len(m) == 2 {
		// Cool for a fraction of the window so accounts re-enter after partial reset.
		// 24h window → 2h cool (previous behavior); 1h → 20m, etc. Clamp [20m, 6h].
		var hours int
		for _, c := range m[1] {
			if c >= '0' && c <= '9' {
				hours = hours*10 + int(c-'0')
			}
		}
		if hours > 0 {
			d := time.Duration(hours) * time.Hour / 12
			if d < 20*time.Minute {
				d = 20 * time.Minute
			}
			if d > 6*time.Hour {
				d = 6 * time.Hour
			}
			return d
		}
	}
	return 2 * time.Hour
}

// ExtractModelName pulls a model id from Grok free-usage error bodies.
func ExtractModelName(errText string) string {
	if errText == "" {
		return ""
	}
	m := reModelFor.FindStringSubmatch(errText)
	if len(m) < 2 {
		return ""
	}
	name := strings.TrimSpace(m[1])
	// strip trailing punctuation
	name = strings.TrimRight(name, ".,;:\"')}]")
	lname := strings.ToLower(name)
	if name == "" {
		return ""
	}
	if !(strings.Contains(lname, "grok") || strings.Contains(lname, "claude") || strings.HasPrefix(lname, "gpt") || strings.Contains(lname, "build")) {
		return ""
	}
	// Map free probe model ids back to public alias when possible.
	if strings.HasPrefix(lname, "grok-4.5") {
		return "grok-4.5"
	}
	if strings.HasPrefix(lname, "grok-4.6") {
		return "grok-4.6"
	}
	if strings.HasPrefix(lname, "grok-4.7") {
		return "grok-4.7"
	}
	return name
}

// ParseTokenPair finds actual/limit token counters in error text.
func ParseTokenPair(errText string) (actual, limit int64, ok bool) {
	m := reTokenPair.FindStringSubmatch(errText)
	if len(m) != 3 {
		return 0, 0, false
	}
	var a, b int64
	for _, ch := range m[1] {
		if ch >= '0' && ch <= '9' {
			a = a*10 + int64(ch-'0')
		}
	}
	for _, ch := range m[2] {
		if ch >= '0' && ch <= '9' {
			b = b*10 + int64(ch-'0')
		}
	}
	if b <= 0 {
		return 0, 0, false
	}
	return a, b, true
}

func parseUpstreamErrorJSON(errText string) (code, message string) {
	errText = strings.TrimSpace(errText)
	if errText == "" || errText[0] != '{' {
		return "", ""
	}
	var raw map[string]any
	if json.Unmarshal([]byte(errText), &raw) != nil {
		return "", ""
	}
	// shapes: {"code":"...","error":"..."} or {"error":{"code":"...","message":"..."}}
	if c, ok := raw["code"].(string); ok {
		code = c
	}
	switch e := raw["error"].(type) {
	case string:
		message = e
	case map[string]any:
		if message == "" {
			if m, ok := e["message"].(string); ok {
				message = m
			} else if m, ok := e["error"].(string); ok {
				message = m
			}
		}
		if code == "" {
			if c, ok := e["code"].(string); ok {
				code = c
			}
		}
	}
	if message == "" {
		if m, ok := raw["message"].(string); ok {
			message = m
		}
	}
	return strings.TrimSpace(code), strings.TrimSpace(message)
}

func defaultModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return "grok-4.5"
	}
	return model
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// IsFreeUsageExhausted is a convenience boolean for probe/UI paths.
func IsFreeUsageExhausted(errText string) bool {
	d := ClassifyUpstreamFailure(0, errText)
	return d.Class == ClassFreeUsage
}

// ShouldEnterCooldownPool reports whether the given upstream status/body text
// should hard-kick the account into the cooldown pool (and optionally soft-block
// a model). Prefer ClassifyUpstreamFailure when you need the full decision.
func ShouldEnterCooldownPool(status int, errText string, requestedModel ...string) bool {
	return ClassifyUpstreamFailure(status, errText, requestedModel...).ShouldCooldown
}
