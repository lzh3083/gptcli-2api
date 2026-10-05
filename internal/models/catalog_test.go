package models

import (
	"testing"

	"github.com/hm2899/grokcli-2api/internal/config"
)

func TestFallbackModelsIncludeOpenAIExtras(t *testing.T) {
	catalog := NewCatalog(config.Config{DefaultModel: "gpt-6.1-sol"}, nil)
	items := catalog.PublicModels(t.Context())
	ids := map[string]bool{}
	for _, item := range items {
		id, _ := item["id"].(string)
		ids[id] = true
	}
	for _, id := range []string{
		"gpt-6.1-sol",
		"gpt-6-astra",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-image-2.5-sunburst",
		"gpt-live-1",
		"text-embedding-3-large",
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
	catalog := NewCatalog(config.Config{DefaultModel: "gpt-6.1-sol"}, nil)
	for input, want := range map[string]string{
		"":                         "gpt-6.1-sol",
		"auto":                     "gpt-6.1-sol",
		"chatgpt":                  "gpt-6.1-sol",
		"gpt-6":                    "gpt-6.1-sol",
		"gpt-6-astra":              "gpt-6-astra",
		"astra":                    "gpt-6-astra",
		"luna":                     "gpt-6-luna",
		"gpt-4":                    "gpt-6.1-sol",
		"gpt-4o":                   "gpt-6.1-sol",
		"o1":                       "gpt-6.1-sol",
		"o3-mini":                  "gpt-6.1-sol",
		"claude-sonnet-4-20250514": "gpt-6.1-sol",
		"web-search":               "gpt-6.1-sol",
		"custom-model":             "custom-model",
	} {
		if got := catalog.Resolve(input); got != want {
			t.Fatalf("Resolve(%q)=%q want %q", input, got, want)
		}
	}
}
