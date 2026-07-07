package gettokenshooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRateLimitEvaluatorBlocksRequestWindowRule(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit) })

	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-1",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
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
			AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
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

	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001")
	if !ok {
		t.Fatal("missing account state")
	}
	if !state.Blocked || state.BlockReason != "1h requests 已满" {
		t.Fatalf("blocked state = %#v, want 1h request block", state)
	}
	if got := evaluator.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex:apikey:abc123", AccountKey: "acct_00000000-0000-4000-8000-000000000001"}}); len(got) != 1 || got[0] != "codex:apikey:abc123" {
		t.Fatalf("deny ids = %#v, want candidate auth id", got)
	}
	if got := DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex:apikey:abc123", AccountKey: "acct_00000000-0000-4000-8000-000000000001"}}); len(got) != 1 || got[0] != "codex:apikey:abc123" {
		t.Fatalf("route guard deny ids = %#v, want rate-limited candidate auth id", got)
	}
}

func TestRateLimitAdmissionReservationDeniesConcurrentRequestWindow(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit) })

	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	accountKey := "acct_00000000-0000-4000-8000-000000000001"
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-admission",
		AccountKey: accountKey,
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	unregister := gettokensrouting.RegisterAdmissionPolicy(rateLimitAdmissionPolicy(evaluator))
	defer unregister()

	first := gettokensrouting.AdmitCandidate(
		context.Background(),
		gettokensrouting.RouteContext{Provider: "codex", Model: "gpt-5.4"},
		gettokensrouting.RouteCandidate{
			ID:    "auth-a",
			Value: &coreauth.Auth{ID: "auth-a", AccountKey: accountKey, Provider: "codex"},
		},
	)
	if !first.Active || !first.Allow {
		t.Fatalf("first admission = %#v, want active allow with reservation", first)
	}
	second := gettokensrouting.AdmitCandidate(
		context.Background(),
		gettokensrouting.RouteContext{Provider: "codex", Model: "gpt-5.4"},
		gettokensrouting.RouteCandidate{
			ID:    "auth-a",
			Value: &coreauth.Auth{ID: "auth-a", AccountKey: accountKey, Provider: "codex"},
		},
	)
	if !second.Active || second.Allow {
		t.Fatalf("second admission = %#v, want active deny for same request window", second)
	}

	first.Lease.Release(context.Background())
	recovered := gettokensrouting.AdmitCandidate(
		context.Background(),
		gettokensrouting.RouteContext{Provider: "codex", Model: "gpt-5.4"},
		gettokensrouting.RouteCandidate{
			ID:    "auth-a",
			Value: &coreauth.Auth{ID: "auth-a", AccountKey: accountKey, Provider: "codex"},
		},
	)
	if !recovered.Active || !recovered.Allow {
		t.Fatalf("recovered admission = %#v, want allow after reservation release", recovered)
	}
	recovered.Lease.Release(context.Background())
}

func TestRateLimitAdmissionNoopsWithoutRequestWindowRulesBeforeReservationWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite")
	store, err := newRateLimitStore(dbPath)
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	accountKey := "acct_00000000-0000-4000-8000-000000000001"
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-token-only",
		AccountKey: accountKey,
		Strategy:   RateLimitStrategyTokenWindow,
		Window:     "24h",
		LimitValue: 1000,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert token rule: %v", err)
	}

	lockDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open lock db: %v", err)
	}
	defer lockDB.Close()
	lockConn, err := lockDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("lock conn: %v", err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin lock transaction: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	decision := evaluator.admitRequestWindow(ctx, &coreauth.Auth{
		ID:         "auth-a",
		AccountKey: accountKey,
		Provider:   "codex",
	})
	if decision.Active {
		t.Fatalf("admission = %#v, want no-op when no request-window rules exist", decision)
	}

	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("rollback lock transaction: %v", err)
	}
	locked = false
	var reservations int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM rate_limit_reservations`).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("reservations = %d, want 0 for token-window-only admission", reservations)
	}
}

func TestRateLimitAdmissionReservationCleanupExpiresOrphan(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	base := time.Now().UTC().Truncate(time.Hour)
	current := base.Add(30 * time.Minute)
	accountKey := "acct_00000000-0000-4000-8000-000000000001"
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-orphan",
		AccountKey: accountKey,
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, base); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return current },
	})
	unregister := gettokensrouting.RegisterAdmissionPolicy(rateLimitAdmissionPolicy(evaluator))
	defer unregister()

	first := gettokensrouting.AdmitCandidate(
		context.Background(),
		gettokensrouting.RouteContext{Provider: "codex", Model: "gpt-5.4"},
		gettokensrouting.RouteCandidate{
			ID:    "auth-a",
			Value: &coreauth.Auth{ID: "auth-a", AccountKey: accountKey, Provider: "codex"},
		},
	)
	if !first.Active || !first.Allow {
		t.Fatalf("first admission = %#v, want active allow with reservation", first)
	}
	first.Lease.Commit(context.Background())

	current = current.Add(defaultRateLimitReservationTTL + time.Second)
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate after orphan ttl: %v", err)
	}
	recovered := gettokensrouting.AdmitCandidate(
		context.Background(),
		gettokensrouting.RouteContext{Provider: "codex", Model: "gpt-5.4"},
		gettokensrouting.RouteCandidate{
			ID:    "auth-a",
			Value: &coreauth.Auth{ID: "auth-a", AccountKey: accountKey, Provider: "codex"},
		},
	)
	if !recovered.Active || !recovered.Allow {
		t.Fatalf("recovered admission = %#v, want orphan cleanup to release capacity", recovered)
	}
	recovered.Lease.Release(context.Background())
}

func TestRateLimitEvaluatorBlocksTokenWindowRule(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-token",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
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
		AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
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

	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001")
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

func TestRateLimitEvaluatorTokenWindowIgnoresFailedUsageTokens(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	accountKey := "acct_00000000-0000-4000-8000-000000000001"
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-token-success-only",
		AccountKey: accountKey,
		Strategy:   RateLimitStrategyTokenWindow,
		Window:     "24h",
		LimitValue: 500,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert token rule: %v", err)
	}
	events := []usageAttributionEvent{
		{
			ID:                "successful-token-event",
			CompletedAtUnixMs: now.Add(2 * time.Minute).UnixMilli(),
			AttributionKey:    "auth-id:codex:apikey:abc123",
			AttributionKind:   "auth_id",
			AccountKey:        accountKey,
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			TotalTokens:       100,
			EvidenceKind:      "auth_id",
		},
		{
			ID:                "failed-token-event",
			CompletedAtUnixMs: now.Add(3 * time.Minute).UnixMilli(),
			AttributionKey:    "auth-id:codex:apikey:abc123",
			AttributionKind:   "auth_id",
			AccountKey:        accountKey,
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			StatusCode:        http.StatusTooManyRequests,
			Failed:            true,
			TotalTokens:       900,
			EvidenceKind:      "auth_id",
		},
	}
	for _, event := range events {
		if err := store.insertUsageAttributionEvent(event); err != nil {
			t.Fatalf("insert event %s: %v", event.ID, err)
		}
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	state, ok := evaluator.StateForAccount(accountKey)
	if !ok {
		t.Fatal("missing account state")
	}
	if state.Blocked {
		t.Fatalf("state = %#v, failed usage tokens should not trip token-window", state)
	}
	if got := state.Rules[0].CurrentUsage; got != 100 {
		t.Fatalf("current usage = %d, want only successful token usage", got)
	}
}

func TestRateLimitEvaluatorCalendarDayWindowStartsAtLocalMidnight(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 5, 22, 14, 30, 0, 0, location)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-calendar-day",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     RateLimitWindowCalendarDay,
		LimitValue: 2,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert calendar day rule: %v", err)
	}
	events := []struct {
		id        string
		timestamp time.Time
	}{
		{id: "previous-day", timestamp: time.Date(2026, 5, 21, 23, 59, 0, 0, location)},
		{id: "day-start", timestamp: time.Date(2026, 5, 22, 0, 5, 0, 0, location)},
		{id: "midday", timestamp: time.Date(2026, 5, 22, 12, 0, 0, 0, location)},
	}
	for _, event := range events {
		if err := store.insertUsageAttributionEvent(usageAttributionEvent{
			ID:                event.id,
			CompletedAtUnixMs: event.timestamp.UTC().UnixMilli(),
			AttributionKey:    "auth-id:codex:apikey:abc123",
			AttributionKind:   "auth_id",
			AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			TotalTokens:       25,
			EvidenceKind:      "auth_id",
		}); err != nil {
			t.Fatalf("insert event %s: %v", event.id, err)
		}
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001")
	if !ok {
		t.Fatal("missing account state")
	}
	if got := state.Rules[0].CurrentUsage; got != 2 {
		t.Fatalf("calendar day usage = %d, want only same-day requests", got)
	}
	if !state.Blocked || state.BlockReason != "00:00-23:59 requests 已满" {
		t.Fatalf("blocked state = %#v, want calendar day request block", state)
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
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
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
		AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
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

	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001")
	if !ok {
		t.Fatal("missing account state")
	}
	if state.Blocked {
		t.Fatalf("state = %#v, warn rule should not block", state)
	}
	if len(state.Rules) != 1 || !state.Rules[0].Exceeded {
		t.Fatalf("rules = %#v, want exceeded warning rule", state.Rules)
	}
	if got := evaluator.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex:apikey:abc123", AccountKey: "acct_00000000-0000-4000-8000-000000000001"}}); len(got) != 0 {
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
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
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
			AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
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
	if state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001"); !ok || !state.Blocked {
		t.Fatalf("initial state = %#v, ok=%v, want blocked", state, ok)
	}

	current = base.Add(2 * time.Hour)
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate recovered window: %v", err)
	}
	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001")
	if !ok {
		t.Fatal("missing recovered account state")
	}
	if state.Blocked || len(state.Rules) != 1 || state.Rules[0].CurrentUsage != 0 {
		t.Fatalf("recovered state = %#v, want unblocked zero usage", state)
	}
	if got := evaluator.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex:apikey:abc123", AccountKey: "acct_00000000-0000-4000-8000-000000000001"}}); len(got) != 0 {
		t.Fatalf("deny ids = %#v, recovered account should not deny", got)
	}
}

func TestRateLimitEvaluatorEvaluateAccountNowPreservesOtherAccountBlocks(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit) })

	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	firstAccount := "acct_00000000-0000-4000-8000-000000000001"
	secondAccount := "acct_00000000-0000-4000-8000-000000000002"
	base := time.Now().UTC().Truncate(time.Hour)
	current := base.Add(30 * time.Minute)
	for _, accountKey := range []string{firstAccount, secondAccount} {
		if err := store.upsertRule(RateLimitRule{
			ID:         "rule-" + accountKey,
			AccountKey: accountKey,
			Strategy:   RateLimitStrategyRequestWindow,
			Window:     "1h",
			LimitValue: 1,
			Action:     RateLimitActionBlock,
			Enabled:    true,
		}, base); err != nil {
			t.Fatalf("upsert rule for %s: %v", accountKey, err)
		}
		if err := store.insertUsageAttributionEvent(usageAttributionEvent{
			ID:                "event-" + accountKey,
			CompletedAtUnixMs: base.Add(time.Minute).UnixMilli(),
			AttributionKey:    "auth-id:" + accountKey,
			AttributionKind:   "auth_id",
			AccountKey:        accountKey,
			Provider:          "codex",
			RequestedModel:    "gpt-5.4",
			TotalTokens:       25,
			EvidenceKind:      "auth_id",
		}); err != nil {
			t.Fatalf("insert event for %s: %v", accountKey, err)
		}
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return current },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate initial blocked windows: %v", err)
	}
	if got := DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{
		{ID: "auth-first", AccountKey: firstAccount},
		{ID: "auth-second", AccountKey: secondAccount},
	}); len(got) != 2 {
		t.Fatalf("initial route guard deny ids = %#v, want both accounts blocked", got)
	}

	current = base.Add(2 * time.Hour)
	if err := evaluator.EvaluateAccountNow(context.Background(), firstAccount); err != nil {
		t.Fatalf("evaluate first account recovery: %v", err)
	}
	firstState, ok := evaluator.StateForAccount(firstAccount)
	if !ok || firstState.Blocked {
		t.Fatalf("first state = %#v, ok=%v, want recovered target account", firstState, ok)
	}
	secondState, ok := evaluator.StateForAccount(secondAccount)
	if !ok || !secondState.Blocked {
		t.Fatalf("second state = %#v, ok=%v, want untouched blocked account", secondState, ok)
	}
	if got := DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{
		{ID: "auth-first", AccountKey: firstAccount},
		{ID: "auth-second", AccountKey: secondAccount},
	}); len(got) != 1 || got[0] != "auth-second" {
		t.Fatalf("route guard deny ids after target refresh = %#v, want only second account", got)
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
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
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
		AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
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
	if _, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001"); ok {
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

func TestRateLimitEvaluatorFeedsAccountRouteGuardPolicy(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit) })

	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{})
	evaluator.replaceStatesForTest([]RateLimitState{{
		AccountKey:  "openai-compatible:MI",
		Blocked:     true,
		BlockReason: "24h tokens 已满",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}})

	decision := accountRouteGuardPolicy{}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "openai-compatibility:mi:abc123", Value: &coreauth.Auth{ID: "openai-compatibility:mi:abc123", AccountKey: "openai-compatible:MI", Provider: "mi"}},
		},
	})
	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != "openai-compatibility:mi:abc123" {
		t.Fatalf("DenyIDs = %#v, want blocked candidate", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, "gettokens account route guard") || !strings.Contains(decision.Reason, AccountRouteGuardSourceRateLimit) {
		t.Fatalf("Reason = %q, want account route guard source", decision.Reason)
	}
}

func TestRateLimitEvaluatorUsesRegisteredStrategy(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	registry := NewRateLimitStrategyRegistry(rateLimitTestStrategy{})
	now := time.Now().UTC().Truncate(time.Hour)
	if _, err := store.upsertRuleWithRegistry(RateLimitRule{
		ID:         "rule-custom",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Strategy:   "test-window",
		Window:     "1h",
		LimitValue: 5,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now, registry); err != nil {
		t.Fatalf("upsert custom strategy rule: %v", err)
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now:      func() time.Time { return now },
		Registry: registry,
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate custom strategy: %v", err)
	}

	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001")
	if !ok {
		t.Fatal("missing account state")
	}
	if !state.Blocked || state.BlockReason != "1h test 已满" {
		t.Fatalf("blocked state = %#v, want registered strategy block", state)
	}

	strategies := registry.List()
	if len(strategies) != 1 || strategies[0].ID != "test-window" {
		t.Fatalf("strategies = %#v, want registered test strategy", strategies)
	}
}

func TestRateLimitEvaluatorSerializesConcurrentEvaluations(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	strategy := &rateLimitBlockingStrategy{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	registry := NewRateLimitStrategyRegistry(strategy)
	accountKey := "acct_00000000-0000-4000-8000-000000000001"
	now := time.Now().UTC().Truncate(time.Hour)
	if _, err := store.upsertRuleWithRegistry(RateLimitRule{
		ID:         "rule-blocking",
		AccountKey: accountKey,
		Strategy:   strategy.ID(),
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now, registry); err != nil {
		t.Fatalf("upsert blocking rule: %v", err)
	}
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now:      func() time.Time { return now },
		Registry: registry,
	})

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- evaluator.EvaluateNow(context.Background())
	}()
	select {
	case <-strategy.started:
	case <-time.After(time.Second):
		t.Fatal("first evaluation did not enter strategy")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- evaluator.EvaluateAccountNow(context.Background(), accountKey)
	}()
	select {
	case <-strategy.started:
		t.Fatal("second evaluation entered strategy before first evaluation finished")
	case <-time.After(25 * time.Millisecond):
	}

	close(strategy.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first evaluation: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second evaluation: %v", err)
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
		"account_key":"acct_00000000-0000-4000-8000-000000000001",
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
			AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
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

	status := performRateLimitRequest(t, router, http.MethodGet, "/v0/management/gettokens/rate-limit-status?account_key=acct_00000000-0000-4000-8000-000000000001", "")
	var state RateLimitState
	if err := json.Unmarshal(status.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !state.Blocked || state.BlockReason != "1h requests 已满" {
		t.Fatalf("state = %#v, want blocked request window", state)
	}
	if state.LastEvaluatedAt == "" || state.UpdatedAt != state.LastEvaluatedAt {
		t.Fatalf("state timestamps = updated %q last %q, want last evaluated mirrored", state.UpdatedAt, state.LastEvaluatedAt)
	}
	if len(state.Sources) != 1 {
		t.Fatalf("state sources = %#v, want one rate-limit source", state.Sources)
	}
	if state.Sources[0].Source != AccountRouteGuardSourceRateLimit || state.Sources[0].RuleID != ruleID || state.Sources[0].UsageValue != 2 || state.Sources[0].LimitValue != 2 {
		t.Fatalf("state source = %#v, want rate-limit rule usage explain", state.Sources[0])
	}
	if state.NextReset == "" || state.Sources[0].NextReset == "" {
		t.Fatalf("state next reset missing: %#v", state)
	}
	if len(state.Rules) != 1 || state.Rules[0].WindowStart == "" || state.Rules[0].WindowEnd == "" || state.Rules[0].NextReset == "" || state.Rules[0].LimitValue != 2 {
		t.Fatalf("rule state missing window explain: %#v", state.Rules)
	}

	events := performRateLimitRequest(t, router, http.MethodGet, "/v0/management/gettokens/rate-limit-events?account_key=acct_00000000-0000-4000-8000-000000000001", "")
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
	listResponse := performRateLimitRequest(t, router, http.MethodGet, "/v0/management/gettokens/rate-limit-rules?account_key=acct_00000000-0000-4000-8000-000000000001", "")
	if err := json.Unmarshal(listResponse.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode listed rules: %v", err)
	}
	if len(rulesResponse.Items) != 0 {
		t.Fatalf("rules after delete = %#v, want empty", rulesResponse.Items)
	}
}

func TestRateLimitManagementRoutesReturnEvaluationError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	accountKey := "acct_00000000-0000-4000-8000-000000000004"
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Registry: NewRateLimitStrategyRegistry(rateLimitFailingStrategy{}),
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

	request := httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/rate-limit-rules", strings.NewReader(`{
		"account_key":"`+accountKey+`",
		"strategy":"failing-window",
		"window":"1h",
		"limit_value":1,
		"action":"block",
		"enabled":true
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want evaluation error", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "forced rate limit evaluation failure") {
		t.Fatalf("body = %s, want evaluation error detail", response.Body.String())
	}
	rules, err := store.listRules(accountKey)
	if err != nil {
		t.Fatalf("list rules after failed create: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("rules after failed create = %#v, want rollback", rules)
	}
}

func TestRateLimitManagementRoutesRollbackFailedDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	registry := NewRateLimitStrategyRegistry(rateLimitRequestWindowStrategy{}, rateLimitFailingStrategy{})
	accountKey := "acct_00000000-0000-4000-8000-000000000005"
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := store.upsertRuleWithRegistry(RateLimitRule{
		ID:         "delete-target",
		AccountKey: accountKey,
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 10,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now, registry); err != nil {
		t.Fatalf("upsert delete target: %v", err)
	}
	if _, err := store.upsertRuleWithRegistry(RateLimitRule{
		ID:         "remaining-failing",
		AccountKey: accountKey,
		Strategy:   "failing-window",
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now, registry); err != nil {
		t.Fatalf("upsert failing rule: %v", err)
	}
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{Registry: registry})
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

	request := httptest.NewRequest(http.MethodDelete, "/v0/management/gettokens/rate-limit-rules/delete-target", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want evaluation error", response.Code, response.Body.String())
	}
	if _, exists, err := store.getRule("delete-target"); err != nil {
		t.Fatalf("get delete target after failed delete: %v", err)
	} else if !exists {
		t.Fatal("delete target missing after failed evaluation, want rollback")
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

type rateLimitFailingStrategy struct{}

func (rateLimitFailingStrategy) ID() string { return "failing-window" }

func (rateLimitFailingStrategy) Name() string { return "失败窗口限流" }

func (rateLimitFailingStrategy) SupportedWindows() []string { return []string{"1h"} }

func (rateLimitFailingStrategy) UsageForRule(context.Context, *rateLimitStore, RateLimitRule, time.Time) (int64, error) {
	return 0, errors.New("forced rate limit evaluation failure")
}

func (rateLimitFailingStrategy) FormatReason(rule RateLimitRule) string {
	return rateLimitRuleWindow(rule) + " failing 已满"
}

type rateLimitBlockingStrategy struct {
	started chan struct{}
	release chan struct{}
}

func (rateLimitBlockingStrategy) ID() string { return "blocking-window" }

func (rateLimitBlockingStrategy) Name() string { return "阻塞窗口限流" }

func (rateLimitBlockingStrategy) SupportedWindows() []string { return []string{"1h"} }

func (s *rateLimitBlockingStrategy) UsageForRule(context.Context, *rateLimitStore, RateLimitRule, time.Time) (int64, error) {
	s.started <- struct{}{}
	<-s.release
	return 1, nil
}

func (rateLimitBlockingStrategy) FormatReason(rule RateLimitRule) string {
	return rateLimitRuleWindow(rule) + " blocking 已满"
}
