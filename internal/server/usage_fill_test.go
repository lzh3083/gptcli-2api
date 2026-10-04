package server

import "testing"

func TestFillMissingUsageEstimatesPromptAndCompletion(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello world this is a test prompt for token estimate"},
		},
	}
	// No upstream usage, completion hint from streamer.
	filled, flags := fillMissingUsage(nil, body, 12)
	if !flags.Missing || !flags.EstimatedCompletion {
		t.Fatalf("flags=%+v", flags)
	}
	if filled["completion_tokens"] != int64(12) && filled["completion_tokens"] != 12 {
		// fillMissingUsage stores int64
		if v, ok := filled["completion_tokens"].(int64); !ok || v != 12 {
			t.Fatalf("completion=%T %v", filled["completion_tokens"], filled["completion_tokens"])
		}
	}
	pt, _ := filled["prompt_tokens"].(int64)
	if pt <= 0 {
		t.Fatalf("expected estimated prompt, got %v", filled["prompt_tokens"])
	}
	tot, _ := filled["total_tokens"].(int64)
	if tot != pt+12 {
		t.Fatalf("total=%v prompt=%v", tot, pt)
	}
	// Preserve real upstream values (map identity + no estimate flags).
	real := map[string]any{"prompt_tokens": float64(100), "completion_tokens": float64(5), "total_tokens": float64(105)}
	filled2, flags2 := fillMissingUsage(real, body, 99)
	if flags2.EstimatedCompletion || flags2.EstimatedPrompt || flags2.Missing {
		t.Fatalf("should not overwrite real usage: %+v map=%v", flags2, filled2)
	}
	if filled2["total_tokens"] != float64(105) {
		t.Fatalf("prompt/total type changed: %#v", filled2)
	}
}

func TestFillMissingUsageResponsesInput(t *testing.T) {
	body := map[string]any{
		"instructions": "You are Codex",
		"input": []any{
			map[string]any{"type": "input_text", "text": "please edit the file carefully with enough text to estimate"},
		},
	}
	filled, flags := fillMissingUsage(map[string]any{}, body, 3)
	if !flags.EstimatedPrompt {
		t.Fatalf("expected prompt estimate for responses input, flags=%+v filled=%v", flags, filled)
	}
	pt, _ := filled["prompt_tokens"].(int64)
	if pt <= 0 {
		t.Fatalf("prompt=%v", filled["prompt_tokens"])
	}
}

func TestUsageFillFlagsApply(t *testing.T) {
	d := map[string]any{}
	usageFillFlags{Missing: true, EstimatedPrompt: true, EstimatedCompletion: true}.apply(d)
	if d["usage_missing"] != true || d["usage_estimated"] != true {
		t.Fatalf("detail=%v", d)
	}
	if d["usage_estimated_fields"] != "prompt,completion" {
		t.Fatalf("fields=%v", d["usage_estimated_fields"])
	}
}

func TestFillMissingUsageHollowZeroMap(t *testing.T) {
	// Upstream omitted usage entirely; streamer saw text/tools.
	filled, flags := fillMissingUsage(nil, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi there enough text"}},
	}, 8)
	if !flags.Missing || !flags.EstimatedCompletion || !flags.EstimatedPrompt {
		t.Fatalf("flags=%+v filled=%v", flags, filled)
	}
	c, _ := filled["completion_tokens"].(int64)
	if c != 8 {
		t.Fatalf("completion=%v", filled["completion_tokens"])
	}
	p, _ := filled["prompt_tokens"].(int64)
	if p <= 0 {
		t.Fatalf("prompt=%v", filled["prompt_tokens"])
	}
}

func TestFillMissingUsageFixesTotalAfterPromptEstimate(t *testing.T) {
	// fillStreamUsage left completion=6, total=6, prompt=0 — classic hollow partial.
	partial := map[string]any{"completion_tokens": int64(6), "total_tokens": int64(6)}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello world this is a reasonably long prompt for token estimate testing please count me"},
		},
	}
	filled, flags := fillMissingUsage(partial, body, 6)
	if !flags.EstimatedPrompt {
		t.Fatalf("expected prompt estimate, flags=%+v filled=%v", flags, filled)
	}
	p, _ := filled["prompt_tokens"].(int64)
	c, _ := filled["completion_tokens"].(int64)
	tot, _ := filled["total_tokens"].(int64)
	if p <= 0 {
		t.Fatalf("prompt=%v", filled["prompt_tokens"])
	}
	if c != 6 {
		t.Fatalf("completion=%v", filled["completion_tokens"])
	}
	if tot != p+c {
		t.Fatalf("total=%v want %v+%v=%v filled=%v", tot, p, c, p+c, filled)
	}
}

func TestMergeUsageMapsPrefersLarger(t *testing.T) {
	a := map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(1)}
	b := map[string]any{"completion_tokens": float64(5), "total_tokens": float64(15)}
	m := mergeUsageMaps(a, b)
	if anyToInt64(m["completion_tokens"]) != 5 {
		t.Fatalf("completion=%v", m["completion_tokens"])
	}
	if anyToInt64(m["prompt_tokens"]) != 10 {
		t.Fatalf("prompt=%v", m["prompt_tokens"])
	}
	if anyToInt64(m["total_tokens"]) != 15 {
		t.Fatalf("total=%v", m["total_tokens"])
	}
}

func TestFillMissingUsagePromptOnlyWithCompletionHint(t *testing.T) {
	// Classic go_chat hollow usage: prompt estimated, completion=0, total=prompt.
	// Streamer saw real tokens → hint > 0 must fill completion.
	partial := map[string]any{"prompt_tokens": int64(54000), "completion_tokens": int64(0), "total_tokens": int64(54000)}
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	filled, flags := fillMissingUsage(partial, body, 120)
	if !flags.EstimatedCompletion {
		t.Fatalf("expected completion estimate, flags=%+v filled=%v", flags, filled)
	}
	c, _ := filled["completion_tokens"].(int64)
	if c != 120 {
		t.Fatalf("completion=%v want 120", c)
	}
	p, _ := filled["prompt_tokens"].(int64)
	if p != 54000 {
		t.Fatalf("prompt=%v", p)
	}
	tot, _ := filled["total_tokens"].(int64)
	if tot != p+c {
		t.Fatalf("total=%v want %v", tot, p+c)
	}
}

func TestFillMissingUsageLiftsTinyFloor(t *testing.T) {
	// Streamer saw ~400 tokens of output but usage only has floor completion=1.
	partial := map[string]any{
		"prompt_tokens":     int64(3735),
		"completion_tokens": int64(1),
		"total_tokens":      int64(3736),
	}
	filled, flags := fillMissingUsage(partial, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, 400)
	if !flags.EstimatedCompletion {
		t.Fatalf("expected lift, flags=%+v filled=%v", flags, filled)
	}
	c, _ := filled["completion_tokens"].(int64)
	if c != 400 {
		t.Fatalf("completion=%v want 400", c)
	}
	tot, _ := filled["total_tokens"].(int64)
	if tot != 3735+400 {
		t.Fatalf("total=%v", tot)
	}
}
