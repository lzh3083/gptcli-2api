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
		// ── GPT-6 旗舰矩阵（2026 最新主力） ─────────────────────────────
		{"id": "gpt-6.1-sol", "name": "GPT-6.1 Sol", "description": "Near-Astra performance at much lower cost (Flagship Value & Recommended Default)", "owned_by": "openai", "context_window": 1050000, "supports_reasoning_effort": true},
		{"id": "gpt-6-astra", "name": "GPT-6 Astra", "description": "Most capable flagship model for complex reasoning, coding, computer use and deep research", "owned_by": "openai", "context_window": 1050000, "supports_reasoning_effort": true},
		{"id": "gpt-6-sol", "name": "GPT-6 Sol", "description": "High intelligence for complex coding and agentic workflows", "owned_by": "openai", "context_window": 1050000, "supports_reasoning_effort": true},
		{"id": "gpt-6-luna", "name": "GPT-6 Luna", "description": "Most efficient model for high-throughput and cost-sensitive workloads", "owned_by": "openai", "context_window": 1050000, "supports_reasoning_effort": true},

		// ── 上一代在售（生产过渡兼容） ───────────────────────────────
		{"id": "gpt-5.6-sol", "name": "GPT-5.6 Sol", "description": "GPT-5.6 flagship model for complex professional work", "owned_by": "openai", "context_window": 1050000, "supports_reasoning_effort": true},
		{"id": "gpt-5.6-terra", "name": "GPT-5.6 Terra", "description": "GPT-5.6 model balancing intelligence and cost", "owned_by": "openai", "context_window": 256000, "supports_reasoning_effort": true},
		{"id": "gpt-5.6-luna", "name": "GPT-5.6 Luna", "description": "GPT-5.6 model optimized for cost-sensitive workloads", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-5.5", "name": "GPT-5.5", "description": "Advanced intelligence for complex coding and technical tasks", "owned_by": "openai", "context_window": 256000, "supports_reasoning_effort": true},
		{"id": "gpt-5.5-pro", "name": "GPT-5.5 Pro", "description": "GPT-5.5 pro version for extended thinking and deep reasoning", "owned_by": "openai", "context_window": 256000, "supports_reasoning_effort": true},
		{"id": "gpt-5.4", "name": "GPT-5.4", "description": "Previous generation balanced flagship model", "owned_by": "openai", "context_window": 128000},

		// ── 专业图像模型 ───────────────────────────────────────────
		{"id": "gpt-image-2.5-sunburst", "name": "GPT Image 2.5 Sunburst", "description": "OpenAI most capable state-of-the-art image generation model", "owned_by": "openai"},
		{"id": "gpt-image-2.5-flare", "name": "GPT Image 2.5 Flare", "description": "Fast and lightweight image generation model", "owned_by": "openai"},
		{"id": "gpt-image-2", "name": "GPT Image 2", "description": "Previous generation high-quality image generation model", "owned_by": "openai"},

		// ── 实时语音与转录 ─────────────────────────────────────────
		{"id": "gpt-live-1", "name": "GPT Live 1", "description": "OpenAI ultra-low latency interactive voice model ($0.05/min)", "owned_by": "openai"},
		{"id": "gpt-realtime-2.1", "name": "GPT Realtime 2.1", "description": "Multimodal speech-to-speech realtime model", "owned_by": "openai"},
		{"id": "gpt-realtime-2.1-mini", "name": "GPT Realtime 2.1 Mini", "description": "Cost-efficient realtime voice and audio model", "owned_by": "openai"},
		{"id": "gpt-realtime-2", "name": "GPT Realtime 2", "description": "Previous generation realtime audio model", "owned_by": "openai"},
		{"id": "gpt-realtime-translate", "name": "GPT Realtime Translate", "description": "Dedicated low-latency translation model", "owned_by": "openai"},
		{"id": "gpt-live-transcribe", "name": "GPT Live Transcribe", "description": "Streaming real-time speech transcription model", "owned_by": "openai"},
		{"id": "gpt-transcribe", "name": "GPT Transcribe", "description": "General speech-to-text audio transcription model", "owned_by": "openai"},

		// ── 开源权重与专有安全模型 ─────────────────────────────────
		{"id": "gpt-oss-120b", "name": "GPT OSS 120B", "description": "OpenAI open-weight 120B model (Apache 2.0)", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-oss-20b", "name": "GPT OSS 20B", "description": "OpenAI open-weight 20B model (Apache 2.0)", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-5.6-cyber", "name": "GPT-5.6 Cyber", "description": "Defensive cybersecurity model for vulnerability research", "owned_by": "openai", "context_window": 256000},
		{"id": "gpt-rosalind-research", "name": "GPT Rosalind Research", "description": "Life sciences reasoning model for approved organizations", "owned_by": "openai", "context_window": 256000, "supports_reasoning_effort": true},

		// ── 文本向量嵌入 ───────────────────────────────────────────
		{"id": "text-embedding-3-large", "name": "Embedding 3 Large", "description": "Most capable text embedding model for search and similarity", "owned_by": "openai"},
		{"id": "text-embedding-3-small", "name": "Embedding 3 Small", "description": "Highly efficient text embedding model", "owned_by": "openai"},

		// ── 经典过渡兼容 ───────────────────────────────────────────
		{"id": "gpt-4o", "name": "GPT-4o (Legacy)", "description": "Legacy omni model (mapped to latest infrastructure)", "owned_by": "openai", "context_window": 128000},
		{"id": "gpt-4o-mini", "name": "GPT-4o mini (Legacy)", "description": "Legacy small model", "owned_by": "openai", "context_window": 128000},
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
	case "gpt-6.1-sol":
		return 1
	case "gpt-6-astra":
		return 2
	case "gpt-6-sol":
		return 3
	case "gpt-6-luna":
		return 4
	case "gpt-5.6-sol":
		return 5
	case "gpt-5.6-terra":
		return 6
	case "gpt-5.6-luna":
		return 7
	case "gpt-5.5":
		return 8
	case "gpt-5.5-pro":
		return 9
	case "gpt-5.4":
		return 10
	case "gpt-image-2.5-sunburst":
		return 11
	case "gpt-image-2.5-flare":
		return 12
	case "gpt-image-2":
		return 13
	case "gpt-live-1":
		return 14
	case "gpt-realtime-2.1":
		return 15
	case "gpt-oss-120b":
		return 16
	case "text-embedding-3-large":
		return 17
	case "text-embedding-3-small":
		return 18
	case "gpt-4o":
		return 25
	default:
		return 30
	}
}

func aliases(defaultModel string) map[string]string {
	return map[string]string{
		"auto":    defaultModel,
		"default": defaultModel,
		"chatgpt": defaultModel, "chatgpt-4": defaultModel, "chatgpt-4o": defaultModel,
		"gpt-6": defaultModel, "gpt-6.1": "gpt-6.1-sol", "gpt-6.1-sol": "gpt-6.1-sol",
		"gpt-6-astra": "gpt-6-astra", "astra": "gpt-6-astra",
		"gpt-6-sol": "gpt-6-sol",
		"gpt-6-luna": "gpt-6-luna", "luna": "gpt-6-luna",
		"gpt-5.6": "gpt-5.6-sol", "gpt-5.6-sol": "gpt-5.6-sol", "gpt-5.6-terra": "gpt-5.6-terra", "gpt-5.6-luna": "gpt-5.6-luna",
		"gpt-5.5": "gpt-5.5", "gpt-5.5-pro": "gpt-5.5-pro", "gpt-5.4": "gpt-5.4",
		"gpt-image": "gpt-image-2.5-sunburst", "gpt-image-2.5": "gpt-image-2.5-sunburst", "dall-e": "gpt-image-2.5-sunburst", "dall-e-3": "gpt-image-2.5-sunburst",
		// Legacy model aliases mapped smoothly to new generation
		"gpt-5": "gpt-5.6-sol", "gpt-5-mini": "gpt-5.6-terra", "gpt-5-nano": "gpt-5.6-luna",
		"gpt-4": defaultModel, "gpt-4o": defaultModel, "gpt-4o-mini": defaultModel, "gpt-3.5-turbo": defaultModel,
		"o1": defaultModel, "o1-mini": defaultModel, "o3": defaultModel, "o3-mini": defaultModel, "o4-mini": defaultModel,
		"claude": defaultModel, "claude-3": defaultModel, "claude-3-5-sonnet": defaultModel, "claude-3-5-sonnet-20240620": defaultModel,
		"claude-3-5-sonnet-20241022": defaultModel, "claude-3-5-haiku": defaultModel, "claude-3-5-haiku-20241022": defaultModel,
		"claude-3-haiku": defaultModel, "claude-3-haiku-20240307": defaultModel, "claude-3-opus": defaultModel, "claude-3-opus-20240229": defaultModel,
		"claude-3-sonnet": defaultModel, "claude-3-sonnet-20240229": defaultModel, "claude-sonnet-4": defaultModel, "claude-sonnet-4-0": defaultModel,
		"claude-sonnet-4-20250514": defaultModel, "claude-sonnet-4-5": defaultModel, "claude-sonnet-4-5-20250929": defaultModel,
		"claude-opus-4": defaultModel, "claude-opus-4-0": defaultModel, "claude-opus-4-20250514": defaultModel, "claude-opus-4-5": defaultModel,
		"claude-haiku-4": defaultModel, "claude-haiku-4-5": defaultModel, "claude-haiku-4-5-20251001": defaultModel,
	}
}
