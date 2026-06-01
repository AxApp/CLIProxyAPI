package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenscodex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestCodexModelRoutingResponsesHTTPDownstreamUpstreamSmoke(t *testing.T) {
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
		Name:  "codex-model-http-smoke",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model == "smoke-model-route" && req.CodexRequest != nil {
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
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "smoke-model-route"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"smoke-model-route","input":[{"type":"message","id":"msg-in"}]}`))
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
	if got := gjson.GetBytes(body, "model").String(); got != "smoke-model-route" {
		t.Fatalf("upstream body model = %q, want smoke-model-route; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "input.0.id").String(); got != "msg-in" {
		t.Fatalf("upstream body input id = %q, want msg-in; body=%s", got, body)
	}

	select {
	case routeCtx := <-capturedRouteContext:
		if routeCtx.RequestKind != gettokenscodex.RequestKindMain || routeCtx.RequestedModel != "smoke-model-route" {
			t.Fatalf("route codex context = %#v, want main context with requested model", routeCtx)
		}
		if routeCtx.SessionID != "session-smoke" || routeCtx.ThreadID != "thread-smoke" || routeCtx.TurnID != "turn-smoke" {
			t.Fatalf("route codex context ids = %#v, want session/thread/turn smoke ids", routeCtx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for route context")
	}
}

func TestCodexDeepSeekOpenAICompatibleResponsesHTTPDownstreamChatUpstreamSmoke(t *testing.T) {
	gin.SetMode(gin.TestMode)

	capturedHeaders := make(chan http.Header, 1)
	capturedBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %s, want /v1/chat/completions", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		capturedHeaders <- r.Header.Clone()
		capturedBody <- bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-deepseek-smoke","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor("openai-compatibility", &config.Config{}))
	auth := &coreauth.Auth{
		ID:       "auth-smoke-deepseek-openai-compatible",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"compat_name": "deepseek",
			"api_key":     "sk-smoke-deepseek",
			"base_url":    upstream.URL + "/v1",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{
		ID:       "deepseek-v4-flash",
		Type:     "openai-compatibility",
		OwnedBy:  "deepseek",
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}},
	}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-flash","input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"xhigh"},"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	setSmokeCodexHeaders(req.Header, "review")
	req.Header.Set("Authorization", "Bearer inbound-should-not-forward")
	resp := httptest.NewRecorder()

	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	if got := gjson.GetBytes(resp.Body.Bytes(), "object").String(); got != "response" {
		t.Fatalf("downstream object = %q, want response; body=%s", got, resp.Body.String())
	}

	headers := waitSmokeHeaders(t, capturedHeaders)
	body := waitSmokeBody(t, capturedBody)
	if got := headers.Get("Authorization"); got != "Bearer sk-smoke-deepseek" {
		t.Fatalf("upstream Authorization = %q, want Bearer sk-smoke-deepseek", got)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "deepseek-v4-flash" {
		t.Fatalf("upstream body model = %q, want deepseek-v4-flash; body=%s", got, body)
	}
	if !gjson.GetBytes(body, "messages").Exists() || gjson.GetBytes(body, "input").Exists() {
		t.Fatalf("upstream body should be chat completions shape only; body=%s", body)
	}
	if got := gjson.GetBytes(body, "thinking.type").String(); got != "enabled" {
		t.Fatalf("upstream thinking.type = %q, want enabled; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "reasoning_effort").String(); got != "max" {
		t.Fatalf("upstream reasoning_effort = %q, want max; body=%s", got, body)
	}
}

func TestCodexDeepSeekOpenAICompatibleResponsesStreamDownstreamChatSSEUpstreamSmoke(t *testing.T) {
	gin.SetMode(gin.TestMode)

	capturedBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %s, want /v1/chat/completions", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		capturedBody <- bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-deepseek-stream-smoke","object":"chat.completion.chunk","created":1773896263,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-deepseek-stream-smoke","object":"chat.completion.chunk","created":1773896263,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor("openai-compatibility", &config.Config{}))
	auth := &coreauth.Auth{
		ID:       "auth-smoke-deepseek-openai-compatible-stream",
		Provider: "openai-compatibility",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"compat_name": "deepseek",
			"api_key":     "sk-smoke-deepseek-stream",
			"base_url":    upstream.URL + "/v1",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{
		ID:       "deepseek-v4-flash",
		Type:     "openai-compatibility",
		OwnedBy:  "deepseek",
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}},
	}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"deepseek-v4-flash","input":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	setSmokeCodexHeaders(req.Header, "review")
	resp := httptest.NewRecorder()

	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	output := resp.Body.String()
	if !strings.Contains(output, "response.output_text.delta") || !strings.Contains(output, "response.completed") {
		t.Fatalf("downstream SSE missing responses events: %s", output)
	}

	body := waitSmokeBody(t, capturedBody)
	if got := gjson.GetBytes(body, "model").String(); got != "deepseek-v4-flash" {
		t.Fatalf("upstream body model = %q, want deepseek-v4-flash; body=%s", got, body)
	}
	if !gjson.GetBytes(body, "messages").Exists() || gjson.GetBytes(body, "input").Exists() {
		t.Fatalf("upstream stream body should be chat completions shape only; body=%s", body)
	}
}

func TestCodexResponsesDevSmokeRequestWindowAdmissionFallsBackWithMockUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceRateLimit) })

	tempDir := t.TempDir()
	t.Setenv("GETTOKENS_USAGE_ATTRIBUTION_SQLITE_PATH", filepath.Join(tempDir, "usage-attribution.sqlite"))
	if err := gettokenshooks.InstallRateLimitHook(gettokenshooks.UsageAttributionOptions{WritableBase: tempDir}); err != nil {
		t.Fatalf("install rate-limit hook: %v", err)
	}

	const (
		accountA = "acct_00000000-0000-4000-8000-0000000000a1"
		accountB = "acct_00000000-0000-4000-8000-0000000000b2"
		model    = "smoke-rate-limit-model"
	)

	managementRouter := gin.New()
	gettokenshooks.ConfigureRateLimitRoutes(managementRouter.Group("/v0/management"), nil, nil)
	ruleBody := `{"id":"rlr-smoke-dev","account_key":"` + accountA + `","strategy":"request-window","window":"1h","limit_value":1,"action":"block","enabled":true}`
	ruleReq := httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/rate-limit-rules", strings.NewReader(ruleBody))
	ruleReq.Header.Set("Content-Type", "application/json")
	ruleResp := httptest.NewRecorder()
	managementRouter.ServeHTTP(ruleResp, ruleReq)
	if ruleResp.Code != http.StatusOK {
		t.Fatalf("create rate-limit rule status = %d, want 200; body=%s", ruleResp.Code, ruleResp.Body.String())
	}

	upstreamAStarted := make(chan struct{}, 1)
	releaseUpstreamA := make(chan struct{})
	upstreamABody := make(chan []byte, 1)
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream A path = %s, want /responses", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream A body: %v", err)
		}
		upstreamABody <- bytes.Clone(body)
		upstreamAStarted <- struct{}{}
		select {
		case <-releaseUpstreamA:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting to release upstream A")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp-smoke-a","output":[{"type":"message","id":"msg-a"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer upstreamA.Close()

	upstreamBBody := make(chan []byte, 1)
	upstreamBAuth := make(chan string, 1)
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream B path = %s, want /responses", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream B body: %v", err)
		}
		upstreamBAuth <- r.Header.Get("Authorization")
		upstreamBBody <- bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp-smoke-b","output":[{"type":"message","id":"msg-b"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer upstreamB.Close()

	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}))
	authA := &coreauth.Auth{
		ID:         "auth-smoke-rate-a",
		AccountKey: accountA,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-smoke-a", "base_url": upstreamA.URL},
	}
	authB := &coreauth.Auth{
		ID:         "auth-smoke-rate-b",
		AccountKey: accountB,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-smoke-b", "base_url": upstreamB.URL},
	}
	if _, err := manager.Register(context.Background(), authA); err != nil {
		t.Fatalf("register auth A: %v", err)
	}
	if _, err := manager.Register(context.Background(), authB); err != nil {
		t.Fatalf("register auth B: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authA.ID, authA.Provider, []*registry.ModelInfo{{ID: model}})
	registry.GetGlobalRegistry().RegisterClient(authB.ID, authB.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authA.ID)
		registry.GetGlobalRegistry().UnregisterClient(authB.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	downstream := httptest.NewServer(router)
	defer downstream.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	firstDone := make(chan string, 1)
	firstErr := make(chan error, 1)
	go func() {
		body, err := postSmokeResponsesRequest(client, downstream.URL, model, "msg-first")
		if err != nil {
			firstErr <- err
			return
		}
		firstDone <- body
	}()

	select {
	case <-upstreamAStarted:
	case err := <-firstErr:
		t.Fatalf("first downstream request failed before reaching upstream A: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first request to reach upstream A")
	}

	secondBody, err := postSmokeResponsesRequest(client, downstream.URL, model, "msg-second")
	if err != nil {
		t.Fatalf("second downstream request failed: %v", err)
	}
	if !strings.Contains(secondBody, "resp-smoke-b") {
		t.Fatalf("second downstream response = %s, want fallback upstream B", secondBody)
	}
	if got := waitSmokeString(t, upstreamBAuth); got != "Bearer sk-smoke-b" {
		t.Fatalf("upstream B Authorization = %q, want Bearer sk-smoke-b", got)
	}
	if got := gjson.GetBytes(waitSmokeBody(t, upstreamBBody), "input.0.id").String(); got != "msg-second" {
		t.Fatalf("upstream B body input id = %q, want msg-second", got)
	}
	if got := gjson.GetBytes(waitSmokeBody(t, upstreamABody), "input.0.id").String(); got != "msg-first" {
		t.Fatalf("upstream A body input id = %q, want msg-first", got)
	}

	close(releaseUpstreamA)
	select {
	case body := <-firstDone:
		if !strings.Contains(body, "resp-smoke-a") {
			t.Fatalf("first downstream response = %s, want upstream A response", body)
		}
	case err := <-firstErr:
		t.Fatalf("first downstream request failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first downstream response")
	}
}

func TestCodexModelRoutingResponsesWebSocketDownstreamUpstreamSmoke(t *testing.T) {
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
		Name:  "codex-model-ws-smoke",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model == "smoke-model-route-ws" && req.CodexRequest != nil {
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
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "smoke-model-route-ws"}})
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

	requestPayload := []byte(`{"type":"response.create","model":"smoke-model-route-ws","input":[{"type":"message","id":"msg-in-ws"}]}`)
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
	if got := gjson.GetBytes(body, "model").String(); got != "smoke-model-route-ws" {
		t.Fatalf("upstream websocket body model = %q, want smoke-model-route-ws; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "input.0.id").String(); got != "msg-in-ws" {
		t.Fatalf("upstream websocket body input id = %q, want msg-in-ws; body=%s", got, body)
	}

	select {
	case routeCtx := <-capturedRouteContext:
		if routeCtx.RequestKind != gettokenscodex.RequestKindMain || routeCtx.RequestedModel != "smoke-model-route-ws" {
			t.Fatalf("route codex context = %#v, want main context with requested model", routeCtx)
		}
		if routeCtx.SessionID != "session-smoke" || routeCtx.ThreadID != "thread-smoke" || routeCtx.TurnID != "turn-smoke" {
			t.Fatalf("route codex context ids = %#v, want session/thread/turn smoke ids", routeCtx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for route context")
	}
}

func setSmokeCodexHeaders(headers http.Header, passthroughLabel string) {
	headers.Set("Version", "0.133.0-smoke")
	headers.Set("X-Codex-Installation-Id", "install-smoke")
	headers.Set("X-Codex-Turn-State", "turn-state-smoke")
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"session-meta-smoke","thread_id":"thread-smoke","thread_source":"subagent","turn_id":"turn-smoke","sandbox":"workspace-write","turn_started_at_unix_ms":1843142400000}`)
	headers.Set("X-Client-Request-Id", "client-smoke")
	headers.Set("X-Codex-Parent-Thread-Id", "parent-smoke")
	headers.Set("X-Codex-Window-Id", "window-smoke")
	headers.Set("X-OpenAI-Subagent", passthroughLabel)
	headers.Set("X-OpenAI-Memgen-Request", "memgen-smoke")
	headers.Set("X-OAI-Attestation", "attestation-smoke")
	headers.Set("Session_id", "session-smoke")
	headers.Set("Session-Id", "session-dash-smoke")
	headers.Set("Thread-Id", "thread-smoke")
}

func assertSmokeUpstreamHeaders(t *testing.T, headers http.Header, passthroughLabel string, authorization string) {
	t.Helper()
	want := map[string]string{
		"Authorization":            authorization,
		"Version":                  "0.133.0-smoke",
		"X-Codex-Installation-Id":  "install-smoke",
		"X-Codex-Turn-State":       "turn-state-smoke",
		"X-Client-Request-Id":      "client-smoke",
		"X-Codex-Parent-Thread-Id": "parent-smoke",
		"X-Codex-Window-Id":        "window-smoke",
		"X-OpenAI-Subagent":        passthroughLabel,
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

func waitSmokeString(t *testing.T, values <-chan string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for smoke string")
	}
	return ""
}

func postSmokeResponsesRequest(client *http.Client, baseURL string, model string, inputID string) (string, error) {
	payload := `{"model":"` + model + `","input":[{"type":"message","id":"` + inputID + `"}]}`
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	setSmokeCodexHeaders(req.Header, "rate-limit")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return string(body), fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return string(body), nil
}
