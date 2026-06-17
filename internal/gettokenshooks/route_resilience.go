package gettokenshooks

import (
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type RouteResilienceScope string

const (
	RouteResilienceScopeAccount  RouteResilienceScope = "account"
	RouteResilienceScopeProvider RouteResilienceScope = "provider"
	RouteResilienceScopeModel    RouteResilienceScope = "model"
)

type RouteResilienceState struct {
	Source        string               `json:"source"`
	Scope         RouteResilienceScope `json:"scope"`
	AuthID        string               `json:"authID,omitempty"`
	AccountKey    string               `json:"accountKey,omitempty"`
	LookupKeys    []string             `json:"lookupKeys,omitempty"`
	Model         string               `json:"model,omitempty"`
	Reason        string               `json:"reason,omitempty"`
	ExpiresAt     time.Time            `json:"expiresAt,omitempty"`
	UpdatedAt     time.Time            `json:"updatedAt,omitempty"`
	RouteBlocking bool                 `json:"routeBlocking"`
}

type ChannelRoutingDroppedReason struct {
	AccountID     string               `json:"accountID,omitempty"`
	AuthID        string               `json:"authID,omitempty"`
	Source        string               `json:"source"`
	Scope         RouteResilienceScope `json:"scope"`
	Reason        string               `json:"reason,omitempty"`
	Model         string               `json:"model,omitempty"`
	ExpiresAt     string               `json:"expiresAt,omitempty"`
	UpdatedAt     string               `json:"updatedAt,omitempty"`
	RouteBlocking bool                 `json:"routeBlocking"`
}

func (s *AccountRouteGuardStore) RouteResilienceStatesForAuth(auth *coreauth.Auth) []RouteResilienceState {
	if s == nil || auth == nil {
		return nil
	}
	return routeResilienceStatesFromBlocks(s.ActiveBlocksForAuth(auth))
}

func RouteResilienceStatesForAuth(auth *coreauth.Auth) []RouteResilienceState {
	return defaultAccountRouteGuardStore.RouteResilienceStatesForAuth(auth)
}

func routeResilienceStatesFromBlocks(blocks []AccountRouteGuardBlock) []RouteResilienceState {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]RouteResilienceState, 0, len(blocks))
	for _, block := range blocks {
		state, ok := routeResilienceStateFromBlock(block)
		if ok {
			out = append(out, state)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].AccountKey != out[j].AccountKey {
			return out[i].AccountKey < out[j].AccountKey
		}
		return out[i].AuthID < out[j].AuthID
	})
	return out
}

func routeResilienceStateFromBlock(block AccountRouteGuardBlock) (RouteResilienceState, bool) {
	block = normalizeAccountRouteGuardBlock(block)
	if strings.TrimSpace(block.Source) == "" || accountRouteGuardBlockKey(block) == "" {
		return RouteResilienceState{}, false
	}
	return RouteResilienceState{
		Source:        strings.TrimSpace(block.Source),
		Scope:         normalizeRouteResilienceScope(block.FailureScope, block.Source),
		AuthID:        strings.TrimSpace(block.AuthID),
		AccountKey:    strings.TrimSpace(block.AccountKey),
		LookupKeys:    append([]string(nil), block.LookupKeys...),
		Model:         strings.TrimSpace(block.Model),
		Reason:        strings.TrimSpace(block.Reason),
		ExpiresAt:     block.ExpiresAt,
		UpdatedAt:     block.UpdatedAt,
		RouteBlocking: true,
	}, true
}

func normalizeRouteResilienceScope(scope RouteResilienceScope, source string) RouteResilienceScope {
	switch RouteResilienceScope(strings.TrimSpace(string(scope))) {
	case RouteResilienceScopeAccount:
		return RouteResilienceScopeAccount
	case RouteResilienceScopeProvider:
		return RouteResilienceScopeProvider
	case RouteResilienceScopeModel:
		return RouteResilienceScopeModel
	default:
		return routeResilienceScopeForRouteGuardSource(source)
	}
}

func channelRoutingDroppedReasonsFromBlocks(accountID string, blocks []AccountRouteGuardBlock) []ChannelRoutingDroppedReason {
	states := routeResilienceStatesFromBlocks(blocks)
	if len(states) == 0 {
		return nil
	}
	out := make([]ChannelRoutingDroppedReason, 0, len(states))
	for _, state := range states {
		if reason, ok := channelRoutingDroppedReasonFromRouteResilienceState(accountID, state); ok {
			out = append(out, reason)
		}
	}
	return out
}

func channelRoutingDroppedReasonFromRouteResilienceState(accountID string, state RouteResilienceState) (ChannelRoutingDroppedReason, bool) {
	source := strings.TrimSpace(state.Source)
	if source == "" {
		return ChannelRoutingDroppedReason{}, false
	}
	reason := ChannelRoutingDroppedReason{
		AccountID:     firstNonEmptyRouteGuardString(accountID, state.AccountKey),
		AuthID:        strings.TrimSpace(state.AuthID),
		Source:        source,
		Scope:         normalizeRouteResilienceScope(state.Scope, source),
		Reason:        strings.TrimSpace(state.Reason),
		Model:         strings.TrimSpace(state.Model),
		RouteBlocking: state.RouteBlocking,
	}
	if !state.ExpiresAt.IsZero() {
		reason.ExpiresAt = state.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if !state.UpdatedAt.IsZero() {
		reason.UpdatedAt = state.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return reason, true
}

func routeResilienceScopeForRouteGuardSource(source string) RouteResilienceScope {
	switch strings.TrimSpace(source) {
	case AccountRouteGuardSourceManualDisabled,
		AccountRouteGuardSourceRateLimit,
		AccountRouteGuardSourceQuotaEmpty,
		AccountRouteGuardSourceAuthError,
		AccountRouteGuardSourceUpstreamRateLimit,
		AccountRouteGuardSourceUpstreamTransientErr:
		return RouteResilienceScopeAccount
	default:
		return RouteResilienceScopeAccount
	}
}
