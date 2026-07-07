package auth

import (
	"context"
	"fmt"
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

func TestRouteDecisionSnapshotsCapLargeCandidateAndTraceSamples(t *testing.T) {
	resetRouteDecisionSnapshotsForTest()
	const totalCandidates = 250
	const wantSampleLimit = 100
	candidates := make([]gettokensrouting.RouteCandidate, 0, totalCandidates)
	denyIDs := make([]string, 0, totalCandidates)
	for index := 0; index < totalCandidates; index++ {
		authID := fmt.Sprintf("auth-scale-%03d", index)
		accountKey := fmt.Sprintf("acct_scale_%03d", index)
		candidates = append(candidates, gettokensrouting.RouteCandidate{
			ID: authID,
			Value: &Auth{
				ID:         authID,
				AccountKey: accountKey,
				Provider:   "codex",
			},
		})
		denyIDs = append(denyIDs, authID)
	}
	selected := &Auth{ID: "auth-scale-249", AccountKey: "acct_scale_249", Provider: "codex"}

	recordRouteDecision(
		routeRequest{Provider: "codex", Model: "gpt-5"},
		"scheduler-routing-policy",
		gettokensrouting.RouteResult{
			Candidates: candidates,
			Trace: []gettokensrouting.DecisionStep{{
				Policy:  "account-route-guard",
				Reason:  "gettokens account route guard: rate-limit=cooldown",
				Before:  totalCandidates,
				After:   0,
				DenyIDs: denyIDs,
			}},
		},
		selected,
		nil,
	)

	items := RecentRouteDecisionSnapshots("codex", 10)
	if len(items) != 1 {
		t.Fatalf("RecentRouteDecisionSnapshots len = %d, want 1", len(items))
	}
	item := items[0]
	if item.CandidateCount != totalCandidates {
		t.Fatalf("CandidateCount = %d, want %d", item.CandidateCount, totalCandidates)
	}
	if len(item.Candidates) != wantSampleLimit {
		t.Fatalf("Candidates len = %d, want capped sample %d", len(item.Candidates), wantSampleLimit)
	}
	if item.Candidates[len(item.Candidates)-1].AuthID != selected.ID {
		t.Fatalf("last candidate sample = %#v, want selected auth retained", item.Candidates[len(item.Candidates)-1])
	}
	if len(item.Trace) != 1 || len(item.Trace[0].DenyIDs) != wantSampleLimit {
		t.Fatalf("trace = %#v, want capped deny ids", item.Trace)
	}
	if len(item.DroppedReasons) != wantSampleLimit {
		t.Fatalf("DroppedReasons len = %d, want capped sample %d", len(item.DroppedReasons), wantSampleLimit)
	}
}

func TestRouteDecisionSnapshotsRetainSelectedOutsideCandidateSample(t *testing.T) {
	resetRouteDecisionSnapshotsForTest()
	defer resetRouteDecisionSnapshotsForTest()
	candidates := make([]gettokensrouting.RouteCandidate, 0, routeDecisionSnapshotSampleLimit)
	for index := 0; index < routeDecisionSnapshotSampleLimit; index++ {
		authID := fmt.Sprintf("auth-sampled-%03d", index)
		candidates = append(candidates, gettokensrouting.RouteCandidate{
			ID: authID,
			Value: &Auth{
				ID:         authID,
				AccountKey: fmt.Sprintf("acct_sampled_%03d", index),
				Provider:   "codex",
			},
		})
	}
	selected := &Auth{ID: "auth-selected-outside-sample", AccountKey: "acct_selected", Provider: "codex"}

	recordRouteDecision(
		routeRequest{Provider: "codex", Model: "gpt-5"},
		"scheduler-default",
		gettokensrouting.RouteResult{
			Candidates:     candidates,
			CandidateCount: routeDecisionSnapshotSampleLimit + 150,
			Trace:          []gettokensrouting.DecisionStep{},
		},
		selected,
		nil,
	)

	items := RecentRouteDecisionSnapshots("codex", 10)
	if len(items) != 1 {
		t.Fatalf("RecentRouteDecisionSnapshots len = %d, want 1", len(items))
	}
	item := items[0]
	if item.CandidateCount != routeDecisionSnapshotSampleLimit+150 {
		t.Fatalf("CandidateCount = %d, want total count", item.CandidateCount)
	}
	if len(item.Candidates) != routeDecisionSnapshotSampleLimit {
		t.Fatalf("Candidates len = %d, want capped sample %d", len(item.Candidates), routeDecisionSnapshotSampleLimit)
	}
	if got := item.Candidates[len(item.Candidates)-1]; got.AuthID != selected.ID || got.AccountKey != selected.AccountKey {
		t.Fatalf("last candidate sample = %#v, want selected auth retained", got)
	}
}

func TestDefaultRouteResultSamplesCandidatesAndPreservesTotalCount(t *testing.T) {
	const totalCandidates = routeDecisionSnapshotSampleLimit + 150
	entries := make([]*scheduledAuth, 0, totalCandidates)
	for index := 0; index < totalCandidates; index++ {
		entries = append(entries, &scheduledAuth{
			auth: &Auth{
				ID:       fmt.Sprintf("auth-%03d", index),
				Provider: "codex",
			},
		})
	}

	result := routeResultFromScheduledEntries(entries)
	if result.CandidateCount != totalCandidates {
		t.Fatalf("CandidateCount = %d, want %d", result.CandidateCount, totalCandidates)
	}
	if len(result.Candidates) != routeDecisionSnapshotSampleLimit {
		t.Fatalf("Candidates len = %d, want %d", len(result.Candidates), routeDecisionSnapshotSampleLimit)
	}
	if result.Candidates[len(result.Candidates)-1].ID != "auth-099" {
		t.Fatalf("last sampled candidate = %q, want auth-099", result.Candidates[len(result.Candidates)-1].ID)
	}
}
