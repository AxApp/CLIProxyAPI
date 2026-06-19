package gettokenshooks

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildBudgetWindowFactsDailyUsesExplicitTimezone(t *testing.T) {
	store := newBudgetWindowFactsTestStore(t)
	now := time.Date(2026, 6, 19, 16, 30, 0, 0, time.UTC)

	facts, err := BuildBudgetWindowFacts(context.Background(), store, "acct-1", []BudgetWindowDefinition{
		{
			ID:       "daily-shanghai",
			Kind:     BudgetWindowKindDaily,
			Metric:   BudgetWindowMetricTokens,
			Limit:    100,
			Timezone: "Asia/Shanghai",
			Enabled:  true,
		},
		{
			ID:       "daily-los-angeles",
			Kind:     BudgetWindowKindDaily,
			Metric:   BudgetWindowMetricTokens,
			Limit:    100,
			Timezone: "America/Los_Angeles",
			Enabled:  true,
		},
	}, BudgetWindowFactsOptions{Now: now})
	if err != nil {
		t.Fatalf("build facts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}

	shanghai := mustBudgetWindowFact(t, facts, "daily-shanghai")
	assertTimeEqual(t, shanghai.StartsAt, time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC), "shanghai startsAt")
	assertTimeEqual(t, shanghai.EndsAt, time.Date(2026, 6, 20, 16, 0, 0, 0, time.UTC), "shanghai endsAt")
	if shanghai.Timezone != "Asia/Shanghai" {
		t.Fatalf("expected Asia/Shanghai timezone, got %q", shanghai.Timezone)
	}

	losAngeles := mustBudgetWindowFact(t, facts, "daily-los-angeles")
	assertTimeEqual(t, losAngeles.StartsAt, time.Date(2026, 6, 19, 7, 0, 0, 0, time.UTC), "los angeles startsAt")
	assertTimeEqual(t, losAngeles.EndsAt, time.Date(2026, 6, 20, 7, 0, 0, 0, time.UTC), "los angeles endsAt")
	if losAngeles.Timezone != "America/Los_Angeles" {
		t.Fatalf("expected America/Los_Angeles timezone, got %q", losAngeles.Timezone)
	}
}

func TestBuildBudgetWindowFactsBoundedUsesHalfOpenCommittedOnlyUsage(t *testing.T) {
	store := newBudgetWindowFactsTestStore(t)
	accountKey := "acct-1"
	startsAt := time.Date(2026, 6, 19, 8, 0, 0, 0, time.UTC)
	endsAt := time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)
	insertBudgetWindowUsageEvent(t, store, "start", accountKey, startsAt, 10, false)
	insertBudgetWindowUsageEvent(t, store, "last-ms", accountKey, endsAt.Add(-time.Millisecond), 20, false)
	insertBudgetWindowUsageEvent(t, store, "end", accountKey, endsAt, 100, false)
	insertBudgetWindowUsageEvent(t, store, "before", accountKey, startsAt.Add(-time.Millisecond), 100, false)
	insertBudgetWindowUsageEvent(t, store, "failed", accountKey, startsAt.Add(10*time.Minute), 100, true)
	insertBudgetWindowUsageEvent(t, store, "other-account", "acct-2", startsAt.Add(20*time.Minute), 100, false)

	facts, err := BuildBudgetWindowFacts(context.Background(), store, accountKey, []BudgetWindowDefinition{
		{
			ID:       "bounded-tokens",
			Kind:     BudgetWindowKindBounded,
			Metric:   BudgetWindowMetricTokens,
			Limit:    100,
			StartsAt: startsAt,
			EndsAt:   endsAt,
			Enabled:  true,
		},
		{
			ID:       "bounded-requests",
			Kind:     BudgetWindowKindBounded,
			Metric:   BudgetWindowMetricRequests,
			Limit:    10,
			StartsAt: startsAt,
			EndsAt:   endsAt,
			Enabled:  true,
		},
	}, BudgetWindowFactsOptions{
		Now: endsAt.Add(30 * time.Minute),
		Calibrations: []AccountQuotaUsageCalibration{{
			ID:         "request-delta",
			AccountKey: accountKey,
			WindowKey:  "bounded-requests",
			Metric:     BudgetWindowMetricRequests,
			Mode:       "delta",
			Value:      1,
			CreatedAt:  startsAt.Add(30 * time.Minute),
		}},
	})
	if err != nil {
		t.Fatalf("build facts: %v", err)
	}

	tokenFact := mustBudgetWindowFact(t, facts, "bounded-tokens")
	if tokenFact.RawUsed != 30 {
		t.Fatalf("expected raw token usage 30, got %.2f", tokenFact.RawUsed)
	}
	if tokenFact.ObservedUsed != 30 || tokenFact.ObservedRemaining != 70 {
		t.Fatalf("expected observed 30 remaining 70, got observed %.2f remaining %.2f", tokenFact.ObservedUsed, tokenFact.ObservedRemaining)
	}
	if tokenFact.ObservedUsedPercent != 30 || tokenFact.ObservedRemainingPercent != 70 {
		t.Fatalf("expected 30/70 percent, got %.2f/%.2f", tokenFact.ObservedUsedPercent, tokenFact.ObservedRemainingPercent)
	}
	if tokenFact.Source != BudgetWindowFactSourceUsageAggregator || tokenFact.RecoverySource != BudgetWindowRecoverySourceWindowEnd {
		t.Fatalf("unexpected source/recovery source: %q/%q", tokenFact.Source, tokenFact.RecoverySource)
	}

	requestFact := mustBudgetWindowFact(t, facts, "bounded-requests")
	if requestFact.RawUsed != 2 {
		t.Fatalf("expected raw request usage 2, got %.2f", requestFact.RawUsed)
	}
	if requestFact.CalibrationDelta != 1 || requestFact.ObservedUsed != 3 {
		t.Fatalf("expected request calibration delta 1 observed 3, got delta %.2f observed %.2f", requestFact.CalibrationDelta, requestFact.ObservedUsed)
	}
}

func TestBuildBudgetWindowFactsMultiDayUsesCalendarDaysInTimezone(t *testing.T) {
	store := newBudgetWindowFactsTestStore(t)
	accountKey := "acct-1"
	now := time.Date(2026, 6, 19, 16, 30, 0, 0, time.UTC)
	expectedStart := time.Date(2026, 6, 17, 16, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2026, 6, 20, 16, 0, 0, 0, time.UTC)
	insertBudgetWindowUsageEvent(t, store, "at-start", accountKey, expectedStart, 10, false)
	insertBudgetWindowUsageEvent(t, store, "inside", accountKey, now, 15, false)
	insertBudgetWindowUsageEvent(t, store, "before", accountKey, expectedStart.Add(-time.Millisecond), 100, false)
	insertBudgetWindowUsageEvent(t, store, "at-end", accountKey, expectedEnd, 100, false)

	facts, err := BuildBudgetWindowFacts(context.Background(), store, accountKey, []BudgetWindowDefinition{{
		ID:        "three-calendar-days",
		Kind:      BudgetWindowKindMultiDay,
		Semantics: BudgetWindowSemanticsCalendar,
		Days:      3,
		Metric:    BudgetWindowMetricTokens,
		Limit:     100,
		Timezone:  "Asia/Shanghai",
		Enabled:   true,
	}}, BudgetWindowFactsOptions{Now: now})
	if err != nil {
		t.Fatalf("build facts: %v", err)
	}

	fact := mustBudgetWindowFact(t, facts, "three-calendar-days")
	assertTimeEqual(t, fact.StartsAt, expectedStart, "multi-day startsAt")
	assertTimeEqual(t, fact.EndsAt, expectedEnd, "multi-day endsAt")
	if fact.RawUsed != 25 {
		t.Fatalf("expected multi-day raw usage 25, got %.2f", fact.RawUsed)
	}
}

func TestBuildBudgetWindowFactsAppliesDeltaCalibrationWithinWindow(t *testing.T) {
	store := newBudgetWindowFactsTestStore(t)
	accountKey := "acct-1"
	startsAt := time.Date(2026, 6, 19, 8, 0, 0, 0, time.UTC)
	endsAt := time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC)
	now := endsAt.Add(30 * time.Minute)
	revokedAt := startsAt.Add(40 * time.Minute)
	insertBudgetWindowUsageEvent(t, store, "raw", accountKey, startsAt.Add(10*time.Minute), 80, false)

	facts, err := BuildBudgetWindowFacts(context.Background(), store, accountKey, []BudgetWindowDefinition{{
		ID:       "bounded-tokens",
		Kind:     BudgetWindowKindBounded,
		Metric:   BudgetWindowMetricTokens,
		Limit:    100,
		StartsAt: startsAt,
		EndsAt:   endsAt,
		Enabled:  true,
	}}, BudgetWindowFactsOptions{
		Now: now,
		Calibrations: []AccountQuotaUsageCalibration{
			{ID: "inside", AccountKey: accountKey, WindowKey: "bounded-tokens", Metric: BudgetWindowMetricTokens, Mode: "delta", Value: 10, CreatedAt: startsAt.Add(20 * time.Minute)},
			{ID: "outside", AccountKey: accountKey, WindowKey: "bounded-tokens", Metric: BudgetWindowMetricTokens, Mode: "delta", Value: 30, CreatedAt: endsAt},
			{ID: "revoked", AccountKey: accountKey, WindowKey: "bounded-tokens", Metric: BudgetWindowMetricTokens, Mode: "delta", Value: 5, CreatedAt: startsAt.Add(30 * time.Minute), RevokedAt: &revokedAt},
			{ID: "expired", AccountKey: accountKey, WindowKey: "bounded-tokens", Metric: BudgetWindowMetricTokens, Mode: "delta", Value: 5, CreatedAt: startsAt.Add(30 * time.Minute), ExpiresAt: startsAt.Add(50 * time.Minute)},
			{ID: "set-effective", AccountKey: accountKey, WindowKey: "bounded-tokens", Metric: BudgetWindowMetricTokens, Mode: "set-effective", Value: 1, CreatedAt: startsAt.Add(30 * time.Minute)},
			{ID: "other-account", AccountKey: "acct-2", WindowKey: "bounded-tokens", Metric: BudgetWindowMetricTokens, Mode: "delta", Value: 2, CreatedAt: startsAt.Add(30 * time.Minute)},
		},
	})
	if err != nil {
		t.Fatalf("build facts: %v", err)
	}

	fact := mustBudgetWindowFact(t, facts, "bounded-tokens")
	if fact.RawUsed != 80 {
		t.Fatalf("expected raw usage 80, got %.2f", fact.RawUsed)
	}
	if fact.CalibrationDelta != 10 {
		t.Fatalf("expected calibration delta 10, got %.2f", fact.CalibrationDelta)
	}
	if fact.CalibratedUsed != 90 || fact.ObservedUsed != 90 || fact.ObservedRemaining != 10 {
		t.Fatalf("expected calibrated/observed 90 remaining 10, got calibrated %.2f observed %.2f remaining %.2f", fact.CalibratedUsed, fact.ObservedUsed, fact.ObservedRemaining)
	}
	if fact.ObservedUsedPercent != 90 || fact.ObservedRemainingPercent != 10 {
		t.Fatalf("expected 90/10 percent, got %.2f/%.2f", fact.ObservedUsedPercent, fact.ObservedRemainingPercent)
	}
}

func newBudgetWindowFactsTestStore(t *testing.T) *rateLimitStore {
	t.Helper()
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	t.Cleanup(func() { _ = store.db.Close() })
	return store
}

func insertBudgetWindowUsageEvent(t *testing.T, store *rateLimitStore, id string, accountKey string, completedAt time.Time, totalTokens int64, failed bool) {
	t.Helper()
	if err := store.insertUsageAttributionEvent(usageAttributionEvent{
		ID:                id,
		StartedAtUnixMs:   completedAt.Add(-time.Second).UnixMilli(),
		CompletedAtUnixMs: completedAt.UnixMilli(),
		AccountKey:        accountKey,
		AttributionKey:    accountKey,
		AttributionKind:   "account",
		StatusCode:        200,
		Failed:            failed,
		TotalTokens:       totalTokens,
		EvidenceKind:      "test",
	}); err != nil {
		t.Fatalf("insert usage event %s: %v", id, err)
	}
}

func mustBudgetWindowFact(t *testing.T, facts []QuotaWindowFacts, id string) QuotaWindowFacts {
	t.Helper()
	for _, fact := range facts {
		if fact.WindowID == id {
			return fact
		}
	}
	t.Fatalf("missing budget window fact %q in %+v", id, facts)
	return QuotaWindowFacts{}
}

func assertTimeEqual(t *testing.T, actual time.Time, expected time.Time, label string) {
	t.Helper()
	if !actual.Equal(expected) {
		t.Fatalf("%s: expected %s, got %s", label, expected.Format(time.RFC3339Nano), actual.Format(time.RFC3339Nano))
	}
}
