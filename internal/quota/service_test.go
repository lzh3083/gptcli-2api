package quota

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSyntheticPoolFromQuotaNoJSONCycle(t *testing.T) {
	item := map[string]any{
		"ok":         true,
		"account_id": "acc-1",
		"exhausted":  false,
		"source":     "billing",
	}
	pool := syntheticPoolFromQuota("acc-1", item)
	if pool == nil {
		t.Fatal("pool is nil")
	}
	if _, ok := pool["last_quota"]; ok {
		t.Fatal("synthetic pool must not embed last_quota (JSON cycle risk)")
	}
	item["pool"] = pool
	if _, err := json.Marshal(item); err != nil {
		t.Fatalf("marshal cyclic risk: %v", err)
	}

	// Exhausted path
	bad := map[string]any{
		"ok":             true,
		"exhausted":      true,
		"exhaust_reason": "quota empty",
		"source":         "billing",
	}
	pool2 := syntheticPoolFromQuota("acc-2", bad)
	bad["pool"] = pool2
	bad["auto_disabled"] = true
	if _, err := json.Marshal(bad); err != nil {
		t.Fatalf("exhausted marshal: %v", err)
	}
	if pool2["pool_status"] != "cooldown" {
		t.Fatalf("pool_status=%v want cooldown", pool2["pool_status"])
	}
	if pool2["disabled_for_quota"] == true {
		t.Fatalf("disabled_for_quota should be false (cool, not permanent disable)")
	}
	if pool2["in_cooldown"] != true {
		t.Fatalf("in_cooldown=%v", pool2["in_cooldown"])
	}
}

func TestSyntheticPoolFromQuotaNilItem(t *testing.T) {
	pool := syntheticPoolFromQuota("acc-x", nil)
	if pool["id"] != "acc-x" {
		t.Fatalf("id=%v", pool["id"])
	}
	if _, err := json.Marshal(pool); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeBillingFree(t *testing.T) {
	raw := map[string]any{
		"config": map[string]any{
			"monthlyLimit": map[string]any{"val": 0},
			"used":         map[string]any{"val": 0},
			"onDemandCap":  map[string]any{"val": 0},
		},
	}
	n := normalizeBilling(raw)
	if n["unlimited_or_free"] != true {
		t.Fatalf("want unlimited_or_free, got %#v", n)
	}
	if n["exhausted"] == true {
		t.Fatal("free $0 should not exhaust from billing alone")
	}
}

func TestParseTokenPair(t *testing.T) {
	a, b, ok := parseTokenPair("tokens (actual/limit): 2368681/2000000")
	if !ok || a != 2368681 || b != 2000000 {
		t.Fatalf("got %d/%d ok=%v", a, b, ok)
	}
	a, b, ok = parseTokenPair("no tokens here")
	if ok {
		t.Fatalf("unexpected %d/%d", a, b)
	}
}

func TestBuildQuotaDisplayFreeTokens(t *testing.T) {
	d := buildQuotaDisplay(map[string]any{
		"free_tokens":          true,
		"tokens_used":          int64(120000),
		"tokens_limit":         int64(1000000),
		"tokens_remaining":     int64(880000),
		"tokens_usage_percent": 12.0,
		"requests_limit":       int64(21),
		"requests_remaining":   int64(18),
	})
	sum, _ := d["summary"].(string)
	if sum == "" || !strings.Contains(sum, "token") {
		t.Fatalf("summary=%q", sum)
	}
}

func TestClassifyAccountPlan(t *testing.T) {
	free := classifyAccountPlan(map[string]any{
		"monthly_limit":     0.0,
		"unlimited_or_free": true,
		"free_tokens":       true,
		"tokens_limit":      int64(1_000_000),
		"probe_model":       "grok-4.5-build-free",
	}, map[string]any{"has_grok_code_access": true})
	if free["account_type"] != "free" {
		t.Fatalf("free got %#v", free)
	}
	paid := classifyAccountPlan(map[string]any{
		"monthly_limit": 30.0,
		"used":          1.0,
	}, nil)
	if paid["account_type"] != "supergrok" {
		t.Fatalf("supergrok got %#v", paid)
	}
	team := classifyAccountPlan(map[string]any{
		"monthly_limit": 0.0,
	}, map[string]any{"team_id": "t1", "organization_type": "team"})
	if team["account_type"] != "team" {
		t.Fatalf("team got %#v", team)
	}
}

func TestNormalizeBillingWeekly(t *testing.T) {
	raw := map[string]any{
		"config": map[string]any{
			"monthlyLimit": map[string]any{"val": 30.0},
			"used":         map[string]any{"val": 12.0},
			"weeklyLimit":  map[string]any{"val": 10.0},
			"weeklyUsed":   map[string]any{"val": 4.0},
			"onDemandCap":  map[string]any{"val": 50.0},
			"onDemandUsed": map[string]any{"val": 1.0},
		},
	}
	n := normalizeBilling(raw)
	if n["unlimited_or_free"] == true {
		t.Fatalf("paid should not be free: %#v", n)
	}
	if floatOf(n["weekly_limit"]) != 10 {
		t.Fatalf("weekly_limit=%v", n["weekly_limit"])
	}
	if floatOf(n["weekly_used"]) != 4 {
		t.Fatalf("weekly_used=%v", n["weekly_used"])
	}
	sum := formatSuperGrokSummary(n)
	if sum == "" || !strings.Contains(sum, "月") || !strings.Contains(sum, "周") {
		t.Fatalf("summary=%q", sum)
	}
}

func TestNormalizeTokenUsageFieldsFull(t *testing.T) {
	out := map[string]any{
		"tokens_limit":     int64(1000),
		"tokens_remaining": int64(0),
	}
	normalizeTokenUsageFields(out)
	if out["tokens_used"] != int64(1000) {
		t.Fatalf("used=%v", out["tokens_used"])
	}
	if out["exhausted"] != true {
		t.Fatal("want exhausted when remaining 0")
	}
}
