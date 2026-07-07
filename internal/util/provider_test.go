package util

import (
	"strings"
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

func TestMaskSensitiveQueryRedactsAccountKeysList(t *testing.T) {
	raw := "account_keys=acct_0001%2Cacct_0002%2Cacct_0003&window=24h"
	masked := MaskSensitiveQuery(raw)

	if strings.Contains(masked, "acct_0001") || strings.Contains(masked, "acct_0002") || strings.Contains(masked, "acct_0003") {
		t.Fatalf("masked query leaked account keys: %s", masked)
	}
	if !strings.Contains(masked, "account_keys=") {
		t.Fatalf("masked query should preserve parameter name: %s", masked)
	}
	if !strings.Contains(masked, "redacted%3A3") {
		t.Fatalf("masked query should include redacted account count, got %s", masked)
	}
	if !strings.Contains(masked, "window=24h") {
		t.Fatalf("masked query should preserve unrelated query values, got %s", masked)
	}
}

func TestMaskSensitiveQueryRedactsRepeatedAccountKeyParams(t *testing.T) {
	raw := "account_key=acct_a&account_key=acct_b&account_key=acct_c"
	masked := MaskSensitiveQuery(raw)

	if strings.Contains(masked, "acct_a") || strings.Contains(masked, "acct_b") || strings.Contains(masked, "acct_c") {
		t.Fatalf("masked query leaked repeated account keys: %s", masked)
	}
	if got := strings.Count(masked, "redacted%3A1"); got != 3 {
		t.Fatalf("expected each repeated account_key value to be redacted with count 1, got %d in %s", got, masked)
	}
}
