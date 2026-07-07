package gettokenshooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestResolveUsageAttributionPathPrefersConfigDir(t *testing.T) {
	t.Setenv("GETTOKENS_USAGE_ATTRIBUTION_SQLITE_PATH", "")
	got, err := resolveUsageAttributionPath(UsageAttributionOptions{
		ConfigFilePath: "/tmp/gettokens/config.yaml",
		WritableBase:   "/tmp/fallback",
	})
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}
	want := "/tmp/gettokens/usage-attribution-v1.sqlite"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestUsageAttributionEventPrefersAuthIDOverSource(t *testing.T) {
	record := coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.4",
		Alias:       "client-gpt",
		Source:      "sk-upstream-secret",
		APIKey:      "relay-key-should-not-be-used",
		AuthID:      "codex:apikey:abc123",
		AccountKey:  "codex-api-key:stable-001",
		AuthIndex:   "auth-index-should-not-win",
		AuthType:    "api-key",
		RequestedAt: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		Latency:     1200 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
		},
	}

	event := buildUsageAttributionEvent(context.Background(), record)
	if event.AttributionKey != "auth-id:codex:apikey:abc123" {
		t.Fatalf("attribution key = %q, want auth-id", event.AttributionKey)
	}
	if event.AccountKey != "codex-api-key:stable-001" {
		t.Fatalf("account key = %q, want account-card id", event.AccountKey)
	}
	if event.APIKeyHash != "" {
		t.Fatalf("api key hash = %q, want empty because record APIKey is relay key", event.APIKeyHash)
	}
	if event.SourceHash == "" {
		t.Fatal("source hash is empty, want diagnostic source hash")
	}
	if event.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want 30", event.TotalTokens)
	}
}

func TestUsageAttributionEventKeepsAuthIndexForOAuthAuthFile(t *testing.T) {
	record := coreusage.Record{
		Provider:  "codex",
		Model:     "gpt-5.4",
		AuthID:    "some-runtime-auth-id",
		AuthIndex: "auth-file-001",
		AuthType:  "oauth",
	}

	event := buildUsageAttributionEvent(context.Background(), record)
	if event.AttributionKey != "auth-index:auth-file-001" {
		t.Fatalf("attribution key = %q, want auth-index", event.AttributionKey)
	}
}

func TestUsageAttributionEventPrefersAuthIDForAPIUnderscoreKeyType(t *testing.T) {
	record := coreusage.Record{
		Provider:  "mi",
		Model:     "gpt-5.4",
		AuthID:    "runtime-mi-auth-id",
		AuthIndex: "auth-index-should-not-win",
		AuthType:  "api_key",
	}

	event := buildUsageAttributionEvent(context.Background(), record)
	if event.AttributionKey != "auth-id:runtime-mi-auth-id" {
		t.Fatalf("attribution key = %q, want auth-id", event.AttributionKey)
	}
}

func TestUsageAttributionPluginRefreshesRateLimitGuardAfterPersist(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit) })

	dbPath := filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite")
	rateStore, err := newRateLimitStore(dbPath)
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := rateStore.upsertRule(RateLimitRule{
		ID:         "instant-token-rule",
		AccountKey: "acct_00000000-0000-4000-8000-000000000003",
		Strategy:   RateLimitStrategyTokenWindow,
		Window:     "1h",
		LimitValue: 100,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert token rule: %v", err)
	}
	evaluator := NewRateLimitEvaluator(rateStore, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(5 * time.Minute) },
	})

	rateLimitMu.Lock()
	previousStore := defaultRateLimitStore
	previousEval := defaultRateLimitEval
	defaultRateLimitStore = rateStore
	defaultRateLimitEval = evaluator
	rateLimitMu.Unlock()
	t.Cleanup(func() {
		rateLimitMu.Lock()
		defaultRateLimitStore = previousStore
		defaultRateLimitEval = previousEval
		rateLimitMu.Unlock()
	})

	plugin := usageAttributionPlugin{store: &usageAttributionStore{db: rateStore.db}}
	plugin.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.4",
		AuthID:      "codex:apikey:instant",
		AccountKey:  "acct_00000000-0000-4000-8000-000000000003",
		AuthType:    "api-key",
		RequestedAt: now.Add(time.Minute),
		Latency:     time.Second,
		Detail: coreusage.Detail{
			TotalTokens: 100,
		},
	})

	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000003")
	if !ok || !state.Blocked {
		t.Fatalf("rate limit state = %#v, exists=%v, want immediate blocked state after usage persist", state, ok)
	}
	if got := DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         "codex:apikey:instant",
		AccountKey: "acct_00000000-0000-4000-8000-000000000003",
		Provider:   "codex",
	}}); len(got) != 1 || got[0] != "codex:apikey:instant" {
		t.Fatalf("route guard deny ids = %#v, want immediate rate-limit deny", got)
	}
}

func TestUsageAttributionStoreSummaryAggregatesBuckets(t *testing.T) {
	store, err := newUsageAttributionStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	first := usageAttributionEvent{
		ID:                "event-1",
		CompletedAtUnixMs: now.UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:abc123",
		AttributionKind:   "auth_id",
		AccountKey:        "codex-api-key:stable-001",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		LatencyMs:         100,
		InputTokens:       10,
		OutputTokens:      20,
		TotalTokens:       30,
		EvidenceKind:      "auth_id",
	}
	second := first
	second.ID = "event-2"
	second.CompletedAtUnixMs = now.Add(10 * time.Minute).UnixMilli()
	second.Failed = true
	second.LatencyMs = 300
	second.InputTokens = 1
	second.OutputTokens = 2
	second.TotalTokens = 3

	if err := store.insert(first); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if err := store.insert(second); err != nil {
		t.Fatalf("insert second: %v", err)
	}

	summary, err := store.summary(24*time.Hour, time.Hour, false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(summary.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(summary.Items))
	}
	item := summary.Items[0]
	if item.AccountKey != "codex-api-key:stable-001" {
		t.Fatalf("account key = %q, want local-id account key", item.AccountKey)
	}
	if item.RequestCount != 2 || item.FailedCount != 1 {
		t.Fatalf("counts = %d/%d, want 2/1", item.RequestCount, item.FailedCount)
	}
	if item.LatencyAverageMs != 200 {
		t.Fatalf("avg latency = %d, want 200", item.LatencyAverageMs)
	}
	if item.TotalTokens != 33 {
		t.Fatalf("total tokens = %d, want 33", item.TotalTokens)
	}
	if len(item.Buckets) != 1 || item.Buckets[0].RequestCount != 2 {
		t.Fatalf("unexpected buckets: %#v", item.Buckets)
	}
}

func TestUsageAttributionStorePrunesEventsOlderThanRetention(t *testing.T) {
	store, err := newUsageAttributionStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	oldEvent := usageAttributionEvent{
		ID:                "event-old-retention",
		CompletedAtUnixMs: now.Add(-45 * 24 * time.Hour).UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:retention",
		AttributionKind:   "auth_id",
		AccountKey:        "codex-api-key:retention-001",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		LatencyMs:         120,
		InputTokens:       40,
		OutputTokens:      10,
		TotalTokens:       50,
		EvidenceKind:      "auth_id",
	}
	recentEvent := oldEvent
	recentEvent.ID = "event-recent-retention"
	recentEvent.CompletedAtUnixMs = now.UnixMilli()
	recentEvent.TotalTokens = 7

	if err := store.insert(oldEvent); err != nil {
		t.Fatalf("insert old event: %v", err)
	}
	if err := store.insert(recentEvent); err != nil {
		t.Fatalf("insert recent event: %v", err)
	}

	summary, err := store.summary(60*24*time.Hour, time.Hour, false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(summary.Items) != 1 {
		t.Fatalf("items = %d, want only retained account: %#v", len(summary.Items), summary.Items)
	}
	item := summary.Items[0]
	if item.RequestCount != 1 || item.TotalTokens != 7 {
		t.Fatalf("retained usage = requests:%d tokens:%d, want 1/7", item.RequestCount, item.TotalTokens)
	}
}

func TestUsageAttributionStoreSummarySupportsAllWindow(t *testing.T) {
	store, err := newUsageAttributionStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	oldEvent := usageAttributionEvent{
		ID:                "event-old",
		CompletedAtUnixMs: now.Add(-5 * 24 * time.Hour).UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:old",
		AttributionKind:   "auth_id",
		AccountKey:        "codex-api-key:old-001",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		LatencyMs:         120,
		InputTokens:       40,
		OutputTokens:      10,
		TotalTokens:       50,
		EvidenceKind:      "auth_id",
	}
	newEvent := oldEvent
	newEvent.ID = "event-new"
	newEvent.AccountKey = "codex-api-key:new-001"
	newEvent.AttributionKey = "auth-id:codex:apikey:new"
	newEvent.CompletedAtUnixMs = now.UnixMilli()

	if err := store.insert(oldEvent); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if err := store.insert(newEvent); err != nil {
		t.Fatalf("insert new: %v", err)
	}

	summary, err := store.summary(-1, time.Hour, false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Window != "all" {
		t.Fatalf("window = %q, want all", summary.Window)
	}
	if len(summary.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(summary.Items))
	}
}

func TestUsageAttributionStoreDetailsPaginatesNewestFirst(t *testing.T) {
	store, err := newUsageAttributionStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	for i := 0; i < 3; i++ {
		event := usageAttributionEvent{
			ID:                "event-" + string(rune('a'+i)),
			RequestID:         "req-" + string(rune('a'+i)),
			CompletedAtUnixMs: now.Add(time.Duration(i) * time.Minute).UnixMilli(),
			AttributionKey:    "auth-id:test",
			AttributionKind:   "auth_id",
			AccountKey:        "account-1",
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			TotalTokens:       int64(10 + i),
			EvidenceKind:      "auth_id",
		}
		if err := store.insert(event); err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}

	out, err := store.details(24*time.Hour, 2, 1, "account-1", "")
	if err != nil {
		t.Fatalf("details: %v", err)
	}
	if out.Limit != 2 || out.Offset != 1 {
		t.Fatalf("pagination = %#v", out)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(out.Items))
	}
	if out.Items[0].RequestID != "req-b" || out.Items[1].RequestID != "req-a" {
		t.Fatalf("unexpected ordering: %#v", out.Items)
	}
}

func TestUsageAttributionDetailsRoute(t *testing.T) {
	store, err := newUsageAttributionStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	usageAttributionMu.Lock()
	defaultAttributionStore = store
	usageAttributionMu.Unlock()
	t.Cleanup(func() {
		usageAttributionMu.Lock()
		defaultAttributionStore = nil
		usageAttributionMu.Unlock()
	})

	if err := store.insert(usageAttributionEvent{
		ID:                "event-1",
		RequestID:         "req-1",
		CompletedAtUnixMs: time.Now().UTC().UnixMilli(),
		AttributionKey:    "auth-id:test",
		AttributionKind:   "auth_id",
		AccountKey:        "account-1",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		TotalTokens:       11,
		EvidenceKind:      "auth_id",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureUsageAttributionRoutes(group, nil, nil)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/usage-attribution/details?limit=1&offset=0&account_key=account-1", nil)
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload UsageAttributionDetailResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, recorder.Body.String())
	}
	if len(payload.Items) != 1 || payload.Items[0].RequestID != "req-1" {
		t.Fatalf("payload = %#v", payload)
	}
}
