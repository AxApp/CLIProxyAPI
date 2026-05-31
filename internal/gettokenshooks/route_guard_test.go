package gettokenshooks

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountRouteGuardManualDisabledDeniesCandidate(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{ID: "codex-auth-1", Provider: "codex"}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     auth.ID,
		AccountKey: "auth-file:codex-auth.json",
		Reason:     "disabled by user",
	})

	decision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "codex-auth-1", Value: &coreauth.Auth{ID: "codex-auth-1", Provider: "codex"}},
			{ID: "codex-auth-2", Value: &coreauth.Auth{ID: "codex-auth-2", Provider: "codex"}},
		},
	})

	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != "codex-auth-1" {
		t.Fatalf("DenyIDs = %#v, want disabled auth only", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, "gettokens account route guard") || !strings.Contains(decision.Reason, AccountRouteGuardSourceManualDisabled) {
		t.Fatalf("Reason = %q", decision.Reason)
	}
	if !store.IsAuthBlocked(auth) {
		t.Fatalf("IsAuthBlocked() = false, want true")
	}
}

func TestAccountRouteGuardSourcesDoNotUnblockEachOther(t *testing.T) {
	store := NewAccountRouteGuardStore()
	authID := "codex-auth-1"
	store.MarkBlocked(AccountRouteGuardBlock{
		Source: AccountRouteGuardSourceManualDisabled,
		AuthID: authID,
		Reason: "disabled by user",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:    AccountRouteGuardSourceRateLimit,
		AuthID:    authID,
		Reason:    "24h tokens full",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	store.ClearAuth(AccountRouteGuardSourceRateLimit, authID)
	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID}}); len(got) != 1 || got[0] != authID {
		t.Fatalf("DenyIDs after clearing rate-limit = %#v, want manual block to remain", got)
	}

	store.ClearAuth(AccountRouteGuardSourceManualDisabled, authID)
	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID}}); len(got) != 0 {
		t.Fatalf("DenyIDs after clearing manual-disabled = %#v, want empty", got)
	}
}

func TestAccountRouteGuardActiveBlocksForAuthReturnsSourceDetails(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{
		ID:         "codex-auth-1",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Provider:   "codex",
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AccountKey: auth.AccountKey,
		Reason:     "1h requests 已满",
		ExpiresAt:  expiresAt,
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:    AccountRouteGuardSourceManualDisabled,
		AuthID:    auth.ID,
		Reason:    "disabled by user",
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
	})

	blocks := store.ActiveBlocksForAuth(auth)
	if len(blocks) != 1 {
		t.Fatalf("active blocks = %#v, want only unexpired rate-limit block", blocks)
	}
	if blocks[0].Source != AccountRouteGuardSourceRateLimit || blocks[0].Reason != "1h requests 已满" {
		t.Fatalf("active block = %#v, want rate-limit source details", blocks[0])
	}
	if !blocks[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", blocks[0].ExpiresAt, expiresAt)
	}
}

func TestAccountRouteGuardPolicyReasonIncludesActiveSources(t *testing.T) {
	store := NewAccountRouteGuardStore()
	authA := &coreauth.Auth{ID: "auth-a", AccountKey: "acct_00000000-0000-4000-8000-000000000001", Provider: "codex"}
	authB := &coreauth.Auth{ID: "auth-b", AccountKey: "acct_00000000-0000-4000-8000-000000000002", Provider: "codex"}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     authA.ID,
		AccountKey: authA.AccountKey,
		Reason:     "disabled by user",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AccountKey: authB.AccountKey,
		Reason:     "1h requests 已满",
	})

	decision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: authA.ID, Value: authA},
			{ID: authB.ID, Value: authB},
		},
	})

	if len(decision.DenyIDs) != 2 || decision.DenyIDs[0] != authA.ID || decision.DenyIDs[1] != authB.ID {
		t.Fatalf("DenyIDs = %#v, want both guarded auths", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, AccountRouteGuardSourceManualDisabled) || !strings.Contains(decision.Reason, AccountRouteGuardSourceRateLimit) {
		t.Fatalf("Reason = %q, want active guard sources", decision.Reason)
	}
}

func TestAccountRouteGuardResultHookBlocksAndClearsTransientFailure(t *testing.T) {
	store := NewAccountRouteGuardStore()
	hook := AccountRouteGuardResultHook{Store: store}
	retryAfter := 2 * time.Minute

	hook.OnResult(context.Background(), coreauth.Result{
		AuthID:     "codex-auth-1",
		Provider:   "codex",
		Model:      "gpt-5",
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota window exhausted"},
	})

	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex-auth-1", Provider: "codex"}}); len(got) != 1 || got[0] != "codex-auth-1" {
		t.Fatalf("DenyIDs after 429 = %#v, want transient block", got)
	}

	hook.OnResult(context.Background(), coreauth.Result{
		AuthID:   "codex-auth-1",
		Provider: "codex",
		Model:    "gpt-5",
		Success:  true,
	})

	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex-auth-1", Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("DenyIDs after success = %#v, want transient block cleared", got)
	}
}

func TestAccountRouteGuardResultHookDoesNotClearManualDisabled(t *testing.T) {
	store := NewAccountRouteGuardStore()
	hook := AccountRouteGuardResultHook{Store: store}
	authID := "codex-auth-1"
	store.MarkBlocked(AccountRouteGuardBlock{
		Source: AccountRouteGuardSourceManualDisabled,
		AuthID: authID,
		Reason: "disabled by user",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:    AccountRouteGuardSourceUpstreamTransientErr,
		AuthID:    authID,
		Reason:    "temporary upstream failure",
		ExpiresAt: time.Now().Add(time.Minute),
	})

	hook.OnResult(context.Background(), coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Success:  true,
	})

	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, Provider: "codex"}}); len(got) != 1 || got[0] != authID {
		t.Fatalf("DenyIDs after success = %#v, want manual-disabled block to remain", got)
	}
	store.ClearAuth(AccountRouteGuardSourceManualDisabled, authID)
	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("DenyIDs after clearing manual-disabled = %#v, want empty", got)
	}
}
