package gettokenshooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRateLimitEvaluatorBlocksRequestWindowRule(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-1",
		AccountKey: "codex-api-key:stable-001",
		MatchKey:   "auth-id:codex:apikey:abc123",
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 2,
		Action:     RateLimitActionBlock,
		Enabled:    true,
		Label:      "1h requests",
	}, now); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	for index := 0; index < 2; index++ {
		if err := store.insertUsageAttributionEvent(usageAttributionEvent{
			ID:                "event-" + string(rune('a'+index)),
			CompletedAtUnixMs: now.Add(time.Duration(index) * time.Minute).UnixMilli(),
			AttributionKey:    "auth-id:codex:apikey:abc123",
			AttributionKind:   "auth_id",
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			TotalTokens:       50,
			EvidenceKind:      "auth_id",
		}); err != nil {
			t.Fatalf("insert event %d: %v", index, err)
		}
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	state, ok := evaluator.StateForAccount("codex-api-key:stable-001")
	if !ok {
		t.Fatal("missing account state")
	}
	if !state.Blocked || state.BlockReason != "1h requests 已满" {
		t.Fatalf("blocked state = %#v, want 1h request block", state)
	}
	if got := evaluator.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex:apikey:abc123"}}); len(got) != 1 || got[0] != "codex:apikey:abc123" {
		t.Fatalf("deny ids = %#v, want candidate auth id", got)
	}
}

func TestRateLimitEvaluatorBlocksTokenWindowRule(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-token",
		AccountKey: "codex-api-key:stable-001",
		MatchKey:   "auth-id:codex:apikey:abc123",
		Strategy:   RateLimitStrategyTokenWindow,
		Window:     "24h",
		LimitValue: 100,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert token rule: %v", err)
	}
	if err := store.insertUsageAttributionEvent(usageAttributionEvent{
		ID:                "token-event",
		CompletedAtUnixMs: now.Add(2 * time.Minute).UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:abc123",
		AttributionKind:   "auth_id",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		TotalTokens:       100,
		EvidenceKind:      "auth_id",
	}); err != nil {
		t.Fatalf("insert token event: %v", err)
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	state, ok := evaluator.StateForAccount("codex-api-key:stable-001")
	if !ok {
		t.Fatal("missing account state")
	}
	if !state.Blocked || state.BlockReason != "24h tokens 已满" {
		t.Fatalf("blocked state = %#v, want 24h token block", state)
	}
	if got := state.Rules[0].CurrentUsage; got != 100 {
		t.Fatalf("current usage = %d, want token sum", got)
	}
}

func TestRateLimitEvaluatorWarnRuleDoesNotDenyCandidate(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-warn",
		AccountKey: "codex-api-key:stable-001",
		MatchKey:   "auth-id:codex:apikey:abc123",
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionWarn,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert warn rule: %v", err)
	}
	if err := store.insertUsageAttributionEvent(usageAttributionEvent{
		ID:                "warn-event",
		CompletedAtUnixMs: now.Add(time.Minute).UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:abc123",
		AttributionKind:   "auth_id",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		TotalTokens:       25,
		EvidenceKind:      "auth_id",
	}); err != nil {
		t.Fatalf("insert warn event: %v", err)
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	state, ok := evaluator.StateForAccount("codex-api-key:stable-001")
	if !ok {
		t.Fatal("missing account state")
	}
	if state.Blocked {
		t.Fatalf("state = %#v, warn rule should not block", state)
	}
	if len(state.Rules) != 1 || !state.Rules[0].Exceeded {
		t.Fatalf("rules = %#v, want exceeded warning rule", state.Rules)
	}
	if got := evaluator.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex:apikey:abc123"}}); len(got) != 0 {
		t.Fatalf("deny ids = %#v, warn rule should not deny", got)
	}
}

func TestRateLimitEvaluatorRecoversWhenWindowSlides(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	base := time.Now().UTC().Truncate(time.Hour)
	current := base.Add(30 * time.Minute)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-slide",
		AccountKey: "codex-api-key:stable-001",
		MatchKey:   "auth-id:codex:apikey:abc123",
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 2,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, base); err != nil {
		t.Fatalf("upsert sliding rule: %v", err)
	}
	for index := 0; index < 2; index++ {
		if err := store.insertUsageAttributionEvent(usageAttributionEvent{
			ID:                "slide-event-" + string(rune('a'+index)),
			CompletedAtUnixMs: base.Add(time.Duration(index) * time.Minute).UnixMilli(),
			AttributionKey:    "auth-id:codex:apikey:abc123",
			AttributionKind:   "auth_id",
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			TotalTokens:       25,
			EvidenceKind:      "auth_id",
		}); err != nil {
			t.Fatalf("insert sliding event %d: %v", index, err)
		}
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return current },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate blocked window: %v", err)
	}
	if state, ok := evaluator.StateForAccount("codex-api-key:stable-001"); !ok || !state.Blocked {
		t.Fatalf("initial state = %#v, ok=%v, want blocked", state, ok)
	}

	current = base.Add(2 * time.Hour)
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate recovered window: %v", err)
	}
	state, ok := evaluator.StateForAccount("codex-api-key:stable-001")
	if !ok {
		t.Fatal("missing recovered account state")
	}
	if state.Blocked || len(state.Rules) != 1 || state.Rules[0].CurrentUsage != 0 {
		t.Fatalf("recovered state = %#v, want unblocked zero usage", state)
	}
	if got := evaluator.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex:apikey:abc123"}}); len(got) != 0 {
		t.Fatalf("deny ids = %#v, recovered account should not deny", got)
	}
}

func TestRateLimitEvaluatorSkipsDisabledRulesAndUnconfiguredCandidates(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-disabled",
		AccountKey: "codex-api-key:stable-001",
		MatchKey:   "auth-id:codex:apikey:abc123",
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionBlock,
		Enabled:    false,
	}, now); err != nil {
		t.Fatalf("upsert disabled rule: %v", err)
	}
	if err := store.insertUsageAttributionEvent(usageAttributionEvent{
		ID:                "disabled-event",
		CompletedAtUnixMs: now.Add(time.Minute).UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:abc123",
		AttributionKind:   "auth_id",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		TotalTokens:       25,
		EvidenceKind:      "auth_id",
	}); err != nil {
		t.Fatalf("insert disabled event: %v", err)
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate disabled rule: %v", err)
	}
	if _, ok := evaluator.StateForAccount("codex-api-key:stable-001"); ok {
		t.Fatal("disabled rule should not create account state")
	}
	deny := evaluator.DenyIDsForCandidates([]*coreauth.Auth{
		{ID: "codex:apikey:abc123"},
		{ID: "codex:apikey:unconfigured"},
	})
	if len(deny) != 0 {
		t.Fatalf("deny ids = %#v, disabled and unconfigured candidates should pass", deny)
	}
}

func TestRateLimitPolicyAddsBlockedCandidateToDenyIDs(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{})
	evaluator.replaceStatesForTest([]RateLimitState{{
		AccountKey:  "openai-compatible:MI",
		MatchKey:    "auth-id:openai-compatibility:mi:abc123",
		Blocked:     true,
		BlockReason: "24h tokens 已满",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}})

	decision := rateLimitPolicy{evaluator: evaluator}.RewriteCandidates(context.Background(), coreauth.RoutePolicyRequest{
		Candidates: []*coreauth.Auth{{ID: "openai-compatibility:mi:abc123", Provider: "mi"}},
	})
	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != "openai-compatibility:mi:abc123" {
		t.Fatalf("DenyIDs = %#v, want blocked candidate", decision.DenyIDs)
	}
}

func TestRateLimitEvaluatorUsesRegisteredStrategy(t *testing.T) {
	previousRegistry := defaultRateLimitRegistry
	defaultRateLimitRegistry = NewRateLimitStrategyRegistry(rateLimitTestStrategy{})
	t.Cleanup(func() {
		defaultRateLimitRegistry = previousRegistry
	})

	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-custom",
		AccountKey: "codex-api-key:stable-001",
		MatchKey:   "auth-id:codex:apikey:abc123",
		Strategy:   "test-window",
		Window:     "1h",
		LimitValue: 5,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert custom strategy rule: %v", err)
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate custom strategy: %v", err)
	}

	state, ok := evaluator.StateForAccount("codex-api-key:stable-001")
	if !ok {
		t.Fatal("missing account state")
	}
	if !state.Blocked || state.BlockReason != "1h test 已满" {
		t.Fatalf("blocked state = %#v, want registered strategy block", state)
	}

	strategies := ListRateLimitStrategies()
	if len(strategies) != 1 || strategies[0].ID != "test-window" {
		t.Fatalf("strategies = %#v, want registered test strategy", strategies)
	}
}

func TestRateLimitManagementRoutesExposeStrategiesCRUDStatusAndEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	rateLimitMu.Lock()
	previousStore := defaultRateLimitStore
	previousEval := defaultRateLimitEval
	defaultRateLimitStore = store
	defaultRateLimitEval = evaluator
	rateLimitMu.Unlock()
	t.Cleanup(func() {
		rateLimitMu.Lock()
		defaultRateLimitStore = previousStore
		defaultRateLimitEval = previousEval
		rateLimitMu.Unlock()
	})

	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureRateLimitRoutes(group, nil, nil)

	strategies := performRateLimitRequest(t, router, http.MethodGet, "/v0/management/gettokens/rate-limit-strategies", "")
	var strategiesResponse struct {
		Items []RateLimitStrategyMeta `json:"items"`
	}
	if err := json.Unmarshal(strategies.Body.Bytes(), &strategiesResponse); err != nil {
		t.Fatalf("decode strategies: %v", err)
	}
	if len(strategiesResponse.Items) != 2 {
		t.Fatalf("strategies = %#v, want built-ins", strategiesResponse.Items)
	}

	createBody := `{
		"account_key":"codex-api-key:stable-001",
		"match_key":"auth-id:codex:apikey:abc123",
		"strategy":"request-window",
		"window":"1h",
		"limit_value":2,
		"action":"block",
		"enabled":true,
		"label":"1h requests"
	}`
	created := performRateLimitRequest(t, router, http.MethodPost, "/v0/management/gettokens/rate-limit-rules", createBody)
	var rulesResponse struct {
		Items []RateLimitRule `json:"items"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode created rules: %v", err)
	}
	if len(rulesResponse.Items) != 1 || rulesResponse.Items[0].ID == "" {
		t.Fatalf("created rules = %#v, want persisted rule with id", rulesResponse.Items)
	}
	ruleID := rulesResponse.Items[0].ID

	for index := 0; index < 2; index++ {
		if err := store.insertUsageAttributionEvent(usageAttributionEvent{
			ID:                "route-event-" + string(rune('a'+index)),
			CompletedAtUnixMs: now.Add(time.Duration(index) * time.Minute).UnixMilli(),
			AttributionKey:    "auth-id:codex:apikey:abc123",
			AttributionKind:   "auth_id",
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			TotalTokens:       50,
			EvidenceKind:      "auth_id",
		}); err != nil {
			t.Fatalf("insert usage event %d: %v", index, err)
		}
	}
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	status := performRateLimitRequest(t, router, http.MethodGet, "/v0/management/gettokens/rate-limit-status?account_key=codex-api-key:stable-001", "")
	var state RateLimitState
	if err := json.Unmarshal(status.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !state.Blocked || state.BlockReason != "1h requests 已满" {
		t.Fatalf("state = %#v, want blocked request window", state)
	}

	events := performRateLimitRequest(t, router, http.MethodGet, "/v0/management/gettokens/rate-limit-events?account_key=codex-api-key:stable-001", "")
	var eventsResponse struct {
		Items []RateLimitEvent `json:"items"`
	}
	if err := json.Unmarshal(events.Body.Bytes(), &eventsResponse); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(eventsResponse.Items) != 1 || eventsResponse.Items[0].RuleID != ruleID || !eventsResponse.Items[0].Blocked {
		t.Fatalf("events = %#v, want persisted block event", eventsResponse.Items)
	}

	deleteResponse := performRateLimitRequest(t, router, http.MethodDelete, "/v0/management/gettokens/rate-limit-rules/"+ruleID, "")
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", deleteResponse.Code)
	}
	listResponse := performRateLimitRequest(t, router, http.MethodGet, "/v0/management/gettokens/rate-limit-rules?account_key=codex-api-key:stable-001", "")
	if err := json.Unmarshal(listResponse.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode listed rules: %v", err)
	}
	if len(rulesResponse.Items) != 0 {
		t.Fatalf("rules after delete = %#v, want empty", rulesResponse.Items)
	}
}

func performRateLimitRequest(t *testing.T, router http.Handler, method string, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code < http.StatusOK || response.Code >= http.StatusMultipleChoices {
		t.Fatalf("%s %s returned %d: %s", method, target, response.Code, response.Body.String())
	}
	return response
}

type rateLimitTestStrategy struct{}

func (rateLimitTestStrategy) ID() string { return "test-window" }

func (rateLimitTestStrategy) Name() string { return "测试窗口限流" }

func (rateLimitTestStrategy) SupportedWindows() []string { return []string{"1h"} }

func (rateLimitTestStrategy) UsageForRule(context.Context, *rateLimitStore, RateLimitRule, time.Time) (int64, error) {
	return 5, nil
}

func (rateLimitTestStrategy) FormatReason(rule RateLimitRule) string {
	return rateLimitRuleWindow(rule) + " test 已满"
}
