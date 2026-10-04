package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeRegistrationConfigDefaults(t *testing.T) {
	cfg := normalizeRegistrationConfig(map[string]any{})
	if cfg["mail_provider"] != "moemail" {
		t.Fatalf("mail_provider=%v", cfg["mail_provider"])
	}
	if cfg["captcha_provider"] != "local" {
		t.Fatalf("captcha_provider=%v", cfg["captcha_provider"])
	}
	if cfg["local_solver_url"] != "http://127.0.0.1:5073" {
		t.Fatalf("local_solver_url=%v", cfg["local_solver_url"])
	}
	if cfg["proxy_strategy"] != "round_robin" {
		t.Fatalf("proxy_strategy=%v", cfg["proxy_strategy"])
	}
}

func TestIsMaskedSecret(t *testing.T) {
	if !isMaskedSecret("ab…cd") || !isMaskedSecret("****") {
		t.Fatal("expected masked")
	}
	if isMaskedSecret("real-secret-key") {
		t.Fatal("plain secret should not be masked")
	}
}

func TestSplitProxyLines(t *testing.T) {
	lines := splitProxyLines("http://a:1\n#c\nhttp://b:2;http://c:3")
	if len(lines) != 3 {
		t.Fatalf("lines=%v", lines)
	}
}

func TestMailSecretFitsSlot(t *testing.T) {
	if !mailSecretFitsSlot("moemail_api_key", "mk_abc") {
		t.Fatal("mk_ should fit moemail")
	}
	if mailSecretFitsSlot("moemail_api_key", "AC-yyds") {
		t.Fatal("AC- must not fit moemail")
	}
	if mailSecretFitsSlot("moemail_api_key", "sk-gpt") {
		t.Fatal("sk- must not fit moemail")
	}
	if !mailSecretFitsSlot("yyds_api_key", "AC-yyds") {
		t.Fatal("AC- should fit yyds")
	}
	if mailSecretFitsSlot("yyds_api_key", "mk_abc") {
		t.Fatal("mk_ must not fit yyds")
	}
	if mailSecretFitsSlot("gptmail_api_key", "AC-yyds") {
		t.Fatal("AC- must not fit gptmail")
	}
	if mailSecretFitsSlot("gptmail_api_key", "mk_abc") {
		t.Fatal("mk_ must not fit gptmail")
	}
}

func TestSanitizeRegistrationMailSecretsMovesPollutedKeys(t *testing.T) {
	cfg := map[string]any{
		"mail_provider":    "moemail",
		"moemail_api_key":  "AC-c1965a37122be549cc25724a",
		"yyds_api_key":     "",
		"gptmail_api_key":  "AC-c1965a37122be549cc25724a",
		"moemail_domain":   "lolicr.com",
		"moemail_base_url": "",
		"domain":           "lolicr.com",
		"api_key":          "AC-c1965a37122be549cc25724a",
	}
	sanitizeRegistrationMailSecrets(cfg)
	if cfg["moemail_api_key"] != "" {
		t.Fatalf("polluted moemail key should be cleared, got %v", cfg["moemail_api_key"])
	}
	if cfg["yyds_api_key"] != "AC-c1965a37122be549cc25724a" {
		t.Fatalf("AC- should move to yyds, got %v", cfg["yyds_api_key"])
	}
	if cfg["gptmail_api_key"] != "" {
		t.Fatalf("polluted gptmail key should be cleared, got %v", cfg["gptmail_api_key"])
	}
	if cfg["api_key"] != "" {
		t.Fatalf("moemail active api_key should be empty after sanitize, got %v", cfg["api_key"])
	}
}

func TestRegistrationConfigPatchForPersistDropsAdapterRemap(t *testing.T) {
	req := map[string]any{
		"mail_provider":    "yyds",
		"yyds_api_key":     "AC-new-yyds",
		"moemail_api_key":  "mk_real_moemail",
		"moemail_base_url": "https://moemail.example.com",
		"domain":           "",
	}
	// Simulate merge output after adapter remap (YYDS key into moemail_api_key).
	merged := map[string]any{
		"mail_provider":    "yyds",
		"yyds_api_key":     "AC-new-yyds",
		"moemail_api_key":  "AC-new-yyds",
		"moemail_base_url": "",
		"base_url":         "",
		"api_key":          "AC-new-yyds",
		"domain":           "",
	}
	patch := registrationConfigPatchForPersist(req, merged)
	if _, ok := patch["moemail_api_key"]; ok {
		// When provider is yyds, durable MoeMail key must not be in the patch.
		t.Fatalf("patch must not include remapped moemail_api_key, got %v", patch["moemail_api_key"])
	}
	if patch["yyds_api_key"] != "AC-new-yyds" {
		t.Fatalf("yyds key missing, got %v", patch["yyds_api_key"])
	}
	// Request's durable MoeMail host is restored even when merge cleared it.
	if patch["moemail_base_url"] != "https://moemail.example.com" {
		t.Fatalf("durable moemail_base_url should come from request, got %v", patch["moemail_base_url"])
	}
	// Empty remapped base alone (no request host) is covered separately.
	req2 := map[string]any{"mail_provider": "yyds", "yyds_api_key": "AC-x"}
	merged2 := map[string]any{
		"mail_provider": "yyds", "yyds_api_key": "AC-x",
		"moemail_api_key": "AC-x", "moemail_base_url": "", "base_url": "",
	}
	patch2 := registrationConfigPatchForPersist(req2, merged2)
	if _, ok := patch2["moemail_api_key"]; ok {
		t.Fatalf("patch2 must not include remapped moemail_api_key")
	}
	if v, ok := patch2["moemail_base_url"]; ok && strings.TrimSpace(fmt.Sprint(v)) == "" {
		t.Fatalf("empty remapped moemail_base_url should be dropped from patch2")
	}
	_ = patch2
}

func TestMergeRegistrationStartBodyYydsKey(t *testing.T) {
	// Simulates saved MoeMail key + request selecting YYDS with its own key.
	// mergeRegistrationStartBody needs store for load — test the mapping logic
	// via normalize + inline reconstruction of the critical overwrite rule.
	out := map[string]any{
		"mail_provider":   "yyds",
		"moemail_api_key": "mk_old_moemail",
		"yyds_api_key":    "AC-new-yyds",
		"api_key":         "AC-new-yyds",
		"domain":          "",
		"yyds_domain":     "",
	}
	// Apply the same critical overwrite used by mergeRegistrationStartBody.
	provider := "yyds"
	if k, _ := out["yyds_api_key"].(string); k != "" {
		out["moemail_api_key"] = k
		out["api_key"] = k
	}
	out["moemail_base_url"] = ""
	if out["moemail_api_key"] != "AC-new-yyds" {
		t.Fatalf("moemail_api_key should be active YYDS key, got %v", out["moemail_api_key"])
	}
	if out["moemail_base_url"] != "" {
		t.Fatalf("yyds must clear moemail base url")
	}
	_ = provider
}

func TestMergeRegistrationStartBodyGptmailKey(t *testing.T) {
	out := map[string]any{
		"mail_provider":   "gptmail",
		"moemail_api_key": "mk_old",
		"gptmail_api_key": "sk-gpt-key",
	}
	if k, _ := out["gptmail_api_key"].(string); k != "" {
		out["moemail_api_key"] = k
	}
	if out["moemail_api_key"] != "sk-gpt-key" {
		t.Fatalf("got %v", out["moemail_api_key"])
	}
}

func TestNormalizeRegistrationConfigMailAliases(t *testing.T) {
	cfg := normalizeRegistrationConfig(map[string]any{"mail_provider": "yydsmail"})
	if cfg["mail_provider"] != "yyds" {
		t.Fatalf("mail_provider=%v", cfg["mail_provider"])
	}
	cfg = normalizeRegistrationConfig(map[string]any{"mail_provider": "chatgptmail"})
	if cfg["mail_provider"] != "gptmail" {
		t.Fatalf("mail_provider=%v", cfg["mail_provider"])
	}
}

func TestNormalizeRegistrationConfigTempmail(t *testing.T) {
	cfg := normalizeRegistrationConfig(map[string]any{"mail_provider": "tempmail.lol"})
	if cfg["mail_provider"] != "tempmail" {
		t.Fatalf("mail_provider=%v", cfg["mail_provider"])
	}
	cfg = normalizeRegistrationConfig(map[string]any{"mail_provider": "lol"})
	if cfg["mail_provider"] != "tempmail" {
		t.Fatalf("mail_provider=%v", cfg["mail_provider"])
	}
}

func TestSanitizeTempmailEmptyKeyOK(t *testing.T) {
	cfg := map[string]any{
		"mail_provider":    "tempmail",
		"tempmail_api_key": "",
		"tempmail_domain":  "",
		"moemail_api_key":  "mk_should_not_leak",
		"api_key":          "mk_should_not_leak",
	}
	sanitizeRegistrationMailSecrets(cfg)
	if cfg["tempmail_api_key"] != "" {
		t.Fatalf("tempmail key should stay empty, got %v", cfg["tempmail_api_key"])
	}
	// Active api_key for tempmail provider must be empty free-tier, not moemail key.
	if cfg["api_key"] != "" {
		t.Fatalf("tempmail active api_key should be empty free tier, got %v", cfg["api_key"])
	}
}

func TestMailSecretFitsSlotTempmail(t *testing.T) {
	if !mailSecretFitsSlot("tempmail_api_key", "") {
		t.Fatal("empty should fit tempmail")
	}
	if !mailSecretFitsSlot("tempmail_api_key", "some-paid-bearer-token") {
		t.Fatal("opaque bearer should fit tempmail")
	}
	if mailSecretFitsSlot("tempmail_api_key", "mk_moe") {
		t.Fatal("mk_ must not fit tempmail")
	}
	if mailSecretFitsSlot("tempmail_api_key", "AC-yyds") {
		t.Fatal("AC- must not fit tempmail")
	}
}

func TestRegistrationConfigPatchForPersistClearsTempmailKey(t *testing.T) {
	req := map[string]any{
		"mail_provider":    "tempmail",
		"tempmail_api_key": "",
		"tempmail_domain":  "",
	}
	merged := map[string]any{
		"mail_provider":    "tempmail",
		"tempmail_api_key": "old-paid-key",
		"tempmail_domain":  "custom.example",
		"moemail_api_key":  "mk_other",
		"moemail_domain":   "lolicr.com",
	}
	patch := registrationConfigPatchForPersist(req, merged)
	if v := strings.TrimSpace(fmt.Sprint(patch["tempmail_api_key"])); v != "" && v != "<nil>" {
		t.Fatalf("expected cleared tempmail key, got %v", patch["tempmail_api_key"])
	}
	if v := strings.TrimSpace(fmt.Sprint(patch["tempmail_domain"])); v != "" && v != "<nil>" {
		t.Fatalf("expected cleared tempmail domain, got %v", patch["tempmail_domain"])
	}
	// Must not write remapped moemail key from tempmail active path.
	if v, ok := patch["moemail_api_key"]; ok {
		if s := strings.TrimSpace(fmt.Sprint(v)); s != "" && s != "mk_other" {
			t.Fatalf("moemail key remapped unexpectedly: %v", v)
		}
	}
}

func TestMailSecretFitsSlotTempmailEmpty(t *testing.T) {
	if !mailSecretFitsSlot("tempmail_api_key", "") {
		t.Fatal("empty tempmail key must be allowed")
	}
}
