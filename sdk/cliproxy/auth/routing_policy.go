package auth

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenscodex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type routeRequest struct {
	Provider  string
	Providers []string
	Model     string
	Options   cliproxyexecutor.Options
	Tried     map[string]struct{}
	Now       time.Time
}

func rewriteScheduledAuthsWithPolicies(ctx context.Context, req routeRequest, entries []*scheduledAuth, extraPolicies []gettokensrouting.Policy) ([]*scheduledAuth, gettokensrouting.RouteResult, bool) {
	if len(entries) == 0 {
		return entries, gettokensrouting.RouteResult{Candidates: []gettokensrouting.RouteCandidate{}, Trace: []gettokensrouting.DecisionStep{}}, false
	}
	policies := routeRewritePolicies(extraPolicies)
	if len(policies) == 0 {
		return entries, gettokensrouting.RouteResult{}, false
	}
	result := gettokensrouting.NewEngine(policies...).Route(ctx, routeContextFromRouteRequest(req, routeCandidatesFromScheduled(entries)))
	if !routeResultActive(result) {
		return entries, result, false
	}
	return scheduledFromRouteCandidates(result.Candidates, entries), result, true
}

func routeRewritePolicies(extraPolicies []gettokensrouting.Policy) []gettokensrouting.Policy {
	policies := gettokensrouting.PolicySnapshot()
	for _, policy := range extraPolicies {
		if policy.Rewrite != nil {
			policies = append(policies, policy)
		}
	}
	return policies
}

func routeResultActive(result gettokensrouting.RouteResult) bool {
	for _, step := range result.Trace {
		if step.Activated {
			return true
		}
	}
	return false
}

func (s *SessionAffinitySelector) routingPolicy() gettokensrouting.Policy {
	if s == nil {
		return gettokensrouting.Policy{}
	}
	return gettokensrouting.Policy{
		Stage:   gettokensrouting.PolicyStageSticky,
		Name:    "session-affinity",
		Rewrite: s.rewriteRouteCandidates,
	}
}

func (s *SessionAffinitySelector) rewriteRouteCandidates(ctx context.Context, routeCtx gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
	if s == nil || s.cache == nil || len(routeCtx.Candidates) == 0 {
		return gettokensrouting.PolicyDecision{}
	}
	primaryID, fallbackID := extractSessionIDs(routeCtx.Options.Headers, routeCtx.Options.OriginalRequest, routeCtx.Options.Metadata)
	if strings.TrimSpace(primaryID) == "" {
		return gettokensrouting.PolicyDecision{}
	}
	providerKey := sessionAffinityProviderKey(routeCtx.Provider)
	modelKey := routeCtx.Model
	candidateIDs := make(map[string]struct{}, len(routeCtx.Candidates))
	for _, candidate := range routeCtx.Candidates {
		id := strings.TrimSpace(candidate.ID)
		if id == "" {
			continue
		}
		candidateIDs[id] = struct{}{}
	}
	cacheKey := sessionAffinityCacheKey(providerKey, primaryID, modelKey)
	if cachedAuthID, ok := s.cache.GetAndRefresh(cacheKey); ok {
		if _, exists := candidateIDs[cachedAuthID]; exists {
			return gettokensrouting.PolicyDecision{OrderIDs: []string{cachedAuthID}, Reason: "session-affinity cache hit"}
		}
	}
	if fallbackID != "" && fallbackID != primaryID {
		fallbackKey := sessionAffinityCacheKey(providerKey, fallbackID, modelKey)
		if cachedAuthID, ok := s.cache.Get(fallbackKey); ok {
			if _, exists := candidateIDs[cachedAuthID]; exists {
				s.cache.Set(cacheKey, cachedAuthID)
				return gettokensrouting.PolicyDecision{OrderIDs: []string{cachedAuthID}, Reason: "session-affinity fallback cache hit"}
			}
		}
	}
	return gettokensrouting.PolicyDecision{}
}

func (s *SessionAffinitySelector) bindRouteResult(req routeRequest, auth *Auth) {
	if s == nil || s.cache == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	primaryID, _ := extractSessionIDs(req.Options.Headers, req.Options.OriginalRequest, req.Options.Metadata)
	if strings.TrimSpace(primaryID) == "" {
		return
	}
	s.cache.Set(sessionAffinityCacheKey(sessionAffinityProviderKey(req.Provider), primaryID, req.Model), strings.TrimSpace(auth.ID))
}

func sessionAffinityProviderKey(provider string) string {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return "mixed"
	}
	return provider
}

func sessionAffinityCacheKey(provider, sessionID, model string) string {
	return sessionAffinityProviderKey(provider) + "::" + strings.TrimSpace(sessionID) + "::" + strings.TrimSpace(model)
}

func rewriteAuthCandidates(ctx context.Context, req routeRequest, auths []*Auth) ([]*Auth, gettokensrouting.RouteResult, bool) {
	if len(auths) == 0 {
		return auths, gettokensrouting.RouteResult{Candidates: []gettokensrouting.RouteCandidate{}, Trace: []gettokensrouting.DecisionStep{}}, false
	}
	policies := gettokensrouting.PolicySnapshot()
	if len(policies) == 0 {
		return auths, routeResultFromAuths(auths), false
	}
	result := gettokensrouting.NewEngine(policies...).Route(ctx, routeContextFromRouteRequest(req, routeCandidatesFromAuths(auths)))
	if !routeResultActive(result) {
		return auths, result, false
	}
	return authsFromRouteCandidates(result.Candidates, auths), result, true
}

func routeCandidatesFromScheduled(entries []*scheduledAuth) []gettokensrouting.RouteCandidate {
	out := make([]gettokensrouting.RouteCandidate, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || strings.TrimSpace(entry.auth.ID) == "" {
			continue
		}
		out = append(out, gettokensrouting.RouteCandidate{
			ID:    strings.TrimSpace(entry.auth.ID),
			Value: entry.auth.Clone(),
		})
	}
	return out
}

func routeCandidateFromScheduled(entry *scheduledAuth) (gettokensrouting.RouteCandidate, bool) {
	if entry == nil || entry.auth == nil || strings.TrimSpace(entry.auth.ID) == "" {
		return gettokensrouting.RouteCandidate{}, false
	}
	return gettokensrouting.RouteCandidate{
		ID:    strings.TrimSpace(entry.auth.ID),
		Value: entry.auth.Clone(),
	}, true
}

func routeCandidatesFromAuths(auths []*Auth) []gettokensrouting.RouteCandidate {
	out := make([]gettokensrouting.RouteCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		out = append(out, gettokensrouting.RouteCandidate{
			ID:    strings.TrimSpace(auth.ID),
			Value: auth.Clone(),
		})
	}
	return out
}

func routeResultFromScheduledEntries(entries []*scheduledAuth) gettokensrouting.RouteResult {
	candidates := make([]gettokensrouting.RouteCandidate, 0, min(len(entries), routeDecisionSnapshotSampleLimit))
	for _, entry := range entries {
		candidate, ok := routeCandidateFromScheduled(entry)
		if !ok {
			continue
		}
		if len(candidates) >= routeDecisionSnapshotSampleLimit {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return gettokensrouting.RouteResult{
		Candidates:     candidates,
		CandidateCount: len(entries),
		Trace:          []gettokensrouting.DecisionStep{},
	}
}

func routeResultFromAuths(auths []*Auth) gettokensrouting.RouteResult {
	return gettokensrouting.RouteResult{
		Candidates: routeCandidatesFromAuths(auths),
		Trace:      []gettokensrouting.DecisionStep{},
	}
}

func scheduledFromRouteCandidates(candidates []gettokensrouting.RouteCandidate, entries []*scheduledAuth) []*scheduledAuth {
	byID := make(map[string]*scheduledAuth, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || strings.TrimSpace(entry.auth.ID) == "" {
			continue
		}
		id := strings.TrimSpace(entry.auth.ID)
		if _, exists := byID[id]; !exists {
			byID[id] = entry
		}
	}
	out := make([]*scheduledAuth, 0, len(candidates))
	for _, candidate := range candidates {
		entry := byID[strings.TrimSpace(candidate.ID)]
		if entry == nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func authsFromRouteCandidates(candidates []gettokensrouting.RouteCandidate, auths []*Auth) []*Auth {
	byID := make(map[string]*Auth, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		id := strings.TrimSpace(auth.ID)
		if _, exists := byID[id]; !exists {
			byID[id] = auth
		}
	}
	out := make([]*Auth, 0, len(candidates))
	for _, candidate := range candidates {
		auth := byID[strings.TrimSpace(candidate.ID)]
		if auth == nil {
			continue
		}
		out = append(out, auth)
	}
	return out
}

func admitAuthCandidate(ctx context.Context, req routeRequest, auth *Auth) gettokensrouting.AdmissionDecision {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return gettokensrouting.AdmissionDecision{}
	}
	candidate := gettokensrouting.RouteCandidate{
		ID:    strings.TrimSpace(auth.ID),
		Value: auth.Clone(),
	}
	return gettokensrouting.AdmitCandidate(ctx, routeContextFromRouteRequest(req, []gettokensrouting.RouteCandidate{candidate}), candidate)
}

func routeContextFromRouteRequest(req routeRequest, candidates []gettokensrouting.RouteCandidate) gettokensrouting.RouteContext {
	codexRequest := gettokenscodex.RequestContextFromMetadata(req.Options.Metadata)
	routeCtx := gettokensrouting.RouteContext{
		Provider:     req.Provider,
		Providers:    append([]string(nil), req.Providers...),
		Model:        req.Model,
		Options:      req.Options,
		CodexRequest: codexRequest,
		Candidates:   candidates,
		Tried:        req.Tried,
		Now:          req.Now,
	}
	if codexRequest != nil {
		routeCtx.ProjectKey = strings.TrimSpace(codexRequest.ProjectKey)
		routeCtx.ProjectName = strings.TrimSpace(codexRequest.ProjectName)
		routeCtx.ProjectKeySource = strings.TrimSpace(codexRequest.ProjectKeySource)
		routeCtx.ProjectKeyConfidence = strings.TrimSpace(codexRequest.ProjectKeyConfidence)
		routeCtx.ProjectMatchKeys = append([]string(nil), codexRequest.ProjectMatchKeys...)
	}
	return routeCtx
}

func markAdmissionDenied(tried map[string]struct{}, auth *Auth) map[string]struct{} {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return tried
	}
	if tried == nil {
		tried = make(map[string]struct{})
	}
	tried[auth.ID] = struct{}{}
	return tried
}

func wrapAdmissionStreamResult(ctx context.Context, result *cliproxyexecutor.StreamResult, lease gettokensrouting.AdmissionLease) *cliproxyexecutor.StreamResult {
	if result == nil || !lease.Active() {
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		success := true
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				success = false
			}
			if ctx == nil {
				out <- chunk
				continue
			}
			select {
			case <-ctx.Done():
				success = false
				discardStreamChunks(result.Chunks)
				lease.Release(ctx)
				return
			case out <- chunk:
			}
		}
		if success {
			lease.Commit(ctx)
		} else {
			lease.Release(ctx)
		}
	}()
	return &cliproxyexecutor.StreamResult{
		Headers: result.Headers,
		Chunks:  out,
	}
}
