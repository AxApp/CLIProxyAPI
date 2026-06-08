package gemini

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToCodexSystemInstructionCamelCase(t *testing.T) {
	input := []byte(`{
		"systemInstruction": {
			"parts": [
				{"text": "You are concise."},
				{"text": "Use Chinese."}
			]
		},
		"contents": [
			{"role": "user", "parts": [{"text": "Hello"}]}
		]
	}`)

	output := ConvertGeminiRequestToCodex("gpt-5", input, false)
	first := gjson.GetBytes(output, "input.0")

	if first.Get("role").String() != "developer" {
		t.Fatalf("expected first input role developer, got %s; output=%s", first.Get("role").String(), string(output))
	}
	if first.Get("content.0.type").String() != "input_text" {
		t.Fatalf("expected system content input_text, got %s; output=%s", first.Get("content.0.type").String(), string(output))
	}
	if first.Get("content.0.text").String() != "You are concise." {
		t.Fatalf("expected first system text, got %q; output=%s", first.Get("content.0.text").String(), string(output))
	}
	if first.Get("content.1.text").String() != "Use Chinese." {
		t.Fatalf("expected second system text, got %q; output=%s", first.Get("content.1.text").String(), string(output))
	}
}

func TestConvertGeminiRequestToCodexSystemInstructionSnakeCaseStillWorks(t *testing.T) {
	input := []byte(`{
		"system_instruction": {
			"parts": [
				{"text": "Keep existing snake case behavior."}
			]
		},
		"contents": [
			{"role": "user", "parts": [{"text": "Hello"}]}
		]
	}`)

	output := ConvertGeminiRequestToCodex("gpt-5", input, false)
	first := gjson.GetBytes(output, "input.0")

	if first.Get("role").String() != "developer" {
		t.Fatalf("expected first input role developer, got %s; output=%s", first.Get("role").String(), string(output))
	}
	if first.Get("content.0.text").String() != "Keep existing snake case behavior." {
		t.Fatalf("expected snake_case system text, got %q; output=%s", first.Get("content.0.text").String(), string(output))
	}
}
