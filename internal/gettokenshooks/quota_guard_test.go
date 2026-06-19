package gettokenshooks

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestQuotaGuardBuildsBlockFromFreshEmptyWindow(t *testing.T) {
	now := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)

	blocks := QuotaEmptyRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey:  "acct_00000000-0000-4000-8000-000000000001",
		AuthIDs:     []string{"codex-auth-1"},
		Source:      "quota-script",
		Fresh:       true,
		EvaluatedAt: now,
		Windows: []AccountQuotaWindowState{{
			Key:       "requests_5h",
			Kind:      "request",
			Remaining: 0,
			Limit:     100,
			ResetAt:   resetAt,
		}},
	}}, now)

	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want one quota-empty block", blocks)
	}
	block := blocks[0]
	if block.Source != AccountRouteGuardSourceQuotaEmpty {
		t.Fatalf("Source = %q, want %q", block.Source, AccountRouteGuardSourceQuotaEmpty)
	}
	if block.AuthID != "codex-auth-1" {
		t.Fatalf("AuthID = %q, want codex-auth-1", block.AuthID)
	}
	if block.AccountKey != "acct_00000000-0000-4000-8000-000000000001" {
		t.Fatalf("AccountKey = %q, want stable account key", block.AccountKey)
	}
	if !block.ExpiresAt.Equal(resetAt) {
		t.Fatalf("ExpiresAt = %v, want resetAt %v", block.ExpiresAt, resetAt)
	}
	if !strings.Contains(block.Reason, "requests_5h") || !strings.Contains(block.Reason, "quota empty") {
		t.Fatalf("Reason = %q, want exhausted window summary", block.Reason)
	}
}

func TestQuotaThresholdGuardBlocksFreshWindowBelowRemainingPercent(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)

	blocks := QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey:  "acct_00000000-0000-4000-8000-000000000101",
		AuthIDs:     []string{"auth-threshold-a"},
		Fresh:       true,
		EvaluatedAt: now,
		Windows: []AccountQuotaWindowState{{
			Key:       "tokens_5h",
			Kind:      "tokens",
			Remaining: 18,
			Limit:     100,
			ResetAt:   resetAt,
		}},
	}}, []AccountQuotaThresholdRule{{
		ID:               "stop-low-tokens",
		AccountKey:       "acct_00000000-0000-4000-8000-000000000101",
		WindowKey:        "tokens_5h",
		Metric:           "remaining-percent",
		Comparator:       "<=",
		ThresholdPercent: 20,
		Enabled:          true,
	}}, now)

	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want one quota-threshold block", blocks)
	}
	block := blocks[0]
	if block.Source != AccountRouteGuardSourceQuotaThreshold {
		t.Fatalf("Source = %q, want quota-threshold", block.Source)
	}
	if block.AuthID != "auth-threshold-a" || block.AccountKey != "acct_00000000-0000-4000-8000-000000000101" {
		t.Fatalf("block identity = %#v, want auth and account", block)
	}
	if !block.ExpiresAt.Equal(resetAt) {
		t.Fatalf("ExpiresAt = %v, want window reset %v", block.ExpiresAt, resetAt)
	}
	if !strings.Contains(block.Reason, "tokens_5h") || !strings.Contains(block.Reason, "18.00% <= 20.00%") {
		t.Fatalf("Reason = %q, want threshold trace", block.Reason)
	}
}

func TestQuotaThresholdGuardEvaluatesDSLConditionAST(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	blocks := QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey:  "acct_00000000-0000-4000-8000-000000000141",
		AuthIDs:     []string{"auth-threshold-dsl"},
		Fresh:       true,
		EvaluatedAt: now,
		Windows: []AccountQuotaWindowState{{
			Key:       "tokens_5h",
			Kind:      "tokens",
			Used:      82,
			Remaining: 18,
			Limit:     100,
			ResetAt:   resetAt,
		}},
	}}, []AccountQuotaThresholdRule{{
		ID:         "stop-low-tokens-dsl",
		AccountKey: "acct_00000000-0000-4000-8000-000000000141",
		Condition: &AccountQuotaRuleCondition{All: []AccountQuotaRuleCondition{
			{Fact: "quota.window", WindowKey: "tokens_5h", Metric: "remaining-percent", Comparator: "<=", Value: 20},
			{Fact: "quota.window", WindowKey: "tokens_5h", Metric: "used-percent", Comparator: ">=", Value: 80},
		}},
		Enabled: true,
	}}, now)

	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want one DSL quota-threshold block", blocks)
	}
	if !strings.Contains(blocks[0].Reason, "trace=all") || !strings.Contains(blocks[0].Reason, "18.00% <= 20.00%") {
		t.Fatalf("reason = %q, want DSL trace and threshold", blocks[0].Reason)
	}
}

func TestQuotaThresholdSimulatorMatchesRuntimeGuardForSameFacts(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	revokedAt := now.Add(-time.Minute)
	rule := AccountQuotaThresholdRule{
		ID:         "stop-low-tokens-dsl",
		AccountKey: "acct_simulator_match",
		Condition: &AccountQuotaRuleCondition{All: []AccountQuotaRuleCondition{
			{Fact: "quota.window", WindowKey: "tokens_5h", Metric: "remaining-percent", Comparator: "<=", Value: 20},
			{Fact: "quota.window", WindowKey: "tokens_5h", Metric: "used-percent", Comparator: ">=", Value: 80},
		}},
		Enabled: true,
	}
	facts := SimulationFacts{
		Now: now,
		Request: RouteRequestFacts{
			Channel: "api",
			Model:   "gpt-4.1",
			Project: "default",
		},
		Accounts: []AccountFacts{{
			AccountID: "acct_simulator_match",
			QuotaWindow: &QuotaWindowFacts{
				WindowID:          "tokens_5h",
				StartsAt:          now.Add(-3 * time.Hour),
				EndsAt:            resetAt,
				ObservedUsed:      52,
				ObservedLimit:     100,
				ObservedRemaining: 48,
				Status:            QuotaFactFresh,
			},
			CalibrationLedger: []CalibrationFact{
				{ID: "cal_active", WindowID: "tokens_5h", Metric: "tokens", Mode: "delta", Value: 34, CreatedAt: now.Add(-time.Minute), ExpiresAt: resetAt},
				{ID: "cal_revoked", WindowID: "tokens_5h", Metric: "tokens", Mode: "delta", Value: 30, CreatedAt: now.Add(-time.Hour), ExpiresAt: resetAt, RevokedAt: &revokedAt},
				{ID: "cal_expired", WindowID: "tokens_5h", Metric: "tokens", Mode: "delta", Value: 30, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Minute)},
			},
		}},
	}

	simulation := SimulateQuotaThresholdRules([]AccountQuotaThresholdRule{rule}, facts)
	if simulation.Summary.BlockedAccounts != 1 || len(simulation.Accounts) != 1 {
		t.Fatalf("simulation = %#v, want one blocked account", simulation)
	}
	decision := simulation.Accounts[0]
	if !decision.Decision.Denied || decision.Decision.Action != "block" || decision.Decision.DenySource != AccountRouteGuardSourceQuotaThreshold {
		t.Fatalf("decision = %#v, want quota-threshold block", decision.Decision)
	}
	if decision.RecoveryAt == nil || !decision.RecoveryAt.Equal(resetAt) {
		t.Fatalf("recoveryAt = %v, want %v", decision.RecoveryAt, resetAt)
	}
	if !reasonTraceContains(decision.ReasonTrace, "quota.calibration.applied") || !reasonTraceContains(decision.ReasonTrace, "quota.calibration.ignored") || !reasonTraceContains(decision.ReasonTrace, "routeguard.action.block") {
		t.Fatalf("reasonTrace = %#v, want calibration and block trace", decision.ReasonTrace)
	}

	state, _ := accountQuotaRuntimeStateFromSimulationFacts(facts.Accounts[0], now)
	state.AuthIDs = []string{"auth-sim-runtime"}
	runtimeBlocks := QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{state}, []AccountQuotaThresholdRule{rule}, now)
	if len(runtimeBlocks) != 1 {
		t.Fatalf("runtimeBlocks = %#v, want one block", runtimeBlocks)
	}
	if runtimeBlocks[0].Source != decision.Decision.DenySource || runtimeBlocks[0].Reason != decision.Decision.Reason || !runtimeBlocks[0].ExpiresAt.Equal(*decision.ExpiresAt) {
		t.Fatalf("runtime block = %#v, simulation decision = %#v expires=%v", runtimeBlocks[0], decision.Decision, decision.ExpiresAt)
	}
}

func TestQuotaThresholdSimulatorSupportsMultipleBudgetWindows(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	dayReset := now.Add(14 * time.Hour)
	multiDayReset := now.Add(48 * time.Hour)
	boundedReset := now.Add(2 * time.Hour)
	facts := SimulationFacts{
		Now: now,
		Accounts: []AccountFacts{{
			AccountID: "acct_budget_windows",
			QuotaWindows: []QuotaWindowFacts{
				{WindowID: "tokens_1d", Kind: "daily", StartsAt: now.Add(-10 * time.Hour), EndsAt: dayReset, ObservedUsed: 91, ObservedLimit: 100, ObservedRemaining: 9, Status: QuotaFactFresh},
				{WindowID: "tokens_7d", Kind: "multi-day", StartsAt: now.Add(-5 * 24 * time.Hour), EndsAt: multiDayReset, ObservedUsed: 60, ObservedLimit: 100, ObservedRemaining: 40, Status: QuotaFactFresh},
				{WindowID: "tokens_campaign", Kind: "bounded", StartsAt: now.Add(-time.Hour), EndsAt: boundedReset, ObservedUsed: 86, ObservedLimit: 100, ObservedRemaining: 14, Status: QuotaFactFresh},
			},
		}},
	}
	cases := []struct {
		name       string
		rule       AccountQuotaThresholdRule
		wantWindow string
		wantReset  time.Time
	}{
		{
			name: "daily",
			rule: AccountQuotaThresholdRule{
				ID:               "daily-stop",
				AccountKey:       "acct_budget_windows",
				WindowKey:        "tokens_1d",
				Metric:           "remaining-percent",
				ThresholdPercent: 10,
				Enabled:          true,
			},
			wantWindow: "tokens_1d",
			wantReset:  dayReset,
		},
		{
			name: "multi-day",
			rule: AccountQuotaThresholdRule{
				ID:         "multi-day-used-stop",
				AccountKey: "acct_budget_windows",
				Condition: &AccountQuotaRuleCondition{
					Fact:       "quota.window",
					WindowKey:  "tokens_7d",
					Metric:     "used-percent",
					Comparator: ">=",
					Value:      60,
				},
				Enabled: true,
			},
			wantWindow: "tokens_7d",
			wantReset:  multiDayReset,
		},
		{
			name: "bounded",
			rule: AccountQuotaThresholdRule{
				ID:         "bounded-stop",
				AccountKey: "acct_budget_windows",
				Condition: &AccountQuotaRuleCondition{All: []AccountQuotaRuleCondition{
					{Fact: "quota.window", WindowKey: "tokens_campaign", Metric: "remaining-percent", Comparator: "<=", Value: 15},
					{Fact: "quota.window", WindowKey: "tokens_campaign", Metric: "used-percent", Comparator: ">=", Value: 80},
				}},
				Enabled: true,
			},
			wantWindow: "tokens_campaign",
			wantReset:  boundedReset,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			simulation := SimulateQuotaThresholdRules([]AccountQuotaThresholdRule{tt.rule}, facts)
			if simulation.Summary.BlockedAccounts != 1 || len(simulation.Accounts) != 1 {
				t.Fatalf("simulation = %#v, want one blocked account", simulation)
			}
			decision := simulation.Accounts[0]
			if decision.ExpiresAt == nil || !decision.ExpiresAt.Equal(tt.wantReset) {
				t.Fatalf("expiresAt = %v, want %v", decision.ExpiresAt, tt.wantReset)
			}
			if !strings.Contains(decision.Decision.Reason, tt.wantWindow) {
				t.Fatalf("reason = %q, want window %s", decision.Decision.Reason, tt.wantWindow)
			}
			state, _ := accountQuotaRuntimeStateFromSimulationFacts(facts.Accounts[0], now)
			runtimeBlocks := QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{state}, []AccountQuotaThresholdRule{tt.rule}, now)
			if len(runtimeBlocks) != 1 {
				t.Fatalf("runtimeBlocks = %#v, want one block", runtimeBlocks)
			}
			if runtimeBlocks[0].Reason != decision.Decision.Reason || !runtimeBlocks[0].ExpiresAt.Equal(*decision.ExpiresAt) {
				t.Fatalf("runtime block = %#v, simulation decision = %#v expires=%v", runtimeBlocks[0], decision.Decision, decision.ExpiresAt)
			}
		})
	}
}

func TestQuotaThresholdSimulatorUsesDiagnosticForStaleDegradedOrMissingFacts(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	rule := AccountQuotaThresholdRule{
		ID:               "stop-low-tokens",
		AccountKey:       "acct_stale",
		WindowKey:        "tokens_5h",
		Metric:           "remaining-percent",
		ThresholdPercent: 20,
		Enabled:          true,
	}
	simulation := SimulateQuotaThresholdRules([]AccountQuotaThresholdRule{rule}, SimulationFacts{
		Now: now,
		Accounts: []AccountFacts{
			{
				AccountID: "acct_stale",
				QuotaWindow: &QuotaWindowFacts{
					WindowID:          "tokens_5h",
					EndsAt:            resetAt,
					ObservedUsed:      82,
					ObservedLimit:     100,
					ObservedRemaining: 18,
					Status:            QuotaFactStale,
				},
			},
			{AccountID: "acct_missing"},
		},
	})
	if simulation.Summary.BlockedAccounts != 0 || simulation.Summary.DiagnosticAccounts != 1 || simulation.Summary.AllowedAccounts != 1 {
		t.Fatalf("summary = %#v, want stale diagnostic and missing unrelated account allowed", simulation.Summary)
	}
	if got := simulation.Accounts[0].Decision.Action; got != "allow" {
		t.Fatalf("first sorted account action = %q, want allow for acct_missing without targeted rule", got)
	}
	if got := simulation.Accounts[1].Decision.Action; got != "diagnostic" {
		t.Fatalf("stale account action = %q, want diagnostic", got)
	}
	if !reasonTraceContains(simulation.Accounts[1].ReasonTrace, "routeguard.action.diagnostic") {
		t.Fatalf("stale reasonTrace = %#v, want diagnostic action trace", simulation.Accounts[1].ReasonTrace)
	}

	missingRule := rule
	missingRule.AccountKey = "acct_missing"
	missingSimulation := SimulateQuotaThresholdRules([]AccountQuotaThresholdRule{missingRule}, SimulationFacts{
		Now:      now,
		Accounts: []AccountFacts{{AccountID: "acct_missing"}},
	})
	if missingSimulation.Summary.DiagnosticAccounts != 1 || missingSimulation.Accounts[0].Decision.Action != "diagnostic" {
		t.Fatalf("missing simulation = %#v, want missing fact diagnostic", missingSimulation)
	}
	if !reasonTraceContains(missingSimulation.Accounts[0].ReasonTrace, "quota.window.missing") {
		t.Fatalf("missing trace = %#v, want quota.window.missing", missingSimulation.Accounts[0].ReasonTrace)
	}
}

func TestQuotaThresholdGuardDoesNotBlockStaleOrRecoveredWindow(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	rules := []AccountQuotaThresholdRule{{
		ID:               "stop-low-tokens",
		AccountKey:       "acct_threshold",
		WindowKey:        "tokens_5h",
		Metric:           "remaining-percent",
		Comparator:       "<=",
		ThresholdPercent: 20,
		Enabled:          true,
	}}
	states := []AccountQuotaRuntimeState{
		{
			AccountKey: "acct_threshold",
			Stale:      true,
			Fresh:      false,
			Windows:    []AccountQuotaWindowState{{Key: "tokens_5h", Remaining: 10, Limit: 100, ResetAt: resetAt}},
		},
		{
			AccountKey: "acct_threshold",
			Fresh:      true,
			Windows:    []AccountQuotaWindowState{{Key: "tokens_5h", Remaining: 45, Limit: 100, ResetAt: resetAt}},
		},
		{
			AccountKey: "acct_threshold",
			Fresh:      true,
			Windows:    []AccountQuotaWindowState{{Key: "tokens_5h", Remaining: 10, Limit: 100}},
		},
	}

	if blocks := QuotaThresholdRouteGuardBlocks(states, rules, now); len(blocks) != 0 {
		t.Fatalf("blocks = %#v, want no threshold block for stale, recovered, or reset-uncertain quota", blocks)
	}
}

func reasonTraceContains(steps []ReasonTraceStep, code string) bool {
	for _, step := range steps {
		if step.Code == code {
			return true
		}
	}
	return false
}

func TestQuotaThresholdGuardUsesManualCalibrationDeltaEffectiveUsage(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	state := ApplyQuotaUsageCalibrations(AccountQuotaRuntimeState{
		AccountKey: "acct_00000000-0000-4000-8000-000000000131",
		AuthIDs:    []string{"auth-calibrated"},
		Fresh:      true,
		Windows: []AccountQuotaWindowState{{
			Key:       "tokens_5h",
			Kind:      "tokens",
			Remaining: 45,
			Limit:     100,
			ResetAt:   resetAt,
		}},
	}, []AccountQuotaUsageCalibration{{
		ID:         "cal_external_30",
		AccountKey: "acct_00000000-0000-4000-8000-000000000131",
		WindowKey:  "tokens_5h",
		Metric:     "tokens",
		Mode:       "delta",
		Value:      30,
		CreatedAt:  now.Add(-time.Minute),
		ExpiresAt:  resetAt,
	}}, now)

	blocks := QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{state}, []AccountQuotaThresholdRule{{
		ID:               "stop-low-tokens",
		AccountKey:       "acct_00000000-0000-4000-8000-000000000131",
		WindowKey:        "tokens_5h",
		Metric:           "remaining-percent",
		ThresholdPercent: 20,
		Enabled:          true,
	}}, now)

	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want calibration-adjusted quota-threshold block", blocks)
	}
	if !strings.Contains(blocks[0].Reason, "15.00% <= 20.00%") {
		t.Fatalf("Reason = %q, want effective remaining percent after calibration", blocks[0].Reason)
	}
}

func TestQuotaUsageCalibrationSetEffectiveDoesNotLowerObservedUsage(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	state := ApplyQuotaUsageCalibrations(AccountQuotaRuntimeState{
		AccountKey: "acct_00000000-0000-4000-8000-000000000132",
		Fresh:      true,
		Windows: []AccountQuotaWindowState{{
			Key:       "tokens_5h",
			Kind:      "tokens",
			Remaining: 45,
			Limit:     100,
			ResetAt:   resetAt,
		}},
	}, []AccountQuotaUsageCalibration{{
		ID:         "cal_set_too_low",
		AccountKey: "acct_00000000-0000-4000-8000-000000000132",
		WindowKey:  "tokens_5h",
		Metric:     "tokens",
		Mode:       "set-effective",
		Value:      30,
		CreatedAt:  now.Add(-time.Minute),
		ExpiresAt:  resetAt,
	}}, now)

	if got := state.Windows[0].Remaining; got != 45 {
		t.Fatalf("effective remaining = %v, want observed remaining preserved when set-effective is below observed usage", got)
	}
}

func TestQuotaUsageCalibrationIgnoresRevokedAndExpiredEntries(t *testing.T) {
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	revokedAt := now.Add(-time.Minute)
	state := ApplyQuotaUsageCalibrations(AccountQuotaRuntimeState{
		AccountKey: "acct_00000000-0000-4000-8000-000000000133",
		Fresh:      true,
		Windows: []AccountQuotaWindowState{{
			Key:       "tokens_5h",
			Kind:      "tokens",
			Remaining: 45,
			Limit:     100,
			ResetAt:   resetAt,
		}},
	}, []AccountQuotaUsageCalibration{
		{
			ID:         "cal_revoked",
			AccountKey: "acct_00000000-0000-4000-8000-000000000133",
			WindowKey:  "tokens_5h",
			Metric:     "tokens",
			Mode:       "delta",
			Value:      30,
			CreatedAt:  now.Add(-time.Hour),
			ExpiresAt:  resetAt,
			RevokedAt:  &revokedAt,
		},
		{
			ID:         "cal_expired",
			AccountKey: "acct_00000000-0000-4000-8000-000000000133",
			WindowKey:  "tokens_5h",
			Metric:     "tokens",
			Mode:       "delta",
			Value:      30,
			CreatedAt:  now.Add(-2 * time.Hour),
			ExpiresAt:  now.Add(-time.Minute),
		},
	}, now)

	if got := state.Windows[0].Remaining; got != 45 {
		t.Fatalf("effective remaining = %v, want revoked/expired calibrations ignored", got)
	}
}

func TestAccountRouteGuardPolicyDeniesQuotaThresholdCandidate(t *testing.T) {
	store := NewAccountRouteGuardStore()
	authA := &coreauth.Auth{ID: "auth-threshold-a", AccountKey: "acct_threshold_a", Provider: "codex"}
	authB := &coreauth.Auth{ID: "auth-threshold-b", AccountKey: "acct_threshold_b", Provider: "codex"}
	store.ReplaceSource(AccountRouteGuardSourceQuotaThreshold, QuotaThresholdRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey: authA.AccountKey,
		AuthIDs:    []string{authA.ID},
		Fresh:      true,
		Windows:    []AccountQuotaWindowState{{Key: "tokens_5h", Remaining: 18, Limit: 100, ResetAt: time.Now().UTC().Add(time.Hour)}},
	}}, []AccountQuotaThresholdRule{{
		ID:               "stop-low-tokens",
		AccountKey:       authA.AccountKey,
		WindowKey:        "tokens_5h",
		Metric:           "remaining-percent",
		Comparator:       "<=",
		ThresholdPercent: 20,
		Enabled:          true,
	}}, time.Now().UTC()))

	decision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: authA.ID, Value: authA},
			{ID: authB.ID, Value: authB},
		},
	})

	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != authA.ID {
		t.Fatalf("DenyIDs = %#v, want quota-threshold auth only", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, AccountRouteGuardSourceQuotaThreshold) {
		t.Fatalf("Reason = %q, want quota-threshold trace", decision.Reason)
	}
}

func TestQuotaGuardUsesLatestResetAtAcrossExhaustedWindows(t *testing.T) {
	now := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	early := now.Add(time.Hour)
	latest := now.Add(8 * time.Hour)

	blocks := QuotaEmptyRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey: "acct_00000000-0000-4000-8000-000000000002",
		Fresh:      true,
		Windows: []AccountQuotaWindowState{
			{Key: "requests_5h", Remaining: 0, ResetAt: early},
			{Key: "tokens_week", Remaining: -1, ResetAt: latest},
			{Key: "billing_balance", Remaining: 50, ResetAt: now.Add(24 * time.Hour)},
		},
	}}, now)

	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want one quota-empty block", blocks)
	}
	if !blocks[0].ExpiresAt.Equal(latest) {
		t.Fatalf("ExpiresAt = %v, want latest reset %v", blocks[0].ExpiresAt, latest)
	}
	if !strings.Contains(blocks[0].Reason, "requests_5h") || !strings.Contains(blocks[0].Reason, "tokens_week") {
		t.Fatalf("Reason = %q, want all exhausted windows", blocks[0].Reason)
	}
	if strings.Contains(blocks[0].Reason, "billing_balance") {
		t.Fatalf("Reason = %q, should not include non-exhausted window", blocks[0].Reason)
	}
}

func TestQuotaGuardDoesNotBlockStaleOrUncertainQuota(t *testing.T) {
	now := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)

	states := []AccountQuotaRuntimeState{
		{
			AccountKey: "acct_stale",
			Stale:      true,
			Fresh:      false,
			Windows:    []AccountQuotaWindowState{{Key: "requests_5h", Remaining: 0, ResetAt: resetAt}},
		},
		{
			AccountKey:     "acct_degraded",
			Fresh:          true,
			Degraded:       true,
			DegradedReason: "quota refresh failed",
			Windows:        []AccountQuotaWindowState{{Key: "requests_5h", Remaining: 0, ResetAt: resetAt}},
		},
		{
			AccountKey: "acct_missing_reset",
			Fresh:      true,
			Windows:    []AccountQuotaWindowState{{Key: "requests_5h", Remaining: 0}},
		},
		{
			AccountKey: "acct_expired_reset",
			Fresh:      true,
			Windows:    []AccountQuotaWindowState{{Key: "requests_5h", Remaining: 0, ResetAt: now.Add(-time.Minute)}},
		},
	}

	if blocks := QuotaEmptyRouteGuardBlocks(states, now); len(blocks) != 0 {
		t.Fatalf("blocks = %#v, want no hard block for stale/degraded/uncertain quota", blocks)
	}
}

func TestQuotaGuardBuildsBlockFromRuntimeAuthQuotaState(t *testing.T) {
	now := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(90 * time.Minute)

	blocks := QuotaEmptyRouteGuardBlocksFromAuths([]*coreauth.Auth{{
		ID:         "codex-auth-runtime",
		AccountKey: "acct_00000000-0000-4000-8000-000000000008",
		Provider:   "codex",
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: resetAt,
		},
	}}, now)

	if len(blocks) != 1 {
		t.Fatalf("blocks = %#v, want one runtime quota block", blocks)
	}
	if blocks[0].Source != AccountRouteGuardSourceQuotaEmpty {
		t.Fatalf("Source = %q, want quota-empty", blocks[0].Source)
	}
	if blocks[0].AuthID != "codex-auth-runtime" || blocks[0].AccountKey != "acct_00000000-0000-4000-8000-000000000008" {
		t.Fatalf("block identity = %#v, want auth id and account key", blocks[0])
	}
	if !blocks[0].ExpiresAt.Equal(resetAt) {
		t.Fatalf("ExpiresAt = %v, want %v", blocks[0].ExpiresAt, resetAt)
	}
}

func TestQuotaGuardSyncAuthClearsRecoveredQuotaOnly(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{
		ID:         "codex-auth-runtime",
		AccountKey: "acct_00000000-0000-4000-8000-000000000009",
		Provider:   "codex",
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: time.Now().UTC().Add(time.Hour),
		},
	}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "request window full",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})
	store.SyncQuotaEmptyAuth(auth, time.Now().UTC())

	auth.Quota = coreauth.QuotaState{}
	store.SyncQuotaEmptyAuth(auth, time.Now().UTC())

	blocks := store.ActiveBlocksForAuth(auth)
	if len(blocks) != 1 {
		t.Fatalf("active blocks = %#v, want only rate-limit after quota recovery", blocks)
	}
	if blocks[0].Source != AccountRouteGuardSourceRateLimit {
		t.Fatalf("remaining source = %q, want rate-limit", blocks[0].Source)
	}
}

func TestAccountRouteGuardResultHookWritesQuotaEmptyForUpstreamQuotaError(t *testing.T) {
	store := NewAccountRouteGuardStore()
	hook := AccountRouteGuardResultHook{Store: store}
	retryAfter := 90 * time.Minute
	before := time.Now().UTC()

	hook.OnResult(context.Background(), coreauth.Result{
		AuthID:     "codex-auth-quota-error",
		Provider:   "codex",
		Model:      "gpt-5",
		Success:    false,
		RetryAfter: &retryAfter,
		Error: &coreauth.Error{
			HTTPStatus: 429,
			Code:       "usage_limit_reached",
			Message:    "quota exhausted",
		},
	})

	blocks := store.ActiveBlocksForAuth(&coreauth.Auth{ID: "codex-auth-quota-error", Provider: "codex"})
	if len(blocks) != 1 {
		t.Fatalf("active blocks = %#v, want quota-empty block", blocks)
	}
	if blocks[0].Source != AccountRouteGuardSourceQuotaEmpty {
		t.Fatalf("Source = %q, want quota-empty", blocks[0].Source)
	}
	if !blocks[0].ExpiresAt.After(before.Add(retryAfter - time.Minute)) {
		t.Fatalf("ExpiresAt = %v, want retryAfter-derived reset", blocks[0].ExpiresAt)
	}
	if !strings.Contains(blocks[0].Reason, "upstream quota") {
		t.Fatalf("Reason = %q, want upstream quota summary", blocks[0].Reason)
	}
}

func TestQuotaGuardClearsOnlyQuotaEmptySourceOnRecovery(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{ID: "codex-auth-1", AccountKey: "acct_00000000-0000-4000-8000-000000000003", Provider: "codex"}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "disabled by user",
	})
	store.ReplaceSource(AccountRouteGuardSourceQuotaEmpty, QuotaEmptyRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey: auth.AccountKey,
		AuthIDs:    []string{auth.ID},
		Fresh:      true,
		Windows:    []AccountQuotaWindowState{{Key: "requests_5h", Remaining: 0, ResetAt: time.Now().UTC().Add(time.Hour)}},
	}}, time.Now().UTC()))

	store.ReplaceSource(AccountRouteGuardSourceQuotaEmpty, nil)

	blocks := store.ActiveBlocksForAuth(auth)
	if len(blocks) != 1 {
		t.Fatalf("active blocks = %#v, want only manual-disabled after quota recovery", blocks)
	}
	if blocks[0].Source != AccountRouteGuardSourceManualDisabled {
		t.Fatalf("remaining source = %q, want manual-disabled", blocks[0].Source)
	}
}

func TestAccountRouteGuardPolicyDeniesQuotaEmptyCandidate(t *testing.T) {
	store := NewAccountRouteGuardStore()
	authA := &coreauth.Auth{ID: "auth-a", AccountKey: "acct_00000000-0000-4000-8000-000000000004", Provider: "codex"}
	authB := &coreauth.Auth{ID: "auth-b", AccountKey: "acct_00000000-0000-4000-8000-000000000005", Provider: "codex"}
	store.ReplaceSource(AccountRouteGuardSourceQuotaEmpty, QuotaEmptyRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey: authA.AccountKey,
		AuthIDs:    []string{authA.ID},
		Fresh:      true,
		Windows:    []AccountQuotaWindowState{{Key: "requests_5h", Remaining: 0, ResetAt: time.Now().UTC().Add(time.Hour)}},
	}}, time.Now().UTC()))

	decision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: authA.ID, Value: authA},
			{ID: authB.ID, Value: authB},
		},
	})

	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != authA.ID {
		t.Fatalf("DenyIDs = %#v, want quota-empty auth only", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, AccountRouteGuardSourceQuotaEmpty) || !strings.Contains(decision.Reason, "requests_5h") {
		t.Fatalf("Reason = %q, want quota-empty source detail", decision.Reason)
	}
}

func TestQuotaGuardHardFilterRunsBeforeStickyOrder(t *testing.T) {
	store := NewAccountRouteGuardStore()
	authA := &coreauth.Auth{ID: "auth-a", AccountKey: "acct_00000000-0000-4000-8000-000000000006", Provider: "codex"}
	authB := &coreauth.Auth{ID: "auth-b", AccountKey: "acct_00000000-0000-4000-8000-000000000007", Provider: "codex"}
	store.ReplaceSource(AccountRouteGuardSourceQuotaEmpty, QuotaEmptyRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey: authA.AccountKey,
		AuthIDs:    []string{authA.ID},
		Fresh:      true,
		Windows:    []AccountQuotaWindowState{{Key: "requests_5h", Remaining: 0, ResetAt: time.Now().UTC().Add(time.Hour)}},
	}}, time.Now().UTC()))

	result := gettokensrouting.NewEngine(
		accountRouteGuardRoutingPolicy(store),
		gettokensrouting.Policy{
			Stage: gettokensrouting.PolicyStageSticky,
			Name:  "test-sticky",
			Rewrite: func(context.Context, gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
				return gettokensrouting.PolicyDecision{OrderIDs: []string{authA.ID}, Reason: "cached auth"}
			},
		},
	).Route(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: authA.ID, Value: authA},
			{ID: authB.ID, Value: authB},
		},
	})

	if len(result.Candidates) != 1 || result.Candidates[0].ID != authB.ID {
		t.Fatalf("candidates = %#v, want sticky unable to resurrect quota-empty auth", result.Candidates)
	}
	if len(result.Trace) < 2 || result.Trace[0].Stage != gettokensrouting.PolicyStageHardFilter || result.Trace[1].Stage != gettokensrouting.PolicyStageSticky {
		t.Fatalf("trace = %#v, want hard filter before sticky", result.Trace)
	}
}
