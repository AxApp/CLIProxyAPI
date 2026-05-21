package gettokenshooks

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	liveSessionsRetention      = 30 * time.Minute
	liveSessionsRetentionLabel = "30m / 200"
	liveSessionsMaxItems       = 200
)

type LiveSessionsSnapshot struct {
	GeneratedAt  string             `json:"generatedAt"`
	SidecarReady bool               `json:"sidecarReady"`
	Source       string             `json:"source"`
	Retention    string             `json:"retentionLabel"`
	Summary      LiveSessionSummary `json:"summary"`
	Sessions     []LiveSession      `json:"sessions"`
}

type LiveSessionSummary struct {
	ActiveSessions    int `json:"activeSessions"`
	ActiveRequests    int `json:"activeRequests"`
	WebsocketSessions int `json:"websocketSessions"`
	HTTPSessions      int `json:"httpSessions"`
	DegradedSessions  int `json:"degradedSessions"`
	ErrorSessions     int `json:"errorSessions"`
}

type LiveSession struct {
	SessionID           string              `json:"sessionID"`
	ExecutionSessionID  string              `json:"executionSessionID,omitempty"`
	DownstreamSessionID string              `json:"downstreamSessionID,omitempty"`
	CodexWindowID       string              `json:"codexWindowID,omitempty"`
	Status              string              `json:"status"`
	StartedAt           string              `json:"startedAt"`
	LastEventAt         string              `json:"lastEventAt"`
	DurationMs          int64               `json:"durationMs"`
	RequestCount        int                 `json:"requestCount"`
	ActiveRequestID     string              `json:"activeRequestID,omitempty"`
	LastRequestID       string              `json:"lastRequestID,omitempty"`
	Model               string              `json:"model"`
	AuthID              string              `json:"authID,omitempty"`
	AuthLabel           string              `json:"authLabel,omitempty"`
	Provider            string              `json:"provider,omitempty"`
	DownstreamTransport string              `json:"downstreamTransport"`
	UpstreamTransport   string              `json:"upstreamTransport"`
	FallbackInferred    bool                `json:"fallbackInferred,omitempty"`
	FallbackConfidence  string              `json:"fallbackConfidence,omitempty"`
	FallbackReason      string              `json:"fallbackReason,omitempty"`
	RecentEvents        []LiveTimelineEvent `json:"recentEvents"`
	Requests            []LiveRequest       `json:"requests"`
}

type LiveRequest struct {
	RequestID           string              `json:"requestID"`
	ClientRequestID     string              `json:"clientRequestID,omitempty"`
	UpstreamRequestID   string              `json:"upstreamRequestID,omitempty"`
	SessionID           string              `json:"sessionID"`
	Sequence            int                 `json:"sequence"`
	Model               string              `json:"model"`
	Status              string              `json:"status"`
	StartedAt           string              `json:"startedAt"`
	CompletedAt         string              `json:"completedAt,omitempty"`
	DownstreamTransport string              `json:"downstreamTransport"`
	UpstreamTransport   string              `json:"upstreamTransport"`
	ConnectionReused    bool                `json:"connectionReused,omitempty"`
	AuthID              string              `json:"authID,omitempty"`
	AuthLabel           string              `json:"authLabel,omitempty"`
	Provider            string              `json:"provider,omitempty"`
	ProxyRoute          string              `json:"proxyRoute,omitempty"`
	Usage               *LiveTokenUsage     `json:"usage,omitempty"`
	Timing              LiveTimingMetrics   `json:"timing,omitempty"`
	Error               *LiveErrorSummary   `json:"error,omitempty"`
	Timeline            []LiveTimelineEvent `json:"timeline"`
}

type LiveTokenUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

type LiveTimingMetrics struct {
	QueueWaitMs           int64   `json:"queueWaitMs,omitempty"`
	AuthSelectMs          int64   `json:"authSelectMs,omitempty"`
	UpstreamConnectMs     int64   `json:"upstreamConnectMs,omitempty"`
	FirstEventMs          int64   `json:"firstEventMs,omitempty"`
	FirstTokenMs          int64   `json:"firstTokenMs,omitempty"`
	AverageEventGapMs     int64   `json:"averageEventGapMs,omitempty"`
	LongestEventGapMs     int64   `json:"longestEventGapMs,omitempty"`
	StreamDurationMs      int64   `json:"streamDurationMs,omitempty"`
	TotalDurationMs       int64   `json:"totalDurationMs,omitempty"`
	ReconnectCount        int     `json:"reconnectCount,omitempty"`
	OutputTokensPerSecond float64 `json:"outputTokensPerSecond,omitempty"`
	TotalTokensPerSecond  float64 `json:"totalTokensPerSecond,omitempty"`
}

type LiveErrorSummary struct {
	StatusCode int    `json:"statusCode,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
}

type LiveTimelineEvent struct {
	ID       string `json:"id"`
	At       string `json:"at"`
	Lane     string `json:"lane"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Severity string `json:"severity"`
	Detail   string `json:"detail,omitempty"`
}

type CodexLiveRequestStart struct {
	ExecutionSessionID  string
	Model               string
	AuthID              string
	AuthLabel           string
	Provider            string
	DownstreamTransport string
	UpstreamTransport   string
}

type liveSessionTracker struct {
	mu         sync.RWMutex
	sessions   map[string]*liveSessionState
	requestMap map[string]string
	seq        int64
}

type liveSessionState struct {
	session  LiveSession
	requests map[string]*liveRequestState
}

type liveRequestState struct {
	request           LiveRequest
	firstEventAt      time.Time
	lastEventAt       time.Time
	eventGapTotalMs   int64
	eventGapCount     int64
	longestEventGap   int64
	upstreamStarted   time.Time
	upstreamConnected time.Time
}

var (
	liveSessionsMu      sync.RWMutex
	defaultLiveSessions = newLiveSessionTracker()
)

func newLiveSessionTracker() *liveSessionTracker {
	return &liveSessionTracker{
		sessions:   map[string]*liveSessionState{},
		requestMap: map[string]string{},
	}
}

func CurrentLiveSessionsSnapshot() LiveSessionsSnapshot {
	tracker := currentLiveSessionTracker()
	return tracker.snapshot(time.Now())
}

func RecordDownstreamWebsocketConnected(sessionID string, clientIP string) {
	currentLiveSessionTracker().recordDownstreamWebsocketConnected(sessionID, clientIP, time.Now())
}

func RecordDownstreamWebsocketRequest(sessionID string, requestID string, model string) {
	currentLiveSessionTracker().recordDownstreamWebsocketRequest(sessionID, requestID, model, time.Now())
}

func RecordDownstreamWebsocketDisconnected(sessionID string, err error) {
	currentLiveSessionTracker().recordDownstreamWebsocketDisconnected(sessionID, err, time.Now())
}

func RecordCodexLiveRequestStarted(ctx context.Context, input CodexLiveRequestStart) {
	currentLiveSessionTracker().recordCodexRequestStarted(ctx, input, time.Now())
}

func RecordCodexLiveUpstreamConnected(requestID string) {
	currentLiveSessionTracker().recordUpstreamConnected(requestID, time.Now())
}

func RecordCodexLiveFirstEvent(requestID string) {
	currentLiveSessionTracker().recordFirstEvent(requestID, time.Now())
}

func RecordCodexLiveUpstreamDisconnected(sessionID string, err error) {
	currentLiveSessionTracker().recordUpstreamDisconnected(sessionID, err, time.Now())
}

func RecordCodexLiveRequestCompleted(requestID string, detail coreusage.Detail, err error) {
	currentLiveSessionTracker().recordRequestCompleted(requestID, detail, err, time.Now())
}

func ObserveCodexLiveUsage(ctx context.Context, record coreusage.Record) {
	if !strings.EqualFold(strings.TrimSpace(record.Provider), "codex") {
		return
	}
	currentLiveSessionTracker().observeUsage(ctx, record, time.Now())
}

func ConfigureLiveSessionRoutes(group *gin.RouterGroup, _ *handlers.BaseAPIHandler, _ *config.Config) {
	if group == nil {
		return
	}
	group.GET("/gettokens/live-sessions", func(c *gin.Context) {
		c.JSON(http.StatusOK, CurrentLiveSessionsSnapshot())
	})
}

func currentLiveSessionTracker() *liveSessionTracker {
	liveSessionsMu.RLock()
	tracker := defaultLiveSessions
	liveSessionsMu.RUnlock()
	if tracker != nil {
		return tracker
	}
	liveSessionsMu.Lock()
	defer liveSessionsMu.Unlock()
	if defaultLiveSessions == nil {
		defaultLiveSessions = newLiveSessionTracker()
	}
	return defaultLiveSessions
}

func (t *liveSessionTracker) recordDownstreamWebsocketConnected(sessionID string, clientIP string, now time.Time) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.ensureSessionLocked(sessionID, now)
	session.session.DownstreamSessionID = sessionID
	session.session.DownstreamTransport = "websocket"
	session.session.UpstreamTransport = firstNonEmptyString(session.session.UpstreamTransport, "unknown")
	session.session.Status = "active"
	session.addEvent(t.nextEventIDLocked(), now, "downstream", "connected", "Codex WebSocket connected", "success", clientIP)
	t.pruneLocked(now)
}

func (t *liveSessionTracker) recordDownstreamWebsocketRequest(sessionID string, requestID string, model string, now time.Time) {
	sessionID = strings.TrimSpace(sessionID)
	requestID = strings.TrimSpace(requestID)
	if sessionID == "" || requestID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.ensureSessionLocked(sessionID, now)
	session.session.Model = firstNonEmptyString(strings.TrimSpace(model), session.session.Model)
	session.session.ActiveRequestID = requestID
	session.session.LastRequestID = requestID
	session.session.Status = "streaming"
	session.addEvent(t.nextEventIDLocked(), now, "downstream", "request", "Codex WebSocket request received", "info", requestID)
	t.requestMap[requestID] = sessionID
	t.ensureRequestLocked(session, requestID, model, now, "websocket", "unknown")
	t.pruneLocked(now)
}

func (t *liveSessionTracker) recordDownstreamWebsocketDisconnected(sessionID string, err error, now time.Time) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.ensureSessionLocked(sessionID, now)
	if session.session.Status == "active" || session.session.Status == "streaming" || session.session.Status == "reconnecting" {
		session.session.Status = "completed"
	}
	severity := "info"
	label := "Codex WebSocket disconnected"
	if err != nil {
		severity = "warning"
		label = sanitizeLiveText(err.Error())
	}
	session.addEvent(t.nextEventIDLocked(), now, "downstream", "disconnected", label, severity, "")
}

func (t *liveSessionTracker) recordCodexRequestStarted(ctx context.Context, input CodexLiveRequestStart, now time.Time) {
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	if requestID == "" {
		requestID = t.generatedRequestID(now)
	}
	sessionID := firstNonEmptyString(strings.TrimSpace(input.ExecutionSessionID), requestID)
	downstream := firstNonEmptyString(strings.TrimSpace(input.DownstreamTransport), "http")
	upstream := firstNonEmptyString(strings.TrimSpace(input.UpstreamTransport), "unknown")

	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.ensureSessionLocked(sessionID, now)
	session.session.ExecutionSessionID = firstNonEmptyString(strings.TrimSpace(input.ExecutionSessionID), session.session.ExecutionSessionID)
	session.session.Model = firstNonEmptyString(strings.TrimSpace(input.Model), session.session.Model)
	session.session.AuthID = firstNonEmptyString(strings.TrimSpace(input.AuthID), session.session.AuthID)
	session.session.AuthLabel = firstNonEmptyString(strings.TrimSpace(input.AuthLabel), session.session.AuthLabel)
	session.session.Provider = firstNonEmptyString(strings.TrimSpace(input.Provider), session.session.Provider)
	session.session.DownstreamTransport = mergeTransport(session.session.DownstreamTransport, downstream)
	session.session.UpstreamTransport = mergeTransport(session.session.UpstreamTransport, upstream)
	session.session.ActiveRequestID = requestID
	session.session.LastRequestID = requestID
	session.session.Status = "streaming"
	session.addEvent(t.nextEventIDLocked(), now, "sidecar", "auth_selected", "Account selected for Codex request", "success", input.AuthID)

	reqState := t.ensureRequestLocked(session, requestID, input.Model, now, downstream, upstream)
	reqState.request.AuthID = strings.TrimSpace(input.AuthID)
	reqState.request.AuthLabel = strings.TrimSpace(input.AuthLabel)
	reqState.request.Provider = strings.TrimSpace(input.Provider)
	reqState.request.Status = "streaming"
	reqState.upstreamStarted = now
	reqState.request.Timeline = append(reqState.request.Timeline, liveEvent(t.nextEventIDLocked(), now, "upstream", "connect_start", "Connecting upstream", "info", upstream))
	t.requestMap[requestID] = sessionID
	t.pruneLocked(now)
}

func (t *liveSessionTracker) recordUpstreamConnected(requestID string, now time.Time) {
	t.updateRequest(requestID, now, func(session *liveSessionState, req *liveRequestState) {
		req.upstreamConnected = now
		if !req.upstreamStarted.IsZero() {
			req.request.Timing.UpstreamConnectMs = maxInt64(0, now.Sub(req.upstreamStarted).Milliseconds())
		}
		req.request.UpstreamTransport = mergeTransport(req.request.UpstreamTransport, "websocket")
		session.session.UpstreamTransport = mergeTransport(session.session.UpstreamTransport, "websocket")
		req.request.Timeline = append(req.request.Timeline, liveEvent(t.nextEventIDLocked(), now, "upstream", "connected", "Upstream WebSocket connected", "success", ""))
	})
}

func (t *liveSessionTracker) recordFirstEvent(requestID string, now time.Time) {
	t.updateRequest(requestID, now, func(_ *liveSessionState, req *liveRequestState) {
		if req.firstEventAt.IsZero() {
			req.firstEventAt = now
			req.request.Timing.FirstEventMs = maxInt64(0, now.Sub(parseLiveTime(req.request.StartedAt)).Milliseconds())
			req.request.Timing.FirstTokenMs = req.request.Timing.FirstEventMs
			req.request.Timeline = append(req.request.Timeline, liveEvent(t.nextEventIDLocked(), now, "upstream", "first_event", "First upstream event received", "success", ""))
		}
		if !req.lastEventAt.IsZero() {
			gap := now.Sub(req.lastEventAt).Milliseconds()
			if gap > 0 {
				req.eventGapTotalMs += gap
				req.eventGapCount++
				if gap > req.longestEventGap {
					req.longestEventGap = gap
				}
			}
		}
		req.lastEventAt = now
	})
}

func (t *liveSessionTracker) recordUpstreamDisconnected(sessionID string, err error, now time.Time) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.ensureSessionLocked(sessionID, now)
	if err == nil {
		if session.session.Status == "active" || session.session.Status == "streaming" || session.session.Status == "upstream_disconnected" {
			session.session.Status = "completed"
		}
		session.addEvent(t.nextEventIDLocked(), now, "upstream", "disconnected", "Upstream WebSocket closed", "info", "")
		return
	}
	session.session.Status = "upstream_disconnected"
	session.addEvent(t.nextEventIDLocked(), now, "upstream", "disconnected", sanitizeLiveError(err, "Upstream WebSocket disconnected"), "warning", "")
}

func (t *liveSessionTracker) recordRequestCompleted(requestID string, detail coreusage.Detail, err error, now time.Time) {
	t.updateRequest(requestID, now, func(session *liveSessionState, req *liveRequestState) {
		req.request.CompletedAt = formatLiveTime(now)
		req.request.Status = "completed"
		if err != nil {
			req.request.Status = "failed"
			req.request.Error = &LiveErrorSummary{Message: sanitizeLiveText(err.Error()), Retryable: true}
			session.session.Status = "failed"
		} else {
			session.session.Status = "completed"
		}
		req.request.Usage = liveUsage(detail)
		fillTiming(&req.request, detail, now)
		req.request.Timeline = append(req.request.Timeline, liveEvent(t.nextEventIDLocked(), now, "sidecar", req.request.Status, "Request "+req.request.Status, eventSeverity(req.request.Status), ""))
		session.session.ActiveRequestID = ""
	})
}

func (t *liveSessionTracker) observeUsage(ctx context.Context, record coreusage.Record, now time.Time) {
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	if requestID == "" {
		requestID = t.generatedRequestID(now)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if sessionID := t.requestMap[requestID]; sessionID != "" {
		if session := t.sessions[sessionID]; session != nil {
			if req := session.requests[requestID]; req != nil {
				req.request.Usage = liveUsage(record.Detail)
				fillTiming(&req.request, record.Detail, now)
				return
			}
		}
	}
	sessionID := requestID
	session := t.ensureSessionLocked(sessionID, record.RequestedAt)
	if record.RequestedAt.IsZero() {
		session = t.ensureSessionLocked(sessionID, now)
	}
	session.session.Model = firstNonEmptyString(record.Alias, record.Model)
	session.session.AuthID = strings.TrimSpace(record.AuthID)
	session.session.Provider = strings.TrimSpace(record.Provider)
	session.session.DownstreamTransport = "http"
	session.session.UpstreamTransport = "http"
	session.session.Status = "completed"
	session.session.LastRequestID = requestID
	req := t.ensureRequestLocked(session, requestID, session.session.Model, parseLiveTime(session.session.StartedAt), "http", "http")
	req.request.AuthID = record.AuthID
	req.request.Provider = record.Provider
	req.request.Status = "completed"
	req.request.CompletedAt = formatLiveTime(now)
	req.request.Usage = liveUsage(record.Detail)
	fillTiming(&req.request, record.Detail, now)
	req.request.Timeline = append(req.request.Timeline, liveEvent(t.nextEventIDLocked(), now, "sidecar", "completed", "HTTP request completed", "success", ""))
	t.requestMap[requestID] = sessionID
	t.pruneLocked(now)
}

func (t *liveSessionTracker) updateRequest(requestID string, now time.Time, update func(*liveSessionState, *liveRequestState)) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sessionID := t.requestMap[requestID]
	session := t.sessions[sessionID]
	if session == nil {
		return
	}
	req := session.requests[requestID]
	if req == nil {
		return
	}
	update(session, req)
	session.touch(now)
}

func (t *liveSessionTracker) snapshot(now time.Time) LiveSessionsSnapshot {
	t.mu.Lock()
	t.pruneLocked(now)
	items := make([]LiveSession, 0, len(t.sessions))
	for _, state := range t.sessions {
		session := state.clone(now)
		items = append(items, session)
	}
	t.mu.Unlock()

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].LastEventAt > items[j].LastEventAt
	})
	summary := LiveSessionSummary{}
	for _, session := range items {
		if session.Status == "active" || session.Status == "streaming" {
			summary.ActiveSessions++
		}
		if session.ActiveRequestID != "" {
			summary.ActiveRequests++
		}
		if session.DownstreamTransport == "websocket" || session.UpstreamTransport == "websocket" {
			summary.WebsocketSessions++
		}
		if session.DownstreamTransport == "http" || session.UpstreamTransport == "http" {
			summary.HTTPSessions++
		}
		if session.Status == "degraded_http" || session.FallbackInferred {
			summary.DegradedSessions++
		}
		if session.Status == "failed" || session.Status == "cancelled" {
			summary.ErrorSessions++
		}
	}
	return LiveSessionsSnapshot{
		GeneratedAt:  formatLiveTime(now),
		SidecarReady: true,
		Source:       "live",
		Retention:    liveSessionsRetentionLabel,
		Summary:      summary,
		Sessions:     items,
	}
}

func (t *liveSessionTracker) ensureSessionLocked(sessionID string, now time.Time) *liveSessionState {
	if now.IsZero() {
		now = time.Now()
	}
	session := t.sessions[sessionID]
	if session != nil {
		session.touch(now)
		return session
	}
	session = &liveSessionState{
		session: LiveSession{
			SessionID:           sessionID,
			Status:              "active",
			StartedAt:           formatLiveTime(now),
			LastEventAt:         formatLiveTime(now),
			DownstreamTransport: "unknown",
			UpstreamTransport:   "unknown",
			RecentEvents:        []LiveTimelineEvent{},
			Requests:            []LiveRequest{},
		},
		requests: map[string]*liveRequestState{},
	}
	t.sessions[sessionID] = session
	return session
}

func (t *liveSessionTracker) ensureRequestLocked(session *liveSessionState, requestID string, model string, now time.Time, downstream string, upstream string) *liveRequestState {
	req := session.requests[requestID]
	if req != nil {
		return req
	}
	sequence := len(session.requests) + 1
	req = &liveRequestState{
		request: LiveRequest{
			RequestID:           requestID,
			SessionID:           session.session.SessionID,
			Sequence:            sequence,
			Model:               firstNonEmptyString(strings.TrimSpace(model), session.session.Model),
			Status:              "active",
			StartedAt:           formatLiveTime(now),
			DownstreamTransport: firstNonEmptyString(strings.TrimSpace(downstream), "unknown"),
			UpstreamTransport:   firstNonEmptyString(strings.TrimSpace(upstream), "unknown"),
			Timeline:            []LiveTimelineEvent{liveEvent(t.nextEventIDLocked(), now, "downstream", "received", "Request received", "info", "")},
		},
	}
	session.requests[requestID] = req
	session.session.RequestCount = len(session.requests)
	return req
}

func (t *liveSessionTracker) pruneLocked(now time.Time) {
	cutoff := now.Add(-liveSessionsRetention)
	type candidate struct {
		id string
		at time.Time
	}
	candidates := make([]candidate, 0, len(t.sessions))
	for id, session := range t.sessions {
		last := parseLiveTime(session.session.LastEventAt)
		if last.Before(cutoff) {
			delete(t.sessions, id)
			continue
		}
		candidates = append(candidates, candidate{id: id, at: last})
	}
	if len(candidates) <= liveSessionsMaxItems {
		return
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].at.Before(candidates[j].at)
	})
	for _, item := range candidates[:len(candidates)-liveSessionsMaxItems] {
		delete(t.sessions, item.id)
	}
	t.requestMap = map[string]string{}
	for sessionID, session := range t.sessions {
		for requestID := range session.requests {
			t.requestMap[requestID] = sessionID
		}
	}
}

func (t *liveSessionTracker) nextEventIDLocked() string {
	t.seq++
	return "evt-" + strconvFormatInt(t.seq)
}

func (t *liveSessionTracker) generatedRequestID(now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	return "gt-req-" + strconvFormatInt(now.UnixMilli()) + "-" + strconvFormatInt(t.seq)
}

func (s *liveSessionState) addEvent(id string, now time.Time, lane string, kind string, label string, severity string, detail string) {
	s.session.RecentEvents = append(s.session.RecentEvents, liveEvent(id, now, lane, kind, label, severity, detail))
	if len(s.session.RecentEvents) > 12 {
		s.session.RecentEvents = s.session.RecentEvents[len(s.session.RecentEvents)-12:]
	}
	s.touch(now)
}

func (s *liveSessionState) touch(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	s.session.LastEventAt = formatLiveTime(now)
	s.session.DurationMs = maxInt64(0, now.Sub(parseLiveTime(s.session.StartedAt)).Milliseconds())
}

func (s *liveSessionState) clone(now time.Time) LiveSession {
	session := s.session
	session.DurationMs = maxInt64(0, now.Sub(parseLiveTime(session.StartedAt)).Milliseconds())
	requests := make([]LiveRequest, 0, len(s.requests))
	for _, reqState := range s.requests {
		req := reqState.request
		if req.CompletedAt == "" {
			fillTiming(&req, coreusage.Detail{}, now)
		}
		requests = append(requests, req)
	}
	sort.SliceStable(requests, func(i, j int) bool {
		return requests[i].Sequence < requests[j].Sequence
	})
	session.Requests = requests
	return session
}

func liveEvent(id string, now time.Time, lane string, kind string, label string, severity string, detail string) LiveTimelineEvent {
	return LiveTimelineEvent{
		ID:       id,
		At:       formatLiveClock(now),
		Lane:     firstNonEmptyString(lane, "sidecar"),
		Kind:     firstNonEmptyString(kind, "event"),
		Label:    sanitizeLiveText(firstNonEmptyString(label, kind)),
		Severity: firstNonEmptyString(severity, "info"),
		Detail:   sanitizeLiveText(detail),
	}
}

func liveUsage(detail coreusage.Detail) *LiveTokenUsage {
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	if total == 0 && detail.CachedTokens == 0 {
		return nil
	}
	return &LiveTokenUsage{
		InputTokens:       detail.InputTokens,
		CachedInputTokens: detail.CachedTokens,
		OutputTokens:      detail.OutputTokens,
		TotalTokens:       total,
	}
}

func fillTiming(req *LiveRequest, detail coreusage.Detail, now time.Time) {
	started := parseLiveTime(req.StartedAt)
	if started.IsZero() {
		return
	}
	totalMs := maxInt64(0, now.Sub(started).Milliseconds())
	if req.CompletedAt != "" {
		completed := parseLiveTime(req.CompletedAt)
		if !completed.IsZero() {
			totalMs = maxInt64(0, completed.Sub(started).Milliseconds())
		}
	}
	req.Timing.TotalDurationMs = totalMs
	if totalMs == 0 && (detail.OutputTokens > 0 || detail.TotalTokens > 0 || detail.InputTokens > 0) {
		totalMs = 1
		req.Timing.TotalDurationMs = totalMs
	}
	if req.Timing.StreamDurationMs == 0 && req.Timing.FirstEventMs > 0 {
		req.Timing.StreamDurationMs = maxInt64(0, totalMs-req.Timing.FirstEventMs)
	}
	seconds := float64(totalMs) / 1000
	if seconds <= 0 {
		return
	}
	if detail.OutputTokens > 0 {
		req.Timing.OutputTokensPerSecond = float64(detail.OutputTokens) / seconds
	}
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	if total > 0 {
		req.Timing.TotalTokensPerSecond = float64(total) / seconds
	}
}

func sanitizeLiveError(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return sanitizeLiveText(err.Error())
}

func sanitizeLiveText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 240 {
		return value[:240] + "..."
	}
	return value
}

func eventSeverity(status string) string {
	if status == "failed" || status == "cancelled" {
		return "error"
	}
	return "success"
}

func mergeTransport(current string, next string) string {
	current = strings.TrimSpace(current)
	next = strings.TrimSpace(next)
	if current == "" || current == "unknown" {
		return firstNonEmptyString(next, "unknown")
	}
	if next == "" || next == "unknown" || current == next {
		return current
	}
	if current == "websocket" && next == "http" {
		return "http"
	}
	return current
}

func parseLiveTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func formatLiveTime(value time.Time) string {
	if value.IsZero() {
		value = time.Now()
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func formatLiveClock(value time.Time) string {
	if value.IsZero() {
		value = time.Now()
	}
	return value.Local().Format("15:04:05.000")
}

func maxInt64(a int64, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func strconvFormatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
