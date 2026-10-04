package postgres

import "testing"

func TestUsageFromOpenAICacheAliases(t *testing.T) {
	prompt, completion, total, cacheRead, _, reasoning := UsageFromOpenAI(map[string]any{
		"prompt_tokens":     100,
		"completion_tokens": 10,
		"total_tokens":      110,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 40,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": 7,
		},
	})
	if prompt != 100 || completion != 10 || total != 110 {
		t.Fatalf("basic tokens %#v %#v %#v", prompt, completion, total)
	}
	if cacheRead != 40 {
		t.Fatalf("cacheRead=%d", cacheRead)
	}
	if reasoning != 7 {
		t.Fatalf("reasoning=%d", reasoning)
	}
	// anthropic-ish aliases
	_, _, _, cr, cc, _ := UsageFromOpenAI(map[string]any{
		"input_tokens":                50,
		"output_tokens":               5,
		"cache_read_input_tokens":     12,
		"cache_creation_input_tokens": 3,
	})
	if cr != 12 || cc != 3 {
		t.Fatalf("alias cache cr=%d cc=%d", cr, cc)
	}
}

func TestUsageTotalsMapWithRateBilled(t *testing.T) {
	u := usageTotals{
		Requests: 2, Success: 2, Fail: 0,
		PromptTokens: 1000, CompletionTokens: 50, TotalTokens: 1050,
		CacheReadTokens: 400,
	}
	m := u.mapWithRate()
	if m["total_tokens"] != int64(650) {
		t.Fatalf("billed total_tokens=%v want 650", m["total_tokens"])
	}
	if m["billed_tokens"] != int64(650) {
		t.Fatalf("billed_tokens=%v", m["billed_tokens"])
	}
	if m["prompt_tokens_billed"] != int64(600) {
		t.Fatalf("prompt_tokens_billed=%v", m["prompt_tokens_billed"])
	}
	if m["total_tokens_raw"] != int64(1050) {
		t.Fatalf("raw=%v", m["total_tokens_raw"])
	}
	if m["cache_read_tokens"] != int64(400) {
		t.Fatalf("cache=%v", m["cache_read_tokens"])
	}
	// never negative
	u2 := usageTotals{TotalTokens: 10, CacheReadTokens: 50}
	if billedTokens(u2) != 0 {
		t.Fatalf("billed negative case %d", billedTokens(u2))
	}
}
