package config

import (
	"testing"
	"time"
)

func TestApplyStoreSettingsOverlaysLiveFields(t *testing.T) {
	cfg := Config{
		DefaultModel:     "env-model",
		SSEKeepalive:     4 * time.Second,
		OutboundMaxTools: 1,
	}
	cfg.ApplyStoreSettings(map[string]any{
		"default_model":             "db-model",
		"sse_keepalive":             float64(12),
		"outbound_max_tools":        float64(3),
		"outbound_max_tools_openai": float64(5),
		"outbound_tool_gap_sec":     float64(0.2),
		// ignored / maintainer-only
		"token_maintain_enabled": true,
	})
	if cfg.DefaultModel != "db-model" {
		t.Fatalf("DefaultModel=%q", cfg.DefaultModel)
	}
	if cfg.SSEKeepalive != 12*time.Second {
		t.Fatalf("SSEKeepalive=%s", cfg.SSEKeepalive)
	}
	if cfg.OutboundMaxTools != 3 {
		t.Fatalf("OutboundMaxTools=%d", cfg.OutboundMaxTools)
	}
	if cfg.OutboundMaxToolsOpenAI != 5 {
		t.Fatalf("OutboundMaxToolsOpenAI=%d", cfg.OutboundMaxToolsOpenAI)
	}
	if cfg.OutboundToolGap != 200*time.Millisecond {
		t.Fatalf("OutboundToolGap=%s", cfg.OutboundToolGap)
	}
}

func TestApplyStoreSettingsIgnoresInvalid(t *testing.T) {
	cfg := Config{DefaultModel: "keep", SSEKeepalive: 4 * time.Second, OutboundMaxTools: 1}
	cfg.ApplyStoreSettings(map[string]any{
		"default_model":      "   ",
		"sse_keepalive":      float64(1),  // below min
		"outbound_max_tools": float64(99), // above max
	})
	if cfg.DefaultModel != "keep" {
		t.Fatalf("empty model should not overwrite: %q", cfg.DefaultModel)
	}
	if cfg.SSEKeepalive != 4*time.Second {
		t.Fatalf("invalid keepalive applied: %s", cfg.SSEKeepalive)
	}
	if cfg.OutboundMaxTools != 1 {
		t.Fatalf("invalid max tools applied: %d", cfg.OutboundMaxTools)
	}
}
