package gettokensrouting

import "testing"

func TestDecideChannelRouteSequentialFiltersAndOrdersPool(t *testing.T) {
	cfg := ChannelRoutingConfig{
		RouteMode:         ChannelRouteModeSequential,
		OrderedAccountIDs: []string{"auth-b", "auth-a"},
		ChannelGroupStates: map[string]ChannelGroupState{
			"group-disabled-channel": {Enabled: false},
		},
	}
	accounts := []AccountSnapshot{
		{ID: "auth-a", Enabled: true, Requestable: true, RouteOrder: 20, GroupIDs: []string{"group-a"}},
		{ID: "auth-b", Enabled: true, Requestable: true, RouteOrder: 30, GroupIDs: []string{"group-a"}},
		{ID: "auth-disabled", Enabled: false, Requestable: true, GroupIDs: []string{"group-a"}},
		{ID: "auth-error", Enabled: true, Requestable: false, GroupIDs: []string{"group-a"}},
		{ID: "auth-channel-disabled", Enabled: true, Requestable: true, GroupIDs: []string{"group-disabled-channel"}},
	}
	groups := []AccountGroupSnapshot{
		{ID: "group-a", Enabled: true, RouteOrder: 10},
		{ID: "group-disabled-channel", Enabled: true, RouteOrder: 1},
	}

	decision := DecideChannelRoute(accounts, groups, cfg, ChannelRouteRequest{})

	if decision.SelectedID != "auth-b" {
		t.Fatalf("selected = %q, want auth-b", decision.SelectedID)
	}
	assertRouteableIDs(t, decision.Candidates, []string{"auth-b", "auth-a"})
	assertFilteredReason(t, decision.Filtered, "auth-disabled", "account-disabled")
	assertFilteredReason(t, decision.Filtered, "auth-error", "account-unrequestable")
	assertFilteredReason(t, decision.Filtered, "auth-channel-disabled", "group-disabled-or-missing")
}

func TestDecideChannelRouteBalancedUsesSessionCountThenOrder(t *testing.T) {
	cfg := ChannelRoutingConfig{
		RouteMode:         ChannelRouteModeBalanced,
		OrderedAccountIDs: []string{"auth-a", "auth-b"},
	}
	accounts := []AccountSnapshot{
		{ID: "auth-a", Enabled: true, Requestable: true, RouteOrder: 0, ActiveSessions: 3},
		{ID: "auth-b", Enabled: true, Requestable: true, RouteOrder: 1, ActiveSessions: 1},
		{ID: "auth-c", Enabled: true, Requestable: true, RouteOrder: 2, ActiveSessions: 1},
	}

	decision := DecideChannelRoute(accounts, nil, cfg, ChannelRouteRequest{})

	if decision.SelectedID != "auth-b" {
		t.Fatalf("selected = %q, want auth-b", decision.SelectedID)
	}
}

func TestDecideChannelRouteDropsLegacyProjectMode(t *testing.T) {
	cfg := ChannelRoutingConfig{
		RouteMode:         "project",
		OrderedAccountIDs: []string{"auth-other", "auth-light", "auth-heavy"},
	}
	accounts := []AccountSnapshot{
		{ID: "auth-heavy", Enabled: true, Requestable: true, GroupIDs: []string{"group-pro"}, ActiveSessions: 10},
		{ID: "auth-light", Enabled: true, Requestable: true, GroupIDs: []string{"group-pro"}, ActiveSessions: 1},
		{ID: "auth-other", Enabled: true, Requestable: true, GroupIDs: []string{"group-default"}, ActiveSessions: 0},
	}
	groups := []AccountGroupSnapshot{
		{ID: "group-pro", Enabled: true},
		{ID: "group-default", Enabled: true},
	}

	decision := DecideChannelRoute(accounts, groups, cfg, ChannelRouteRequest{})

	if decision.SelectedID != "auth-other" {
		t.Fatalf("selected = %q, want auth-other from normalized sequential route", decision.SelectedID)
	}
	assertRouteableIDs(t, decision.Candidates, []string{"auth-other", "auth-light", "auth-heavy"})
	for _, step := range decision.Steps {
		if step == "mode:project" {
			t.Fatalf("legacy project mode step remained: %#v", decision.Steps)
		}
	}
}

func TestDecideChannelRouteExcludesTriedAccounts(t *testing.T) {
	cfg := ChannelRoutingConfig{RouteMode: ChannelRouteModeSequential}
	accounts := []AccountSnapshot{
		{ID: "auth-a", Enabled: true, Requestable: true, RouteOrder: 1},
		{ID: "auth-b", Enabled: true, Requestable: true, RouteOrder: 2},
	}

	decision := DecideChannelRoute(accounts, nil, cfg, ChannelRouteRequest{Tried: map[string]struct{}{"auth-a": {}}})

	if decision.SelectedID != "auth-b" {
		t.Fatalf("selected = %q, want auth-b", decision.SelectedID)
	}
	assertFilteredReason(t, decision.Filtered, "auth-a", "tried")
}

func assertRouteableIDs(t *testing.T, candidates []RouteableCandidate, want []string) {
	t.Helper()
	got := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		got = append(got, candidate.Account.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidate ids = %#v, want %#v", got, want)
		}
	}
}

func assertFilteredReason(t *testing.T, filtered []FilteredAccount, accountID string, reason string) {
	t.Helper()
	for _, item := range filtered {
		if item.AccountID == accountID {
			if item.Reason != reason {
				t.Fatalf("filtered reason for %s = %q, want %q", accountID, item.Reason, reason)
			}
			return
		}
	}
	t.Fatalf("missing filtered account %s in %#v", accountID, filtered)
}
