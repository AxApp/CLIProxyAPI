package gettokensrouting

import (
	"context"
	"testing"
)

func TestEngineEmptyPolicyPreservesCandidateOrder(t *testing.T) {
	engine := NewEngine()
	result := engine.Route(context.Background(), RouteContext{
		Candidates: []RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})

	assertCandidateIDs(t, result.Candidates, []string{"auth-a", "auth-b"})
	if len(result.Trace) != 0 {
		t.Fatalf("Trace length = %d, want 0", len(result.Trace))
	}
}

func TestEngineRunsHardFilterBeforeRequestPolicy(t *testing.T) {
	allowFallback := true
	engine := NewEngine(
		Policy{
			Stage: PolicyStageRequest,
			Name:  "request-allow",
			Rewrite: func(ctx context.Context, routeCtx RouteContext) PolicyDecision {
				return PolicyDecision{
					AllowIDs:      []string{"blocked"},
					OrderIDs:      []string{"blocked", "fallback"},
					AllowFallback: &allowFallback,
					Reason:        "request requested blocked auth",
				}
			},
		},
		Policy{
			Stage: PolicyStageHardFilter,
			Name:  "hard-guard",
			Rewrite: func(ctx context.Context, routeCtx RouteContext) PolicyDecision {
				return PolicyDecision{
					DenyIDs: []string{"blocked"},
					Reason:  "manual disabled",
				}
			},
		},
	)

	result := engine.Route(context.Background(), RouteContext{
		Candidates: []RouteCandidate{
			{ID: "blocked"},
			{ID: "fallback"},
		},
	})

	assertCandidateIDs(t, result.Candidates, []string{"fallback"})
	if len(result.Trace) != 2 {
		t.Fatalf("Trace length = %d, want 2", len(result.Trace))
	}
	if result.Trace[0].Stage != PolicyStageHardFilter || result.Trace[0].After != 1 {
		t.Fatalf("first trace step = %#v, want hard-filter reducing candidates to 1", result.Trace[0])
	}
	if result.Trace[1].Stage != PolicyStageRequest || result.Trace[1].After != 1 {
		t.Fatalf("second trace step = %#v, want request policy unable to re-add blocked auth", result.Trace[1])
	}
}

func TestEngineAppliesRequestOrderAfterHardFilter(t *testing.T) {
	engine := NewEngine(
		Policy{
			Stage: PolicyStageHardFilter,
			Name:  "hard-guard",
			Rewrite: func(ctx context.Context, routeCtx RouteContext) PolicyDecision {
				return PolicyDecision{DenyIDs: []string{"blocked"}}
			},
		},
		Policy{
			Stage: PolicyStageRequest,
			Name:  "request-order",
			Rewrite: func(ctx context.Context, routeCtx RouteContext) PolicyDecision {
				return PolicyDecision{OrderIDs: []string{"auth-c", "auth-a", "blocked"}}
			},
		},
	)

	result := engine.Route(context.Background(), RouteContext{
		Candidates: []RouteCandidate{
			{ID: "auth-a"},
			{ID: "blocked"},
			{ID: "auth-c"},
		},
	})

	assertCandidateIDs(t, result.Candidates, []string{"auth-c", "auth-a"})
}

func TestEngineNormalizesDuplicateAndBlankDecisionIDs(t *testing.T) {
	engine := NewEngine(Policy{
		Stage: PolicyStageRequest,
		Name:  "request-order",
		Rewrite: func(ctx context.Context, routeCtx RouteContext) PolicyDecision {
			return PolicyDecision{
				OrderIDs: []string{" auth-b ", "", "auth-b", "auth-a"},
				DenyIDs:  []string{"missing", " ", "missing"},
			}
		},
	})

	result := engine.Route(context.Background(), RouteContext{
		Candidates: []RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})

	assertCandidateIDs(t, result.Candidates, []string{"auth-b", "auth-a"})
	if len(result.Trace) != 1 {
		t.Fatalf("Trace length = %d, want 1", len(result.Trace))
	}
	if got := result.Trace[0].OrderIDs; len(got) != 2 || got[0] != "auth-b" || got[1] != "auth-a" {
		t.Fatalf("Trace OrderIDs = %#v, want normalized order", got)
	}
	if got := result.Trace[0].DenyIDs; len(got) != 1 || got[0] != "missing" {
		t.Fatalf("Trace DenyIDs = %#v, want normalized deny ids", got)
	}
}

func assertCandidateIDs(t *testing.T, candidates []RouteCandidate, want []string) {
	t.Helper()
	got := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" {
			continue
		}
		got = append(got, candidate.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate ids = %#v, want %#v", got, want)
		}
	}
}
