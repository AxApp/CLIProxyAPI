package gettokenshooks

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountRouteGuardManualDisabledDeniesCandidate(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{ID: "codex-auth-1", Provider: "codex"}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     auth.ID,
		AccountKey: "auth-file:codex-auth.json",
		MatchKey:   "auth-id:codex-auth-1",
		Reason:     "disabled by user",
	})

	decision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), coreauth.RoutePolicyRequest{
		Candidates: []*coreauth.Auth{
			{ID: "codex-auth-1", Provider: "codex"},
			{ID: "codex-auth-2", Provider: "codex"},
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
