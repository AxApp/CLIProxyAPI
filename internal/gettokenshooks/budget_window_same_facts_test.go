package gettokenshooks

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBudgetWindowFactsDriveSimulatorAndRuntimeGuardWithSameDecision(t *testing.T) {
	store := newBudgetWindowFactsTestStore(t)
	accountKey := "acct_budget_same_facts"
	now := time.Date(2026, 6, 19, 16, 30, 0, 0, time.UTC)
	windowStart := time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 6, 20, 16, 0, 0, 0, time.UTC)
	insertBudgetWindowUsageEvent(t, store, "same-facts-raw", accountKey, windowStart.Add(10*time.Minute), 70, false)
	insertBudgetWindowUsageEvent(t, store, "same-facts-failed", accountKey, windowStart.Add(20*time.Minute), 100, true)

	windowFacts, err := BuildBudgetWindowFacts(context.Background(), store, accountKey, []BudgetWindowDefinition{{
		ID:       "tokens_daily",
		Kind:     BudgetWindowKindDaily,
		Metric:   BudgetWindowMetricTokens,
		Limit:    100,
		Timezone: "Asia/Shanghai",
		Enabled:  true,
	}}, BudgetWindowFactsOptions{
		Now: now,
		Calibrations: []AccountQuotaUsageCalibration{{
			ID:         "external-delta-15",
			AccountKey: accountKey,
			WindowKey:  "tokens_daily",
			Metric:     BudgetWindowMetricTokens,
			Mode:       "delta",
			Value:      15,
			CreatedAt:  windowStart.Add(30 * time.Minute),
		}},
	})
	if err != nil {
		t.Fatalf("BuildBudgetWindowFacts: %v", err)
	}
	if len(windowFacts) != 1 {
		t.Fatalf("windowFacts = %#v, want one fact", windowFacts)
	}
	fact := windowFacts[0]
	assertTimeEqual(t, fact.StartsAt, windowStart, "budget fact startsAt")
	assertTimeEqual(t, fact.EndsAt, windowEnd, "budget fact endsAt")
	if fact.RawUsed != 70 || fact.CalibrationDelta != 15 || fact.ObservedUsed != 85 || fact.ObservedRemaining != 15 {
		t.Fatalf("fact = %#v, want raw 70 delta 15 observed 85 remaining 15", fact)
	}

	rule := AccountQuotaThresholdRule{
		ID:               "stop-daily-low-tokens",
		AccountKey:       accountKey,
		WindowKey:        "tokens_daily",
		Metric:           "remaining-percent",
		Comparator:       "<=",
		ThresholdPercent: 20,
		Enabled:          true,
	}
	simulation := SimulateQuotaThresholdRules([]AccountQuotaThresholdRule{rule}, SimulationFacts{
		Now: now,
		Accounts: []AccountFacts{{
			AccountID:    accountKey,
			QuotaWindows: windowFacts,
		}},
	})
	if simulation.Summary.BlockedAccounts != 1 || len(simulation.Accounts) != 1 {
		t.Fatalf("simulation = %#v, want one blocked account", simulation)
	}
	decision := simulation.Accounts[0]
	if !decision.Decision.Denied || decision.Decision.Action != "block" || decision.Decision.DenySource != AccountRouteGuardSourceQuotaThreshold {
		t.Fatalf("decision = %#v, want quota-threshold block", decision.Decision)
	}
	if decision.ExpiresAt == nil || !decision.ExpiresAt.Equal(windowEnd) || decision.RecoveryAt == nil || !decision.RecoveryAt.Equal(windowEnd) {
		t.Fatalf("decision expiry/recovery = %v/%v, want %v", decision.ExpiresAt, decision.RecoveryAt, windowEnd)
	}
	if !strings.Contains(decision.Decision.Reason, "tokens_daily") || !strings.Contains(decision.Decision.Reason, "15.00% <= 20.00%") {
		t.Fatalf("reason = %q, want daily window threshold trace", decision.Decision.Reason)
	}

	runtimeState, trace := accountQuotaRuntimeStateFromQuotaWindowFacts(accountKey, windowFacts, now)
	if len(trace) > 0 {
		t.Fatalf("runtime conversion trace = %#v, want no missing-window diagnostics", trace)
	}
	runtimeState.AuthIDs = []string{"auth-budget-same-facts"}
	runtimeBlocks := QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{runtimeState}, []AccountQuotaThresholdRule{rule}, now)
	if len(runtimeBlocks) != 1 {
		t.Fatalf("runtimeBlocks = %#v, want one block from same facts", runtimeBlocks)
	}
	runtimeBlock := runtimeBlocks[0]
	if runtimeBlock.Source != decision.Decision.DenySource || runtimeBlock.Reason != decision.Decision.Reason || !runtimeBlock.ExpiresAt.Equal(*decision.ExpiresAt) {
		t.Fatalf("runtime block = %#v, simulation decision = %#v expires=%v", runtimeBlock, decision.Decision, decision.ExpiresAt)
	}
}

func TestBudgetWindowFactsDoNotBlockSimulatorOrRuntimeWhenThresholdNotMet(t *testing.T) {
	store := newBudgetWindowFactsTestStore(t)
	accountKey := "acct_budget_same_facts_allow"
	now := time.Date(2026, 6, 19, 16, 30, 0, 0, time.UTC)
	windowStart := time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC)
	insertBudgetWindowUsageEvent(t, store, "same-facts-allow", accountKey, windowStart.Add(10*time.Minute), 60, false)

	windowFacts, err := BuildBudgetWindowFacts(context.Background(), store, accountKey, []BudgetWindowDefinition{{
		ID:       "tokens_daily",
		Kind:     BudgetWindowKindDaily,
		Metric:   BudgetWindowMetricTokens,
		Limit:    100,
		Timezone: "Asia/Shanghai",
		Enabled:  true,
	}}, BudgetWindowFactsOptions{Now: now})
	if err != nil {
		t.Fatalf("BuildBudgetWindowFacts: %v", err)
	}

	rule := AccountQuotaThresholdRule{
		ID:               "stop-daily-low-tokens",
		AccountKey:       accountKey,
		WindowKey:        "tokens_daily",
		Metric:           "remaining-percent",
		Comparator:       "<=",
		ThresholdPercent: 20,
		Enabled:          true,
	}
	simulation := SimulateQuotaThresholdRules([]AccountQuotaThresholdRule{rule}, SimulationFacts{
		Now: now,
		Accounts: []AccountFacts{{
			AccountID:    accountKey,
			QuotaWindows: windowFacts,
		}},
	})
	if simulation.Summary.BlockedAccounts != 0 || len(simulation.Accounts) != 1 || simulation.Accounts[0].Decision.Action != "allow" {
		t.Fatalf("simulation = %#v, want allow", simulation)
	}
	runtimeState, trace := accountQuotaRuntimeStateFromQuotaWindowFacts(accountKey, windowFacts, now)
	if len(trace) > 0 {
		t.Fatalf("runtime conversion trace = %#v, want no diagnostics", trace)
	}
	runtimeBlocks := QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{runtimeState}, []AccountQuotaThresholdRule{rule}, now)
	if len(runtimeBlocks) != 0 {
		t.Fatalf("runtimeBlocks = %#v, want no block", runtimeBlocks)
	}
}
