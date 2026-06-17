package gettokenshooks

import (
	"testing"
	"time"

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

func TestChannelRoutingExplainFilteredIncludesStructuredRouteGuardDroppedReason(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceRateLimit) })

	auth := &coreauth.Auth{
		ID:         "auth-a",
		AccountKey: "acct_00000000-0000-4000-8000-000000000101",
		Label:      "公司 A",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	updatedAt := time.Now().UTC().Add(-time.Minute)
	DefaultAccountRouteGuardStore().MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Model:      "gpt-5",
		Reason:     "cooldown",
		ExpiresAt:  expiresAt,
		UpdatedAt:  updatedAt,
	})

	response := explainChannelRoutingRuntime([]*coreauth.Auth{auth}, channelRoutingPolicyConfig{
		Channel:            "codex",
		RouteMode:          gettokensrouting.ChannelRouteModeSequential,
		OrderedAccountIDs:  []string{auth.AccountKey},
		ChannelGroupStates: map[string]gettokensrouting.ChannelGroupState{},
	}, ChannelRoutingExplainRequest{RequestedModel: "gpt-5"}, nil)

	if len(response.Filtered) != 1 {
		t.Fatalf("filtered = %#v, want one filtered account", response.Filtered)
	}
	filtered := response.Filtered[0]
	if filtered.ID != auth.AccountKey || filtered.Reason != "account-unrequestable" {
		t.Fatalf("filtered = %#v, want account-unrequestable account", filtered)
	}
	if len(filtered.DroppedReasons) != 1 {
		t.Fatalf("droppedReasons = %#v, want one route guard reason", filtered.DroppedReasons)
	}
	dropped := filtered.DroppedReasons[0]
	if dropped.AccountID != auth.AccountKey || dropped.AuthID != auth.ID {
		t.Fatalf("dropped account/auth = %#v, want account/auth ids", dropped)
	}
	if dropped.Source != AccountRouteGuardSourceRateLimit || dropped.Scope != RouteResilienceScopeAccount || dropped.Reason != "cooldown" {
		t.Fatalf("dropped = %#v, want rate-limit/account/cooldown", dropped)
	}
	if dropped.Model != "gpt-5" || dropped.ExpiresAt == "" || dropped.UpdatedAt == "" || !dropped.RouteBlocking {
		t.Fatalf("dropped metadata = %#v, want model/times/routeBlocking", dropped)
	}
}

func TestChannelRoutingExplainModelScopedDroppedReasonDoesNotHideProviderCandidates(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceUpstreamTransientErr)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceUpstreamTransientErr) })

	blocked := &coreauth.Auth{
		ID:         "auth-model-blocked",
		AccountKey: "acct_00000000-0000-4000-8000-000000000201",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	}
	available := &coreauth.Auth{
		ID:         "auth-provider-still-available",
		AccountKey: "acct_00000000-0000-4000-8000-000000000202",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	}
	DefaultAccountRouteGuardStore().MarkBlocked(AccountRouteGuardBlock{
		Source:       AccountRouteGuardSourceUpstreamTransientErr,
		FailureScope: RouteResilienceScopeModel,
		AuthID:       blocked.ID,
		AccountKey:   blocked.AccountKey,
		Model:        "gpt-5",
		Reason:       "model capacity",
	})

	response := explainChannelRoutingRuntime([]*coreauth.Auth{blocked, available}, channelRoutingPolicyConfig{
		Channel:            "codex",
		RouteMode:          gettokensrouting.ChannelRouteModeSequential,
		OrderedAccountIDs:  []string{blocked.AccountKey, available.AccountKey},
		ChannelGroupStates: map[string]gettokensrouting.ChannelGroupState{},
	}, ChannelRoutingExplainRequest{RequestedModel: "gpt-5"}, nil)

	if len(response.Candidates) != 1 || response.Candidates[0].ID != available.AccountKey {
		t.Fatalf("candidates = %#v, want same-provider available account kept", response.Candidates)
	}
	if len(response.Filtered) != 1 || len(response.Filtered[0].DroppedReasons) != 1 {
		t.Fatalf("filtered = %#v, want one model-scoped dropped reason", response.Filtered)
	}
	dropped := response.Filtered[0].DroppedReasons[0]
	if dropped.Scope != RouteResilienceScopeModel || dropped.Source != AccountRouteGuardSourceUpstreamTransientErr || dropped.Reason != "model capacity" {
		t.Fatalf("dropped = %#v, want model-scoped upstream-error", dropped)
	}
}

func TestChannelRoutingExplainModelScopedDroppedReasonDoesNotBlockOtherModels(t *testing.T) {
	ClearAccountRouteGuardSource(AccountRouteGuardSourceUpstreamTransientErr)
	t.Cleanup(func() { ClearAccountRouteGuardSource(AccountRouteGuardSourceUpstreamTransientErr) })

	auth := &coreauth.Auth{
		ID:         "auth-model-specific",
		AccountKey: "acct_00000000-0000-4000-8000-000000000301",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	}
	DefaultAccountRouteGuardStore().MarkBlocked(AccountRouteGuardBlock{
		Source:       AccountRouteGuardSourceUpstreamTransientErr,
		FailureScope: RouteResilienceScopeModel,
		AuthID:       auth.ID,
		AccountKey:   auth.AccountKey,
		Model:        "gpt-5",
		Reason:       "model capacity",
	})

	response := explainChannelRoutingRuntime([]*coreauth.Auth{auth}, channelRoutingPolicyConfig{
		Channel:            "codex",
		RouteMode:          gettokensrouting.ChannelRouteModeSequential,
		OrderedAccountIDs:  []string{auth.AccountKey},
		ChannelGroupStates: map[string]gettokensrouting.ChannelGroupState{},
	}, ChannelRoutingExplainRequest{RequestedModel: "gpt-5-mini"}, nil)

	if len(response.Candidates) != 1 || response.Candidates[0].ID != auth.AccountKey {
		t.Fatalf("candidates = %#v, want model-specific block to keep other requested models routeable", response.Candidates)
	}
	if len(response.Filtered) != 0 {
		t.Fatalf("filtered = %#v, want no dropped reasons for a different requested model", response.Filtered)
	}
}
