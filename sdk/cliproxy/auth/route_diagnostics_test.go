package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRouteDecisionSnapshotsRecordSelectedAccountAndTrace(t *testing.T) {
	resetRouteDecisionSnapshotsForTest()
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Name: "route-diagnostics-order",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "route-diagnostics-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{
				OrderIDs: []string{"auth-b", "auth-a"},
				Reason:   "prefer auth-b",
			}
		},
	})
	defer unregister()
	registerSchedulerModels(t, "codex", "route-diagnostics-model", "auth-a", "auth-b")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "auth-a", AccountKey: "acct_a", Provider: "codex", Status: StatusActive},
		&Auth{ID: "auth-b", AccountKey: "acct_b", Provider: "codex", Status: StatusActive},
	)

	selected, err := scheduler.pickSingle(context.Background(), "codex", "route-diagnostics-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if selected == nil || selected.ID != "auth-b" {
		t.Fatalf("selected = %#v, want auth-b", selected)
	}

	items := RecentRouteDecisionSnapshots("codex", 10)
	if len(items) != 1 {
		t.Fatalf("RecentRouteDecisionSnapshots len = %d, want 1", len(items))
	}
	item := items[0]
	if item.SelectedAccountKey != "acct_b" || item.SelectedAuthID != "auth-b" {
		t.Fatalf("selected snapshot = %#v, want acct_b/auth-b", item)
	}
	if item.Source != "scheduler-routing-policy" {
		t.Fatalf("source = %q, want scheduler-routing-policy", item.Source)
	}
	if len(item.Trace) != 1 || item.Trace[0].Reason != "prefer auth-b" {
		t.Fatalf("trace = %#v, want prefer auth-b", item.Trace)
	}
	if len(item.Candidates) != 2 || item.Candidates[0].AccountKey != "acct_b" {
		t.Fatalf("candidates = %#v, want reordered acct_b first", item.Candidates)
	}
}
