package server

import (
	"encoding/json"
	"strings"

	"github.com/hm2899/grokcli-2api/internal/protocol/anthropic"
	"github.com/hm2899/grokcli-2api/internal/protocol/responses"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
)

// fillMissingUsage patches a usage map when the upstream SSE omitted or only
// partially sent the final usage frame (soft-close / short tool turns / xAI).
//
// Policy:
//   - Never overwrite non-zero upstream fields with smaller estimates.
//   - completion: max(existing, completionHint) when missing/partial.
//   - prompt: estimate from request body when still 0.
//   - total: always prompt+completion (+reasoning if needed) when estimated
//     or when total is inconsistent (e.g. total==completion while prompt>0).
func fillMissingUsage(usage map[string]any, requestBody map[string]any, completionHint int) (map[string]any, usageFillFlags) {
	flags := usageFillFlags{}
	if usage == nil {
		usage = map[string]any{}
		flags.Missing = true
	}
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usage)

	// Complete real usage (all core fields present) and estimate not better → keep.
	if prompt > 0 && completion > 0 && total >= prompt+completion && completionHint <= 0 {
		return usage, flags
	}
	if prompt > 0 && completion > 0 && total >= prompt+completion && completionHint > 0 && completion >= int64(completionHint) {
		return usage, flags
	}

	// Incomplete / missing usage — fill gaps.
	if prompt <= 0 || completion <= 0 || total <= 0 || total < prompt+completion {
		flags.Missing = true
	}

	if completionHint > 0 {
		if completion <= 0 {
			completion = int64(completionHint)
			flags.EstimatedCompletion = true
			flags.Missing = true
		} else if int64(completionHint) > completion {
			// Lift only when the upstream frame is partial / floor-valued.
			// Solid real usage (total >= prompt+completion and completion > 2)
			// is never overwritten by a larger streamer estimate.
			solid := prompt > 0 && completion > 2 && total >= prompt+completion
			incomplete := prompt <= 0 || total <= 0 || total <= prompt || total < prompt+completion
			tinyFloor := completion <= 2 && int64(completionHint) >= completion*4
			if !solid && (incomplete || tinyFloor) {
				completion = int64(completionHint)
				flags.EstimatedCompletion = true
				flags.Missing = true
			}
		}
	}
	if prompt <= 0 {
		if est := estimatePromptTokens(requestBody); est > 0 {
			prompt = int64(est)
			flags.EstimatedPrompt = true
			flags.Missing = true
		}
	}

	// Fix total whenever inconsistent or missing after fills.
	wantTotal := prompt + completion
	if reasoning > 0 && wantTotal > 0 && total < wantTotal+reasoning {
		// Some providers put reasoning outside completion; keep prompt+completion baseline.
	}
	if total <= 0 || total < wantTotal {
		if wantTotal > 0 {
			total = wantTotal
			flags.EstimatedTotal = true
			flags.Missing = true
		}
	}
	// Classic bug: fillStreamUsage set total=completion while prompt was still 0;
	// after prompt estimate, total must be rewritten.
	if flags.EstimatedPrompt && total > 0 && total < prompt+completion {
		total = prompt + completion
		flags.EstimatedTotal = true
	}

	if !flags.EstimatedPrompt && !flags.EstimatedCompletion && !flags.EstimatedTotal {
		return usage, flags
	}

	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
		"input_tokens":      prompt,
		"output_tokens":     completion,
	}
	if cacheRead > 0 {
		out["cache_read_tokens"] = cacheRead
		out["cached_tokens"] = cacheRead
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cacheRead}
	}
	if cacheCreate > 0 {
		out["cache_creation_tokens"] = cacheCreate
	}
	if reasoning > 0 {
		out["reasoning_tokens"] = reasoning
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	for k, v := range usage {
		if _, has := out[k]; has {
			continue
		}
		if k == "" || v == nil {
			continue
		}
		out[k] = v
	}
	return out, flags
}

type usageFillFlags struct {
	Missing             bool
	EstimatedPrompt     bool
	EstimatedCompletion bool
	EstimatedTotal      bool
}

func (f usageFillFlags) apply(detail map[string]any) {
	if detail == nil {
		return
	}
	if f.Missing {
		detail["usage_missing"] = true
	}
	if f.EstimatedPrompt || f.EstimatedCompletion || f.EstimatedTotal {
		detail["usage_estimated"] = true
		parts := make([]string, 0, 3)
		if f.EstimatedPrompt {
			parts = append(parts, "prompt")
		}
		if f.EstimatedCompletion {
			parts = append(parts, "completion")
		}
		if f.EstimatedTotal {
			parts = append(parts, "total")
		}
		if len(parts) > 0 {
			detail["usage_estimated_fields"] = strings.Join(parts, ",")
		}
	}
}

// mergeUsageMaps prefers non-zero numeric fields from both partial SSE usage frames.
func mergeUsageMaps(dst, src map[string]any) map[string]any {
	if src == nil {
		return dst
	}
	if dst == nil {
		out := make(map[string]any, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	for k, v := range src {
		if v == nil {
			continue
		}
		// Nested detail maps: shallow-merge preferring non-zero ints.
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeUsageMaps(dm, sm)
			} else {
				dst[k] = sm
			}
			continue
		}
		// Prefer larger numeric token counts when both present.
		if isUsageTokenKey(k) {
			sv := anyToInt64(v)
			dv := anyToInt64(dst[k])
			if sv > dv {
				dst[k] = v
			} else if _, exists := dst[k]; !exists {
				dst[k] = v
			}
			continue
		}
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
	return dst
}

func isUsageTokenKey(k string) bool {
	switch k {
	case "prompt_tokens", "completion_tokens", "total_tokens",
		"input_tokens", "output_tokens",
		"cache_read_tokens", "cache_creation_tokens", "cached_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
		"reasoning_tokens", "thinking_tokens":
		return true
	default:
		return false
	}
}

func anyToInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	}
	return 0
}

// estimatePromptTokens approximates input tokens from the client request body.
// Handles Anthropic messages, OpenAI chat messages, and Responses `input`.
func estimatePromptTokens(raw map[string]any) int {
	if raw == nil {
		return 0
	}
	if _, hasMsg := raw["messages"]; hasMsg {
		if n, _ := anthropic.CountTokensForRequest(raw)["input_tokens"].(int); n > 0 {
			return n
		}
	}
	if raw["input"] != nil || raw["instructions"] != nil {
		converted := map[string]any{
			"messages": responses.InputToMessages(raw["input"], stringValue(raw["instructions"])),
		}
		if tools, ok := raw["tools"]; ok {
			converted["tools"] = tools
		}
		if n, _ := anthropic.CountTokensForRequest(converted)["input_tokens"].(int); n > 0 {
			return n
		}
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) == 0 {
		return 0
	}
	n := (len(encoded) + 3) / 4
	if n > 200_000 {
		n = 200_000
	}
	return n
}
