package gettokenscodex

import (
	"net/http"
	"testing"
)

func TestExtractRequestContextSubagentAndTurnMetadata(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-OpenAI-Subagent", " review ")
	headers.Set("Session_id", "session-header")
	headers.Set("X-Client-Request-Id", "client-1")
	headers.Set("thread-id", "thread-header")
	headers.Set("x-codex-window-id", "window-1")
	headers.Set("x-codex-parent-thread-id", "parent-1")
	headers.Set("x-codex-installation-id", "install-1")
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"session-meta","thread_id":"thread-meta","thread_source":"subagent","turn_id":"turn-1","sandbox":"workspace-write","turn_started_at_unix_ms":1843142400000,"workspaces":{"/private/repo":{"associated_remote_urls":{"origin":"https://secret.example/repo"}}}}`)
	body := []byte(`{"model":"gpt-5.1","prompt_cache_key":"prompt-cache-1","client_metadata":{"x-codex-window-id":"window-body"}}`)

	got := ExtractRequestContext(headers, body, "fallback-model")

	if got.RequestKind != RequestKindSubagent {
		t.Fatalf("RequestKind = %q, want %q", got.RequestKind, RequestKindSubagent)
	}
	if got.SubagentSource != "review" {
		t.Fatalf("SubagentSource = %q, want review", got.SubagentSource)
	}
	if got.RequestedModel != "gpt-5.1" {
		t.Fatalf("RequestedModel = %q, want gpt-5.1", got.RequestedModel)
	}
	if got.SessionID != "session-header" {
		t.Fatalf("SessionID = %q, want session-header", got.SessionID)
	}
	if got.ClientRequestID != "client-1" {
		t.Fatalf("ClientRequestID = %q, want client-1", got.ClientRequestID)
	}
	if got.ThreadID != "thread-header" {
		t.Fatalf("ThreadID = %q, want thread-header", got.ThreadID)
	}
	if got.ThreadSource != "subagent" {
		t.Fatalf("ThreadSource = %q, want subagent", got.ThreadSource)
	}
	if got.TurnID != "turn-1" {
		t.Fatalf("TurnID = %q, want turn-1", got.TurnID)
	}
	if got.Sandbox != "workspace-write" {
		t.Fatalf("Sandbox = %q, want workspace-write", got.Sandbox)
	}
	if got.TurnStartedAtUnixMs != 1843142400000 {
		t.Fatalf("TurnStartedAtUnixMs = %d, want 1843142400000", got.TurnStartedAtUnixMs)
	}
	if got.CodexWindowID != "window-1" {
		t.Fatalf("CodexWindowID = %q, want window-1", got.CodexWindowID)
	}
	if got.ParentThreadID != "parent-1" {
		t.Fatalf("ParentThreadID = %q, want parent-1", got.ParentThreadID)
	}
	if got.InstallationID != "install-1" {
		t.Fatalf("InstallationID = %q, want install-1", got.InstallationID)
	}
	if got.PromptCacheKey != "prompt-cache-1" {
		t.Fatalf("PromptCacheKey = %q, want prompt-cache-1", got.PromptCacheKey)
	}
	if got.MetadataParseError != "" {
		t.Fatalf("MetadataParseError = %q, want empty", got.MetadataParseError)
	}
}

func TestExtractRequestContextMainRequestAndMalformedMetadata(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{broken`)
	body := []byte(`{"input":"hello"}`)

	got := ExtractRequestContext(headers, body, "fallback-model")

	if got.RequestKind != RequestKindMain {
		t.Fatalf("RequestKind = %q, want %q", got.RequestKind, RequestKindMain)
	}
	if got.SubagentSource != "" {
		t.Fatalf("SubagentSource = %q, want empty", got.SubagentSource)
	}
	if got.RequestedModel != "fallback-model" {
		t.Fatalf("RequestedModel = %q, want fallback-model", got.RequestedModel)
	}
	if got.MetadataParseError == "" {
		t.Fatalf("MetadataParseError = empty, want bounded parse error")
	}
}

func TestCodexResponsesClientContextHeadersContainsLatestSubagentHeaders(t *testing.T) {
	headers := CodexResponsesClientContextHeaders()
	want := []string{
		"Version",
		"x-codex-installation-id",
		"x-codex-turn-state",
		"x-codex-turn-metadata",
		"x-client-request-id",
		"x-codex-parent-thread-id",
		"x-codex-window-id",
		"x-openai-subagent",
		"x-openai-memgen-request",
		"x-oai-attestation",
		"session-id",
		"thread-id",
	}
	for _, key := range want {
		if !headers.Contains(key) {
			t.Fatalf("CodexResponsesClientContextHeaders missing %s", key)
		}
	}
	if headers.Contains("Authorization") || headers.Contains("Cookie") {
		t.Fatalf("CodexResponsesClientContextHeaders must not include credential headers: %#v", headers)
	}
}
