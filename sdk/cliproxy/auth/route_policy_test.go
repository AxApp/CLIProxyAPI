package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type namedRoutePolicy struct {
	reason string
}

func (p *namedRoutePolicy) RewriteCandidates(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
	return RoutePolicyDecision{Reason: p.reason}
}

type stagedTestRoutePolicy struct {
	stage    gettokensrouting.PolicyStage
	decision RoutePolicyDecision
}

func (p stagedTestRoutePolicy) RoutePolicyStage() gettokensrouting.PolicyStage {
	return p.stage
}

func (p stagedTestRoutePolicy) RewriteCandidates(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
	if req.Model != "route-policy-staged-hard-deny-model" {
		return RoutePolicyDecision{}
	}
	return p.decision
}

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

func TestRoutePolicySnapshotPreservesRegistrationOrder(t *testing.T) {
	first := &namedRoutePolicy{reason: "first"}
	second := &namedRoutePolicy{reason: "second"}
	unregisterFirst := RegisterRoutePolicy(first)
	defer unregisterFirst()
	unregisterSecond := RegisterRoutePolicy(second)
	defer unregisterSecond()

	snapshot := routePolicySnapshot()
	if len(snapshot) < 2 {
		t.Fatalf("routePolicySnapshot length = %d, want at least 2", len(snapshot))
	}
	if snapshot[len(snapshot)-2] != first || snapshot[len(snapshot)-1] != second {
		t.Fatalf("latest snapshot policies are not in registration order")
	}
}

func TestSchedulerRoutePolicyDenyCannotBeBypassedByLaterAllow(t *testing.T) {
	unregisterDeny := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		if req.Model != "route-policy-hard-deny-model" {
			return RoutePolicyDecision{}
		}
		return RoutePolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"}
	}))
	defer unregisterDeny()
	allowFallback := true
	unregisterAllow := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		if req.Model != "route-policy-hard-deny-model" {
			return RoutePolicyDecision{}
		}
		return RoutePolicyDecision{
			AllowIDs:      []string{"blocked"},
			OrderIDs:      []string{"blocked", "fallback"},
			AllowFallback: &allowFallback,
			Reason:        "request allow",
		}
	}))
	defer unregisterAllow()
	registerSchedulerModels(t, "codex", "route-policy-hard-deny-model", "blocked", "fallback")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "blocked", Provider: "codex", Status: StatusActive},
		&Auth{ID: "fallback", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "route-policy-hard-deny-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "fallback" {
		t.Fatalf("picked auth = %#v, want fallback", got)
	}
}

func TestSchedulerRoutePolicyHardFilterRunsBeforeEarlierRequestPolicy(t *testing.T) {
	allowFallback := true
	unregisterAllow := RegisterRoutePolicy(stagedTestRoutePolicy{
		stage: gettokensrouting.PolicyStageRequest,
		decision: RoutePolicyDecision{
			AllowIDs:      []string{"blocked"},
			OrderIDs:      []string{"blocked", "fallback"},
			AllowFallback: &allowFallback,
			Reason:        "request allow",
		},
	})
	defer unregisterAllow()
	unregisterDeny := RegisterRoutePolicy(stagedTestRoutePolicy{
		stage:    gettokensrouting.PolicyStageHardFilter,
		decision: RoutePolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"},
	})
	defer unregisterDeny()
	registerSchedulerModels(t, "codex", "route-policy-staged-hard-deny-model", "blocked", "fallback")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "blocked", Provider: "codex", Status: StatusActive},
		&Auth{ID: "fallback", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "route-policy-staged-hard-deny-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "fallback" {
		t.Fatalf("picked auth = %#v, want fallback", got)
	}
}

func TestLegacySessionAffinityRoutePolicyDenyCannotBeBypassed(t *testing.T) {
	unregister := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		return RoutePolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"}
	}))
	defer unregister()

	manager := NewManager(nil, NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
	}), nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, err := manager.Register(context.Background(), &Auth{ID: "blocked", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(blocked): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "fallback", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(fallback): %v", err)
	}

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-route-policy-deny"}}}
	got, _, errPick := manager.pickNext(context.Background(), "codex", "", opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil || got.ID != "fallback" {
		t.Fatalf("pickNext() auth = %#v, want fallback", got)
	}
}

func TestLegacyMixedSessionAffinityRoutePolicyDenyCannotBeBypassed(t *testing.T) {
	unregister := RegisterRoutePolicy(RoutePolicyFunc(func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
		if req.Provider != "mixed" {
			return RoutePolicyDecision{}
		}
		return RoutePolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"}
	}))
	defer unregister()

	manager := NewManager(nil, NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &FillFirstSelector{},
		TTL:      time.Hour,
	}), nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, err := manager.Register(context.Background(), &Auth{ID: "blocked", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(blocked): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "fallback", Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("Register(fallback): %v", err)
	}

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-route-policy-mixed-deny"}}}
	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"codex", "claude"}, "", opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil || got.ID != "fallback" || provider != "claude" {
		t.Fatalf("pickNextMixed() = (%#v, %q), want fallback/claude", got, provider)
	}
}

func TestSchedulerSessionAffinityStickyPolicyBindsSelectedAuth(t *testing.T) {
	manager := NewManager(nil, NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &RoundRobinSelector{},
		TTL:      time.Hour,
	}), nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-a): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-b): %v", err)
	}

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-sticky-policy"}}}
	if primaryID, _ := extractSessionIDs(opts.Headers, nil, nil); primaryID != "header:session-sticky-policy" {
		t.Fatalf("session id = %q, want header:session-sticky-policy", primaryID)
	}
	first, _, errFirst := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errFirst != nil {
		t.Fatalf("first pickNext() error = %v", errFirst)
	}
	if cached, ok := manager.scheduler.sessionAffinity.cache.Get("codex::header:session-sticky-policy::"); !ok || first == nil || cached != first.ID {
		t.Fatalf("sticky cache after first pick = (%q, %v), want %q", cached, ok, first.ID)
	}
	second, _, errSecond := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errSecond != nil {
		t.Fatalf("second pickNext() error = %v", errSecond)
	}
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("sticky picks = (%#v, %#v), want same auth", first, second)
	}
}
