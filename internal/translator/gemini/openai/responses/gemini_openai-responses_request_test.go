package responses

import (
	"strconv"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToGemini_StripsTrailingAssistantPrefill(t *testing.T) {
	inputJSON := `{
		"model": "gpt-5.4",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "previous answer"}]
			}
		]
	}`

	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	contents := resultJSON.Get("contents").Array()

	if len(contents) != 1 {
		t.Fatalf("contents length = %d, want 1. contents=%s", len(contents), resultJSON.Get("contents").Raw)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("final remaining role = %q, want %q", got, "user")
	}
}

func TestConvertOpenAIResponsesRequestToGemini_SystemAndDeveloperRolesBecomeSystemInstruction(t *testing.T) {
	input := []byte(`{
		"instructions": "base instruction",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "system text"}]
			},
			{
				"type": "message",
				"role": "developer",
				"content": [{"type": "input_text", "text": "developer text"}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "hello"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToGemini("gemini-test", input, false)
	result := gjson.ParseBytes(out)

	parts := result.Get("systemInstruction.parts")
	if got := parts.Get("#").Int(); got != 3 {
		t.Fatalf("systemInstruction parts count = %d, want 3; output=%s", got, string(out))
	}

	wantParts := []string{"base instruction", "system text", "developer text"}
	for idx, want := range wantParts {
		if got := parts.Get(strconv.Itoa(idx) + ".text").String(); got != want {
			t.Fatalf("systemInstruction.parts.%d.text = %q, want %q", idx, got, want)
		}
	}

	contents := result.Get("contents")
	if got := contents.Get("#").Int(); got != 1 {
		t.Fatalf("contents count = %d, want 1; output=%s", got, string(out))
	}
	if got := contents.Get("0.role").String(); got != "user" {
		t.Fatalf("contents.0.role = %q, want user", got)
	}
	if got := contents.Get("0.parts.0.text").String(); got != "hello" {
		t.Fatalf("contents.0.parts.0.text = %q, want hello", got)
	}
}
