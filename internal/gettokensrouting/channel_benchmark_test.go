package gettokensrouting

import (
	"fmt"
	"testing"
)

func BenchmarkDecideChannelRouteBalanced4000Accounts(b *testing.B) {
	accounts := buildChannelRouteBenchmarkAccounts(4000, false)
	cfg := ChannelRoutingConfig{RouteMode: ChannelRouteModeBalanced}
	req := ChannelRouteRequest{}

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		decision := DecideChannelRoute(accounts, nil, cfg, req)
		if decision.SelectedID == "" || len(decision.Candidates) != 4000 {
			b.Fatalf("decision = %#v, want 4000 candidates and a selected account", decision)
		}
	}
}

func BenchmarkDecideChannelRouteSequential4000Accounts(b *testing.B) {
	accounts := buildChannelRouteBenchmarkAccounts(4000, false)
	cfg := ChannelRoutingConfig{RouteMode: ChannelRouteModeSequential}
	req := ChannelRouteRequest{}

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		decision := DecideChannelRoute(accounts, nil, cfg, req)
		if decision.SelectedID == "" || len(decision.Candidates) != 4000 {
			b.Fatalf("decision = %#v, want 4000 candidates and a selected account", decision)
		}
	}
}

func BenchmarkDecideChannelRouteBalanced4000FilteredTo2Accounts(b *testing.B) {
	accounts := buildChannelRouteBenchmarkAccounts(4000, true)
	cfg := ChannelRoutingConfig{RouteMode: ChannelRouteModeBalanced}
	req := ChannelRouteRequest{}

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		decision := DecideChannelRoute(accounts, nil, cfg, req)
		if decision.SelectedID == "" || len(decision.Candidates) != 2 || len(decision.Filtered) != 3998 {
			b.Fatalf("decision = %#v, want 2 candidates and 3998 filtered accounts", decision)
		}
	}
}

func buildChannelRouteBenchmarkAccounts(count int, mostlyFiltered bool) []AccountSnapshot {
	accounts := make([]AccountSnapshot, 0, count)
	for index := 0; index < count; index++ {
		enabled := true
		requestable := true
		if mostlyFiltered && index >= 2 {
			if index%2 == 0 {
				enabled = false
			} else {
				requestable = false
			}
		}
		accounts = append(accounts, AccountSnapshot{
			ID:             fmt.Sprintf("auth-benchmark-%04d", index),
			Enabled:        enabled,
			Requestable:    requestable,
			RouteOrder:     index,
			ActiveSessions: index % 17,
		})
	}
	return accounts
}
