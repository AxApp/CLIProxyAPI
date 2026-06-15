package gettokenshooks

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBuildChannelRoutingRuntimePoolDoesNotPanicWhenOnlyInMemoryRouteGuardsExist(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit) })

	auth := &coreauth.Auth{
		ID:         "auth-a",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Label:      "公司 1",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	}
	DefaultAccountRouteGuardStore().MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "cooldown",
	})

	candidates, filtered := buildChannelRoutingRuntimePool([]*coreauth.Auth{auth}, channelRoutingPolicyConfig{
		Channel:            "codex",
		RouteMode:          gettokensrouting.ChannelRouteModeSequential,
		OrderedAccountIDs:  []string{auth.AccountKey},
		ChannelGroupStates: map[string]gettokensrouting.ChannelGroupState{},
	}, ChannelRoutingExplainRequest{})

	if len(candidates) != 0 {
		t.Fatalf("candidates = %#v, want none when auth is blocked in memory only", candidates)
	}
	if len(filtered) != 1 {
		t.Fatalf("filtered = %#v, want one filtered account", filtered)
	}
	if filtered[0].ID != auth.AccountKey || filtered[0].Reason == "" {
		t.Fatalf("filtered[0] = %#v, want blocked account with reason", filtered[0])
	}
}
