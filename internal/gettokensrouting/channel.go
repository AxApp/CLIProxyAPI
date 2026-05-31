package gettokensrouting

import (
	"sort"
	"strconv"
	"strings"
)

type ChannelRouteMode string

const (
	ChannelRouteModeSequential ChannelRouteMode = "sequential"
	ChannelRouteModeBalanced   ChannelRouteMode = "balanced"
)

type AccountSnapshot struct {
	ID             string
	Enabled        bool
	Requestable    bool
	RouteOrder     int
	GroupIDs       []string
	ActiveSessions int
}

type AccountGroupSnapshot struct {
	ID         string
	Enabled    bool
	RouteOrder int
}

type ChannelGroupState struct {
	Enabled    bool
	RouteOrder *int
}

type ChannelRoutingConfig struct {
	Channel            string
	RouteMode          ChannelRouteMode
	OrderedAccountIDs  []string
	ChannelGroupStates map[string]ChannelGroupState
}

type ChannelRouteRequest struct {
	Tried map[string]struct{}
}

type ChannelRouteDecision struct {
	SelectedID string
	Candidates []RouteableCandidate
	Filtered   []FilteredAccount
	Steps      []string
}

type RouteableCandidate struct {
	Account      AccountSnapshot
	EffectiveKey routeSortKey
}

type FilteredAccount struct {
	AccountID string
	Reason    string
}

type routeSortKey struct {
	GroupOrder   int
	AccountOrder int
	ChannelOrder int
	AccountID    string
}

func DecideChannelRoute(accounts []AccountSnapshot, groups []AccountGroupSnapshot, cfg ChannelRoutingConfig, req ChannelRouteRequest) ChannelRouteDecision {
	mode := normalizeChannelRouteMode(cfg.RouteMode, ChannelRouteModeSequential)
	steps := []string{"mode:" + string(mode)}
	return decideScopedRoute(accounts, groups, cfg, req, mode, steps)
}

func decideScopedRoute(accounts []AccountSnapshot, groups []AccountGroupSnapshot, cfg ChannelRoutingConfig, req ChannelRouteRequest, mode ChannelRouteMode, steps []string) ChannelRouteDecision {
	candidates, filtered := BuildRouteablePool(accounts, groups, cfg, req)
	steps = append(steps, "candidates:"+strconv.Itoa(len(candidates)))
	var selected string
	switch normalizeChannelRouteMode(mode, ChannelRouteModeSequential) {
	case ChannelRouteModeBalanced:
		selected = selectBalanced(candidates)
	default:
		selected = selectSequential(candidates)
	}
	return ChannelRouteDecision{
		SelectedID: selected,
		Candidates: candidates,
		Filtered:   filtered,
		Steps:      steps,
	}
}

func BuildRouteablePool(accounts []AccountSnapshot, groups []AccountGroupSnapshot, cfg ChannelRoutingConfig, req ChannelRouteRequest) ([]RouteableCandidate, []FilteredAccount) {
	groupLookup := map[string]AccountGroupSnapshot{}
	for _, group := range groups {
		id := strings.TrimSpace(group.ID)
		if id == "" {
			continue
		}
		group.ID = id
		groupLookup[id] = group
	}
	channelOrder := orderedIDRank(cfg.OrderedAccountIDs)
	candidates := make([]RouteableCandidate, 0, len(accounts))
	filtered := make([]FilteredAccount, 0)
	for _, account := range accounts {
		account.ID = strings.TrimSpace(account.ID)
		if account.ID == "" {
			continue
		}
		if _, tried := req.Tried[account.ID]; tried {
			filtered = append(filtered, FilteredAccount{AccountID: account.ID, Reason: "tried"})
			continue
		}
		if !account.Enabled {
			filtered = append(filtered, FilteredAccount{AccountID: account.ID, Reason: "account-disabled"})
			continue
		}
		if !account.Requestable {
			filtered = append(filtered, FilteredAccount{AccountID: account.ID, Reason: "account-unrequestable"})
			continue
		}
		groupID, groupOrder, ok := effectiveGroupForAccount(account, groupLookup, cfg.ChannelGroupStates)
		if !ok {
			filtered = append(filtered, FilteredAccount{AccountID: account.ID, Reason: "group-disabled-or-missing"})
			continue
		}
		_ = groupID
		candidates = append(candidates, RouteableCandidate{
			Account: account,
			EffectiveKey: routeSortKey{
				GroupOrder:   groupOrder,
				AccountOrder: account.RouteOrder,
				ChannelOrder: lookupRank(channelOrder, account.ID),
				AccountID:    account.ID,
			},
		})
	}
	sortRouteableCandidates(candidates)
	return candidates, filtered
}

func effectiveGroupForAccount(account AccountSnapshot, groups map[string]AccountGroupSnapshot, channelStates map[string]ChannelGroupState) (string, int, bool) {
	groupIDs := normalizeIDs(account.GroupIDs)
	if len(groupIDs) == 0 {
		return "", 0, true
	}
	bestID := ""
	bestOrder := 0
	found := false
	for _, groupID := range groupIDs {
		id, order, ok := effectiveGroupOrder(groupID, groups, channelStates)
		if !ok {
			continue
		}
		if !found || order < bestOrder || (order == bestOrder && id < bestID) {
			bestID = id
			bestOrder = order
			found = true
		}
	}
	return bestID, bestOrder, found
}

func effectiveGroupOrder(groupID string, groups map[string]AccountGroupSnapshot, channelStates map[string]ChannelGroupState) (string, int, bool) {
	groupID = strings.TrimSpace(groupID)
	group, ok := groups[groupID]
	if !ok || !group.Enabled {
		return "", 0, false
	}
	if state, exists := channelStates[groupID]; exists {
		if !state.Enabled {
			return "", 0, false
		}
		if state.RouteOrder != nil {
			return groupID, *state.RouteOrder, true
		}
	}
	return groupID, group.RouteOrder, true
}

func selectSequential(candidates []RouteableCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].Account.ID
}

func selectBalanced(candidates []RouteableCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.Account.ActiveSessions < best.Account.ActiveSessions {
			best = candidate
			continue
		}
		if candidate.Account.ActiveSessions == best.Account.ActiveSessions && lessRouteSortKey(candidate.EffectiveKey, best.EffectiveKey) {
			best = candidate
		}
	}
	return best.Account.ID
}

func sortRouteableCandidates(candidates []RouteableCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		return lessRouteSortKey(candidates[i].EffectiveKey, candidates[j].EffectiveKey)
	})
}

func lessRouteSortKey(left, right routeSortKey) bool {
	if left.GroupOrder != right.GroupOrder {
		return left.GroupOrder < right.GroupOrder
	}
	if left.ChannelOrder != right.ChannelOrder {
		return left.ChannelOrder < right.ChannelOrder
	}
	if left.AccountOrder != right.AccountOrder {
		return left.AccountOrder < right.AccountOrder
	}
	return left.AccountID < right.AccountID
}

func orderedIDRank(ids []string) map[string]int {
	out := make(map[string]int, len(ids))
	for index, id := range normalizeIDs(ids) {
		out[id] = index
	}
	return out
}

func lookupRank(ranks map[string]int, id string) int {
	if rank, ok := ranks[id]; ok {
		return rank
	}
	return 1_000_000
}

func normalizeChannelRouteMode(mode ChannelRouteMode, fallback ChannelRouteMode) ChannelRouteMode {
	switch mode {
	case ChannelRouteModeSequential, ChannelRouteModeBalanced:
		return mode
	default:
		return fallback
	}
}
