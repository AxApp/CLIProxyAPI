package auth

import (
	"context"
	"testing"
	"time"

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

func TestRouteDecisionSnapshotsParseRouteGuardDroppedReasonsFromTrace(t *testing.T) {
	resetRouteDecisionSnapshotsForTest()
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageHardFilter,
		Name:  "account-route-guard",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "route-diagnostics-guard-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{
				DenyIDs: []string{"auth-a"},
				Reason:  "gettokens account route guard: rate-limit=cooldown",
			}
		},
	})
	defer unregister()
	registerSchedulerModels(t, "codex", "route-diagnostics-guard-model", "auth-a", "auth-b")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "auth-a", AccountKey: "acct_a", Provider: "codex", Status: StatusActive},
		&Auth{ID: "auth-b", AccountKey: "acct_b", Provider: "codex", Status: StatusActive},
	)

	selected, err := scheduler.pickSingle(context.Background(), "codex", "route-diagnostics-guard-model", cliproxyexecutor.Options{}, nil)
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
	if len(item.DroppedReasons) != 1 {
		t.Fatalf("DroppedReasons = %#v, want one parsed route guard reason", item.DroppedReasons)
	}
	dropped := item.DroppedReasons[0]
	if dropped.AuthID != "auth-a" {
		t.Fatalf("AuthID = %q, want auth-a", dropped.AuthID)
	}
	if dropped.Source != "rate-limit" || dropped.Scope != "account" || dropped.Reason != "cooldown" {
		t.Fatalf("dropped = %#v, want rate-limit/account/cooldown", dropped)
	}
	if dropped.Model != "route-diagnostics-guard-model" || dropped.UpdatedAt.IsZero() || !dropped.RouteBlocking {
		t.Fatalf("dropped metadata = %#v, want model/updatedAt/routeBlocking", dropped)
	}
}

func TestRouteDecisionDiagnosticsModelSourceKeepsModelScope(t *testing.T) {
	recordedAt := time.Now().UTC()
	reasons := routeDecisionDroppedReasonsFromTrace([]gettokensrouting.DecisionStep{{
		Stage:     gettokensrouting.PolicyStageHardFilter,
		Policy:    "account-route-guard",
		Reason:    "gettokens account route guard: model-unavailable=model capacity",
		Before:    2,
		After:     1,
		DenyIDs:   []string{"auth-a"},
		Activated: true,
	}}, []gettokensrouting.RouteCandidate{{
		ID: "auth-a",
		Value: &Auth{
			ID:         "auth-a",
			AccountKey: "acct_a",
			Provider:   "codex",
		},
	}}, "gpt-5", recordedAt)

	if len(reasons) != 1 {
		t.Fatalf("reasons = %#v, want one model-scoped dropped reason", reasons)
	}
	reason := reasons[0]
	if reason.Source != "model-unavailable" || reason.Scope != "model" {
		t.Fatalf("reason scope = %#v, want model-unavailable/model", reason)
	}
	if reason.AccountKey != "acct_a" || reason.AuthID != "auth-a" || reason.Model != "gpt-5" {
		t.Fatalf("reason identity/model = %#v, want acct_a/auth-a/gpt-5", reason)
	}
	if !reason.RouteBlocking || !reason.UpdatedAt.Equal(recordedAt) {
		t.Fatalf("reason metadata = %#v, want blocking recordedAt", reason)
	}
}
