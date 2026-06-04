package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

type websocketHTTPFallbackCaptureExecutor struct {
	mu       sync.Mutex
	payloads map[string][][]byte
}

func (e *websocketHTTPFallbackCaptureExecutor) Identifier() string { return "codex" }

func (e *websocketHTTPFallbackCaptureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketHTTPFallbackCaptureExecutor) ExecuteStream(_ context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}

	e.mu.Lock()
	if e.payloads == nil {
		e.payloads = make(map[string][][]byte)
	}
	e.payloads[authID] = append(e.payloads[authID], bytes.Clone(req.Payload))
	e.mu.Unlock()

	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	if authID == "ws-auth" {
		chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("websocket auth failed")}
	} else {
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`)}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketHTTPFallbackCaptureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *websocketHTTPFallbackCaptureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketHTTPFallbackCaptureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *websocketHTTPFallbackCaptureExecutor) Payloads(authID string) [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	src := e.payloads[authID]
	out := make([][]byte, len(src))
	for i := range src {
		out[i] = bytes.Clone(src[i])
	}
	return out
}

type websocketTransportStatusTestError struct{}

func (websocketTransportStatusTestError) Error() string {
	return "stream closed before response.completed"
}

func (websocketTransportStatusTestError) StatusCode() int { return http.StatusRequestTimeout }

func (websocketTransportStatusTestError) TransportFailureKind() string {
	return cliproxyexecutor.TransportFailureKindWebsocket
}

type websocketTransportFailureExecutor struct{}

func (websocketTransportFailureExecutor) Identifier() string { return "codex" }

func (websocketTransportFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (websocketTransportFailureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, websocketTransportStatusTestError{}
}

func (websocketTransportFailureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (websocketTransportFailureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (websocketTransportFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestManagerExecuteStreamWebsocketHTTPFallbackStripsGenerateOnlyForHTTPAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &websocketHTTPFallbackCaptureExecutor{}
	manager.RegisterExecutor(executor)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("ws-auth", "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	reg.RegisterClient("http-auth", "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient("ws-auth")
		reg.UnregisterClient("http-auth")
	})

	if _, err := manager.Register(context.Background(), &Auth{
		ID:         "ws-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
		Status:     StatusActive,
	}); err != nil {
		t.Fatalf("register websocket auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{
		ID:         "http-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "false"},
		Status:     StatusActive,
	}); err != nil {
		t.Fatalf("register http auth: %v", err)
	}

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","stream":true,"generate":{"mode":"realtime"},"input":[{"type":"message","id":"msg-1"}],"metadata":{"keep":true}}`),
	}
	result, err := manager.ExecuteStream(ctx, []string{"codex"}, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for range result.Chunks {
	}

	wsPayloads := executor.Payloads("ws-auth")
	if len(wsPayloads) != 1 {
		t.Fatalf("websocket auth payload count = %d, want 1", len(wsPayloads))
	}
	if !gjson.GetBytes(wsPayloads[0], "generate").Exists() {
		t.Fatalf("websocket auth payload must retain generate: %s", wsPayloads[0])
	}

	httpPayloads := executor.Payloads("http-auth")
	if len(httpPayloads) != 1 {
		t.Fatalf("http fallback auth payload count = %d, want 1", len(httpPayloads))
	}
	if gjson.GetBytes(httpPayloads[0], "generate").Exists() {
		t.Fatalf("http fallback payload must strip generate: %s", httpPayloads[0])
	}
	if !gjson.GetBytes(httpPayloads[0], "metadata.keep").Bool() {
		t.Fatalf("http fallback payload must preserve unrelated fields: %s", httpPayloads[0])
	}
}

func TestManagerExecuteStreamWebsocketTransportFailureOnlyOpensWebsocketCircuit(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(websocketTransportFailureExecutor{})

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("ws-auth", "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
	t.Cleanup(func() {
		reg.UnregisterClient("ws-auth")
	})

	if _, err := manager.Register(context.Background(), &Auth{
		ID:         "ws-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
		Status:     StatusActive,
	}); err != nil {
		t.Fatalf("register websocket auth: %v", err)
	}

	_, err := manager.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		[]string{"codex"},
		cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","stream":true}`)},
		cliproxyexecutor.Options{},
	)
	if err == nil {
		t.Fatal("expected websocket transport failure")
	}

	updated, ok := manager.GetByID("ws-auth")
	if !ok || updated == nil {
		t.Fatal("expected websocket auth to remain registered")
	}
	if updated.Failed != 1 {
		t.Fatalf("failed count = %d, want 1", updated.Failed)
	}
	if updated.Unavailable {
		t.Fatal("websocket transport failure must not mark auth unavailable")
	}
	if updated.Status == StatusError {
		t.Fatalf("status = %q, want non-error auth status", updated.Status)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("model states = %#v, want no auth/model cooldown from websocket transport failure", updated.ModelStates)
	}
	if AuthAllowsWebsockets(updated) {
		t.Fatal("expected temporary websocket circuit to disable websocket transport")
	}
}
