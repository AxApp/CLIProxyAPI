package gettokensrouting

import (
	"context"
	"sort"
	"strings"
	"time"

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
	Provider   string
	Providers  []string
	Model      string
	Options    cliproxyexecutor.Options
	Candidates []RouteCandidate
	Tried      map[string]struct{}
	Now        time.Time
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

type PolicyDecision struct {
	AllowIDs      []string
	DenyIDs       []string
	OrderIDs      []string
	AllowFallback *bool
	Reason        string
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
