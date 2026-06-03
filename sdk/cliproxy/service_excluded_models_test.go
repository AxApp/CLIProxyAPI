package cliproxy

import (
	"strings"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRegisterModelsForAuth_UsesPreMergedExcludedModelsAttribute(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"gemini-cli": {"gemini-2.5-pro"},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-gemini-cli",
		Provider: "gemini-cli",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "gemini-2.5-flash",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := registry.GetAvailableModelsByProvider("gemini-cli")
	if len(models) == 0 {
		t.Fatal("expected gemini-cli models to be registered")
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if strings.EqualFold(modelID, "gemini-2.5-flash") {
			t.Fatalf("expected model %q to be excluded by auth attribute", modelID)
		}
	}

	seenGlobalExcluded := false
	for _, model := range models {
		if model == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.ID), "gemini-2.5-pro") {
			seenGlobalExcluded = true
			break
		}
	}
	if !seenGlobalExcluded {
		t.Fatal("expected global excluded model to be present when attribute override is set")
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityImageModelType(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-image",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"compat_name":  "images",
			"provider_key": "images",
			"base_url":     "https://example.com/v1",
			"openai_compat_models": `[
				{"name":"upstream-image","alias":"compat-image","image":true},
				{"name":"upstream-chat","alias":"compat-chat"}
			]`,
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	var imageModel *internalregistry.ModelInfo
	var chatModel *internalregistry.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.TrimSpace(model.ID) {
		case "compat-image":
			imageModel = model
		case "compat-chat":
			chatModel = model
		}
	}
	if imageModel == nil {
		t.Fatal("expected compat-image to be registered")
	}
	if imageModel.Type != internalregistry.OpenAIImageModelType {
		t.Fatalf("image model type = %q, want %q", imageModel.Type, internalregistry.OpenAIImageModelType)
	}
	if imageModel.Thinking != nil {
		t.Fatalf("image model thinking = %+v, want nil", imageModel.Thinking)
	}
	if chatModel == nil {
		t.Fatal("expected compat-chat to be registered")
	}
	if chatModel.Type != "openai-compatibility" {
		t.Fatalf("chat model type = %q, want openai-compatibility", chatModel.Type)
	}
	if chatModel.Thinking == nil {
		t.Fatal("expected chat model to keep default thinking support")
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityUsesAuthModelAttributes(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-model-attrs",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":            "api_key",
			"compat_name":          "deepseek",
			"provider_key":         "deepseek",
			"base_url":             "https://api.deepseek.com/v1",
			"openai_compat_models": `[{"name":"deepseek-v4-flash"},{"name":"deepseek-v4-pro"}]`,
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	for _, id := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		found := false
		for _, model := range models {
			if model != nil && model.ID == id {
				found = true
				if model.Type != "openai-compatibility" {
					t.Fatalf("%s type = %q, want openai-compatibility", id, model.Type)
				}
			}
		}
		if !found {
			t.Fatalf("expected default DeepSeek model %s to be registered, got %#v", id, models)
		}
	}
}

func TestRegisterModelsForAuth_CodexNonAPIKeyDoesNotRegisterOpenAICompatibleBuiltins(t *testing.T) {
	service := &Service{cfg: &config.Config{}}
	testCases := []struct {
		name       string
		attributes map[string]string
	}{
		{name: "oauth", attributes: map[string]string{"auth_kind": "oauth", "plan_type": "pro"}},
		{name: "auth-file-without-kind", attributes: map[string]string{"plan_type": "pro"}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			auth := &coreauth.Auth{
				ID:         "auth-codex-no-openai-compat-builtins-" + testCase.name,
				Provider:   "codex",
				Status:     coreauth.StatusActive,
				Attributes: testCase.attributes,
			}

			modelRegistry := internalregistry.GetGlobalRegistry()
			modelRegistry.UnregisterClient(auth.ID)
			t.Cleanup(func() {
				modelRegistry.UnregisterClient(auth.ID)
			})

			service.registerModelsForAuth(auth)

			for _, modelID := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
				if modelRegistry.ClientSupportsModel(auth.ID, modelID) {
					t.Fatalf("Codex non-API-key auth should not register OpenAI-compatible builtin %s", modelID)
				}
			}
			if !modelRegistry.ClientSupportsModel(auth.ID, "gpt-5.5") {
				t.Fatal("Codex non-API-key auth should still register native Codex models")
			}
		})
	}
}

func TestRegisterModelsForAuth_OpenAICompatibilityDoesNotFallbackToConfig(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "deepseek",
					BaseURL: "https://api.deepseek.com/v1",
					Models: []config.OpenAICompatibilityModel{
						{Name: "deepseek-v4-flash"},
					},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-openai-compat-no-model-attrs",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":    "api_key",
			"compat_name":  "deepseek",
			"provider_key": "deepseek",
			"base_url":     "https://api.deepseek.com/v1",
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	if models := modelRegistry.GetModelsForClient(auth.ID); len(models) != 0 {
		t.Fatalf("models len = %d, want 0 without openai_compat_models attr; models=%#v", len(models), models)
	}
}

func TestRegisterModelsForAuth_AccountStoreOpenAICompatibilityModels(t *testing.T) {
	service := &Service{cfg: &config.Config{AccountStoreDB: "/tmp/accounts-v1.sqlite"}}
	auth := &coreauth.Auth{
		ID:       "auth-account-store-openai-compat-deepseek",
		Provider: "deepseek",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":            "api_key",
			"compat_name":          "deepseek",
			"provider_key":         "deepseek",
			"base_url":             "https://api.deepseek.com/v1",
			"openai_compat_models": `[{"name":"deepseek-v4-flash"}]`,
		},
	}

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := modelRegistry.GetModelsForClient(auth.ID)
	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1; models=%#v", len(models), models)
	}
	if models[0].ID != "deepseek-v4-flash" {
		t.Fatalf("model ID = %q, want deepseek-v4-flash", models[0].ID)
	}
	providers := modelRegistry.GetModelProviders("deepseek-v4-flash")
	found := false
	for _, provider := range providers {
		if provider == "deepseek" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("deepseek-v4-flash providers = %v, want deepseek", providers)
	}
}
