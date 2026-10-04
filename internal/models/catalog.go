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
		// ── OpenAI 主力旗舰全模态 ──────────────────────────────────
		{"id": "gpt-4o", "name": "GPT-4o", "description": "OpenAI flagship omni multimodal model", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4o-mini", "name": "GPT-4o mini", "description": "Fast, affordable small model for focused tasks", "owned_by": "openai", "context_window": 128000},
		{"id": "chatgpt-4o-latest", "name": "ChatGPT 4o Latest", "description": "ChatGPT latest dynamic model tracking chatgpt.com", "owned_by": "openai", "context_window": 128000},

		// ── o 系列深度推理主力 ─────────────────────────────────────
		{"id": "o3-mini", "name": "o3-mini", "description": "OpenAI efficient reasoning model with high speed and low cost", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "o1", "name": "o1", "description": "OpenAI flagship reasoning model for math, science and coding", "owned_by": "openai", "context_window": 200000, "supports_reasoning_effort": true},
		{"id": "o1-mini", "name": "o1-mini", "description": "Fast reasoning model especially strong at code and math", "owned_by": "openai", "context_window": 128000, "supports_reasoning_effort": true},
		{"id": "o1-preview", "name": "o1-preview", "description": "OpenAI foundational reasoning preview model", "owned_by": "openai", "context_window": 128000, "supports_reasoning_effort": true},

		// ── 研究级前沿旗舰与经典生产模型 ───────────────────────────
		{"id": "gpt-4.5-preview", "name": "GPT-4.5 Preview", "description": "OpenAI research-grade largest flagship model", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4-turbo", "name": "GPT-4 Turbo", "description": "GPT-4 Turbo with 128k context and vision capabilities", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4", "name": "GPT-4", "description": "OpenAI foundational high-intelligence model", "owned_by": "openai", "context_window": 8192},
		{"id": "gpt-3.5-turbo", "name": "GPT-3.5 Turbo", "description": "Fast, inexpensive model for simple tasks", "owned_by": "openai", "context_window": 16385},

		// ── 官方多模态、绘图与向量嵌入 ─────────────────────────────
		{"id": "dall-e-3", "name": "DALL·E 3", "description": "State-of-the-art OpenAI image generation model", "owned_by": "openai"},
		{"id": "dall-e-2", "name": "DALL·E 2", "description": "Previous generation OpenAI image generation model", "owned_by": "openai"},
		{"id": "text-embedding-3-small", "name": "Embedding 3 Small", "description": "Highly efficient text embedding model", "owned_by": "openai"},
		{"id": "text-embedding-3-large", "name": "Embedding 3 Large", "description": "Most capable text embedding model for search and similarity", "owned_by": "openai"},
		{"id": "text-embedding-ada-002", "name": "Embedding Ada 002", "description": "Previous generation text embedding model", "owned_by": "openai"},
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
	case "gpt-4o":
		return 1
	case "gpt-4o-mini":
		return 2
	case "chatgpt-4o-latest":
		return 3
	case "o3-mini":
		return 4
	case "o1":
		return 5
	case "o1-mini":
		return 6
	case "gpt-4.5-preview":
		return 7
	case "gpt-4-turbo":
		return 8
	case "gpt-4":
		return 9
	case "gpt-3.5-turbo":
		return 10
	case "o1-preview":
		return 11
	case "dall-e-3":
		return 12
	case "text-embedding-3-small":
		return 13
	case "text-embedding-3-large":
		return 14
	default:
		return 20
	}
}

func aliases(defaultModel string) map[string]string {
	return map[string]string{
		"auto": defaultModel,
		"chatgpt": defaultModel, "chatgpt-4": defaultModel, "chatgpt-4o": "chatgpt-4o-latest",
		"gpt-4": defaultModel, "gpt-4o": defaultModel, "gpt-3.5-turbo": "gpt-3.5-turbo",
		"gpt-4.5": "gpt-4.5-preview",
		"o3-mini": "o3-mini",
		"o1": "o1", "o1-mini": "o1-mini", "o1-preview": "o1-preview",
		"dall-e": "dall-e-3",
		"claude": defaultModel, "claude-3": defaultModel, "claude-3-5-sonnet": defaultModel, "claude-3-5-sonnet-20240620": defaultModel,
		"claude-3-5-sonnet-20241022": defaultModel, "claude-3-5-haiku": defaultModel, "claude-3-5-haiku-20241022": defaultModel,
		"claude-3-haiku": defaultModel, "claude-3-haiku-20240307": defaultModel, "claude-3-opus": defaultModel, "claude-3-opus-20240229": defaultModel,
		"claude-3-sonnet": defaultModel, "claude-3-sonnet-20240229": defaultModel, "claude-sonnet-4": defaultModel, "claude-sonnet-4-0": defaultModel,
		"claude-sonnet-4-20250514": defaultModel, "claude-sonnet-4-5": defaultModel, "claude-sonnet-4-5-20250929": defaultModel,
		"claude-opus-4": defaultModel, "claude-opus-4-0": defaultModel, "claude-opus-4-20250514": defaultModel, "claude-opus-4-5": defaultModel,
		"claude-haiku-4": defaultModel, "claude-haiku-4-5": defaultModel, "claude-haiku-4-5-20251001": defaultModel,
	}
}
