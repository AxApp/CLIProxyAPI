package gettokenshooks

import (
	"context"
	"net/http"
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
	if decision.Reason != "gettokens account route guard" {
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
