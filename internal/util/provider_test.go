package util

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestOpenAICompatibilityAliasMatchesRealNameOnlyWhenAliasEmpty(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "deepseek",
				Models: []config.OpenAICompatibilityModel{
					{Name: "deepseek-v4-flash"},
					{Name: "deepseek-v4-pro", Alias: "ds-pro"},
				},
			},
		},
	}

	if !IsOpenAICompatibilityAlias("deepseek-v4-flash", cfg) {
		t.Fatal("expected unaliased real model to be client-facing")
	}
	if IsOpenAICompatibilityAlias("deepseek-v4-pro", cfg) {
		t.Fatal("expected aliased real model to be hidden from client-facing lookup")
	}
	if !IsOpenAICompatibilityAlias("ds-pro", cfg) {
		t.Fatal("expected configured alias to be client-facing")
	}

	_, flash := GetOpenAICompatibilityConfig("deepseek-v4-flash", cfg)
	if flash == nil || flash.Name != "deepseek-v4-flash" || flash.Alias != "" {
		t.Fatalf("unexpected flash mapping: %#v", flash)
	}
	_, pro := GetOpenAICompatibilityConfig("ds-pro", cfg)
	if pro == nil || pro.Name != "deepseek-v4-pro" || pro.Alias != "ds-pro" {
		t.Fatalf("unexpected pro mapping: %#v", pro)
	}
	_, hidden := GetOpenAICompatibilityConfig("deepseek-v4-pro", cfg)
	if hidden != nil {
		t.Fatalf("expected real model with configured alias to be hidden, got %#v", hidden)
	}
}

func TestOpenAICompatibilityAliasUsesDeepSeekDefaultsWhenModelsEmpty(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1"},
		},
	}

	if !IsOpenAICompatibilityAlias("deepseek-v4-flash", cfg) {
		t.Fatal("expected default DeepSeek flash model to be client-facing")
	}
	_, model := GetOpenAICompatibilityConfig("deepseek-v4-pro", cfg)
	if model == nil || model.Name != "deepseek-v4-pro" {
		t.Fatalf("unexpected default DeepSeek pro mapping: %#v", model)
	}
}
