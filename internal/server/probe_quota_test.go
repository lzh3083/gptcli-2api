package server

import (
	"context"
	"testing"
)

func TestAttachQuotaAfterProbeNilSafe(t *testing.T) {
	// No Quota service → no panic, nil snap.
	pool := map[string]any{"id": "a1"}
	got := attachQuotaAfterProbe(context.Background(), Options{}, "a1", pool)
	if got != nil {
		t.Fatalf("expected nil without Quota service, got %#v", got)
	}
	if pool["last_quota"] != nil {
		t.Fatalf("pool should stay unchanged: %#v", pool)
	}
	attachQuotasAfterProbeBatch(context.Background(), Options{}, []map[string]any{
		{"account_id": "a1", "pool": map[string]any{}},
	})
}

func TestAttachQuotasAfterProbeBatchKeepsExisting(t *testing.T) {
	existing := map[string]any{"account_type": "free", "tokens_limit": float64(100)}
	item := map[string]any{
		"account_id": "acc",
		"pool":       map[string]any{"last_quota": existing},
	}
	attachQuotasAfterProbeBatch(context.Background(), Options{}, []map[string]any{item})
	// Without Quota service, should still promote type from existing snap.
	if item["account_type"] != "free" {
		t.Fatalf("expected account_type promoted from last_quota, got %#v", item["account_type"])
	}
}
