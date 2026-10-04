package postgres

import (
	"testing"
)

func TestCompactQuotaSnapshotExhausted(t *testing.T) {
	snap := compactQuotaSnapshot("acc-1", map[string]any{
		"ok":            true,
		"exhausted":     true,
		"auto_disabled": true,
		"used":          10.0,
		"monthly_limit": 10.0,
		"email":         "a@b.c",
		"display":       map[string]any{"summary": "$10.00 / $10.00"},
		"source":        "billing",
	})
	if snap["account_id"] != "acc-1" {
		t.Fatalf("account_id=%v", snap["account_id"])
	}
	if !truthyAny(snap["exhausted"]) {
		t.Fatal("expected exhausted")
	}
	if snap["summary"] != "$10.00 / $10.00" {
		t.Fatalf("summary=%v", snap["summary"])
	}
	if snap["source"] != "billing" {
		t.Fatalf("source=%v", snap["source"])
	}
}

func TestCompactQuotaSnapshotHealthy(t *testing.T) {
	snap := compactQuotaSnapshot("acc-2", map[string]any{
		"ok":            true,
		"exhausted":     false,
		"used":          1.0,
		"monthly_limit": 20.0,
		"fetched_at":    int64(123),
	})
	if !truthyAny(snap["ok"]) {
		t.Fatal("expected ok")
	}
	if truthyAny(snap["exhausted"]) {
		t.Fatal("expected not exhausted")
	}
	if snap["fetched_at"] != int64(123) {
		t.Fatalf("fetched_at=%v", snap["fetched_at"])
	}
}

func TestTruthyAny(t *testing.T) {
	if !truthyAny(true) || truthyAny(false) {
		t.Fatal("bool")
	}
	if !truthyAny("true") || !truthyAny("YES") || truthyAny("no") {
		t.Fatal("string")
	}
	if !truthyAny(1) || truthyAny(0) {
		t.Fatal("int")
	}
}

func TestFirstNonEmptyString(t *testing.T) {
	if got := firstNonEmptyString("", "  ", "x", "y"); got != "x" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmptyString(); got != "" {
		t.Fatalf("empty got %q", got)
	}
}

func TestMergeQuotaSnapshotsKeepsTypeAndUsageOnFailedProbe(t *testing.T) {
	prev := map[string]any{
		"ok":                   true,
		"account_id":           "acc-1",
		"account_type":         "free",
		"plan":                 "free",
		"plan_label":           "Free",
		"tokens_limit":         int64(2_000_000),
		"tokens_used":          int64(100_000),
		"tokens_remaining":     int64(1_900_000),
		"tokens_usage_percent": 5.0,
		"source":               "free_tokens",
		"fetched_at":           int64(1000),
		"summary":              "token 100000 / 2000000",
		"display":              map[string]any{"summary": "token 100000 / 2000000"},
	}
	// Failed live probe: only error — used to wipe last_quota and blank the UI on refresh.
	next := compactQuotaSnapshot("acc-1", map[string]any{
		"ok":         false,
		"account_id": "acc-1",
		"error":      "billing HTTP 502: bad gateway",
		"fetched_at": int64(2000),
		"source":     "billing",
	})
	merged := mergeQuotaSnapshots(prev, next)
	if merged["account_type"] != "free" {
		t.Fatalf("account_type lost: %#v", merged["account_type"])
	}
	if merged["plan_label"] != "Free" {
		t.Fatalf("plan_label lost: %#v", merged["plan_label"])
	}
	if merged["tokens_limit"] != int64(2_000_000) && merged["tokens_limit"] != float64(2_000_000) {
		// JSON numbers may be float64 after round-trip; here still int64 from prev.
		if v, ok := merged["tokens_limit"].(int64); !ok || v != 2_000_000 {
			if vf, ok := merged["tokens_limit"].(float64); !ok || vf != 2_000_000 {
				t.Fatalf("tokens_limit lost: %#v", merged["tokens_limit"])
			}
		}
	}
	if merged["tokens_used"] != int64(100_000) && merged["tokens_used"] != float64(100_000) {
		if v, ok := merged["tokens_used"].(int64); !ok || v != 100_000 {
			if vf, ok := merged["tokens_used"].(float64); !ok || vf != 100_000 {
				t.Fatalf("tokens_used lost: %#v", merged["tokens_used"])
			}
		}
	}
	if err := stringFromAny(merged["error"]); err == "" {
		t.Fatal("expected error retained from failed probe")
	}
	// Still paint as previously-ok for hydrate (type + usage visible).
	if !truthyAny(merged["ok"]) {
		t.Fatalf("ok should stay true from prev when new probe failed: %#v", merged["ok"])
	}
	if merged["fetched_at"] != int64(2000) {
		t.Fatalf("fetched_at should update to new probe: %#v", merged["fetched_at"])
	}
}

func TestMergeQuotaSnapshotsFreshTypeWins(t *testing.T) {
	prev := map[string]any{
		"ok": true, "account_type": "free", "plan": "free", "plan_label": "Free",
		"tokens_limit": int64(100), "tokens_used": int64(10),
	}
	next := map[string]any{
		"ok": true, "account_type": "supergrok", "plan": "supergrok", "plan_label": "SuperGrok",
		"monthly_limit": 25.0, "used": 1.0, "remaining": 24.0, "fetched_at": int64(9),
	}
	merged := mergeQuotaSnapshots(prev, next)
	if merged["account_type"] != "supergrok" {
		t.Fatalf("fresh plan should win: %#v", merged["account_type"])
	}
	if merged["monthly_limit"] != 25.0 {
		t.Fatalf("monthly_limit=%#v", merged["monthly_limit"])
	}
}

func TestMergeQuotaSnapshotsDoesNotDemoteToUnknown(t *testing.T) {
	prev := map[string]any{"account_type": "supergrok", "plan": "supergrok", "plan_label": "SuperGrok", "ok": true}
	next := map[string]any{"ok": true, "account_type": "unknown", "plan": "unknown", "plan_label": "未知", "used": 0.0}
	merged := mergeQuotaSnapshots(prev, next)
	if merged["account_type"] != "supergrok" {
		t.Fatalf("unknown must not demote: %#v", merged["account_type"])
	}
}

func TestShouldSkipQuotaWrite(t *testing.T) {
	if !shouldSkipQuotaWrite(map[string]any{"ok": false, "error": "billing HTTP 502"}) {
		t.Fatal("error-only should skip")
	}
	// plan=free alone after connection stampede must NOT write / clobber usage.
	if !shouldSkipQuotaWrite(map[string]any{
		"ok": false, "plan": "free", "account_type": "free",
		"error": "Too many open connections", "source": "billing",
	}) {
		t.Fatal("error+plan free without usage must skip")
	}
	if shouldSkipQuotaWrite(map[string]any{"ok": true, "account_type": "free", "tokens_limit": int64(1)}) {
		t.Fatal("usable free snap must write")
	}
	if shouldSkipQuotaWrite(map[string]any{"ok": false, "account_type": "free", "tokens_used": int64(1), "error": "x"}) {
		t.Fatal("merged type+usage must write even with error")
	}
	if shouldSkipQuotaWrite(map[string]any{"ok": true, "exhausted": true, "summary": "额度耗尽"}) {
		t.Fatal("exhausted must write")
	}
}

func TestMergeThenSkipLogic(t *testing.T) {
	prev := map[string]any{
		"ok": true, "account_type": "supergrok", "plan": "supergrok",
		"monthly_limit": 25.0, "used": 2.0, "summary": "$2 / $25",
	}
	next := compactQuotaSnapshot("a", map[string]any{
		"ok": false, "error": "timeout", "fetched_at": int64(9),
	})
	merged := mergeQuotaSnapshots(prev, next)
	if shouldSkipQuotaWrite(merged) {
		t.Fatalf("merged healthy history must not skip write: %#v", merged)
	}
	if merged["account_type"] != "supergrok" {
		t.Fatalf("type=%v", merged["account_type"])
	}
}

func TestSanitizeLastQuotaForAPIDropsErrorShell(t *testing.T) {
	if SanitizeLastQuotaForAPI(map[string]any{
		"ok": false, "error": "Too many open connections", "plan": "free",
	}) != nil {
		t.Fatal("error shell must be hidden from list API")
	}
	good := map[string]any{"ok": true, "plan": "free", "tokens_limit": float64(1e6), "tokens_used": float64(0)}
	if SanitizeLastQuotaForAPI(good) == nil {
		t.Fatal("good snap must pass through")
	}
}
