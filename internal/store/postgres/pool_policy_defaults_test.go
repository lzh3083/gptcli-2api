package postgres

import "testing"

func TestDefaultPoolPolicyHasAdminUIKeys(t *testing.T) {
	p := defaultPoolPolicy()
	required := []string{
		"cooldown_default_sec", "cooldown_auth_sec", "cooldown_rate_limit_sec",
		"cooldown_server_error_sec", "cooldown_max_sec", "soft_model_block_ttl_sec",
		"durable_model_block_ttl_sec", "probe_fail_kick_streak", "probe_fail_disable_streak",
		"probe_kick_cooldown_sec", "max_failover_attempts",
	}
	for _, k := range required {
		if _, ok := p[k]; !ok {
			t.Fatalf("missing %s in defaultPoolPolicy", k)
		}
	}
	if p["cooldown_default_sec"] != float64(20) {
		t.Fatalf("default cooldown=%v", p["cooldown_default_sec"])
	}
	if p["max_failover_attempts"] != int64(4) {
		t.Fatalf("max_failover=%v", p["max_failover_attempts"])
	}
}
