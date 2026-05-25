package auth

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// RoutePolicy can rewrite ready auth candidates before the built-in selector runs.
// Policies receive only candidates that already passed disabled, cooldown, and model checks.
type RoutePolicy interface {
	RewriteCandidates(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision
}

// RoutePolicyFunc adapts a function to RoutePolicy.
type RoutePolicyFunc func(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision

// RewriteCandidates implements RoutePolicy.
func (fn RoutePolicyFunc) RewriteCandidates(ctx context.Context, req RoutePolicyRequest) RoutePolicyDecision {
	if fn == nil {
		return RoutePolicyDecision{}
	}
	return fn(ctx, req)
}

// RoutePolicyRequest describes the current routing decision point.
type RoutePolicyRequest struct {
	Provider   string
	Providers  []string
	Model      string
	Options    cliproxyexecutor.Options
	Candidates []*Auth
	Tried      map[string]struct{}
	Now        time.Time
}

// RoutePolicyDecision describes candidate filtering and ordering preferences.
type RoutePolicyDecision struct {
	AllowIDs      []string
	DenyIDs       []string
	OrderIDs      []string
	AllowFallback *bool
	Reason        string
}

var routePolicies = struct {
	sync.RWMutex
	nextID int
	items  map[int]RoutePolicy
}{items: make(map[int]RoutePolicy)}

// RegisterRoutePolicy installs a process-wide route policy and returns a cleanup function.
func RegisterRoutePolicy(policy RoutePolicy) func() {
	if policy == nil {
		return func() {}
	}
	routePolicies.Lock()
	routePolicies.nextID++
	id := routePolicies.nextID
	routePolicies.items[id] = policy
	routePolicies.Unlock()

	return func() {
		routePolicies.Lock()
		delete(routePolicies.items, id)
		routePolicies.Unlock()
	}
}

func routePolicySnapshot() []RoutePolicy {
	routePolicies.RLock()
	defer routePolicies.RUnlock()
	ids := make([]int, 0, len(routePolicies.items))
	for id := range routePolicies.items {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]RoutePolicy, 0, len(ids))
	for _, id := range ids {
		policy := routePolicies.items[id]
		if policy != nil {
			out = append(out, policy)
		}
	}
	return out
}

func rewriteScheduledAuths(ctx context.Context, req RoutePolicyRequest, entries []*scheduledAuth) ([]*scheduledAuth, bool) {
	if len(entries) == 0 {
		return entries, false
	}
	policies := routePolicySnapshot()
	if len(policies) == 0 {
		return entries, false
	}
	enginePolicies := make([]gettokensrouting.Policy, 0, len(policies))
	for _, policy := range policies {
		policy := policy
		enginePolicies = append(enginePolicies, gettokensrouting.Policy{
			Stage: routePolicyStage(policy),
			Name:  routePolicyName(policy),
			Rewrite: func(ctx context.Context, routeCtx gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
				req.Provider = routeCtx.Provider
				req.Providers = append([]string(nil), routeCtx.Providers...)
				req.Model = routeCtx.Model
				req.Options = routeCtx.Options
				req.Candidates = authCandidatesFromRouteCandidates(routeCtx.Candidates)
				req.Tried = routeCtx.Tried
				req.Now = routeCtx.Now
				decision, ok := safeEvaluateRoutePolicy(policy, ctx, req)
				if !ok {
					return gettokensrouting.PolicyDecision{}
				}
				return gettokensrouting.PolicyDecision{
					AllowIDs:      decision.AllowIDs,
					DenyIDs:       decision.DenyIDs,
					OrderIDs:      decision.OrderIDs,
					AllowFallback: decision.AllowFallback,
					Reason:        decision.Reason,
				}
			},
		})
	}
	result := gettokensrouting.NewEngine(enginePolicies...).Route(ctx, gettokensrouting.RouteContext{
		Provider:   req.Provider,
		Providers:  append([]string(nil), req.Providers...),
		Model:      req.Model,
		Options:    req.Options,
		Candidates: routeCandidatesFromScheduled(entries),
		Tried:      req.Tried,
		Now:        req.Now,
	})
	active := false
	for _, step := range result.Trace {
		if step.Activated {
			active = true
			break
		}
	}
	if !active {
		return entries, false
	}
	return scheduledFromRouteCandidates(result.Candidates), true
}

type stagedRoutePolicy interface {
	RoutePolicyStage() gettokensrouting.PolicyStage
}

func routePolicyStage(policy RoutePolicy) gettokensrouting.PolicyStage {
	if staged, ok := policy.(stagedRoutePolicy); ok {
		return staged.RoutePolicyStage()
	}
	return gettokensrouting.PolicyStageRequest
}

func routePolicyName(policy RoutePolicy) string {
	if policy == nil {
		return ""
	}
	name := routePolicyTypeName(policy)
	if name == "" {
		return "route-policy"
	}
	return name
}

func routePolicyTypeName(value any) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(reflect.TypeOf(value).String()), "*"), "auth."))
}

func routeCandidatesFromScheduled(entries []*scheduledAuth) []gettokensrouting.RouteCandidate {
	out := make([]gettokensrouting.RouteCandidate, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || strings.TrimSpace(entry.auth.ID) == "" {
			continue
		}
		out = append(out, gettokensrouting.RouteCandidate{
			ID:    strings.TrimSpace(entry.auth.ID),
			Value: entry,
		})
	}
	return out
}

func authCandidatesFromRouteCandidates(candidates []gettokensrouting.RouteCandidate) []*Auth {
	out := make([]*Auth, 0, len(candidates))
	for _, candidate := range candidates {
		entry, ok := candidate.Value.(*scheduledAuth)
		if !ok || entry == nil || entry.auth == nil {
			continue
		}
		out = append(out, entry.auth.Clone())
	}
	return out
}

func scheduledFromRouteCandidates(candidates []gettokensrouting.RouteCandidate) []*scheduledAuth {
	out := make([]*scheduledAuth, 0, len(candidates))
	for _, candidate := range candidates {
		entry, ok := candidate.Value.(*scheduledAuth)
		if !ok || entry == nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func cloneAuthCandidates(entries []*scheduledAuth) []*Auth {
	out := make([]*Auth, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.auth == nil {
			continue
		}
		out = append(out, entry.auth.Clone())
	}
	return out
}

func safeEvaluateRoutePolicy(policy RoutePolicy, ctx context.Context, req RoutePolicyRequest) (decision RoutePolicyDecision, ok bool) {
	defer func() {
		if recover() != nil {
			decision = RoutePolicyDecision{}
			ok = false
		}
	}()
	decision = policy.RewriteCandidates(ctx, req)
	ok = true
	return decision, ok
}

func routePolicyDecisionActive(decision RoutePolicyDecision) bool {
	return len(decision.AllowIDs) > 0 ||
		len(decision.DenyIDs) > 0 ||
		len(decision.OrderIDs) > 0 ||
		decision.AllowFallback != nil
}

func applyRoutePolicyDecision(entries []*scheduledAuth, decision RoutePolicyDecision) []*scheduledAuth {
	if len(entries) == 0 {
		return entries
	}
	deny := authIDSet(decision.DenyIDs)
	allow := authIDSet(decision.AllowIDs)
	order := normalizeAuthIDs(decision.OrderIDs)

	fallback := true
	if len(allow) > 0 {
		fallback = false
	}
	if decision.AllowFallback != nil {
		fallback = *decision.AllowFallback
	}

	filtered := make([]*scheduledAuth, 0, len(entries))
	byID := make(map[string]*scheduledAuth, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || strings.TrimSpace(entry.auth.ID) == "" {
			continue
		}
		id := strings.TrimSpace(entry.auth.ID)
		if _, denied := deny[id]; denied {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[id]; !ok && !fallback {
				continue
			}
		}
		filtered = append(filtered, entry)
		if _, exists := byID[id]; !exists {
			byID[id] = entry
		}
	}
	if len(filtered) == 0 {
		return filtered
	}

	out := make([]*scheduledAuth, 0, len(filtered))
	used := make(map[string]struct{}, len(filtered))
	for _, id := range order {
		entry := byID[id]
		if entry == nil {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[id]; !ok && !fallback {
				continue
			}
		}
		out = append(out, entry)
		used[id] = struct{}{}
	}
	if len(allow) > 0 && len(order) == 0 {
		for _, entry := range filtered {
			id := strings.TrimSpace(entry.auth.ID)
			if _, ok := allow[id]; ok {
				out = append(out, entry)
				used[id] = struct{}{}
			}
		}
	}
	if fallback {
		for _, entry := range filtered {
			id := strings.TrimSpace(entry.auth.ID)
			if _, ok := used[id]; ok {
				continue
			}
			out = append(out, entry)
		}
	}
	return out
}

func authIDSet(ids []string) map[string]struct{} {
	normalized := normalizeAuthIDs(ids)
	if len(normalized) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(normalized))
	for _, id := range normalized {
		out[id] = struct{}{}
	}
	return out
}

func normalizeAuthIDs(ids []string) []string {
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
