package responses

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func prettyJSONForTest(raw []byte) string {
	if !gjson.ValidBytes(raw) {
		return string(raw)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MergeConsecutiveFunctionCalls(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"exec_command:0","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call","call_id":"exec_command:1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"exec_command:0","output":"ok0"},
			{"type":"function_call_output","call_id":"exec_command:1","output":"ok1"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, true)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	msgs := gjson.GetBytes(out, "messages")
	if !msgs.Exists() || !msgs.IsArray() {
		t.Fatalf("messages should be an array")
	}
	if got := len(msgs.Array()); got != 3 {
		t.Fatalf("messages count = %d, want %d", got, 3)
	}

	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want %q", got, "assistant")
	}
	if got := len(gjson.GetBytes(out, "messages.0.tool_calls").Array()); got != 2 {
		t.Fatalf("messages.0.tool_calls length = %d, want %d", got, 2)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != "exec_command:0" {
		t.Fatalf("messages.0.tool_calls.0.id = %q, want %q", got, "exec_command:0")
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.1.id").String(); got != "exec_command:1" {
		t.Fatalf("messages.0.tool_calls.1.id = %q, want %q", got, "exec_command:1")
	}

	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "exec_command:0" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "exec_command:0")
	}
	if got := gjson.GetBytes(out, "messages.2.tool_call_id").String(); got != "exec_command:1" {
		t.Fatalf("messages.2.tool_call_id = %q, want %q", got, "exec_command:1")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SplitFunctionCallsWhenInterrupted(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},
			{"type":"message","role":"user","content":"next"},
			{"type":"function_call","call_id":"call_b","name":"tool_b","arguments":"{}"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, false)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 3 {
		t.Fatalf("messages count = %d, want %d", got, 3)
	}
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.id").String(); got != "call_a" {
		t.Fatalf("messages.0.tool_calls.0.id = %q, want %q", got, "call_a")
	}
	if got := gjson.GetBytes(out, "messages.2.tool_calls.0.id").String(); got != "call_b" {
		t.Fatalf("messages.2.tool_calls.0.id = %q, want %q", got, "call_b")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DefersMessageUntilToolOutput(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type":"function_call","call_id":"call_x","name":"exec_command","arguments":"{\"cmd\":\"echo hi\"}"},
			{"type":"message","role":"user","content":"Approved command prefix saved"},
			{"type":"function_call_output","call_id":"call_x","output":"ok"},
			{"type":"message","role":"user","content":"next"}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("kimi-k2.6", raw, true)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 4 {
		t.Fatalf("messages count = %d, want %d", got, 4)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want %q", got, "assistant")
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "tool" {
		t.Fatalf("messages.1.role = %q, want %q", got, "tool")
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_x" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_x")
	}
	if got := gjson.GetBytes(out, "messages.2.role").String(); got != "user" {
		t.Fatalf("messages.2.role = %q, want %q", got, "user")
	}
	if got := gjson.GetBytes(out, "messages.2.content").String(); got != "Approved command prefix saved" {
		t.Fatalf("messages.2.content = %q, want %q", got, "Approved command prefix saved")
	}
	if got := gjson.GetBytes(out, "messages.3.content").String(); got != "next" {
		t.Fatalf("messages.3.content = %q, want %q", got, "next")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_ConvertsNestedFunctionTools(t *testing.T) {
	raw := []byte(`{
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "read_file",
					"description": "Read a file",
					"parameters": {
						"type": "object",
						"properties": {
							"path": {"type": "string"}
						},
						"required": ["path"]
					}
				}
			}
		]
	}`)
	t.Logf("input json:\n%s", prettyJSONForTest(raw))

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", raw, false)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "read_file" {
		t.Fatalf("tools.0.function.name = %q, want read_file", got)
	}
	if got := gjson.GetBytes(out, "tools.0.function.description").String(); got != "Read a file" {
		t.Fatalf("tools.0.function.description = %q, want Read a file", got)
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.properties.path.type").String(); got != "string" {
		t.Fatalf("tools.0.function.parameters.properties.path.type = %q, want string", got)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SkipsFunctionToolsWithoutNames(t *testing.T) {
	raw := []byte(`{
		"tools": [
			{"type": "function", "function": {"description": "missing name", "parameters": {}}},
			{"type": "function", "function": {"name": "list_files", "parameters": {}}}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", raw, false)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1; output=%s", len(tools), out)
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "list_files" {
		t.Fatalf("tools.0.function.name = %q, want list_files", got)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DoesNotInjectReasoningContentGlobally(t *testing.T) {
	raw := []byte(`{
		"input": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "private chain"}]},
			{"type": "message", "role": "user", "content": "continue"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("test-model", raw, false)
	t.Logf("output json:\n%s", prettyJSONForTest(out))

	for _, msg := range gjson.GetBytes(out, "messages").Array() {
		if msg.Get("reasoning_content").Exists() {
			t.Fatalf("reasoning_content should not be injected by the generic converter: %s", out)
		}
	}
	if got := len(gjson.GetBytes(out, "messages").Array()); got != 1 {
		t.Fatalf("messages length = %d, want 1; output=%s", got, out)
	}
}
