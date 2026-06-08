package claude

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestConvertGeminiResponseToClaude_SignatureOnlyPart(t *testing.T) {
	requestJSON := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"test"}]}]}`)
	thinkingChunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "Thinking before signature.", "thought": true}]
			}
		}]
	}`)
	signatureChunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "", "thoughtSignature": "sig_12345678901234567890"}]
			}
		}]
	}`)

	var param any
	ctx := context.Background()
	out1 := ConvertGeminiResponseToClaude(ctx, "gemini-2.5-pro", requestJSON, requestJSON, thinkingChunk, &param)
	out2 := ConvertGeminiResponseToClaude(ctx, "gemini-2.5-pro", requestJSON, requestJSON, signatureChunk, &param)
	output := string(bytes.Join(out1, nil)) + string(bytes.Join(out2, nil))

	if !strings.Contains(output, `"type":"signature_delta"`) {
		t.Fatalf("expected signature_delta for signature-only part, got: %s", output)
	}
	if strings.Contains(string(bytes.Join(out2, nil)), `"content_block":{"type":"text"`) {
		t.Fatalf("signature-only part must not open an empty text block: %s", string(bytes.Join(out2, nil)))
	}
	if strings.Contains(string(bytes.Join(out2, nil)), `"type":"content_block_stop"`) {
		t.Fatalf("signature-only part must not stop an unopened block: %s", string(bytes.Join(out2, nil)))
	}
}

func TestConvertGeminiResponseToClaude_FinalEventsWithThoughtsTokenOnly(t *testing.T) {
	requestJSON := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"test"}]}]}`)
	thinkingChunk := []byte(`{
		"candidates": [{
			"content": {
				"parts": [{"text": "Thinking only.", "thought": true}]
			}
		}]
	}`)
	finishChunk := []byte(`{
		"candidates": [{
			"finishReason": "STOP",
			"content": {"parts": []}
		}],
		"usageMetadata": {
			"promptTokenCount": 7,
			"thoughtsTokenCount": 5
		}
	}`)

	var param any
	ctx := context.Background()
	ConvertGeminiResponseToClaude(ctx, "gemini-2.5-pro", requestJSON, requestJSON, thinkingChunk, &param)
	finishOut := bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-2.5-pro", requestJSON, requestJSON, finishChunk, &param), nil)
	doneOut := bytes.Join(ConvertGeminiResponseToClaude(ctx, "gemini-2.5-pro", requestJSON, requestJSON, []byte("[DONE]"), &param), nil)
	output := string(finishOut) + string(doneOut)

	if !strings.Contains(output, `"type":"message_delta"`) {
		t.Fatalf("expected message_delta when finish chunk has thoughtsTokenCount only, got: %s", output)
	}
	if !strings.Contains(output, `"output_tokens":5`) {
		t.Fatalf("expected output_tokens to include thoughtsTokenCount, got: %s", output)
	}
	if !strings.Contains(output, `"type":"message_stop"`) {
		t.Fatalf("expected message_stop after [DONE], got: %s", output)
	}
}
