// Package reasoning normalizes thinking / reasoning effort labels across clients.
//
// Two layers:
//
//  1. Client labels (Claude Code / Anthropic API / Codex) — preserved for usage
//     logs and admin UI:
//
//     low | medium | high | xhigh | max | ultracode
//
//  2. Upstream (Grok / cli-chat-proxy) — only THREE levels:
//
//     low | medium | high
//
// Claude Code UI effort menu → API (output_config.effort):
//
//	low | medium | high | xhigh | max
//
// "ultracode" is NOT an API effort level — Claude Code pairs xhigh with standing
// multi-agent orchestration permission. We still accept the label for logging and
// fold it to xhigh (client) / high (upstream).
//
// Codex aliases:
//
//	Low → low · Base → medium · High / Ultra / Proactive → high
//	legacy: auto→low, default→medium, standard→high, extra-high→xhigh
package reasoning

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Upstream levels emitted to Grok (3-tier only).
const (
	Low    = "low"
	Medium = "medium"
	High   = "high"
)

// Client-facing labels (Claude Code / Anthropic effort parameter).
const (
	ClientLow       = "low"
	ClientMedium    = "medium"
	ClientHigh      = "high"
	ClientXHigh     = "xhigh"
	ClientMax       = "max"
	ClientUltracode = "ultracode"
)

// XHigh is a client top-ish tier. Upstream always folds it to High.
// Kept for call-site readability when mapping Claude Code xhigh.
const XHigh = ClientXHigh

// Normalize maps free-form client effort labels (and budgets) to a client-facing
// label: low|medium|high|xhigh|max|ultracode. Empty means "no reasoning effort".
// Use ToUpstream when writing Grok request bodies.
func Normalize(value any) string {
	return NormalizeClient(value)
}

// NormalizeClient is the explicit client-label entry point (same as Normalize).
func NormalizeClient(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case bool:
		if v {
			return ClientMedium
		}
		return ""
	case float64:
		return BudgetToClient(int(v))
	case float32:
		return BudgetToClient(int(v))
	case int:
		return BudgetToClient(v)
	case int64:
		return BudgetToClient(int(v))
	case int32:
		return BudgetToClient(int(v))
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return ""
		}
		return BudgetToClient(int(n))
	case string:
		return normalizeClientString(v)
	case map[string]any:
		// Prefer explicit effort fields, then budget, then type/enabled.
		for _, key := range []string{
			"effort", "reasoning_effort", "thinking_effort",
			"intensity", "level", "thinking_intensity",
		} {
			if vv, ok := v[key]; ok && vv != nil {
				if got := NormalizeClient(vv); got != "" {
					return got
				}
			}
		}
		if vv, ok := v["budget_tokens"]; ok && vv != nil {
			if got := NormalizeClient(vv); got != "" {
				return got
			}
		}
		tt := strings.ToLower(strings.TrimSpace(fmt.Sprint(v["type"])))
		switch tt {
		case "", "disabled", "none", "false", "off", "0":
			// fall through to enabled flag
		case ClientLow, ClientMedium, ClientHigh, ClientXHigh, ClientMax, ClientUltracode:
			return tt
		case "x-high":
			return ClientXHigh
		case "enabled", "true", "on", "adaptive":
			// Adaptive thinking without explicit effort → medium (balanced default).
			if got := NormalizeClient(v["budget_tokens"]); got != "" {
				return got
			}
			return ClientMedium
		case "auto", "default", "standard":
			return normalizeClientString(tt)
		default:
			if got := normalizeClientString(tt); got != "" {
				return got
			}
		}
		if v["enabled"] == true {
			return ClientMedium
		}
		return ""
	default:
		return normalizeClientString(fmt.Sprint(v))
	}
}

func normalizeClientString(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	// normalize separators: extra-high / extra_high / extra high
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.Join(strings.Fields(s), "-")

	switch s {
	case "none", "null", "false", "off", "disabled", "0", "no":
		return ""
	// ── low ───────────────────────────────────────────────────
	// Claude Code: low · Codex: Low / auto · misc: minimal/fast
	case ClientLow, "minimal", "min", "l", "lite", "fast", "auto":
		return ClientLow
	// ── medium ────────────────────────────────────────────────
	// Claude Code: medium · Codex: Base / default · misc: adaptive/enabled
	case ClientMedium, "default", "normal", "balanced", "mid", "m", "med",
		"base", "adaptive", "enabled", "true", "on", "1":
		return ClientMedium
	// ── high ──────────────────────────────────────────────────
	// Claude Code / API default: high · Codex: High / Proactive / standard
	// (Ultra also folds to high upstream via ultracode below)
	case ClientHigh, "standard", "std", "h", "hard", "deep", "proactive":
		return ClientHigh
	// ── xhigh ─────────────────────────────────────────────────
	// Claude Code / API: xhigh · Codex: extra-high
	case ClientXHigh, "x-high", "extra-high", "extrahigh", "extra",
		"highest", "maxx":
		return ClientXHigh
	// ── max ───────────────────────────────────────────────────
	// Anthropic API top effort (absolute maximum capability).
	case ClientMax, "maximum", "maxi":
		return ClientMax
	// ── ultracode ─────────────────────────────────────────────
	// Claude Code UI mode + Codex Ultra — preserve label for usage detail;
	// ToUpstream folds both to high for Grok.
	case ClientUltracode, "ultra-code", "ultra", "ultra-high", "ultrahigh":
		return ClientUltracode
	}
	if strings.HasPrefix(s, "extra") && strings.Contains(s, "high") {
		return ClientXHigh
	}
	if strings.Contains(s, "ultra") {
		return ClientUltracode
	}
	// Unknown non-empty labels: do not pass garbage.
	return ""
}

// ToUpstream folds a client label onto Grok's low|medium|high.
//
//	low                          → low
//	medium (incl. Codex Base)    → medium
//	high (incl. Codex Proactive) → high
//	xhigh | max | ultracode (incl. Codex Ultra) → high
func ToUpstream(client string) string {
	switch NormalizeClient(client) {
	case ClientLow:
		return Low
	case ClientMedium:
		return Medium
	case ClientHigh, ClientXHigh, ClientMax, ClientUltracode:
		return High
	default:
		return ""
	}
}

// BudgetToClient maps Claude-style thinking.budget_tokens onto client effort labels.
// Large budgets map to xhigh/max so admin usage can distinguish them even though
// Grok only receives high.
func BudgetToClient(n int) string {
	if n <= 0 {
		return ""
	}
	if n <= 2048 {
		return ClientLow
	}
	if n <= 8192 {
		return ClientMedium
	}
	if n <= 32000 {
		return ClientHigh
	}
	if n <= 100000 {
		return ClientXHigh
	}
	return ClientMax
}

// BudgetToLevel maps budgets onto Grok's 3 tiers (upstream).
func BudgetToLevel(n int) string {
	return ToUpstream(BudgetToClient(n))
}

// FromRequest extracts a client-facing effort label from a chat/completions,
// Messages, or Responses-shaped body.
//
// Sources (priority):
//  1. reasoning_effort / thinking_effort / effort / thinking_intensity
//  2. output_config.effort  (modern Anthropic / Claude Code)
//  3. reasoning.effort
//  4. thinking / thinking.budget_tokens / thinking.effort
//  5. text.* nested (Responses text config)
func FromRequest(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	for _, key := range []string{"reasoning_effort", "thinking_effort", "effort", "thinking_intensity"} {
		if v, ok := raw[key]; ok && v != nil {
			if got := NormalizeClient(v); got != "" {
				return got
			}
		}
	}
	// Claude API / Claude Code: output_config.effort
	if oc, ok := raw["output_config"].(map[string]any); ok && oc != nil {
		if v, ok := oc["effort"]; ok && v != nil {
			if got := NormalizeClient(v); got != "" {
				return got
			}
		}
	}
	if v, ok := raw["reasoning"]; ok && v != nil {
		if got := NormalizeClient(v); got != "" {
			return got
		}
	}
	if v, ok := raw["thinking"]; ok && v != nil {
		if got := NormalizeClient(v); got != "" {
			return got
		}
	}
	if text, ok := raw["text"].(map[string]any); ok {
		if got := FromRequest(text); got != "" {
			return got
		}
	}
	return ""
}

// FromRequestUpstream is FromRequest folded to Grok low|medium|high.
func FromRequestUpstream(raw map[string]any) string {
	return ToUpstream(FromRequest(raw))
}

// ApplyCanonical writes a Grok-safe reasoning_effort (low|medium|high) into body
// when a client effort is present. Returns the upstream level (never xhigh/max/ultracode).
func ApplyCanonical(body map[string]any) string {
	if body == nil {
		return ""
	}
	client := FromRequest(body)
	if client == "" {
		if v, ok := body["reasoning_effort"]; ok {
			client = NormalizeClient(v)
		}
	}
	up := ToUpstream(client)
	if up == "" {
		return ""
	}
	body["reasoning_effort"] = up
	return up
}

// ClientLabels is the ordered set of Claude Code / Anthropic effort labels.
func ClientLabels() []string {
	return []string{
		ClientLow, ClientMedium, ClientHigh, ClientXHigh, ClientMax, ClientUltracode,
	}
}

// IsClientLabel reports whether s is a known client effort label.
func IsClientLabel(s string) bool {
	switch NormalizeClient(s) {
	case ClientLow, ClientMedium, ClientHigh, ClientXHigh, ClientMax, ClientUltracode:
		return true
	default:
		return false
	}
}
