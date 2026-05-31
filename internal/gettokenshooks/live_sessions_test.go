package gettokenshooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	var raw map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw snapshot: %v", err)
	}
	sessionsAny, ok := raw["sessions"].([]any)
	if !ok || len(sessionsAny) != 1 {
		t.Fatalf("raw sessions = %#v", raw["sessions"])
	}
	firstSession, ok := sessionsAny[0].(map[string]any)
	if !ok {
		t.Fatalf("raw session type = %#v", sessionsAny[0])
	}
	if _, exists := firstSession["requests"]; exists {
		t.Fatalf("snapshot row unexpectedly exposed requests: %#v", firstSession)
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
	if len(session.Requests) != 0 {
		t.Fatalf("requests = %d, want 0 for row feed", len(session.Requests))
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

func TestLiveSessionsActiveAuthCountsIncludeOnlyActiveRequests(t *testing.T) {
	resetLiveSessionTrackerForTest(t)

	activeCtx := internallogging.WithRequestID(context.Background(), "active-req-1")
	RecordCodexLiveRequestStarted(activeCtx, CodexLiveRequestStart{
		ExecutionSessionID:  "active-session-1",
		Model:               "gpt-5.5",
		AuthID:              "auth-active",
		Provider:            "codex",
		DownstreamTransport: "websocket",
		UpstreamTransport:   "websocket",
	})

	completedCtx := internallogging.WithRequestID(context.Background(), "completed-req-1")
	ObserveCodexLiveUsage(completedCtx, coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.5",
		AuthID:      "auth-completed",
		RequestedAt: time.Now().Add(-1 * time.Second),
		Latency:     time.Second,
		Detail:      coreusage.Detail{OutputTokens: 12, TotalTokens: 20},
	})

	counts := currentLiveSessionActiveAuthCounts()
	if counts["auth-active"] != 1 {
		t.Fatalf("active auth count = %d, want 1: %#v", counts["auth-active"], counts)
	}
	if counts["auth-completed"] != 0 {
		t.Fatalf("completed auth count = %d, want 0: %#v", counts["auth-completed"], counts)
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
	detail := currentLiveSessionDetailSnapshotForTest(t, "ws-session-1")
	usage := detail.Requests[0].Usage
	if usage == nil || usage.OutputTokens != 60 {
		t.Fatalf("usage not applied to websocket request: %#v", detail.Requests[0])
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
	if session.RequestCount != 2 {
		t.Fatalf("request count = %d, want 2: %#v", session.RequestCount, session)
	}
	detail := currentLiveSessionDetailSnapshotForTest(t, conversationID)
	if len(detail.Requests) != 2 {
		t.Fatalf("detail requests len=%d, want 2: %#v", len(detail.Requests), detail.Requests)
	}
	for _, request := range detail.Requests {
		if request.SessionID != conversationID {
			t.Fatalf("request %s sessionID = %q, want %q", request.RequestID, request.SessionID, conversationID)
		}
		if request.ClientRequestID != conversationID {
			t.Fatalf("request %s clientRequestID = %q, want %q", request.RequestID, request.ClientRequestID, conversationID)
		}
	}
}

func TestLiveSessionsSnapshotEnrichesProjectNameFromLocalCodexSession(t *testing.T) {
	resetLiveSessionTrackerForTest(t)
	codexHome := filepath.Join(t.TempDir(), ".codex")
	sessionsDir := filepath.Join(codexHome, "sessions", "2026", "05", "25")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		t.Fatalf("mkdir sessions dir: %v", err)
	}
	conversationID := "0198f708-8dbf-7c90-a20a-30f4ebf7244f"
	sessionPath := filepath.Join(sessionsDir, "rollout-2026-05-25T10-00-00-gettokens.jsonl")
	payload := "" +
		"{\"timestamp\":\"2026-05-25T10:00:00.000Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"" + conversationID + "\",\"cwd\":\"/Users/linhey/Desktop/linhay-open-sources/GetTokens\",\"git\":{\"repository_url\":\"git@github.com:linhay/GetTokens.git\"}}}\n" +
		"{\"timestamp\":\"2026-05-25T10:00:01.000Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"检查 live session 项目名\"}]}}\n"
	if err := os.WriteFile(sessionPath, []byte(payload), 0600); err != nil {
		t.Fatalf("write codex session: %v", err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	RecordDownstreamWebsocketConnected("passthrough-1", "127.0.0.1")
	RecordDownstreamWebsocketRequest("passthrough-1", "ws-req-1", "gpt-5.5", CodexLiveSessionIdentity{
		ConversationID:  conversationID,
		ClientRequestID: conversationID,
		CodexWindowID:   conversationID + ":0",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := CurrentLiveSessionsSnapshot()
		if len(snapshot.Sessions) != 1 {
			t.Fatalf("sessions = %d, want 1: %#v", len(snapshot.Sessions), snapshot.Sessions)
		}
		if got := snapshot.Sessions[0].ProjectName; got == "GetTokens" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("projectName was not eventually enriched")
}

func TestLiveSessionsSnapshotDoesNotBlockOnProjectLookupRefresh(t *testing.T) {
	resetLiveSessionTrackerForTest(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	startedLookup := make(chan struct{}, 1)
	releaseLookup := make(chan struct{})
	originalBuilder := buildLiveSessionProjectLookupFunc
	buildLiveSessionProjectLookupFunc = func(codexHome string) (map[string]string, error) {
		startedLookup <- struct{}{}
		<-releaseLookup
		return map[string]string{"conv-no-project": "GetTokens"}, nil
	}
	t.Cleanup(func() {
		buildLiveSessionProjectLookupFunc = originalBuilder
		close(releaseLookup)
	})

	RecordDownstreamWebsocketConnected("passthrough-1", "127.0.0.1")
	RecordDownstreamWebsocketRequest("passthrough-1", "ws-req-1", "gpt-5.5", CodexLiveSessionIdentity{
		ConversationID: "conv-no-project",
	})

	startedAt := time.Now()
	snapshot := CurrentLiveSessionsSnapshot()
	elapsed := time.Since(startedAt)
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("snapshot blocked on project lookup for %v", elapsed)
	}
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(snapshot.Sessions))
	}
	if got := strings.TrimSpace(snapshot.Sessions[0].ProjectName); got != "" {
		t.Fatalf("projectName = %q, want empty while refresh is async", got)
	}
	select {
	case <-startedLookup:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected background project lookup refresh to start")
	}
}

func TestLiveSessionsPrunesRequestsWithinLongSession(t *testing.T) {
	resetLiveSessionTrackerForTest(t)

	conversationID := "conv-long"
	RecordDownstreamWebsocketConnected("passthrough-1", "127.0.0.1")
	for i := 0; i < liveSessionMaxRequestsPerSession+5; i++ {
		RecordDownstreamWebsocketRequest("passthrough-1", "req-"+strconv.Itoa(i), "gpt-5.5", CodexLiveSessionIdentity{
			ConversationID: conversationID,
		})
	}

	snapshot := CurrentLiveSessionsSnapshot()
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(snapshot.Sessions))
	}
	requests := currentLiveSessionDetailSnapshotForTest(t, conversationID).Requests
	if len(requests) != liveSessionMaxRequestsPerSession {
		t.Fatalf("requests = %d, want %d", len(requests), liveSessionMaxRequestsPerSession)
	}
	if requests[0].RequestID != "req-5" {
		t.Fatalf("first retained request = %q, want req-5", requests[0].RequestID)
	}
	if requests[0].Sequence != 6 {
		t.Fatalf("first retained sequence = %d, want 6", requests[0].Sequence)
	}
	lastRequest := requests[len(requests)-1]
	if lastRequest.RequestID != "req-54" {
		t.Fatalf("last retained request = %q, want req-54", lastRequest.RequestID)
	}
	if lastRequest.Sequence != 55 {
		t.Fatalf("last retained sequence = %d, want 55", lastRequest.Sequence)
	}
	tracker := currentLiveSessionTracker()
	tracker.mu.RLock()
	_, oldRequestMapped := tracker.requestMap["req-0"]
	requestMapSize := len(tracker.requestMap)
	tracker.mu.RUnlock()
	if oldRequestMapped {
		t.Fatal("trimmed request req-0 is still present in requestMap")
	}
	if requestMapSize != liveSessionMaxRequestsPerSession {
		t.Fatalf("requestMap size = %d, want %d", requestMapSize, liveSessionMaxRequestsPerSession)
	}
}

func TestLiveSessionsHistoryPersistsTrimmedRequests(t *testing.T) {
	resetLiveSessionTrackerForTest(t)
	store := installLiveSessionHistoryStoreForTest(t)

	conversationID := "conv-history"
	RecordDownstreamWebsocketConnected("passthrough-1", "127.0.0.1")
	for i := 0; i < liveSessionMaxRequestsPerSession+5; i++ {
		RecordDownstreamWebsocketRequest("passthrough-1", "req-"+strconv.Itoa(i), "gpt-5.5", CodexLiveSessionIdentity{
			ConversationID: conversationID,
		})
	}

	snapshot := CurrentLiveSessionsSnapshot()
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].RequestCount != liveSessionMaxRequestsPerSession {
		t.Fatalf("live snapshot did not trim to recent requests: %#v", snapshot.Sessions)
	}
	history, err := store.history(-1, 100, 0, conversationID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history.Items) != liveSessionMaxRequestsPerSession+5 {
		t.Fatalf("history items = %d, want %d", len(history.Items), liveSessionMaxRequestsPerSession+5)
	}
	foundOldest := false
	foundLatest := false
	for _, request := range history.Items {
		if request.RequestID == "req-0" && request.SessionID == conversationID {
			if request.Sequence != 1 {
				t.Fatalf("history req-0 sequence = %d, want 1", request.Sequence)
			}
			foundOldest = true
		}
		if request.RequestID == "req-54" && request.SessionID == conversationID {
			if request.Sequence != 55 {
				t.Fatalf("history req-54 sequence = %d, want 55", request.Sequence)
			}
			foundLatest = true
		}
	}
	if !foundOldest {
		t.Fatalf("trimmed request req-0 not found in persisted history: %#v", history.Items)
	}
	if !foundLatest {
		t.Fatalf("latest request req-54 not found in persisted history: %#v", history.Items)
	}
}

func TestLiveSessionsTimingSummaryAveragesRetainedRequests(t *testing.T) {
	now := time.Date(2026, 5, 27, 15, 30, 0, 0, time.UTC)
	requests := []LiveRequest{
		{
			RequestID:   "req-a",
			SessionID:   "session-1",
			Sequence:    4,
			Status:      "completed",
			StartedAt:   formatLiveTime(now.Add(-10 * time.Second)),
			CompletedAt: formatLiveTime(now.Add(-8 * time.Second)),
			Timing: LiveTimingMetrics{
				TotalDurationMs:       2000,
				FirstEventMs:          400,
				FirstTokenMs:          650,
				StreamDurationMs:      1350,
				QueueWaitMs:           20,
				AuthSelectMs:          40,
				UpstreamConnectMs:     100,
				AverageEventGapMs:     80,
				LongestEventGapMs:     300,
				ReconnectCount:        0,
				OutputTokensPerSecond: 20,
				TotalTokensPerSecond:  200,
			},
		},
		{
			RequestID:   "req-b",
			SessionID:   "session-1",
			Sequence:    5,
			Status:      "completed",
			StartedAt:   formatLiveTime(now.Add(-8 * time.Second)),
			CompletedAt: formatLiveTime(now.Add(-5 * time.Second)),
			Timing: LiveTimingMetrics{
				TotalDurationMs:       3000,
				FirstEventMs:          500,
				FirstTokenMs:          850,
				StreamDurationMs:      2150,
				QueueWaitMs:           40,
				AuthSelectMs:          80,
				UpstreamConnectMs:     200,
				AverageEventGapMs:     120,
				LongestEventGapMs:     500,
				ReconnectCount:        2,
				OutputTokensPerSecond: 40,
				TotalTokensPerSecond:  400,
			},
		},
	}

	summary := buildLiveTimingSummary(requests, "", now)
	if summary == nil {
		t.Fatal("summary is nil")
	}
	if summary.Window != "retained_requests" || summary.SampleCount != 2 || summary.SequenceFrom != 4 || summary.SequenceTo != 5 {
		t.Fatalf("unexpected summary window/count/range: %#v", summary)
	}
	if summary.ActiveIncluded {
		t.Fatalf("activeIncluded = true, want false")
	}
	if got := liveTimingSummaryInt64Value(t, summary.Averages.TotalDurationMs); got != 2500 {
		t.Fatalf("avg total = %d, want 2500", got)
	}
	if got := liveTimingSummaryInt64Value(t, summary.Averages.FirstEventMs); got != 450 {
		t.Fatalf("avg first event = %d, want 450", got)
	}
	if got := liveTimingSummaryInt64Value(t, summary.Averages.FirstTokenMs); got != 750 {
		t.Fatalf("avg first token = %d, want 750", got)
	}
	if got := liveTimingSummaryInt64Value(t, summary.Averages.QueueWaitMs); got != 30 {
		t.Fatalf("avg queue = %d, want 30", got)
	}
	if got := liveTimingSummaryIntValue(t, summary.Averages.ReconnectCount); got != 1 {
		t.Fatalf("avg reconnect = %d, want 1", got)
	}
	if got := liveTimingSummaryFloat64Value(t, summary.Averages.OutputTokensPerSecond); got != 30 {
		t.Fatalf("avg output rate = %v, want 30", got)
	}
}

func TestLiveSessionsTimingSummaryProjectsOnlyActiveRequest(t *testing.T) {
	now := time.Date(2026, 5, 27, 15, 30, 0, 0, time.UTC)
	requests := []LiveRequest{
		{
			RequestID: "req-stale",
			SessionID: "session-1",
			Sequence:  1,
			Status:    "streaming",
			StartedAt: formatLiveTime(now.Add(-30 * time.Second)),
			Timing: LiveTimingMetrics{
				TotalDurationMs: 1800,
				FirstEventMs:    400,
			},
		},
		{
			RequestID: "req-live",
			SessionID: "session-1",
			Sequence:  2,
			Status:    "streaming",
			StartedAt: formatLiveTime(now.Add(-7 * time.Second)),
			Timing: LiveTimingMetrics{
				FirstEventMs: 420,
			},
		},
	}

	summary := buildLiveTimingSummary(requests, "req-live", now)
	if summary == nil {
		t.Fatal("summary is nil")
	}
	if !summary.ActiveIncluded {
		t.Fatalf("activeIncluded = false, want true")
	}
	if got := liveTimingSummaryInt64Value(t, summary.Averages.TotalDurationMs); got != 4400 {
		t.Fatalf("avg total = %d, want 4400", got)
	}
	if got := liveTimingSummaryInt64Value(t, summary.Averages.FirstEventMs); got != 410 {
		t.Fatalf("avg first event = %d, want 410", got)
	}
	if summary.Averages.FirstTokenMs != nil {
		t.Fatalf("first token was invented: %#v", *summary.Averages.FirstTokenMs)
	}

	later := buildLiveTimingSummary(requests, "req-live", now.Add(3*time.Second))
	if later == nil {
		t.Fatal("later summary is nil")
	}
	if got := liveTimingSummaryInt64Value(t, later.Averages.TotalDurationMs); got != 5900 {
		t.Fatalf("later avg total = %d, want 5900", got)
	}
}

func TestLiveSessionsTimingSummaryIgnoresMissingTimingValues(t *testing.T) {
	now := time.Date(2026, 5, 27, 15, 30, 0, 0, time.UTC)
	requests := []LiveRequest{
		{
			RequestID: "req-empty",
			SessionID: "session-1",
			Sequence:  10,
			Status:    "completed",
			StartedAt: formatLiveTime(now.Add(-10 * time.Second)),
		},
		{
			RequestID: "req-timed",
			SessionID: "session-1",
			Sequence:  11,
			Status:    "completed",
			StartedAt: formatLiveTime(now.Add(-5 * time.Second)),
			Timing: LiveTimingMetrics{
				FirstTokenMs: 900,
			},
		},
	}

	summary := buildLiveTimingSummary(requests, "", now)
	if summary == nil {
		t.Fatal("summary is nil")
	}
	if summary.SampleCount != 2 || summary.SequenceFrom != 10 || summary.SequenceTo != 11 {
		t.Fatalf("unexpected summary count/range: %#v", summary)
	}
	if summary.Averages.TotalDurationMs != nil {
		t.Fatalf("total duration should be absent: %#v", *summary.Averages.TotalDurationMs)
	}
	if got := liveTimingSummaryInt64Value(t, summary.Averages.FirstTokenMs); got != 900 {
		t.Fatalf("first token avg = %d, want 900", got)
	}
}

func TestLiveSessionsDeleteRouteClearsMemoryOnly(t *testing.T) {
	resetLiveSessionTrackerForTest(t)
	installLiveSessionHistoryStoreForTest(t)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureLiveSessionRoutes(group, nil, nil)

	RecordDownstreamWebsocketConnected("session-1", "127.0.0.1")
	RecordDownstreamWebsocketRequest("session-1", "req-1", "gpt-5.5")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v0/management/gettokens/live-sessions", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if snapshot := CurrentLiveSessionsSnapshot(); len(snapshot.Sessions) != 0 {
		t.Fatalf("sessions after clear = %d, want 0", len(snapshot.Sessions))
	}

	historyRecorder := httptest.NewRecorder()
	historyRequest := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/live-sessions/history?window=all&session_id=session-1", nil)
	router.ServeHTTP(historyRecorder, historyRequest)
	if historyRecorder.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s", historyRecorder.Code, historyRecorder.Body.String())
	}
	var history LiveSessionHistoryResponse
	if err := json.Unmarshal(historyRecorder.Body.Bytes(), &history); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	if len(history.Items) != 1 || history.Items[0].RequestID != "req-1" {
		t.Fatalf("history after clear = %#v, want req-1", history.Items)
	}
}

func TestLiveSessionsDeleteRouteClearsTracker(t *testing.T) {
	resetLiveSessionTrackerForTest(t)
	RecordDownstreamWebsocketConnected("session-1", "127.0.0.1")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureLiveSessionRoutes(group, nil, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v0/management/gettokens/live-sessions", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if snapshot := CurrentLiveSessionsSnapshot(); len(snapshot.Sessions) != 0 {
		t.Fatalf("sessions after clear = %d, want 0", len(snapshot.Sessions))
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

func TestExtractCodexLiveSessionIdentityDerivesProjectNameFromTurnMetadataWorkspaces(t *testing.T) {
	headers := http.Header{
		"X-Codex-Turn-Metadata": []string{`{"session_id":"session-1","thread_id":"thread-1","workspaces":{"/Users/linhey/Desktop/FlowUp-Libs/Overloaded-v2":{"has_changes":false}}}`},
	}

	identity := ExtractCodexLiveSessionIdentity(headers, nil)
	if identity.ProjectName != "Overloaded-v2" {
		t.Fatalf("projectName = %q, want Overloaded-v2", identity.ProjectName)
	}
	if identity.ConversationID != "session-1" {
		t.Fatalf("conversationID = %q, want session-1", identity.ConversationID)
	}
}

func TestLiveSessionsSnapshotUsesProjectNameFromTurnMetadata(t *testing.T) {
	resetLiveSessionTrackerForTest(t)
	headers := http.Header{
		"X-Codex-Turn-Metadata": []string{`{"session_id":"session-live","thread_id":"thread-live","workspaces":{"/Users/linhey/Desktop/FlowUp-Libs/Overloaded-v2":{"has_changes":false}}}`},
	}
	identity := ExtractCodexLiveSessionIdentity(headers, nil)

	RecordDownstreamWebsocketConnected("passthrough-1", "127.0.0.1")
	RecordDownstreamWebsocketRequest("passthrough-1", "ws-req-1", "gpt-5.5", identity)

	snapshot := CurrentLiveSessionsSnapshot()
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1: %#v", len(snapshot.Sessions), snapshot.Sessions)
	}
	if got := snapshot.Sessions[0].ProjectName; got != "Overloaded-v2" {
		t.Fatalf("projectName = %q, want Overloaded-v2", got)
	}
}

func resetLiveSessionTrackerForTest(t *testing.T) {
	t.Helper()
	liveSessionsMu.Lock()
	defaultLiveSessions = newLiveSessionTracker()
	liveSessionsMu.Unlock()
	liveSessionHistoryMu.Lock()
	defaultLiveSessionHistory = nil
	liveSessionHistoryMu.Unlock()
	t.Cleanup(func() {
		liveSessionsMu.Lock()
		defaultLiveSessions = newLiveSessionTracker()
		liveSessionsMu.Unlock()
		liveSessionHistoryMu.Lock()
		defaultLiveSessionHistory = nil
		liveSessionHistoryMu.Unlock()
	})
}

func installLiveSessionHistoryStoreForTest(t *testing.T) *liveSessionHistoryStore {
	t.Helper()
	store, err := newLiveSessionHistoryStore(filepath.Join(t.TempDir(), "live-sessions-v1.sqlite"))
	if err != nil {
		t.Fatalf("new live session history store: %v", err)
	}
	liveSessionHistoryMu.Lock()
	defaultLiveSessionHistory = store
	liveSessionHistoryMu.Unlock()
	return store
}

func currentLiveSessionDetailSnapshotForTest(t *testing.T, sessionID string) LiveSession {
	t.Helper()
	tracker := currentLiveSessionTracker()
	tracker.mu.RLock()
	state := tracker.sessions[sessionID]
	if state == nil {
		tracker.mu.RUnlock()
		t.Fatalf("session %q not found in tracker", sessionID)
	}
	detail := state.clone(time.Now())
	tracker.mu.RUnlock()
	return detail
}

func liveTimingSummaryInt64Value(t *testing.T, value *int64) int64 {
	t.Helper()
	if value == nil {
		t.Fatal("expected int64 timing summary value")
	}
	return *value
}

func liveTimingSummaryIntValue(t *testing.T, value *int) int {
	t.Helper()
	if value == nil {
		t.Fatal("expected int timing summary value")
	}
	return *value
}

func liveTimingSummaryFloat64Value(t *testing.T, value *float64) float64 {
	t.Helper()
	if value == nil {
		t.Fatal("expected float64 timing summary value")
	}
	return *value
}
