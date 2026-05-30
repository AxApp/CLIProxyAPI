package openai

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenscodex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestCodexSubagentResponsesHTTPDownstreamUpstreamSmoke(t *testing.T) {
	gin.SetMode(gin.TestMode)

	capturedHeaders := make(chan http.Header, 1)
	capturedBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream path = %s, want /responses", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		capturedHeaders <- r.Header.Clone()
		capturedBody <- bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp-smoke-http","output":[{"type":"message","id":"msg-out","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer upstream.Close()

	capturedRouteContext := make(chan gettokenscodex.RequestContext, 1)
	unregisterPolicy := gettokensrouting.RegisterPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageRequest,
		Name:  "codex-subagent-http-smoke",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model == "smoke-subagent-model" && req.CodexRequest != nil {
				capturedRouteContext <- *req.CodexRequest
			}
			return gettokensrouting.PolicyDecision{}
		},
	})
	defer unregisterPolicy()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}))
	auth := &coreauth.Auth{
		ID:         "auth-smoke-http",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-smoke-http", "base_url": upstream.URL},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "smoke-subagent-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"smoke-subagent-model","input":[{"type":"message","id":"msg-in"}]}`))
	req.Header.Set("Content-Type", "application/json")
	setSmokeCodexHeaders(req.Header, "review")
	req.Header.Set("Authorization", "Bearer inbound-should-not-forward")
	req.Header.Set("Cookie", "session=should-not-forward")
	resp := httptest.NewRecorder()

	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "resp-smoke-http") {
		t.Fatalf("downstream body missing upstream response id: %s", resp.Body.String())
	}

	headers := waitSmokeHeaders(t, capturedHeaders)
	body := waitSmokeBody(t, capturedBody)
	assertSmokeUpstreamHeaders(t, headers, "review", "Bearer sk-smoke-http")
	if got := gjson.GetBytes(body, "model").String(); got != "smoke-subagent-model" {
		t.Fatalf("upstream body model = %q, want smoke-subagent-model; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "input.0.id").String(); got != "msg-in" {
		t.Fatalf("upstream body input id = %q, want msg-in; body=%s", got, body)
	}

	select {
	case routeCtx := <-capturedRouteContext:
		if routeCtx.RequestKind != gettokenscodex.RequestKindSubagent || routeCtx.SubagentSource != "review" {
			t.Fatalf("route codex context = %#v, want review subagent", routeCtx)
		}
		if routeCtx.SessionID != "session-smoke" || routeCtx.ThreadID != "thread-smoke" || routeCtx.TurnID != "turn-smoke" {
			t.Fatalf("route codex context ids = %#v, want session/thread/turn smoke ids", routeCtx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for route context")
	}
}

func TestCodexSubagentResponsesWebSocketDownstreamUpstreamSmoke(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedHeaders := make(chan http.Header, 1)
	capturedBody := make(chan []byte, 1)
	releaseUpstream := make(chan struct{})
	t.Cleanup(func() { close(releaseUpstream) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream path = %s, want /responses", r.URL.Path)
		}
		capturedHeaders <- r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream websocket: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream websocket payload: %v", err)
		}
		capturedBody <- bytes.Clone(payload)
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-smoke-ws","output":[{"type":"message","id":"msg-out","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		if err := conn.WriteMessage(websocket.TextMessage, completed); err != nil {
			t.Fatalf("write upstream websocket response: %v", err)
		}
		select {
		case <-releaseUpstream:
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()

	capturedRouteContext := make(chan gettokenscodex.RequestContext, 1)
	unregisterPolicy := gettokensrouting.RegisterPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageRequest,
		Name:  "codex-subagent-ws-smoke",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model == "smoke-subagent-model-ws" && req.CodexRequest != nil {
				capturedRouteContext <- *req.CodexRequest
			}
			return gettokensrouting.PolicyDecision{}
		},
	})
	defer unregisterPolicy()

	manager := coreauth.NewManager(nil, nil, nil)
	wsExecutor := runtimeexecutor.NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	manager.RegisterExecutor(wsExecutor)
	auth := &coreauth.Auth{
		ID:       "auth-smoke-ws",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":    "sk-smoke-ws",
			"base_url":   upstream.URL,
			"websockets": "true",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "smoke-subagent-model-ws"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		wsExecutor.CloseExecutionSession(coreauth.CloseAllExecutionSessionsID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	downstream := httptest.NewServer(router)
	defer downstream.Close()

	wsURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/responses/ws"
	downstreamHeaders := http.Header{}
	setSmokeCodexHeaders(downstreamHeaders, "collab_spawn")
	downstreamHeaders.Set("Authorization", "Bearer inbound-should-not-forward")
	downstreamHeaders.Set("Cookie", "session=should-not-forward")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, downstreamHeaders)
	if err != nil {
		t.Fatalf("dial downstream websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	requestPayload := []byte(`{"type":"response.create","model":"smoke-subagent-model-ws","input":[{"type":"message","id":"msg-in-ws"}]}`)
	if err := conn.WriteMessage(websocket.TextMessage, requestPayload); err != nil {
		t.Fatalf("write downstream websocket request: %v", err)
	}
	_, downstreamPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read downstream websocket response: %v", err)
	}
	if got := gjson.GetBytes(downstreamPayload, "response.id").String(); got != "resp-smoke-ws" {
		t.Fatalf("downstream websocket response id = %q, want resp-smoke-ws; payload=%s", got, downstreamPayload)
	}

	headers := waitSmokeHeaders(t, capturedHeaders)
	body := waitSmokeBody(t, capturedBody)
	assertSmokeUpstreamHeaders(t, headers, "collab_spawn", "Bearer sk-smoke-ws")
	if got := gjson.GetBytes(body, "type").String(); got != "response.create" {
		t.Fatalf("upstream websocket body type = %q, want response.create; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "smoke-subagent-model-ws" {
		t.Fatalf("upstream websocket body model = %q, want smoke-subagent-model-ws; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "input.0.id").String(); got != "msg-in-ws" {
		t.Fatalf("upstream websocket body input id = %q, want msg-in-ws; body=%s", got, body)
	}

	select {
	case routeCtx := <-capturedRouteContext:
		if routeCtx.RequestKind != gettokenscodex.RequestKindSubagent || routeCtx.SubagentSource != "collab_spawn" {
			t.Fatalf("route codex context = %#v, want collab_spawn subagent", routeCtx)
		}
		if routeCtx.SessionID != "session-smoke" || routeCtx.ThreadID != "thread-smoke" || routeCtx.TurnID != "turn-smoke" {
			t.Fatalf("route codex context ids = %#v, want session/thread/turn smoke ids", routeCtx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for route context")
	}
}

func setSmokeCodexHeaders(headers http.Header, subagent string) {
	headers.Set("Version", "0.133.0-smoke")
	headers.Set("X-Codex-Installation-Id", "install-smoke")
	headers.Set("X-Codex-Turn-State", "turn-state-smoke")
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"session-meta-smoke","thread_id":"thread-smoke","thread_source":"subagent","turn_id":"turn-smoke","sandbox":"workspace-write","turn_started_at_unix_ms":1843142400000}`)
	headers.Set("X-Client-Request-Id", "client-smoke")
	headers.Set("X-Codex-Parent-Thread-Id", "parent-smoke")
	headers.Set("X-Codex-Window-Id", "window-smoke")
	headers.Set("X-OpenAI-Subagent", subagent)
	headers.Set("X-OpenAI-Memgen-Request", "memgen-smoke")
	headers.Set("X-OAI-Attestation", "attestation-smoke")
	headers.Set("Session_id", "session-smoke")
	headers.Set("Session-Id", "session-dash-smoke")
	headers.Set("Thread-Id", "thread-smoke")
}

func assertSmokeUpstreamHeaders(t *testing.T, headers http.Header, subagent string, authorization string) {
	t.Helper()
	want := map[string]string{
		"Authorization":            authorization,
		"Version":                  "0.133.0-smoke",
		"X-Codex-Installation-Id":  "install-smoke",
		"X-Codex-Turn-State":       "turn-state-smoke",
		"X-Client-Request-Id":      "client-smoke",
		"X-Codex-Parent-Thread-Id": "parent-smoke",
		"X-Codex-Window-Id":        "window-smoke",
		"X-OpenAI-Subagent":        subagent,
		"X-OpenAI-Memgen-Request":  "memgen-smoke",
		"X-OAI-Attestation":        "attestation-smoke",
		"Session-Id":               "session-dash-smoke",
		"Thread-Id":                "thread-smoke",
	}
	for key, value := range want {
		if got := headers.Get(key); got != value {
			t.Fatalf("upstream header %s = %q, want %q; headers=%#v", key, got, value, headers)
		}
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); !strings.Contains(got, `"turn_id":"turn-smoke"`) {
		t.Fatalf("upstream turn metadata = %q, want turn-smoke", got)
	}
	if got := headers.Get("Cookie"); got != "" {
		t.Fatalf("upstream Cookie = %q, want empty", got)
	}
}

func waitSmokeHeaders(t *testing.T, headersCh <-chan http.Header) http.Header {
	t.Helper()
	select {
	case headers := <-headersCh:
		return headers
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream headers")
	}
	return nil
}

func waitSmokeBody(t *testing.T, bodyCh <-chan []byte) []byte {
	t.Helper()
	select {
	case body := <-bodyCh:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream body")
	}
	return nil
}
