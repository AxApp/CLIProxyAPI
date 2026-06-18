package registry

import "testing"

func TestCodexStaticModelsIncludeGPT55(t *testing.T) {
	tierModels := map[string][]*ModelInfo{
		"free": GetCodexFreeModels(),
		"team": GetCodexTeamModels(),
		"plus": GetCodexPlusModels(),
		"pro":  GetCodexProModels(),
	}

	for tier, models := range tierModels {
		t.Run(tier, func(t *testing.T) {
			model := findModelInfo(models, "gpt-5.5")
			if model == nil {
				t.Fatalf("expected codex %s tier to include gpt-5.5", tier)
			}
			assertGPT55ModelInfo(t, tier, model)
		})
	}

	model := LookupStaticModelInfo("gpt-5.5")
	if model == nil {
		t.Fatal("expected LookupStaticModelInfo to find gpt-5.5")
	}
	assertGPT55ModelInfo(t, "lookup", model)
}

func TestCodexStaticModelsIncludeDeepSeekV4OpenAICompatibleModels(t *testing.T) {
	models := GetStaticModelDefinitionsByChannel("codex")
	for _, id := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		model := findModelInfo(models, id)
		if model == nil {
			t.Fatalf("expected codex static definitions to include %s", id)
		}
		if model.OwnedBy != "deepseek" {
			t.Fatalf("%s OwnedBy = %q, want deepseek", id, model.OwnedBy)
		}
		if model.Type != "openai-compatibility" {
			t.Fatalf("%s Type = %q, want openai-compatibility", id, model.Type)
		}
		assertThinkingLevels(t, id, model, []string{"low", "medium", "high", "xhigh", "max"})
	}
}

func TestClaudeStaticModelsIncludeOpus48(t *testing.T) {
	model := findModelInfo(GetClaudeModels(), "claude-opus-4-8")
	if model == nil {
		t.Fatal("expected Claude static definitions to include claude-opus-4-8")
	}
	if model.OwnedBy != "anthropic" {
		t.Fatalf("OwnedBy = %q, want anthropic", model.OwnedBy)
	}
	if model.Type != "claude" {
		t.Fatalf("Type = %q, want claude", model.Type)
	}
	assertThinkingLevels(t, "claude-opus-4-8", model, []string{"low", "medium", "high", "xhigh", "max"})

	if LookupStaticModelInfo("claude-opus-4-8") == nil {
		t.Fatal("expected LookupStaticModelInfo to find claude-opus-4-8")
	}
}

func TestClaudeStaticModelsIncludeFable5(t *testing.T) {
	model := findModelInfo(GetClaudeModels(), "claude-fable-5")
	if model == nil {
		t.Fatal("expected Claude static definitions to include claude-fable-5")
	}
	if model.OwnedBy != "anthropic" {
		t.Fatalf("OwnedBy = %q, want anthropic", model.OwnedBy)
	}
	if model.Type != "claude" {
		t.Fatalf("Type = %q, want claude", model.Type)
	}
	if model.ContextLength != 1000000 {
		t.Fatalf("ContextLength = %d, want 1000000", model.ContextLength)
	}
	if model.MaxCompletionTokens != 128000 {
		t.Fatalf("MaxCompletionTokens = %d, want 128000", model.MaxCompletionTokens)
	}
	assertThinkingLevels(t, "claude-fable-5", model, []string{"low", "medium", "high", "xhigh", "max"})

	if LookupStaticModelInfo("claude-fable-5") == nil {
		t.Fatal("expected LookupStaticModelInfo to find claude-fable-5")
	}
}

func TestKimiStaticModelsIncludeK27Code(t *testing.T) {
	model := findModelInfo(GetKimiModels(), "kimi-k2.7-code")
	if model == nil {
		t.Fatal("expected Kimi static definitions to include kimi-k2.7-code")
	}
	if model.OwnedBy != "moonshot" {
		t.Fatalf("OwnedBy = %q, want moonshot", model.OwnedBy)
	}
	if model.Type != "kimi" {
		t.Fatalf("Type = %q, want kimi", model.Type)
	}
	if model.ContextLength != 262144 {
		t.Fatalf("ContextLength = %d, want 262144", model.ContextLength)
	}
	if model.MaxCompletionTokens != 65536 {
		t.Fatalf("MaxCompletionTokens = %d, want 65536", model.MaxCompletionTokens)
	}
	if model.Thinking == nil || model.Thinking.Min != 1024 || model.Thinking.Max != 32000 || model.Thinking.ZeroAllowed || !model.Thinking.DynamicAllowed {
		t.Fatalf("unexpected thinking support: %+v", model.Thinking)
	}
}

func TestXAIStaticModelsIncludeComposer25Fast(t *testing.T) {
	model := findModelInfo(GetXAIModels(), "grok-composer-2.5-fast")
	if model == nil {
		t.Fatal("expected XAI static definitions to include grok-composer-2.5-fast")
	}
	if model.OwnedBy != "xai" {
		t.Fatalf("OwnedBy = %q, want xai", model.OwnedBy)
	}
	if model.Type != "xai" {
		t.Fatalf("Type = %q, want xai", model.Type)
	}
	if model.DisplayName != "Composer 2.5 Fast" {
		t.Fatalf("DisplayName = %q, want Composer 2.5 Fast", model.DisplayName)
	}
	if model.ContextLength != 200000 {
		t.Fatalf("ContextLength = %d, want 200000", model.ContextLength)
	}
	if model.MaxCompletionTokens != 32768 {
		t.Fatalf("MaxCompletionTokens = %d, want 32768", model.MaxCompletionTokens)
	}
	assertThinkingLevels(t, "grok-composer-2.5-fast", model, []string{"low", "medium", "high"})
}

func TestWithXAIBuiltinsAddsVideoModel(t *testing.T) {
	models := WithXAIBuiltins(nil)
	found := false
	for _, model := range models {
		if model != nil && model.ID == xaiBuiltinVideoModelID {
			found = true
			if model.OwnedBy != "xai" {
				t.Fatalf("OwnedBy = %q, want xai", model.OwnedBy)
			}
		}
	}
	if !found {
		t.Fatalf("expected %s builtin model", xaiBuiltinVideoModelID)
	}
}

func TestWithXAIBuiltinsAddsPreviewVideoModel(t *testing.T) {
	model := findModelInfo(WithXAIBuiltins(nil), xaiBuiltinVideo15PreviewModelID)
	if model == nil {
		t.Fatalf("expected %s builtin model", xaiBuiltinVideo15PreviewModelID)
	}
	if model.OwnedBy != "xai" {
		t.Fatalf("OwnedBy = %q, want xai", model.OwnedBy)
	}
	if model.Type != "xai" {
		t.Fatalf("Type = %q, want xai", model.Type)
	}
}

func TestValidateModelsCatalogAllowsMissingSections(t *testing.T) {
	data := validTestModelsCatalog()
	data.XAI = nil

	if err := validateModelsCatalog(data); err != nil {
		t.Fatalf("validateModelsCatalog() error = %v", err)
	}
}

func TestValidateModelsCatalogRejectsInvalidDefinitions(t *testing.T) {
	data := validTestModelsCatalog()
	data.Claude = []*ModelInfo{{ID: ""}}

	if err := validateModelsCatalog(data); err == nil {
		t.Fatal("expected invalid model definition error")
	}
}

func validTestModelsCatalog() *staticModelsJSON {
	models := []*ModelInfo{{ID: "test-model"}}
	return &staticModelsJSON{
		Claude:      models,
		Gemini:      models,
		Vertex:      models,
		GeminiCLI:   models,
		AIStudio:    models,
		CodexFree:   models,
		CodexTeam:   models,
		CodexPlus:   models,
		CodexPro:    models,
		Kimi:        models,
		Antigravity: models,
		XAI:         models,
	}
}

func findModelInfo(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func assertGPT55ModelInfo(t *testing.T, source string, model *ModelInfo) {
	t.Helper()

	if model.ID != "gpt-5.5" {
		t.Fatalf("%s id mismatch: got %q", source, model.ID)
	}
	if model.Object != "model" {
		t.Fatalf("%s object mismatch: got %q", source, model.Object)
	}
	if model.Created != 1776902400 {
		t.Fatalf("%s created timestamp mismatch: got %d", source, model.Created)
	}
	if model.OwnedBy != "openai" {
		t.Fatalf("%s owned_by mismatch: got %q", source, model.OwnedBy)
	}
	if model.Type != "openai" {
		t.Fatalf("%s type mismatch: got %q", source, model.Type)
	}
	if model.DisplayName != "GPT 5.5" {
		t.Fatalf("%s display name mismatch: got %q", source, model.DisplayName)
	}
	if model.Version != "gpt-5.5" {
		t.Fatalf("%s version mismatch: got %q", source, model.Version)
	}
	if model.Description != "Frontier model for complex coding, research, and real-world work." {
		t.Fatalf("%s description mismatch: got %q", source, model.Description)
	}
	if model.ContextLength != 272000 {
		t.Fatalf("%s context length mismatch: got %d", source, model.ContextLength)
	}
	if model.MaxCompletionTokens != 128000 {
		t.Fatalf("%s max completion tokens mismatch: got %d", source, model.MaxCompletionTokens)
	}
	if len(model.SupportedParameters) != 1 || model.SupportedParameters[0] != "tools" {
		t.Fatalf("%s supported parameters mismatch: got %v", source, model.SupportedParameters)
	}
	if model.Thinking == nil {
		t.Fatalf("%s missing thinking support", source)
	}

	want := []string{"low", "medium", "high", "xhigh"}
	if len(model.Thinking.Levels) != len(want) {
		t.Fatalf("%s thinking level count mismatch: got %d, want %d", source, len(model.Thinking.Levels), len(want))
	}
	for i, level := range want {
		if model.Thinking.Levels[i] != level {
			t.Fatalf("%s thinking level %d mismatch: got %q, want %q", source, i, model.Thinking.Levels[i], level)
		}
	}
}

func assertThinkingLevels(t *testing.T, source string, model *ModelInfo, want []string) {
	t.Helper()

	if model.Thinking == nil {
		t.Fatalf("%s missing thinking support", source)
	}
	if len(model.Thinking.Levels) != len(want) {
		t.Fatalf("%s thinking level count mismatch: got %d, want %d", source, len(model.Thinking.Levels), len(want))
	}
	for i, level := range want {
		if model.Thinking.Levels[i] != level {
			t.Fatalf("%s thinking level %d mismatch: got %q, want %q", source, i, model.Thinking.Levels[i], level)
		}
	}
}
