package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSchedulerRoutePolicyOrdersReadyCandidates(t *testing.T) {
	unregister := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		if req.Model != "route-policy-order-model" {
			return RoutePolicyDecision{}
		}
		return RoutePolicyDecision{OrderIDs: []string{"auth-b", "auth-a"}}
	}))
	defer unregister()
	registerSchedulerModels(t, "codex", "route-policy-order-model", "auth-a", "auth-b")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "auth-a", Provider: "codex", Status: StatusActive},
		&Auth{ID: "auth-b", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "route-policy-order-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("picked auth = %#v, want auth-b", got)
	}
}

func TestSchedulerRoutePolicyCannotBypassCooldown(t *testing.T) {
	unregister := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		if req.Model != "route-policy-cooldown-model" {
			return RoutePolicyDecision{}
		}
		return RoutePolicyDecision{OrderIDs: []string{"slow", "fast"}}
	}))
	defer unregister()
	registerSchedulerModels(t, "codex", "route-policy-cooldown-model", "slow", "fast")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{
			ID:       "slow",
			Provider: "codex",
			Status:   StatusActive,
			ModelStates: map[string]*ModelState{
				"route-policy-cooldown-model": {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(time.Hour),
				},
			},
		},
		&Auth{ID: "fast", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "route-policy-cooldown-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "fast" {
		t.Fatalf("picked auth = %#v, want fast", got)
	}
}

func TestSchedulerRoutePolicyStrictAllow(t *testing.T) {
	fallback := false
	unregister := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		if req.Model != "route-policy-allow-model" {
			return RoutePolicyDecision{}
		}
		return RoutePolicyDecision{
			AllowIDs:      []string{"auth-b"},
			AllowFallback: &fallback,
		}
	}))
	defer unregister()
	registerSchedulerModels(t, "codex", "route-policy-allow-model", "auth-a", "auth-b")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "auth-a", Provider: "codex", Status: StatusActive},
		&Auth{ID: "auth-b", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "route-policy-allow-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("picked auth = %#v, want auth-b", got)
	}
}

func TestSchedulerRoutePolicyOrdersMixedProviderCandidates(t *testing.T) {
	unregister := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		if req.Model != "route-policy-mixed-model" {
			return RoutePolicyDecision{}
		}
		if req.Provider != "mixed" {
			t.Fatalf("Provider = %q, want mixed", req.Provider)
		}
		return RoutePolicyDecision{OrderIDs: []string{"claude-auth", "codex-auth"}}
	}))
	defer unregister()
	registerSchedulerModels(t, "codex", "route-policy-mixed-model", "codex-auth")
	registerSchedulerModels(t, "claude", "route-policy-mixed-model", "claude-auth")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "codex-auth", Provider: "codex", Status: StatusActive},
		&Auth{ID: "claude-auth", Provider: "claude", Status: StatusActive},
	)

	got, provider, err := scheduler.pickMixed(context.Background(), []string{"codex", "claude"}, "route-policy-mixed-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickMixed: %v", err)
	}
	if got == nil || got.ID != "claude-auth" || provider != "claude" {
		t.Fatalf("picked = (%#v, %q), want claude-auth/claude", got, provider)
	}
}
