package gettokensrouting

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenscodex"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type PolicyStage string

const (
	PolicyStageHardFilter PolicyStage = "hard-filter"
	PolicyStagePoolScope  PolicyStage = "pool-scope"
	PolicyStageRequest    PolicyStage = "request"
	PolicyStageSticky     PolicyStage = "sticky"
)

type RouteContext struct {
	Provider     string
	Providers    []string
	Model        string
	Options      cliproxyexecutor.Options
	CodexRequest *gettokenscodex.RequestContext
	Candidates   []RouteCandidate
	Tried        map[string]struct{}
	Now          time.Time
}

type RouteCandidate struct {
	ID    string
	Value any
}

type Policy struct {
	Stage   PolicyStage
	Name    string
	Rewrite func(context.Context, RouteContext) PolicyDecision
}

type AdmissionPolicy struct {
	Name  string
	Admit func(context.Context, RouteContext, RouteCandidate) AdmissionDecision
}

type PolicyDecision struct {
	AllowIDs      []string
	DenyIDs       []string
	OrderIDs      []string
	AllowFallback *bool
	Reason        string
}

type AdmissionDecision struct {
	Active bool
	Allow  bool
	Reason string
	Lease  AdmissionLease
}

type AdmissionLease struct {
	state *admissionLeaseState
}

type admissionLeaseState struct {
	once    sync.Once
	commit  func(context.Context)
	release func(context.Context)
}

type DecisionStep struct {
	Stage     PolicyStage
	Policy    string
	Reason    string
	Before    int
	After     int
	AllowIDs  []string
	DenyIDs   []string
	OrderIDs  []string
	Fallback  *bool
	Activated bool
}

type RouteResult struct {
	Candidates []RouteCandidate
	Trace      []DecisionStep
}

type Engine struct {
	policies []Policy
}

var registeredPolicies = struct {
	sync.RWMutex
	nextID int
	items  map[int]Policy
}{items: make(map[int]Policy)}

var registeredAdmissionPolicies = struct {
	sync.RWMutex
	nextID int
	items  map[int]AdmissionPolicy
}{items: make(map[int]AdmissionPolicy)}

// RegisterPolicy installs a process-wide GetTokens routing policy.
func RegisterPolicy(policy Policy) func() {
	if policy.Rewrite == nil {
		return func() {}
	}
	registeredPolicies.Lock()
	registeredPolicies.nextID++
	id := registeredPolicies.nextID
	registeredPolicies.items[id] = policy
	registeredPolicies.Unlock()

	return func() {
		registeredPolicies.Lock()
		delete(registeredPolicies.items, id)
		registeredPolicies.Unlock()
	}
}

// RegisterAdmissionPolicy installs a process-wide GetTokens admission policy.
func RegisterAdmissionPolicy(policy AdmissionPolicy) func() {
	if policy.Admit == nil {
		return func() {}
	}
	registeredAdmissionPolicies.Lock()
	registeredAdmissionPolicies.nextID++
	id := registeredAdmissionPolicies.nextID
	registeredAdmissionPolicies.items[id] = policy
	registeredAdmissionPolicies.Unlock()

	return func() {
		registeredAdmissionPolicies.Lock()
		delete(registeredAdmissionPolicies.items, id)
		registeredAdmissionPolicies.Unlock()
	}
}

// PolicySnapshot returns registered policies in registration order.
func PolicySnapshot() []Policy {
	registeredPolicies.RLock()
	defer registeredPolicies.RUnlock()
	ids := make([]int, 0, len(registeredPolicies.items))
	for id := range registeredPolicies.items {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]Policy, 0, len(ids))
	for _, id := range ids {
		policy := registeredPolicies.items[id]
		if policy.Rewrite != nil {
			out = append(out, policy)
		}
	}
	return out
}

// AdmissionPolicySnapshot returns registered admission policies in registration order.
func AdmissionPolicySnapshot() []AdmissionPolicy {
	registeredAdmissionPolicies.RLock()
	defer registeredAdmissionPolicies.RUnlock()
	ids := make([]int, 0, len(registeredAdmissionPolicies.items))
	for id := range registeredAdmissionPolicies.items {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]AdmissionPolicy, 0, len(ids))
	for _, id := range ids {
		policy := registeredAdmissionPolicies.items[id]
		if policy.Admit != nil {
			out = append(out, policy)
		}
	}
	return out
}

func NewAdmissionLease(commit func(context.Context), release func(context.Context)) AdmissionLease {
	if commit == nil && release == nil {
		return AdmissionLease{}
	}
	return AdmissionLease{state: &admissionLeaseState{commit: commit, release: release}}
}

func (l AdmissionLease) Commit(ctx context.Context) {
	if l.state == nil {
		return
	}
	l.state.once.Do(func() {
		if l.state.commit != nil {
			l.state.commit(ctx)
		}
	})
}

func (l AdmissionLease) Release(ctx context.Context) {
	if l.state == nil {
		return
	}
	l.state.once.Do(func() {
		if l.state.release != nil {
			l.state.release(ctx)
		}
	})
}

func (l AdmissionLease) Active() bool {
	return l.state != nil
}

func AdmitCandidate(ctx context.Context, routeCtx RouteContext, candidate RouteCandidate) AdmissionDecision {
	if strings.TrimSpace(candidate.ID) == "" {
		return AdmissionDecision{}
	}
	policies := AdmissionPolicySnapshot()
	if len(policies) == 0 {
		return AdmissionDecision{}
	}
	if routeCtx.Now.IsZero() {
		routeCtx.Now = time.Now()
	}
	leases := []AdmissionLease{}
	active := false
	for _, policy := range policies {
		if policy.Admit == nil {
			continue
		}
		decision := safeAdmitPolicy(policy, ctx, routeCtx, candidate)
		if !decision.Active {
			continue
		}
		active = true
		if !decision.Allow {
			releaseAdmissionLeases(ctx, leases)
			return AdmissionDecision{
				Active: true,
				Allow:  false,
				Reason: strings.TrimSpace(decision.Reason),
			}
		}
		if decision.Lease.Active() {
			leases = append(leases, decision.Lease)
		}
	}
	if !active {
		return AdmissionDecision{}
	}
	return AdmissionDecision{
		Active: true,
		Allow:  true,
		Lease:  combineAdmissionLeases(leases),
	}
}

func NewEngine(policies ...Policy) Engine {
	ordered := append([]Policy(nil), policies...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left := policyStageRank(ordered[i].Stage)
		right := policyStageRank(ordered[j].Stage)
		if left == right {
			return false
		}
		return left < right
	})
	return Engine{policies: ordered}
}

func (e Engine) Route(ctx context.Context, routeCtx RouteContext) RouteResult {
	current := cloneCandidates(routeCtx.Candidates)
	trace := make([]DecisionStep, 0, len(e.policies))
	if routeCtx.Now.IsZero() {
		routeCtx.Now = time.Now()
	}
	for _, policy := range e.policies {
		if policy.Rewrite == nil {
			continue
		}
		before := len(current)
		routeCtx.Candidates = cloneCandidates(current)
		decision := safeRewritePolicy(policy, ctx, routeCtx)
		active := policyDecisionActive(decision)
		if active {
			current = applyPolicyDecision(current, decision)
		}
		trace = append(trace, DecisionStep{
			Stage:     policy.Stage,
			Policy:    strings.TrimSpace(policy.Name),
			Reason:    strings.TrimSpace(decision.Reason),
			Before:    before,
			After:     len(current),
			AllowIDs:  normalizeIDs(decision.AllowIDs),
			DenyIDs:   normalizeIDs(decision.DenyIDs),
			OrderIDs:  normalizeIDs(decision.OrderIDs),
			Fallback:  cloneBool(decision.AllowFallback),
			Activated: active,
		})
	}
	return RouteResult{
		Candidates: current,
		Trace:      trace,
	}
}

func safeRewritePolicy(policy Policy, ctx context.Context, routeCtx RouteContext) (decision PolicyDecision) {
	defer func() {
		if recover() != nil {
			decision = PolicyDecision{}
		}
	}()
	return policy.Rewrite(ctx, routeCtx)
}

func safeAdmitPolicy(policy AdmissionPolicy, ctx context.Context, routeCtx RouteContext, candidate RouteCandidate) (decision AdmissionDecision) {
	defer func() {
		if recover() != nil {
			decision = AdmissionDecision{
				Active: true,
				Allow:  false,
				Reason: "admission policy failed",
			}
		}
	}()
	return policy.Admit(ctx, routeCtx, candidate)
}

func combineAdmissionLeases(leases []AdmissionLease) AdmissionLease {
	filtered := make([]AdmissionLease, 0, len(leases))
	for _, lease := range leases {
		if lease.Active() {
			filtered = append(filtered, lease)
		}
	}
	if len(filtered) == 0 {
		return AdmissionLease{}
	}
	return NewAdmissionLease(func(ctx context.Context) {
		for _, lease := range filtered {
			lease.Commit(ctx)
		}
	}, func(ctx context.Context) {
		releaseAdmissionLeases(ctx, filtered)
	})
}

func releaseAdmissionLeases(ctx context.Context, leases []AdmissionLease) {
	for i := len(leases) - 1; i >= 0; i-- {
		leases[i].Release(ctx)
	}
}

func policyStageRank(stage PolicyStage) int {
	switch stage {
	case PolicyStageHardFilter:
		return 0
	case PolicyStagePoolScope:
		return 1
	case PolicyStageRequest:
		return 2
	case PolicyStageSticky:
		return 3
	default:
		return 100
	}
}

func policyDecisionActive(decision PolicyDecision) bool {
	return len(decision.AllowIDs) > 0 ||
		len(decision.DenyIDs) > 0 ||
		len(decision.OrderIDs) > 0 ||
		decision.AllowFallback != nil
}

func applyPolicyDecision(candidates []RouteCandidate, decision PolicyDecision) []RouteCandidate {
	if len(candidates) == 0 {
		return candidates
	}
	deny := idSet(decision.DenyIDs)
	allow := idSet(decision.AllowIDs)
	order := normalizeIDs(decision.OrderIDs)

	fallback := true
	if len(allow) > 0 {
		fallback = false
	}
	if decision.AllowFallback != nil {
		fallback = *decision.AllowFallback
	}

	filtered := make([]RouteCandidate, 0, len(candidates))
	byID := make(map[string]RouteCandidate, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		id := strings.TrimSpace(candidate.ID)
		if _, denied := deny[id]; denied {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[id]; !ok && !fallback {
				continue
			}
		}
		filtered = append(filtered, candidate)
		if _, exists := byID[id]; !exists {
			byID[id] = candidate
		}
	}
	if len(filtered) == 0 {
		return filtered
	}

	out := make([]RouteCandidate, 0, len(filtered))
	used := make(map[string]struct{}, len(filtered))
	for _, id := range order {
		candidate, exists := byID[id]
		if !exists {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[id]; !ok && !fallback {
				continue
			}
		}
		out = append(out, candidate)
		used[id] = struct{}{}
	}
	if len(allow) > 0 && len(order) == 0 {
		for _, candidate := range filtered {
			id := strings.TrimSpace(candidate.ID)
			if _, ok := allow[id]; ok {
				out = append(out, candidate)
				used[id] = struct{}{}
			}
		}
	}
	if fallback {
		for _, candidate := range filtered {
			id := strings.TrimSpace(candidate.ID)
			if _, ok := used[id]; ok {
				continue
			}
			out = append(out, candidate)
		}
	}
	return out
}

func cloneCandidates(candidates []RouteCandidate) []RouteCandidate {
	out := make([]RouteCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func idSet(ids []string) map[string]struct{} {
	normalized := normalizeIDs(ids)
	if len(normalized) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(normalized))
	for _, id := range normalized {
		out[id] = struct{}{}
	}
	return out
}

func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	next := *value
	return &next
}
