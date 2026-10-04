package models

import (
	"testing"

	"github.com/hm2899/grokcli-2api/internal/config"
)

func TestFallbackModelsIncludeOpenAIExtras(t *testing.T) {
	catalog := NewCatalog(config.Config{DefaultModel: "gpt-4o"}, nil)
	items := catalog.PublicModels(t.Context())
	ids := map[string]bool{}
	for _, item := range items {
		id, _ := item["id"].(string)
		ids[id] = true
	}
	for _, id := range []string{
		"gpt-5",
		"gpt-5-mini",
		"gpt-5-codex",
		"gpt-4.1",
		"gpt-4.1-mini",
		"gpt-4o",
		"gpt-4o-mini",
		"chatgpt-4o-latest",
		"o3",
		"o3-mini",
		"o4-mini",
		"gpt-image-2",
		"text-embedding-3-small",
	} {
		if !ids[id] {
			t.Fatalf("missing model %s in %#v", id, items)
		}
	}
	if ids["grok-build"] || ids["grok-search"] {
		t.Fatalf("legacy grok models should not exist in catalog")
	}
}

func TestResolveAliases(t *testing.T) {
	catalog := NewCatalog(config.Config{DefaultModel: "gpt-4o"}, nil)
	for input, want := range map[string]string{
		"":                         "gpt-4o",
		"auto":                     "gpt-4o",
		"chatgpt":                  "gpt-4o",
		"gpt-5":                    "gpt-5",
		"gpt-4.1":                  "gpt-4.1",
		"o4-mini":                  "o4-mini",
		"gpt-4":                    "gpt-4o",
		"o3-mini":                  "o3-mini",
		"claude-sonnet-4-20250514": "gpt-4o",
		"web-search":               "gpt-4o",
		"custom-model":             "custom-model",
	} {
		if got := catalog.Resolve(input); got != want {
			t.Fatalf("Resolve(%q)=%q want %q", input, got, want)
		}
	}
}
