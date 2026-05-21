package gettokenshooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestLiveSessionsRouteReturnsWebsocketRequestSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetLiveSessionTrackerForTest(t)

	RecordDownstreamWebsocketConnected("ws-session-1", "127.0.0.1")
	RecordDownstreamWebsocketRequest("ws-session-1", "ws-req-1", "gpt-5.5")
	requestCtx := internallogging.WithRequestID(context.Background(), "ws-req-1")
	RecordCodexLiveRequestStarted(requestCtx, CodexLiveRequestStart{
		ExecutionSessionID:  "ws-session-1",
		Model:               "gpt-5.5",
		AuthID:              "auth-file:team-codex",
		AuthLabel:           "team-codex@example.com",
		Provider:            "codex",
		DownstreamTransport: "websocket",
		UpstreamTransport:   "websocket",
	})
	RecordCodexLiveFirstEvent("ws-req-1")
	RecordCodexLiveRequestCompleted("ws-req-1", coreusage.Detail{OutputTokens: 120, TotalTokens: 200}, nil)

	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureLiveSessionRoutes(group, nil, nil)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/live-sessions", nil)
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var snapshot LiveSessionsSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if snapshot.Source != "live" || !snapshot.SidecarReady {
		t.Fatalf("snapshot source/ready mismatch: %#v", snapshot)
	}
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1: %#v", len(snapshot.Sessions), snapshot.Sessions)
	}
	session := snapshot.Sessions[0]
	if session.SessionID != "ws-session-1" || session.DownstreamTransport != "websocket" || session.UpstreamTransport != "websocket" {
		t.Fatalf("unexpected session transport: %#v", session)
	}
	if len(session.Requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(session.Requests))
	}
	if rate := session.Requests[0].Timing.OutputTokensPerSecond; rate <= 0 {
		t.Fatalf("expected output token rate, got %#v", session.Requests[0].Timing)
	}
}

func TestLiveSessionsObserveUsageRecordCreatesHTTPCompletedSession(t *testing.T) {
	resetLiveSessionTrackerForTest(t)

	ctx := internallogging.WithRequestID(context.Background(), "http-req-1")
	ctx = internallogging.WithEndpoint(ctx, "POST /v1/responses")
	ObserveCodexLiveUsage(ctx, coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.4",
		Alias:       "gpt-5.4",
		AuthID:      "codex:apikey:local-1",
		AuthType:    "api-key",
		RequestedAt: time.Now().Add(-2 * time.Second),
		Latency:     2 * time.Second,
		Detail:      coreusage.Detail{OutputTokens: 80, TotalTokens: 120},
	})

	snapshot := CurrentLiveSessionsSnapshot()
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(snapshot.Sessions))
	}
	session := snapshot.Sessions[0]
	if session.SessionID != "http-req-1" || session.DownstreamTransport != "http" || session.UpstreamTransport != "http" {
		t.Fatalf("unexpected http session: %#v", session)
	}
	if session.Status != "completed" {
		t.Fatalf("status = %q, want completed", session.Status)
	}
}

func TestLiveSessionsObserveUsageRecordUpdatesExistingWebsocketRequest(t *testing.T) {
	resetLiveSessionTrackerForTest(t)

	RecordDownstreamWebsocketConnected("ws-session-1", "127.0.0.1")
	RecordDownstreamWebsocketRequest("ws-session-1", "ws-req-1", "gpt-5.5")
	ctx := internallogging.WithRequestID(context.Background(), "ws-req-1")

	ObserveCodexLiveUsage(ctx, coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.5",
		Alias:       "gpt-5.5",
		AuthID:      "auth-file:team-codex",
		RequestedAt: time.Now().Add(-1 * time.Second),
		Latency:     time.Second,
		Detail:      coreusage.Detail{OutputTokens: 60, TotalTokens: 100},
	})

	snapshot := CurrentLiveSessionsSnapshot()
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(snapshot.Sessions))
	}
	if snapshot.Sessions[0].DownstreamTransport != "websocket" {
		t.Fatalf("downstream transport changed: %#v", snapshot.Sessions[0])
	}
	usage := snapshot.Sessions[0].Requests[0].Usage
	if usage == nil || usage.OutputTokens != 60 {
		t.Fatalf("usage not applied to websocket request: %#v", snapshot.Sessions[0].Requests[0])
	}
}

func TestLiveSessionsCoalescesRequestsByCodexConversationID(t *testing.T) {
	resetLiveSessionTrackerForTest(t)

	conversationID := "0198f708-8dbf-7c90-a20a-30f4ebf7244f"
	RecordDownstreamWebsocketConnected("passthrough-1", "127.0.0.1")
	RecordDownstreamWebsocketRequest("passthrough-1", "ws-req-1", "gpt-5.5", CodexLiveSessionIdentity{
		ConversationID:  conversationID,
		ClientRequestID: conversationID,
		CodexWindowID:   conversationID + ":0",
	})
	RecordDownstreamWebsocketConnected("passthrough-2", "127.0.0.1")
	RecordDownstreamWebsocketRequest("passthrough-2", "ws-req-2", "gpt-5.5", CodexLiveSessionIdentity{
		ConversationID:  conversationID,
		ClientRequestID: conversationID,
		CodexWindowID:   conversationID + ":0",
	})

	snapshot := CurrentLiveSessionsSnapshot()
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1: %#v", len(snapshot.Sessions), snapshot.Sessions)
	}
	session := snapshot.Sessions[0]
	if session.SessionID != conversationID {
		t.Fatalf("sessionID = %q, want conversation id %q", session.SessionID, conversationID)
	}
	if session.DownstreamSessionID == "" {
		t.Fatalf("expected passthrough downstream session id to be retained: %#v", session)
	}
	if session.CodexWindowID != conversationID+":0" {
		t.Fatalf("codexWindowID = %q", session.CodexWindowID)
	}
	if session.RequestCount != 2 || len(session.Requests) != 2 {
		t.Fatalf("request count = %d len=%d, want 2: %#v", session.RequestCount, len(session.Requests), session.Requests)
	}
	for _, request := range session.Requests {
		if request.SessionID != conversationID {
			t.Fatalf("request %s sessionID = %q, want %q", request.RequestID, request.SessionID, conversationID)
		}
		if request.ClientRequestID != conversationID {
			t.Fatalf("request %s clientRequestID = %q, want %q", request.RequestID, request.ClientRequestID, conversationID)
		}
	}
}

func TestExtractCodexLiveSessionIdentityPrefersCodexConversationFields(t *testing.T) {
	conversationID := "0198f708-8dbf-7c90-a20a-30f4ebf7244f"
	headers := http.Header{
		"X-Client-Request-Id": []string{"fallback-client-request"},
		"X-Codex-Window-Id":   []string{conversationID + ":1"},
	}
	payload := []byte(`{
		"prompt_cache_key": "` + conversationID + `",
		"client_metadata": {
			"x-codex-window-id": "` + conversationID + `:2"
		}
	}`)

	identity := ExtractCodexLiveSessionIdentity(headers, payload)
	if identity.ConversationID != conversationID {
		t.Fatalf("conversationID = %q, want %q", identity.ConversationID, conversationID)
	}
	if identity.PromptCacheKey != conversationID {
		t.Fatalf("promptCacheKey = %q, want %q", identity.PromptCacheKey, conversationID)
	}
	if identity.CodexWindowID != conversationID+":1" {
		t.Fatalf("codexWindowID = %q, want header value", identity.CodexWindowID)
	}
	if identity.ClientRequestID != "fallback-client-request" {
		t.Fatalf("clientRequestID = %q", identity.ClientRequestID)
	}
}

func resetLiveSessionTrackerForTest(t *testing.T) {
	t.Helper()
	liveSessionsMu.Lock()
	defaultLiveSessions = newLiveSessionTracker()
	liveSessionsMu.Unlock()
	t.Cleanup(func() {
		liveSessionsMu.Lock()
		defaultLiveSessions = newLiveSessionTracker()
		liveSessionsMu.Unlock()
	})
}
