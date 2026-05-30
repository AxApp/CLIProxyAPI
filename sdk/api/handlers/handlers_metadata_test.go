package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenscodex"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

func TestRequestExecutionMetadataIncludesExecutionSessionWithoutIdempotencyKey(t *testing.T) {
	ctx := WithExecutionSessionID(context.Background(), "session-1")

	meta := requestExecutionMetadata(ctx)
	if got := meta[coreexecutor.ExecutionSessionMetadataKey]; got != "session-1" {
		t.Fatalf("ExecutionSessionMetadataKey = %v, want %q", got, "session-1")
	}
	if _, ok := meta[idempotencyKeyMetadataKey]; ok {
		t.Fatalf("unexpected idempotency key in metadata: %v", meta[idempotencyKeyMetadataKey])
	}
}

func TestSetReasoningEffortMetadataUsesSuffixOverBody(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai", "gpt-5.4(high)", []byte(`{"reasoning_effort":"low"}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "high" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "high")
	}
}

func TestSetReasoningEffortMetadataSupportsOpenAIResponses(t *testing.T) {
	meta := make(map[string]any)

	setReasoningEffortMetadata(meta, "openai-response", "gpt-5.4", []byte(`{"reasoning":{"effort":"medium"}}`))

	if got := meta[coreexecutor.ReasoningEffortMetadataKey]; got != "medium" {
		t.Fatalf("ReasoningEffortMetadataKey = %v, want %q", got, "medium")
	}
}

func TestSetServiceTierMetadataExtractsValue(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"service_tier":"priority"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "priority" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "priority")
	}
}

func TestSetServiceTierMetadataDefaultsWhenMissing(t *testing.T) {
	meta := make(map[string]any)

	setServiceTierMetadata(meta, []byte(`{"model":"gpt-5.4"}`))

	gotServiceTier := meta[coreexecutor.ServiceTierMetadataKey]
	if gotServiceTier != "default" {
		t.Fatalf("ServiceTierMetadataKey = %v, want %q", gotServiceTier, "default")
	}
}

func TestAttachCodexRequestMetadataForOpenAIResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-OpenAI-Subagent", "collab_spawn")
	ginCtx.Request.Header.Set("Session_id", "session-1")
	ginCtx.Request.Header.Set("X-Client-Request-Id", "client-1")
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"thread-1","thread_source":"subagent","turn_id":"turn-1","turn_started_at_unix_ms":1843142400000}`)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	meta := requestExecutionMetadata(ctx)

	attachCodexRequestMetadata(meta, headersFromContext(ctx), []byte(`{"model":"gpt-5.1"}`), "gpt-5.1", "openai-response")

	raw := meta[gettokenscodex.MetadataKey]
	reqCtx, ok := raw.(gettokenscodex.RequestContext)
	if !ok {
		t.Fatalf("metadata[%s] = %T, want gettokenscodex.RequestContext", gettokenscodex.MetadataKey, raw)
	}
	if reqCtx.RequestKind != gettokenscodex.RequestKindMain || reqCtx.RequestedModel != "gpt-5.1" {
		t.Fatalf("codex context = %#v, want main request context with requested model", reqCtx)
	}
	if got := meta[gettokenscodex.RequestKindMetadataKey]; got != string(gettokenscodex.RequestKindMain) {
		t.Fatalf("request kind metadata = %v, want main", got)
	}
	if got := meta[gettokenscodex.SessionIDMetadataKey]; got != "session-1" {
		t.Fatalf("session metadata = %v, want session-1", got)
	}
	if got := meta[gettokenscodex.ThreadIDMetadataKey]; got != "thread-1" {
		t.Fatalf("thread metadata = %v, want thread-1", got)
	}
	if got := meta[gettokenscodex.TurnIDMetadataKey]; got != "turn-1" {
		t.Fatalf("turn metadata = %v, want turn-1", got)
	}
}

func TestAttachCodexRequestMetadataSkipsNonCodexHandlers(t *testing.T) {
	meta := make(map[string]any)

	attachCodexRequestMetadata(meta, nil, []byte(`{"model":"gpt-5.1"}`), "gpt-5.1", "openai")

	if _, ok := meta[gettokenscodex.MetadataKey]; ok {
		t.Fatalf("unexpected codex request context for non-Codex handler")
	}
}
