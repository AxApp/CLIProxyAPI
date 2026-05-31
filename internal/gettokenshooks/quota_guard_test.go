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
