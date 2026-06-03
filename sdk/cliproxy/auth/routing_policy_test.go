package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenscodex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func registerTestRoutingPolicy(policy gettokensrouting.Policy) func() {
	if policy.Stage == "" {
		policy.Stage = gettokensrouting.PolicyStageRequest
	}
	return gettokensrouting.RegisterPolicy(policy)
}

func TestRewriteScheduledAuthsWithPoliciesCarriesCodexRequestContext(t *testing.T) {
	codexCtx := gettokenscodex.RequestContext{
		RequestKind:    gettokenscodex.RequestKindMain,
		RequestedModel: "gpt-5.1",
	}
	entries := []*scheduledAuth{{auth: &Auth{ID: "auth-a"}}}
	policy := gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageRequest,
		Name:  "assert-codex-context",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.CodexRequest == nil {
				t.Fatalf("RouteContext.CodexRequest = nil, want context")
			}
			if req.CodexRequest.RequestedModel != "gpt-5.1" {
				t.Fatalf("RouteContext.CodexRequest.RequestedModel = %q, want gpt-5.1", req.CodexRequest.RequestedModel)
			}
			return gettokensrouting.PolicyDecision{OrderIDs: []string{"auth-a"}, Reason: "codex context observed"}
		},
	}

	got, changed := rewriteScheduledAuthsWithPolicies(context.Background(), routeRequest{
		Provider: "codex",
		Model:    "gpt-5.1",
		Options: cliproxyexecutor.Options{Metadata: map[string]any{
			gettokenscodex.MetadataKey: codexCtx,
		}},
		Now: time.Now(),
	}, entries, []gettokensrouting.Policy{policy})

	if !changed {
		t.Fatalf("changed = false, want true")
	}
	if len(got) != 1 || got[0].auth.ID != "auth-a" {
		t.Fatalf("got entries = %#v, want auth-a", got)
	}
}

func TestSchedulerGetTokensRoutingOrdersReadyCandidates(t *testing.T) {
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Name: "test-order",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-order-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{OrderIDs: []string{"auth-b", "auth-a"}}
		},
	})
	defer unregister()
	registerSchedulerModels(t, "codex", "routing-engine-order-model", "auth-a", "auth-b")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "auth-a", Provider: "codex", Status: StatusActive},
		&Auth{ID: "auth-b", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "routing-engine-order-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("picked auth = %#v, want auth-b", got)
	}
}

func TestSchedulerGetTokensRoutingCannotBypassCooldown(t *testing.T) {
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Name: "test-cooldown",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-cooldown-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{OrderIDs: []string{"slow", "fast"}}
		},
	})
	defer unregister()
	registerSchedulerModels(t, "codex", "routing-engine-cooldown-model", "slow", "fast")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{
			ID:       "slow",
			Provider: "codex",
			Status:   StatusActive,
			ModelStates: map[string]*ModelState{
				"routing-engine-cooldown-model": {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: time.Now().Add(time.Hour),
				},
			},
		},
		&Auth{ID: "fast", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "routing-engine-cooldown-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "fast" {
		t.Fatalf("picked auth = %#v, want fast", got)
	}
}

func TestSchedulerGetTokensRoutingStrictAllow(t *testing.T) {
	fallback := false
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Name: "test-allow",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-allow-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{
				AllowIDs:      []string{"auth-b"},
				AllowFallback: &fallback,
			}
		},
	})
	defer unregister()
	registerSchedulerModels(t, "codex", "routing-engine-allow-model", "auth-a", "auth-b")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "auth-a", Provider: "codex", Status: StatusActive},
		&Auth{ID: "auth-b", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "routing-engine-allow-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("picked auth = %#v, want auth-b", got)
	}
}

func TestSchedulerGetTokensRoutingOrdersMixedProviderCandidates(t *testing.T) {
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Name: "test-mixed",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-mixed-model" {
				return gettokensrouting.PolicyDecision{}
			}
			if req.Provider != "mixed" {
				t.Fatalf("Provider = %q, want mixed", req.Provider)
			}
			return gettokensrouting.PolicyDecision{OrderIDs: []string{"claude-auth", "codex-auth"}}
		},
	})
	defer unregister()
	registerSchedulerModels(t, "codex", "routing-engine-mixed-model", "codex-auth")
	registerSchedulerModels(t, "claude", "routing-engine-mixed-model", "claude-auth")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "codex-auth", Provider: "codex", Status: StatusActive},
		&Auth{ID: "claude-auth", Provider: "claude", Status: StatusActive},
	)

	got, provider, err := scheduler.pickMixed(context.Background(), []string{"codex", "claude"}, "routing-engine-mixed-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickMixed: %v", err)
	}
	if got == nil || got.ID != "claude-auth" || provider != "claude" {
		t.Fatalf("picked = (%#v, %q), want claude-auth/claude", got, provider)
	}
}

func TestGetTokensRoutingSnapshotPreservesRegistrationOrder(t *testing.T) {
	first := gettokensrouting.Policy{Name: "first", Rewrite: func(context.Context, gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
		return gettokensrouting.PolicyDecision{}
	}}
	second := gettokensrouting.Policy{Name: "second", Rewrite: func(context.Context, gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
		return gettokensrouting.PolicyDecision{}
	}}
	unregisterFirst := registerTestRoutingPolicy(first)
	defer unregisterFirst()
	unregisterSecond := registerTestRoutingPolicy(second)
	defer unregisterSecond()

	snapshot := gettokensrouting.PolicySnapshot()
	if len(snapshot) < 2 {
		t.Fatalf("PolicySnapshot length = %d, want at least 2", len(snapshot))
	}
	if snapshot[len(snapshot)-2].Name != "first" || snapshot[len(snapshot)-1].Name != "second" {
		t.Fatalf("latest snapshot policies are not in registration order")
	}
}

func TestSchedulerGetTokensRoutingDenyCannotBeBypassedByLaterAllow(t *testing.T) {
	unregisterDeny := registerTestRoutingPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageHardFilter,
		Name:  "test-hard-deny",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-hard-deny-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"}
		},
	})
	defer unregisterDeny()
	allowFallback := true
	unregisterAllow := registerTestRoutingPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageRequest,
		Name:  "test-request-allow",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-hard-deny-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{
				AllowIDs:      []string{"blocked"},
				OrderIDs:      []string{"blocked", "fallback"},
				AllowFallback: &allowFallback,
				Reason:        "request allow",
			}
		},
	})
	defer unregisterAllow()
	registerSchedulerModels(t, "codex", "routing-engine-hard-deny-model", "blocked", "fallback")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "blocked", Provider: "codex", Status: StatusActive},
		&Auth{ID: "fallback", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "routing-engine-hard-deny-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "fallback" {
		t.Fatalf("picked auth = %#v, want fallback", got)
	}
}

func TestSchedulerGetTokensRoutingHardFilterRunsBeforeEarlierRequestPolicy(t *testing.T) {
	allowFallback := true
	unregisterAllow := registerTestRoutingPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageRequest,
		Name:  "test-request-allow",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-staged-hard-deny-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{
				AllowIDs:      []string{"blocked"},
				OrderIDs:      []string{"blocked", "fallback"},
				AllowFallback: &allowFallback,
				Reason:        "request allow",
			}
		},
	})
	defer unregisterAllow()
	unregisterDeny := registerTestRoutingPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageHardFilter,
		Name:  "test-hard-deny",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Model != "routing-engine-staged-hard-deny-model" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"}
		},
	})
	defer unregisterDeny()
	registerSchedulerModels(t, "codex", "routing-engine-staged-hard-deny-model", "blocked", "fallback")

	scheduler := newSchedulerForTest(
		&FillFirstSelector{},
		&Auth{ID: "blocked", Provider: "codex", Status: StatusActive},
		&Auth{ID: "fallback", Provider: "codex", Status: StatusActive},
	)

	got, err := scheduler.pickSingle(context.Background(), "codex", "routing-engine-staged-hard-deny-model", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle: %v", err)
	}
	if got == nil || got.ID != "fallback" {
		t.Fatalf("picked auth = %#v, want fallback", got)
	}
}

func TestSessionAffinityGetTokensRoutingDenyCannotBeBypassed(t *testing.T) {
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageHardFilter,
		Name:  "test-hard-deny",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			return gettokensrouting.PolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"}
		},
	})
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

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-routing-engine-deny"}}}
	got, _, errPick := manager.pickNext(context.Background(), "codex", "", opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil || got.ID != "fallback" {
		t.Fatalf("pickNext() auth = %#v, want fallback", got)
	}
}

func TestMixedSessionAffinityGetTokensRoutingDenyCannotBeBypassed(t *testing.T) {
	unregister := registerTestRoutingPolicy(gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageHardFilter,
		Name:  "test-hard-deny",
		Rewrite: func(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			if req.Provider != "mixed" {
				return gettokensrouting.PolicyDecision{}
			}
			return gettokensrouting.PolicyDecision{DenyIDs: []string{"blocked"}, Reason: "hard guard"}
		},
	})
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

	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-routing-engine-mixed-deny"}}}
	got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"codex", "claude"}, "", opts, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNextMixed() error = %v", errPick)
	}
	if got == nil || got.ID != "fallback" || provider != "claude" {
		t.Fatalf("pickNextMixed() = (%#v, %q), want fallback/claude", got, provider)
	}
}

func TestManagerAdmissionDenyFallsBackToNextAuth(t *testing.T) {
	unregister := gettokensrouting.RegisterAdmissionPolicy(gettokensrouting.AdmissionPolicy{
		Name: "test-admission-deny-first",
		Admit: func(ctx context.Context, req gettokensrouting.RouteContext, candidate gettokensrouting.RouteCandidate) gettokensrouting.AdmissionDecision {
			if candidate.ID == "auth-a" {
				return gettokensrouting.AdmissionDecision{Active: true, Allow: false, Reason: "test admission deny"}
			}
			return gettokensrouting.AdmissionDecision{}
		},
	})
	defer unregister()

	executor := &admissionCaptureExecutor{id: "codex"}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.executors["codex"] = executor
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-a): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-b): %v", err)
	}

	if _, err := manager.executeMixedOnce(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, 0); err != nil {
		t.Fatalf("executeMixedOnce() error = %v", err)
	}
	if executor.lastAuthID != "auth-b" {
		t.Fatalf("executed auth = %q, want fallback auth-b", executor.lastAuthID)
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

type admissionCaptureExecutor struct {
	id         string
	lastAuthID string
}

func (e *admissionCaptureExecutor) Identifier() string { return e.id }

func (e *admissionCaptureExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth != nil {
		e.lastAuthID = auth.ID
	}
	return cliproxyexecutor.Response{}, nil
}

func (e *admissionCaptureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *admissionCaptureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *admissionCaptureExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth != nil {
		e.lastAuthID = auth.ID
	}
	return cliproxyexecutor.Response{}, nil
}

func (e *admissionCaptureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestSessionAffinityKeepsSoftBlockedLeaseBeforeFailureBudget(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:      &FillFirstSelector{},
		TTL:           time.Hour,
		FailureBudget: 2,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-a): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-b): %v", err)
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-soft-blocked-lease"}}}
	first, _, errFirst := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errFirst != nil {
		t.Fatalf("first pickNext() error = %v", errFirst)
	}
	if first == nil || first.ID != "auth-a" {
		t.Fatalf("first auth = %#v, want auth-a", first)
	}

	updated := first.Clone()
	updated.Unavailable = true
	updated.NextRetryAfter = time.Now().Add(time.Hour)
	updated.Quota = QuotaState{Exceeded: true, NextRecoverAt: time.Now().Add(time.Hour)}
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update(auth-a soft blocked): %v", err)
	}
	selector.RecordRouteFailure(opts, "codex", "", first.ID, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage limit"})

	second, _, errSecond := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errSecond != nil {
		t.Fatalf("second pickNext() error = %v", errSecond)
	}
	if second == nil || second.ID != "auth-a" {
		t.Fatalf("second auth = %#v, want sticky auth-a before failure budget", second)
	}
}

func TestSessionAffinityReleasesSoftBlockedLeaseAfterFailureBudget(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:      &FillFirstSelector{},
		TTL:           time.Hour,
		FailureBudget: 2,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-a): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-b): %v", err)
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-failure-budget-release"}}}
	first, _, errFirst := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errFirst != nil {
		t.Fatalf("first pickNext() error = %v", errFirst)
	}
	if first == nil || first.ID != "auth-a" {
		t.Fatalf("first auth = %#v, want auth-a", first)
	}
	updated := first.Clone()
	updated.Unavailable = true
	updated.NextRetryAfter = time.Now().Add(time.Hour)
	updated.Quota = QuotaState{Exceeded: true, NextRecoverAt: time.Now().Add(time.Hour)}
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update(auth-a soft blocked): %v", err)
	}
	selector.RecordRouteFailure(opts, "codex", "", first.ID, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage limit #1"})
	selector.RecordRouteFailure(opts, "codex", "", first.ID, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage limit #2"})

	second, _, errSecond := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errSecond != nil {
		t.Fatalf("second pickNext() error = %v", errSecond)
	}
	if second == nil || second.ID != "auth-b" {
		t.Fatalf("second auth = %#v, want auth-b after failure budget", second)
	}
}

func TestSessionAffinityPoolEpochBumpInvalidatesStaleBinding(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:      &FillFirstSelector{},
		TTL:           time.Hour,
		FailureBudget: 2,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-a): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-b): %v", err)
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-epoch-rebind"}}}

	// Establish binding to auth-a
	first, _, errFirst := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errFirst != nil {
		t.Fatalf("first pickNext() error = %v", errFirst)
	}
	if first == nil || first.ID != "auth-a" {
		t.Fatalf("first auth = %#v, want auth-a", first)
	}
	// Exhaust auth-a failure budget
	updated := first.Clone()
	updated.Unavailable = true
	updated.NextRetryAfter = time.Now().Add(time.Hour)
	updated.Quota = QuotaState{Exceeded: true, NextRecoverAt: time.Now().Add(time.Hour)}
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update(auth-a soft blocked): %v", err)
	}
	selector.RecordRouteFailure(opts, "codex", "", first.ID, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage limit #1"})
	selector.RecordRouteFailure(opts, "codex", "", first.ID, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage limit #2"})

	// But now simulate account pool change: auth-b becomes active (bump epoch manually)
	selector.BumpPoolEpoch()

	// Session should now rebind to auth-b even though auth-a is soft-blocked with budget exceeded
	second, _, errSecond := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errSecond != nil {
		t.Fatalf("second pickNext() error = %v", errSecond)
	}
	if second == nil || second.ID != "auth-b" {
		t.Fatalf("second auth = %#v, want auth-b after pool epoch bump", second)
	}
}

func TestSessionAffinityDisabledAccountMustNotStick(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback:      &FillFirstSelector{},
		TTL:           time.Hour,
		FailureBudget: 2,
	})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-a): %v", err)
	}
	if _, err := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "codex", Status: StatusActive}); err != nil {
		t.Fatalf("Register(auth-b): %v", err)
	}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"session-disable-must-not-stick"}}}

	// Bind session to auth-a
	first, _, errFirst := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errFirst != nil {
		t.Fatalf("first pickNext() error = %v", errFirst)
	}
	if first == nil || first.ID != "auth-a" {
		t.Fatalf("first auth = %#v, want auth-a", first)
	}

	// Disable auth-a
	updated, ok := manager.SetRouteDisabled("auth-a", true)
	if !ok || updated == nil || !updated.Disabled {
		t.Fatalf("SetRouteDisabled disable failed: ok=%v auth=%+v", ok, updated)
	}
	selector.InvalidateAuth("auth-a")
	selector.BumpPoolEpoch()

	// Next pick must NOT return the disabled auth-a
	second, _, errSecond := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errSecond != nil {
		t.Fatalf("second pickNext() error = %v", errSecond)
	}
	if second == nil || second.ID == "auth-a" {
		t.Fatalf("second auth = %#v, must not be disabled auth-a", second)
	}
	if second.ID != "auth-b" {
		t.Fatalf("second auth = %s, want auth-b", second.ID)
	}

	// Re-enable auth-a
	_, _ = manager.SetRouteDisabled("auth-a", false)
	// But session already bound to auth-b; should stick to auth-b
	third, _, errThird := manager.pickNext(context.Background(), "codex", "", opts, nil)
	if errThird != nil {
		t.Fatalf("third pickNext() error = %v", errThird)
	}
	if third == nil || third.ID != "auth-b" {
		t.Fatalf("third auth = %#v, want sticky auth-b", third)
	}
}
