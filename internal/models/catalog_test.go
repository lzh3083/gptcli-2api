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
		"gpt-4o",
		"gpt-4o-mini",
		"chatgpt-4o-latest",
		"o3-mini",
		"o1",
		"o1-mini",
		"o1-preview",
		"gpt-4.5-preview",
		"gpt-4-turbo",
		"gpt-4",
		"gpt-3.5-turbo",
		"dall-e-3",
		"text-embedding-3-small",
		"text-embedding-3-large",
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
		"gpt-4":                    "gpt-4o",
		"gpt-4o":                   "gpt-4o",
		"gpt-3.5-turbo":            "gpt-3.5-turbo",
		"gpt-4.5":                  "gpt-4.5-preview",
		"o3-mini":                  "o3-mini",
		"o1":                       "o1",
		"dall-e":                   "dall-e-3",
		"claude-sonnet-4-20250514": "gpt-4o",
		"web-search":               "gpt-4o",
		"custom-model":             "custom-model",
	} {
		if got := catalog.Resolve(input); got != want {
			t.Fatalf("Resolve(%q)=%q want %q", input, got, want)
		}
	}
}
