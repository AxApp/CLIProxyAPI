package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutePreservesResponsesPayloadFieldsUpstream(t *testing.T) {
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %s, want /responses", r.URL.Path)
		}
		body := readTestRequestBody(t, r)
		capturedPayload <- bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: codexPassthroughPayload(),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case payload := <-capturedPayload:
		assertCodexPassthroughPayload(t, payload)
		if got := gjson.GetBytes(payload, "stream").Bool(); !got {
			t.Fatalf("stream = %v, want true; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream HTTP payload")
	}
}

func TestCodexExecuteUsesOpenAIResponsesFormatBaseURL(t *testing.T) {
	legacyHits := 0
	legacyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacyHits++
		http.Error(w, "legacy base_url should not be used", http.StatusTeapot)
	}))
	defer legacyServer.Close()

	capturedPayload := make(chan []byte, 1)
	responsesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("request path = %s, want /v1/responses", r.URL.Path)
		}
		body := readTestRequestBody(t, r)
		capturedPayload <- bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}` + "\n\n"))
	}))
	defer responsesServer.Close()

	exec := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":                          "sk-test",
		"base_url":                         legacyServer.URL,
		"format_base_url:openai_responses": responsesServer.URL + "/v1",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: codexPassthroughPayload(),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if legacyHits != 0 {
		t.Fatalf("legacy base_url hits = %d, want 0", legacyHits)
	}

	select {
	case payload := <-capturedPayload:
		assertCodexPassthroughPayload(t, payload)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream HTTP payload")
	}
}

func TestCodexExecuteStreamPreservesResponsesPayloadFieldsUpstream(t *testing.T) {
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %s, want /responses", r.URL.Path)
		}
		body := readTestRequestBody(t, r)
		capturedPayload <- bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: codexPassthroughPayload(),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	stream, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	select {
	case payload := <-capturedPayload:
		assertCodexPassthroughPayload(t, payload)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream stream payload")
	}
}

func readTestRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return body
}

func codexPassthroughPayload() []byte {
	return []byte(`{
  "model": "gpt-5-codex",
  "previous_response_id": "resp-1",
  "prompt_cache_retention": "24h",
  "safety_identifier": "safe-user",
  "stream_options": {"include_usage": true},
  "metadata": {"tenant": "acme"},
  "custom_passthrough": {"nested": ["keep", 42]},
  "input": [{"type": "message", "id": "msg-1"}]
}`)
}

func assertCodexPassthroughPayload(t *testing.T, payload []byte) {
	t.Helper()
	if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5-codex" {
		t.Fatalf("model = %q, want gpt-5-codex; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "prompt_cache_retention").String(); got != "24h" {
		t.Fatalf("prompt_cache_retention = %q, want 24h; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "safety_identifier").String(); got != "safe-user" {
		t.Fatalf("safety_identifier = %q, want safe-user; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "stream_options.include_usage").Bool(); !got {
		t.Fatalf("stream_options.include_usage = %v, want true; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "metadata.tenant").String(); got != "acme" {
		t.Fatalf("metadata.tenant = %q, want acme; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "custom_passthrough.nested.1").Int(); got != 42 {
		t.Fatalf("custom_passthrough.nested.1 = %d, want 42; payload=%s", got, payload)
	}
}
