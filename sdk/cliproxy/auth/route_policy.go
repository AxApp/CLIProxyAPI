package auth

import (
	"context"
	"strings"
	"sync"
	"time"

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
	out := make([]RoutePolicy, 0, len(routePolicies.items))
	for _, policy := range routePolicies.items {
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
	current := append([]*scheduledAuth(nil), entries...)
	active := false
	for _, policy := range policies {
		req.Candidates = cloneAuthCandidates(current)
		decision, ok := safeEvaluateRoutePolicy(policy, ctx, req)
		if !ok || !routePolicyDecisionActive(decision) {
			continue
		}
		active = true
		current = applyRoutePolicyDecision(current, decision)
	}
	return current, active
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
