package thinking

import "testing"

func TestExtractReasoningEffortUsesSuffixOverBody(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning_effort":"low"}`), "openai", "gpt-5.4(high)")
	if got != "high" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "high")
	}
}

func TestExtractReasoningEffortConvertsBudgetToLevel(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`), "claude", "claude-sonnet-4-5")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortSupportsOpenAIResponses(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning":{"effort":"medium"}}`), "openai-response", "gpt-5.4")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortMissingConfigIsEmpty(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai", "gpt-5.4")
	if got != "" {
		t.Fatalf("ExtractReasoningEffort() = %q, want empty", got)
	}
}

func TestExtractTranslatedReasoningEffortUsesCodexShape(t *testing.T) {
	got := ExtractTranslatedReasoningEffort([]byte(`{"reasoning":{"effort":"high"}}`), "codex")
	if got != "high" {
		t.Fatalf("ExtractTranslatedReasoningEffort() = %q, want %q", got, "high")
	}
}

func TestExtractTranslatedReasoningEffortMissingConfigIsEmpty(t *testing.T) {
	got := ExtractTranslatedReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "codex")
	if got != "" {
		t.Fatalf("ExtractTranslatedReasoningEffort() = %q, want empty", got)
	}
}
