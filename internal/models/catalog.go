package models

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
)

type Catalog struct {
	cfg   config.Config
	store *postgres.Connector
}

func NewCatalog(cfg config.Config, store *postgres.Connector) *Catalog {
	return &Catalog{cfg: cfg, store: store}
}

func (c *Catalog) OpenAIList(ctx context.Context) map[string]any {
	return map[string]any{"object": "list", "data": c.PublicModels(ctx)}
}

func (c *Catalog) PublicModels(ctx context.Context) []map[string]any {
	if c != nil && c.store != nil {
		rows, err := c.store.ListModels(ctx, false)
		if err == nil && len(rows) > 0 {
			models := make([]map[string]any, 0, len(rows)+2)
			now := time.Now().Unix()
			for _, row := range rows {
				if strings.TrimSpace(row.ID) == "" {
					continue
				}
				models = append(models, publicModelEntry(row, now))
			}
			models = mergeExtraModels(models, c.defaultModel())
			sort.SliceStable(models, func(i, j int) bool {
				return modelSortKey(models[i], c.defaultModel()) < modelSortKey(models[j], c.defaultModel())
			})
			return models
		}
	}
	return fallbackModels(c.defaultModel())
}

func (c *Catalog) Resolve(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return c.defaultModel()
	}
	low := strings.ToLower(m)
	if low == "web-search" {
		return c.defaultModel()
	}
	if resolved, ok := aliases(c.defaultModel())[m]; ok {
		return resolved
	}
	if resolved, ok := aliases(c.defaultModel())[low]; ok {
		return resolved
	}
	return m
}

func (c *Catalog) defaultModel() string {
	if c == nil || strings.TrimSpace(c.cfg.DefaultModel) == "" {
		return "gpt-4o"
	}
	return strings.TrimSpace(c.cfg.DefaultModel)
}

func publicModelEntry(row postgres.ModelRecord, now int64) map[string]any {
	entry := map[string]any{
		"id":       row.ID,
		"object":   "model",
		"created":  now,
		"owned_by": row.OwnedBy,
	}
	if row.Name != nil && *row.Name != "" {
		entry["name"] = *row.Name
	}
	if row.Description != nil && *row.Description != "" {
		entry["description"] = *row.Description
	}
	if row.ContextWindow != nil {
		entry["context_window"] = *row.ContextWindow
	}
	if row.SupportsReasoningEffort != nil {
		entry["supports_reasoning_effort"] = *row.SupportsReasoningEffort
	}
	for _, key := range []string{"max_completion_tokens", "reasoning_effort", "reasoning_efforts", "auto_compact_threshold_percent", "supported_in_api"} {
		if value, ok := row.Extra[key]; ok && value != nil {
			entry[key] = value
		}
	}
	return entry
}

func fallbackModels(defaultModel string) []map[string]any {
	now := time.Now().Unix()
	models := []map[string]any{{
		"id":       defaultModel,
		"object":   "model",
		"created":  now,
		"owned_by": "openai",
	}}
	models = mergeExtraModels(models, defaultModel)
	sort.SliceStable(models, func(i, j int) bool {
		return modelSortKey(models[i], defaultModel) < modelSortKey(models[j], defaultModel)
	})
	return models
}

func mergeExtraModels(models []map[string]any, defaultModel string) []map[string]any {
	have := make(map[string]bool, len(models)+32)
	for _, item := range models {
		if id, _ := item["id"].(string); id != "" {
			have[strings.ToLower(id)] = true
		}
	}
	now := time.Now().Unix()
	for _, extra := range []map[string]any{
		// ── GPT-5 / GPT-6 新一代主力旗舰 ─────────────────────────────
		{"id": "gpt-5", "name": "GPT-5", "description": "OpenAI flagship intelligence model with configurable reasoning effort", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "gpt-5-mini", "name": "GPT-5 Mini", "description": "Strong intelligence for cost-sensitive, low-latency workloads", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "gpt-5-nano", "name": "GPT-5 nano", "description": "Fastest and most cost-efficient version of GPT-5", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-5-codex", "name": "GPT-5 Codex", "description": "Version of GPT-5 optimized for agentic coding in Codex", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "gpt-5-chat-latest", "name": "GPT-5 Chat Latest", "description": "Dynamic latest GPT-5 model used in ChatGPT", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "gpt-5.5", "name": "GPT-5.5", "description": "New class of intelligence for complex coding and professional work", "owned_by": "openai", "context_window": 256000, "supports_reasoning_effort": true},
		{"id": "gpt-5.6-sol", "name": "GPT-5.6 Sol", "description": "GPT-5.6 flagship model for complex professional work", "owned_by": "openai", "context_window": 256000, "supports_reasoning_effort": true},
		{"id": "gpt-5.6-terra", "name": "GPT-5.6 Terra", "description": "GPT-5.6 model that balances intelligence and cost", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "gpt-5.6-luna", "name": "GPT-5.6 Luna", "description": "GPT-5.6 model optimized for cost-sensitive workloads", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-6-astra", "name": "GPT-6 Astra", "description": "OpenAI next-generation flagship for the most demanding reasoning and coding", "owned_by": "openai", "context_window": 300000, "supports_reasoning_effort": true},
		{"id": "gpt-6.1-sol", "name": "GPT-6.1 Sol", "description": "Near-Astra performance for complex work at lower cost", "owned_by": "openai", "context_window": 256000, "supports_reasoning_effort": true},

		// ── GPT-4.1 最新非推理智能矩阵 ─────────────────────────────
		{"id": "gpt-4.1", "name": "GPT-4.1", "description": "OpenAI smartest non-reasoning flagship model", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4.1-mini", "name": "GPT-4.1 Mini", "description": "Smaller, faster version of GPT-4.1", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4.1-nano", "name": "GPT-4.1 nano", "description": "Fastest, most cost-efficient version of GPT-4.1", "owned_by": "openai", "context_window": 128000},

		// ── o-series 深度逻辑推理主力 ──────────────────────────────
		{"id": "o3", "name": "o3", "description": "OpenAI flagship reasoning model for complex tasks", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "o3-mini", "name": "o3-mini", "description": "OpenAI latest efficient reasoning model", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "o4-mini", "name": "o4-mini", "description": "Fast, cost-efficient next-generation reasoning model", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "o1", "name": "o1", "description": "OpenAI foundational reasoning model", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "o1-mini", "name": "o1-mini", "description": "OpenAI lightweight reasoning model", "owned_by": "openai", "context_window": 128000, "supports_reasoning_effort": true},

		// ── GPT-4o 全模态经典主力 ──────────────────────────────────
		{"id": "gpt-4o", "name": "GPT-4o", "description": "OpenAI flagship omni multimodal model", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4o-mini", "name": "GPT-4o mini", "description": "Fast, affordable small model for focused tasks", "owned_by": "openai", "context_window": 128000},
		{"id": "chatgpt-4o-latest", "name": "ChatGPT 4o Latest", "description": "ChatGPT latest dynamic model", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4.5-preview", "name": "GPT-4.5 Preview", "description": "OpenAI research-grade large language model", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4-turbo", "name": "GPT-4 Turbo", "description": "OpenAI GPT-4 Turbo with 128k context", "owned_by": "openai", "context_window": 128000},

		// ── 生图与嵌入向量 ─────────────────────────────────────────
		{"id": "gpt-image-2", "name": "GPT Image 2", "description": "State-of-the-art OpenAI image generation model", "owned_by": "openai"},
		{"id": "text-embedding-3-small", "name": "Embedding 3 Small", "description": "OpenAI text embedding 3 small", "owned_by": "openai"},
		{"id": "text-embedding-3-large", "name": "Embedding 3 Large", "description": "OpenAI text embedding 3 large", "owned_by": "openai"},
	} {
		id := extra["id"].(string)
		if have[strings.ToLower(id)] {
			continue
		}
		extra["object"] = "model"
		extra["created"] = now
		extra["official"] = true
		extra["sort_order"] = sortOrderFor(id, defaultModel)
		models = append(models, extra)
		have[strings.ToLower(id)] = true
	}
	return models
}

func modelSortKey(item map[string]any, defaultModel string) string {
	id, _ := item["id"].(string)
	order := sortOrderFor(id, defaultModel)
	return fmt.Sprintf("%02d:%s", order, id)
}

func sortOrderFor(id, defaultModel string) int {
	switch id {
	case defaultModel:
		return 0
	case "gpt-5":
		return 1
	case "gpt-5-mini":
		return 2
	case "gpt-5-codex":
		return 3
	case "gpt-5.5":
		return 4
	case "gpt-6-astra":
		return 5
	case "gpt-4.1":
		return 6
	case "gpt-4.1-mini":
		return 7
	case "o3-mini":
		return 8
	case "o3":
		return 9
	case "o4-mini":
		return 10
	case "gpt-4o":
		return 11
	case "gpt-4o-mini":
		return 12
	case "chatgpt-4o-latest":
		return 13
	case "o1":
		return 14
	case "o1-mini":
		return 15
	case "gpt-image-2":
		return 16
	case "text-embedding-3-small":
		return 17
	case "text-embedding-3-large":
		return 18
	default:
		return 30
	}
}

func aliases(defaultModel string) map[string]string {
	return map[string]string{
		"auto": defaultModel,
		"chatgpt": defaultModel, "chatgpt-4": defaultModel, "chatgpt-4o": "chatgpt-4o-latest",
		"gpt-5": "gpt-5", "gpt-5-mini": "gpt-5-mini", "gpt-5-nano": "gpt-5-nano",
		"gpt-5-codex": "gpt-5-codex", "gpt-5.5": "gpt-5.5",
		"gpt-4.1": "gpt-4.1", "gpt-4.1-mini": "gpt-4.1-mini", "gpt-4.1-nano": "gpt-4.1-nano",
		"gpt-4": defaultModel, "gpt-4o": defaultModel, "gpt-3.5-turbo": defaultModel,
		"gpt-4.5": "gpt-4.5-preview",
		"o3": "o3", "o3-mini": "o3-mini", "o4-mini": "o4-mini",
		"o1": "o1", "o1-mini": "o1-mini", "o1-preview": "o1-preview",
		"claude": defaultModel, "claude-3": defaultModel, "claude-3-5-sonnet": defaultModel, "claude-3-5-sonnet-20240620": defaultModel,
		"claude-3-5-sonnet-20241022": defaultModel, "claude-3-5-haiku": defaultModel, "claude-3-5-haiku-20241022": defaultModel,
		"claude-3-haiku": defaultModel, "claude-3-haiku-20240307": defaultModel, "claude-3-opus": defaultModel, "claude-3-opus-20240229": defaultModel,
		"claude-3-sonnet": defaultModel, "claude-3-sonnet-20240229": defaultModel, "claude-sonnet-4": defaultModel, "claude-sonnet-4-0": defaultModel,
		"claude-sonnet-4-20250514": defaultModel, "claude-sonnet-4-5": defaultModel, "claude-sonnet-4-5-20250929": defaultModel,
		"claude-opus-4": defaultModel, "claude-opus-4-0": defaultModel, "claude-opus-4-20250514": defaultModel, "claude-opus-4-5": defaultModel,
		"claude-haiku-4": defaultModel, "claude-haiku-4-5": defaultModel, "claude-haiku-4-5-20251001": defaultModel,
	}
}
