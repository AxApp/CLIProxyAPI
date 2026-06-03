package gettokensrouting

import "testing"

func TestRuntimeAccountProjectionMarksCoarseAvailableAccount(t *testing.T) {
	snapshot := BuildRuntimeAccountProjectionSnapshot([]RuntimeAccountInput{{
		AuthID:     "auth-a",
		AccountKey: "acct-a",
		Provider:   "codex",
		Status:     "active",
		Attributes: map[string]string{"priority": "7"},
	}}, RuntimeAccountProjectionOptions{
		ActiveSessionsByID: map[string]int{"auth-a": 2},
	})

	projection, ok := snapshot.Find("auth-a", "")
	if !ok {
		t.Fatal("projection not found by auth id")
	}
	if !projection.Present || !projection.Enabled || !projection.Requestable || !projection.CoarseAvailable {
		t.Fatalf("projection = %#v, want coarse available", projection)
	}
	if projection.AccountKey != "acct-a" || projection.ActiveSessions != 2 || projection.RouteOrder != 7 {
		t.Fatalf("unexpected projection fields: %#v", projection)
	}
	if byAccount, ok := snapshot.Find("", "acct-a"); !ok || byAccount.AuthID != "auth-a" {
		t.Fatalf("projection not found by account key: %#v ok=%v", byAccount, ok)
	}
}

func TestRuntimeAccountProjectionAggregatesHardReasons(t *testing.T) {
	snapshot := BuildRuntimeAccountProjectionSnapshot([]RuntimeAccountInput{{
		AuthID:      "auth-blocked",
		AccountKey:  "acct-blocked",
		Provider:    "codex",
		Status:      "error",
		Disabled:    true,
		Unavailable: true,
	}}, RuntimeAccountProjectionOptions{
		GuardBlocksByAuthID: map[string][]RuntimeAccountGuardBlock{
			"auth-blocked": {{Source: "rate-limit"}, {Source: "quota-empty"}},
		},
	})

	projection, ok := snapshot.Find("auth-blocked", "")
	if !ok {
		t.Fatal("projection not found")
	}
	if projection.CoarseAvailable || projection.Enabled || projection.Requestable {
		t.Fatalf("projection = %#v, want unavailable", projection)
	}
	for _, reason := range []string{"account-disabled", "account-unavailable", "quota-empty", "rate-limit", "status-error"} {
		if !containsRuntimeReason(projection.FilteredReasons, reason) {
			t.Fatalf("reasons = %#v, want %q", projection.FilteredReasons, reason)
		}
	}
}

func TestDetachedRuntimeAccountProjectionUsesStableReason(t *testing.T) {
	projection := DetachedRuntimeAccountProjection("auth-old", "acct-old")
	if projection.Present || projection.CoarseAvailable || projection.Enabled || projection.Requestable {
		t.Fatalf("projection = %#v, want detached unavailable", projection)
	}
	if projection.AuthID != "auth-old" || projection.AccountKey != "acct-old" || len(projection.FilteredReasons) != 1 || projection.FilteredReasons[0] != RuntimeAccountReasonDetached {
		t.Fatalf("unexpected detached projection: %#v", projection)
	}
}

func containsRuntimeReason(reasons []string, target string) bool {
	for _, reason := range reasons {
		if reason == target {
			return true
		}
	}
	return false
}
