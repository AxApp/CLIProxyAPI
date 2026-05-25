package gettokenshooks

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
	"github.com/tidwall/gjson"
)

const (
	liveSessionsRetention       = 30 * time.Minute
	liveSessionsRetentionLabel  = "30m / 200"
	liveSessionsMaxItems        = 200
	liveSessionProjectLookupTTL = 10 * time.Second
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
	ProjectName         string              `json:"projectName,omitempty"`
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

type CodexLiveSessionIdentity struct {
	ConversationID  string
	ClientRequestID string
	PromptCacheKey  string
	CodexWindowID   string
}

type CodexLiveRequestStart struct {
	ExecutionSessionID  string
	ConversationID      string
	ClientRequestID     string
	PromptCacheKey      string
	CodexWindowID       string
	Model               string
	AuthID              string
	AuthLabel           string
	Provider            string
	DownstreamTransport string
	UpstreamTransport   string
}

type liveSessionTracker struct {
	mu                       sync.RWMutex
	sessions                 map[string]*liveSessionState
	requestMap               map[string]string
	projectLookupCodexHome   string
	projectLookup            map[string]string
	projectLookupLastRefresh time.Time
	seq                      int64
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

type codexSessionMetaEnvelope struct {
	ID  string `json:"id"`
	Cwd string `json:"cwd"`
	Git struct {
		RepositoryURL string `json:"repository_url"`
	} `json:"git"`
}

type codexTurnContextEnvelope struct {
	Cwd string `json:"cwd"`
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

func RecordDownstreamWebsocketRequest(sessionID string, requestID string, model string, identities ...CodexLiveSessionIdentity) {
	currentLiveSessionTracker().recordDownstreamWebsocketRequest(sessionID, requestID, model, firstCodexLiveSessionIdentity(identities), time.Now())
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

func ExtractCodexLiveSessionIdentity(headers http.Header, payload []byte) CodexLiveSessionIdentity {
	identity := CodexLiveSessionIdentity{}
	if headers != nil {
		identity.ClientRequestID = strings.TrimSpace(headers.Get("x-client-request-id"))
		identity.CodexWindowID = strings.TrimSpace(headers.Get("x-codex-window-id"))
		identity.ConversationID = strings.TrimSpace(headers.Get("session_id"))
	}
	if len(payload) > 0 {
		if identity.PromptCacheKey == "" {
			identity.PromptCacheKey = strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String())
		}
		if identity.CodexWindowID == "" {
			identity.CodexWindowID = strings.TrimSpace(gjson.GetBytes(payload, "client_metadata.x-codex-window-id").String())
		}
		if identity.ClientRequestID == "" {
			identity.ClientRequestID = strings.TrimSpace(gjson.GetBytes(payload, "client_metadata.x-client-request-id").String())
		}
	}
	identity.ConversationID = firstNonEmptyString(
		identity.ConversationID,
		identity.PromptCacheKey,
		conversationIDFromCodexWindowID(identity.CodexWindowID),
		identity.ClientRequestID,
	)
	return identity
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

func (t *liveSessionTracker) recordDownstreamWebsocketRequest(sessionID string, requestID string, model string, identity CodexLiveSessionIdentity, now time.Time) {
	sessionID = strings.TrimSpace(sessionID)
	requestID = strings.TrimSpace(requestID)
	if sessionID == "" || requestID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.ensureSessionForIdentityLocked(sessionID, identity, now)
	session.session.Model = firstNonEmptyString(strings.TrimSpace(model), session.session.Model)
	session.session.ActiveRequestID = requestID
	session.session.LastRequestID = requestID
	session.session.Status = "streaming"
	session.addEvent(t.nextEventIDLocked(), now, "downstream", "request", "Codex WebSocket request received", "info", requestID)
	t.requestMap[requestID] = session.session.SessionID
	req := t.ensureRequestLocked(session, requestID, model, now, "websocket", "unknown")
	req.request.ClientRequestID = strings.TrimSpace(identity.ClientRequestID)
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
	identity := CodexLiveSessionIdentity{
		ConversationID:  input.ConversationID,
		ClientRequestID: input.ClientRequestID,
		PromptCacheKey:  input.PromptCacheKey,
		CodexWindowID:   input.CodexWindowID,
	}
	downstream := firstNonEmptyString(strings.TrimSpace(input.DownstreamTransport), "http")
	upstream := firstNonEmptyString(strings.TrimSpace(input.UpstreamTransport), "unknown")

	t.mu.Lock()
	defer t.mu.Unlock()
	session := t.ensureSessionForIdentityLocked(sessionID, identity, now)
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
	reqState.request.ClientRequestID = strings.TrimSpace(identity.ClientRequestID)
	reqState.request.AuthID = strings.TrimSpace(input.AuthID)
	reqState.request.AuthLabel = strings.TrimSpace(input.AuthLabel)
	reqState.request.Provider = strings.TrimSpace(input.Provider)
	reqState.request.Status = "streaming"
	reqState.upstreamStarted = now
	reqState.request.Timeline = append(reqState.request.Timeline, liveEvent(t.nextEventIDLocked(), now, "upstream", "connect_start", "Connecting upstream", "info", upstream))
	t.requestMap[requestID] = session.session.SessionID
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
	t.enrichSnapshotProjectNames(items, now)
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

func (t *liveSessionTracker) enrichSnapshotProjectNames(items []LiveSession, now time.Time) {
	needsLookup := false
	for _, item := range items {
		if strings.TrimSpace(item.ProjectName) == "" {
			needsLookup = true
			break
		}
	}
	if !needsLookup {
		return
	}

	codexHome, err := resolveLiveSessionCodexHome()
	if err != nil || strings.TrimSpace(codexHome) == "" {
		return
	}
	lookup, err := t.liveSessionProjectLookup(codexHome, now)
	if err != nil || len(lookup) == 0 {
		return
	}

	for index := range items {
		if strings.TrimSpace(items[index].ProjectName) != "" {
			continue
		}
		if projectName := findLiveSessionProjectName(lookup, items[index]); projectName != "" {
			items[index].ProjectName = projectName
		}
	}
}

func (t *liveSessionTracker) liveSessionProjectLookup(codexHome string, now time.Time) (map[string]string, error) {
	t.mu.RLock()
	if t.projectLookupCodexHome == codexHome &&
		!t.projectLookupLastRefresh.IsZero() &&
		now.Sub(t.projectLookupLastRefresh) < liveSessionProjectLookupTTL {
		lookup := cloneLiveSessionProjectLookup(t.projectLookup)
		t.mu.RUnlock()
		return lookup, nil
	}
	t.mu.RUnlock()

	lookup, err := buildLiveSessionProjectLookup(codexHome)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	t.projectLookupCodexHome = codexHome
	t.projectLookup = cloneLiveSessionProjectLookup(lookup)
	t.projectLookupLastRefresh = now
	t.mu.Unlock()
	return lookup, nil
}

func buildLiveSessionProjectLookup(codexHome string) (map[string]string, error) {
	paths, err := listCodexSessionJSONLPaths(codexHome)
	if err != nil {
		return nil, err
	}
	lookup := map[string]string{}
	for _, absolutePath := range paths {
		relativePath, err := filepath.Rel(codexHome, absolutePath)
		if err != nil {
			continue
		}
		relativePath = filepath.ToSlash(relativePath)
		sessionID, projectName := readLiveSessionCodexProjectIdentity(absolutePath, relativePath)
		if strings.TrimSpace(projectName) == "" {
			continue
		}
		addLiveSessionProjectLookupKey(lookup, relativePath, projectName)
		addLiveSessionProjectLookupKey(lookup, strings.TrimSuffix(filepath.Base(relativePath), filepath.Ext(relativePath)), projectName)
		addLiveSessionProjectLookupKey(lookup, sessionID, projectName)
	}
	return lookup, nil
}

func listCodexSessionJSONLPaths(codexHome string) ([]string, error) {
	roots := []string{
		filepath.Join(codexHome, "sessions"),
		filepath.Join(codexHome, "archived_sessions"),
	}
	paths := []string{}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".jsonl") {
				paths = append(paths, path)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func readLiveSessionCodexProjectIdentity(absolutePath string, relativePath string) (string, string) {
	file, err := os.Open(absolutePath)
	if err != nil {
		return "", ""
	}
	defer file.Close()

	var meta codexSessionMetaEnvelope
	currentCWD := ""
	sessionID := ""
	projectName := ""
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		var envelope struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			continue
		}
		switch envelope.Type {
		case "session_meta":
			if err := json.Unmarshal(envelope.Payload, &meta); err == nil {
				sessionID = strings.TrimSpace(meta.ID)
				projectName = deriveLiveSessionProjectName(meta, currentCWD)
			}
		case "turn_context":
			var turnContext codexTurnContextEnvelope
			if err := json.Unmarshal(envelope.Payload, &turnContext); err == nil && strings.TrimSpace(turnContext.Cwd) != "" {
				currentCWD = turnContext.Cwd
				projectName = deriveLiveSessionProjectName(meta, currentCWD)
			}
		}
		if sessionID != "" && strings.TrimSpace(projectName) != "" {
			break
		}
	}
	return sessionID, strings.TrimSpace(projectName)
}

func deriveLiveSessionProjectName(meta codexSessionMetaEnvelope, cwd string) string {
	if projectName := liveSessionPathBase(cwd); projectName != "" {
		return projectName
	}
	if projectName := liveSessionPathBase(meta.Cwd); projectName != "" {
		return projectName
	}
	if projectName := liveSessionRepoName(meta.Git.RepositoryURL); projectName != "" {
		return projectName
	}
	return ""
}

func addLiveSessionProjectLookupKey(lookup map[string]string, key string, projectName string) {
	key = strings.TrimSpace(key)
	projectName = strings.TrimSpace(projectName)
	if key == "" || projectName == "" {
		return
	}
	lookup[key] = projectName
}

func cloneLiveSessionProjectLookup(source map[string]string) map[string]string {
	if len(source) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func findLiveSessionProjectName(lookup map[string]string, session LiveSession) string {
	for _, key := range []string{
		session.SessionID,
		session.ExecutionSessionID,
		session.DownstreamSessionID,
		session.CodexWindowID,
		conversationIDFromCodexWindowID(session.CodexWindowID),
	} {
		if projectName := lookup[strings.TrimSpace(key)]; projectName != "" {
			return projectName
		}
	}
	return ""
}

func resolveLiveSessionCodexHome() (string, error) {
	if override := strings.TrimSpace(os.Getenv("CODEX_HOME")); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func liveSessionPathBase(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

func liveSessionRepoName(repositoryURL string) string {
	repositoryURL = strings.TrimSpace(repositoryURL)
	if repositoryURL == "" {
		return ""
	}
	repositoryURL = strings.TrimSuffix(repositoryURL, ".git")
	repositoryURL = strings.TrimSuffix(repositoryURL, "/")
	parts := strings.Split(repositoryURL, "/")
	return strings.TrimSpace(parts[len(parts)-1])
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

func (t *liveSessionTracker) ensureSessionForIdentityLocked(fallbackSessionID string, identity CodexLiveSessionIdentity, now time.Time) *liveSessionState {
	identity = normalizeCodexLiveSessionIdentity(identity)
	sessionID := firstNonEmptyString(identity.ConversationID, strings.TrimSpace(fallbackSessionID))
	session := t.ensureSessionLocked(sessionID, now)
	if fallbackSessionID = strings.TrimSpace(fallbackSessionID); fallbackSessionID != "" && fallbackSessionID != sessionID {
		if transient := t.sessions[fallbackSessionID]; transient != nil && transient != session {
			mergeLiveSessionState(session, transient)
			delete(t.sessions, fallbackSessionID)
			for requestID, mappedSessionID := range t.requestMap {
				if mappedSessionID == fallbackSessionID {
					t.requestMap[requestID] = sessionID
				}
			}
		}
	}
	if identity.CodexWindowID != "" {
		session.session.CodexWindowID = identity.CodexWindowID
	}
	return session
}

func (t *liveSessionTracker) ensureRequestLocked(session *liveSessionState, requestID string, model string, now time.Time, downstream string, upstream string) *liveRequestState {
	req := session.requests[requestID]
	if req != nil {
		req.request.SessionID = session.session.SessionID
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

func normalizeCodexLiveSessionIdentity(identity CodexLiveSessionIdentity) CodexLiveSessionIdentity {
	identity.ConversationID = strings.TrimSpace(identity.ConversationID)
	identity.ClientRequestID = strings.TrimSpace(identity.ClientRequestID)
	identity.PromptCacheKey = strings.TrimSpace(identity.PromptCacheKey)
	identity.CodexWindowID = strings.TrimSpace(identity.CodexWindowID)
	identity.ConversationID = firstNonEmptyString(
		identity.ConversationID,
		identity.PromptCacheKey,
		conversationIDFromCodexWindowID(identity.CodexWindowID),
		identity.ClientRequestID,
	)
	return identity
}

func conversationIDFromCodexWindowID(windowID string) string {
	windowID = strings.TrimSpace(windowID)
	if windowID == "" {
		return ""
	}
	if before, _, found := strings.Cut(windowID, ":"); found {
		return strings.TrimSpace(before)
	}
	return windowID
}

func firstCodexLiveSessionIdentity(identities []CodexLiveSessionIdentity) CodexLiveSessionIdentity {
	for _, identity := range identities {
		return identity
	}
	return CodexLiveSessionIdentity{}
}

func mergeLiveSessionState(target *liveSessionState, source *liveSessionState) {
	if target == nil || source == nil || target == source {
		return
	}
	target.session.ExecutionSessionID = firstNonEmptyString(target.session.ExecutionSessionID, source.session.ExecutionSessionID)
	target.session.ProjectName = firstNonEmptyString(target.session.ProjectName, source.session.ProjectName)
	target.session.DownstreamSessionID = firstNonEmptyString(target.session.DownstreamSessionID, source.session.DownstreamSessionID)
	target.session.CodexWindowID = firstNonEmptyString(target.session.CodexWindowID, source.session.CodexWindowID)
	target.session.Model = firstNonEmptyString(target.session.Model, source.session.Model)
	target.session.AuthID = firstNonEmptyString(target.session.AuthID, source.session.AuthID)
	target.session.AuthLabel = firstNonEmptyString(target.session.AuthLabel, source.session.AuthLabel)
	target.session.Provider = firstNonEmptyString(target.session.Provider, source.session.Provider)
	target.session.DownstreamTransport = mergeTransport(target.session.DownstreamTransport, source.session.DownstreamTransport)
	target.session.UpstreamTransport = mergeTransport(target.session.UpstreamTransport, source.session.UpstreamTransport)
	if target.session.ActiveRequestID == "" {
		target.session.ActiveRequestID = source.session.ActiveRequestID
	}
	if source.session.LastRequestID != "" {
		target.session.LastRequestID = source.session.LastRequestID
	}
	if statusRankForMerge(source.session.Status) > statusRankForMerge(target.session.Status) {
		target.session.Status = source.session.Status
	}
	if parseLiveTime(source.session.StartedAt).Before(parseLiveTime(target.session.StartedAt)) {
		target.session.StartedAt = source.session.StartedAt
	}
	if parseLiveTime(source.session.LastEventAt).After(parseLiveTime(target.session.LastEventAt)) {
		target.session.LastEventAt = source.session.LastEventAt
		target.session.DurationMs = source.session.DurationMs
	}
	target.session.RecentEvents = append(target.session.RecentEvents, source.session.RecentEvents...)
	if len(target.session.RecentEvents) > 12 {
		target.session.RecentEvents = target.session.RecentEvents[len(target.session.RecentEvents)-12:]
	}
	for requestID, request := range source.requests {
		if _, exists := target.requests[requestID]; exists {
			continue
		}
		request.request.SessionID = target.session.SessionID
		request.request.Sequence = len(target.requests) + 1
		target.requests[requestID] = request
	}
	target.session.RequestCount = len(target.requests)
}

func statusRankForMerge(status string) int {
	switch status {
	case "failed", "cancelled":
		return 6
	case "upstream_disconnected":
		return 5
	case "reconnecting", "degraded_http":
		return 4
	case "streaming":
		return 3
	case "active":
		return 2
	case "completed":
		return 1
	default:
		return 0
	}
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
