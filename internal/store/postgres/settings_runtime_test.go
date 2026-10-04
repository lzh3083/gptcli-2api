package postgres

import (
	"testing"
)

func TestOutboundProxyPoolSummary(t *testing.T) {
	sum := outboundProxyPoolSummary(map[string]any{
		"enabled":        true,
		"proxy":          "http://a:1\nhttp://b:2\n#c\n",
		"proxy_strategy": "random",
	})
	if sum["count"] != 2 {
		t.Fatalf("count=%v want 2", sum["count"])
	}
	if sum["strategy"] != "random" {
		t.Fatalf("strategy=%v", sum["strategy"])
	}
	if sum["source"] != "settings" {
		t.Fatalf("source=%v", sum["source"])
	}
	sum2 := outboundProxyPoolSummary(map[string]any{"enabled": false})
	if sum2["enabled"] != false || sum2["count"] != 0 {
		t.Fatalf("empty summary=%v", sum2)
	}
}

func TestAsSettingsHelpers(t *testing.T) {
	// Compile-time smoke: ApplyStoreSettings lives in config package.
}
