package gettokenshooks

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type ChannelRoutingExplainRequest struct {
	Channel              string   `json:"channel"`
	RequestedModel       string   `json:"requestedModel,omitempty"`
	TriedAccountIDs      []string `json:"triedAccountIDs,omitempty"`
	StickyAccountID      string   `json:"stickyAccountID,omitempty"`
	ProjectKey           string   `json:"projectKey,omitempty"`
	ProjectName          string   `json:"projectName,omitempty"`
	ProjectKeySource     string   `json:"projectKeySource,omitempty"`
	ProjectKeyConfidence string   `json:"projectKeyConfidence,omitempty"`
	ProjectMatchKeys     []string `json:"projectMatchKeys,omitempty"`
}

type ChannelRoutingExplainResponse struct {
	Channel              string                                 `json:"channel"`
	RouteMode            gettokensrouting.ChannelRouteMode      `json:"routeMode"`
	RequestedModel       string                                 `json:"requestedModel,omitempty"`
	SelectedAccountID    string                                 `json:"selectedAccountID,omitempty"`
	Candidates           []ChannelRoutingExplainCandidate       `json:"candidates"`
	Filtered             []ChannelRoutingExplainFilteredAccount `json:"filtered"`
	Steps                []string                               `json:"steps"`
	SnapshotVersion      string                                 `json:"snapshotVersion,omitempty"`
	PolicyVersion        string                                 `json:"policyVersion,omitempty"`
	ProjectCandidatePool *ChannelRoutingExplainProjectCandidate `json:"projectCandidatePool,omitempty"`
	Shadow               *ChannelRoutingExplainShadowDecision   `json:"shadow,omitempty"`
}

type ChannelRoutingExplainCandidate struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"displayName,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	RouteOrder     int      `json:"routeOrder,omitempty"`
	GroupID        string   `json:"groupID,omitempty"`
	GroupOrder     int      `json:"groupOrder,omitempty"`
	ChannelOrder   int      `json:"channelOrder,omitempty"`
	ActiveSessions int      `json:"activeSessions,omitempty"`
	RouteIDs       []string `json:"routeIDs,omitempty"`
}

type ChannelRoutingExplainFilteredAccount struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type ChannelRoutingExplainProjectCandidate struct {
	Evaluated            bool     `json:"evaluated"`
	Activated            bool     `json:"activated"`
	Reason               string   `json:"reason,omitempty"`
	RuleID               string   `json:"ruleID,omitempty"`
	ProjectKey           string   `json:"projectKey,omitempty"`
	ProjectName          string   `json:"projectName,omitempty"`
	ProjectKeySource     string   `json:"projectKeySource,omitempty"`
	ProjectKeyConfidence string   `json:"projectKeyConfidence,omitempty"`
	AllowAccountIDs      []string `json:"allowAccountIDs,omitempty"`
	FilteredAccountIDs   []string `json:"filteredAccountIDs,omitempty"`
	BeforeCandidateCount int      `json:"beforeCandidateCount,omitempty"`
	AfterCandidateCount  int      `json:"afterCandidateCount,omitempty"`
}

type ChannelRoutingExplainShadowDecision struct {
	Enabled           bool                              `json:"enabled"`
	RouteMode         gettokensrouting.ChannelRouteMode `json:"routeMode,omitempty"`
	SelectedAccountID string                            `json:"selectedAccountID,omitempty"`
	Candidates        []ChannelRoutingExplainCandidate  `json:"candidates,omitempty"`
	Diff              bool                              `json:"diff"`
	Steps             []string                          `json:"steps,omitempty"`
}

type explainRuntimeAccount struct {
	ID          string
	DisplayName string
	Provider    string
	RouteOrder  int
	RouteIDs    []string
	ModelSet    map[string]struct{}
}

type explainRuntimeCandidate struct {
	Account gettokensrouting.AccountSnapshot
	View    explainRuntimeAccount
	GroupID string
	Key     routeSortKey
}

type routeSortKey struct {
	GroupOrder   int
	AccountOrder int
	ChannelOrder int
	AccountID    string
}

func ConfigureChannelRoutingExplainRoutes(group *gin.RouterGroup, handler *handlers.BaseAPIHandler, _ *config.Config) {
	if group == nil {
		return
	}
	group.POST("/gettokens/channel-routing/explain", func(c *gin.Context) {
		var input ChannelRoutingExplainRequest
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		channel := strings.TrimSpace(strings.ToLower(input.Channel))
		if channel != "codex" && channel != "claude" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "channel must be codex or claude"})
			return
		}
		if handler == nil || handler.AuthManager == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager is not initialized"})
			return
		}
		cfg, ok := loadChannelRoutingPolicyConfig(channel)
		if !ok {
			cfg = channelRoutingPolicyConfig{
				Channel:            channel,
				RouteMode:          gettokensrouting.ChannelRouteModeSequential,
				OrderedAccountIDs:  []string{},
				ChannelGroupStates: map[string]gettokensrouting.ChannelGroupState{},
			}
		}
		projectRules := []ProjectCandidatePoolRule{}
		if store, ok := loadProjectCandidatePoolPolicyStore(); ok {
			projectRules = listProjectCandidatePoolRules(store, channel)
		}
		result := explainChannelRoutingRuntime(handler.AuthManager.SnapshotAuths(), cfg, input, projectRules)
		c.JSON(http.StatusOK, result)
	})
}

func explainChannelRoutingRuntime(auths []*coreauth.Auth, cfg channelRoutingPolicyConfig, input ChannelRoutingExplainRequest, projectRules []ProjectCandidatePoolRule) ChannelRoutingExplainResponse {
	normalized := normalizeChannelRoutingExplainConfig(cfg)
	result := decideChannelRoutingRuntime(auths, normalized, input, projectRules)
	result.SnapshotVersion = channelRoutingExplainSnapshotVersion(normalized)
	result.PolicyVersion = "channel-routing-sidecar-v1"
	shadowMode := gettokensrouting.ChannelRouteModeSequential
	if normalized.RouteMode == gettokensrouting.ChannelRouteModeSequential {
		shadowMode = gettokensrouting.ChannelRouteModeBalanced
	}
	shadowConfig := normalized
	shadowConfig.RouteMode = shadowMode
	shadow := decideChannelRoutingRuntime(auths, shadowConfig, input, projectRules)
	result.Shadow = &ChannelRoutingExplainShadowDecision{
		Enabled:           true,
		RouteMode:         shadow.RouteMode,
		SelectedAccountID: shadow.SelectedAccountID,
		Candidates:        append([]ChannelRoutingExplainCandidate(nil), shadow.Candidates...),
		Diff:              shadow.SelectedAccountID != result.SelectedAccountID,
		Steps:             append([]string(nil), shadow.Steps...),
	}
	return result
}

func decideChannelRoutingRuntime(auths []*coreauth.Auth, cfg channelRoutingPolicyConfig, input ChannelRoutingExplainRequest, projectRules []ProjectCandidatePoolRule) ChannelRoutingExplainResponse {
	steps := []string{"mode:" + string(cfg.RouteMode)}
	candidates, filtered := buildChannelRoutingRuntimePool(auths, cfg, input)
	requestedModel := strings.TrimSpace(input.RequestedModel)
	if requestedModel != "" {
		candidates, filtered = applyRequestedModelRuntimeExplain(candidates, filtered, requestedModel)
		steps = append(steps, "model:"+requestedModel)
	}
	candidates, filtered, projectInfo := applyProjectCandidatePoolRuntimeExplain(candidates, filtered, cfg.Channel, input, projectRules)
	if projectInfo != nil && projectInfo.Reason != "" {
		steps = append(steps, projectInfo.Reason)
	}
	ordered := orderRuntimeExplainCandidates(candidates, cfg.RouteMode)
	steps = append(steps, "candidates:"+strconv.Itoa(len(ordered)))

	selected := ""
	stickyID := strings.TrimSpace(input.StickyAccountID)
	if stickyID != "" {
		if hit := findRuntimeExplainCandidate(ordered, stickyID); hit != nil {
			selected = hit.Account.ID
			steps = append(steps, "sticky:hit:"+selected)
		} else if reason, ok := findRuntimeExplainFilteredReason(filtered, stickyID); ok {
			steps = append(steps, "sticky:invalidated:"+reason)
		} else {
			steps = append(steps, "sticky:miss")
		}
	}
	if selected == "" && len(ordered) > 0 {
		selected = ordered[0].Account.ID
	}
	return ChannelRoutingExplainResponse{
		Channel:              cfg.Channel,
		RouteMode:            cfg.RouteMode,
		RequestedModel:       requestedModel,
		SelectedAccountID:    selected,
		Candidates:           mapRuntimeExplainCandidates(ordered),
		Filtered:             filtered,
		Steps:                steps,
		ProjectCandidatePool: projectInfo,
	}
}

func buildChannelRoutingRuntimePool(auths []*coreauth.Auth, cfg channelRoutingPolicyConfig, input ChannelRoutingExplainRequest) ([]explainRuntimeCandidate, []ChannelRoutingExplainFilteredAccount) {
	sessionCounts := currentLiveSessionActiveAuthCounts()
	accountGroups := make(map[string][]string)
	groups := make([]gettokensrouting.AccountGroupSnapshot, 0, len(cfg.AccountGroups))
	for _, group := range cfg.AccountGroups {
		groupID := strings.TrimSpace(group.ID)
		if groupID == "" {
			continue
		}
		groups = append(groups, gettokensrouting.AccountGroupSnapshot{
			ID:         groupID,
			Enabled:    group.Enabled,
			RouteOrder: group.RouteOrder,
		})
		for _, accountID := range group.AccountIDs {
			accountID = strings.TrimSpace(accountID)
			if accountID == "" {
				continue
			}
			accountGroups[accountID] = append(accountGroups[accountID], groupID)
		}
	}
	groupLookup := make(map[string]gettokensrouting.AccountGroupSnapshot, len(groups))
	for _, group := range groups {
		groupLookup[group.ID] = group
	}
	blockedByAuthID := mergeAccountRouteGuardBlocks(nil, activePersistedChannelRuntimeBlocksForCandidates(auths))
	for id, blocks := range DefaultAccountRouteGuardStore().ActiveBlocksForCandidates(auths) {
		if blockedByAuthID == nil {
			blockedByAuthID = map[string][]AccountRouteGuardBlock{}
		}
		blockedByAuthID[id] = append(blockedByAuthID[id], blocks...)
	}
	accountMap := map[string]*explainRuntimeCandidate{}
	order := orderedIDRank(cfg.OrderedAccountIDs)
	tried := idSet(input.TriedAccountIDs)

	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if !authSupportsChannel(auth, cfg.Channel) {
			continue
		}
		accountID := strings.TrimSpace(auth.AccountKey)
		if accountID == "" {
			accountID = "auth-id:" + strings.TrimSpace(auth.ID)
		}
		if accountID == "" {
			continue
		}
		item := accountMap[accountID]
		if item == nil {
			item = &explainRuntimeCandidate{
				Account: gettokensrouting.AccountSnapshot{
					ID:          accountID,
					Enabled:     false,
					Requestable: false,
					RouteOrder:  channelRoutingAuthRouteOrder(auth),
					GroupIDs:    append([]string(nil), accountGroups[accountID]...),
				},
				View: explainRuntimeAccount{
					ID:          accountID,
					DisplayName: firstNonEmptyString(strings.TrimSpace(auth.Label), accountID, strings.TrimSpace(auth.ID)),
					Provider:    strings.TrimSpace(auth.Provider),
					RouteOrder:  channelRoutingAuthRouteOrder(auth),
					RouteIDs:    []string{strings.TrimSpace(auth.ID)},
					ModelSet:    map[string]struct{}{},
				},
			}
			accountMap[accountID] = item
		}
		if routeOrder := channelRoutingAuthRouteOrder(auth); routeOrder < item.Account.RouteOrder {
			item.Account.RouteOrder = routeOrder
			item.View.RouteOrder = routeOrder
		}
		item.Account.ActiveSessions += sessionCounts[strings.TrimSpace(auth.ID)]
		item.Account.Enabled = item.Account.Enabled || (!auth.Disabled && auth.Status != coreauth.StatusDisabled)
		if !auth.Disabled && auth.Status != coreauth.StatusDisabled && !auth.Unavailable && auth.Status != coreauth.StatusError && len(blockedByAuthID[strings.TrimSpace(auth.ID)]) == 0 {
			item.Account.Requestable = true
		}
		if strings.TrimSpace(auth.Label) != "" {
			item.View.DisplayName = strings.TrimSpace(auth.Label)
		}
		if provider := strings.TrimSpace(auth.Provider); provider != "" {
			item.View.Provider = provider
		}
		if authID := strings.TrimSpace(auth.ID); authID != "" {
			item.View.RouteIDs = appendUniqueString(item.View.RouteIDs, authID)
			for _, model := range registry.GetGlobalRegistry().GetModelsForClient(authID) {
				if model == nil {
					continue
				}
				for _, key := range []string{strings.TrimSpace(model.ID), strings.TrimSpace(model.Name), strings.TrimSpace(model.DisplayName)} {
					if key != "" {
						item.View.ModelSet[key] = struct{}{}
					}
				}
			}
		}
	}

	accounts := make([]gettokensrouting.AccountSnapshot, 0, len(accountMap))
	viewByID := make(map[string]explainRuntimeAccount, len(accountMap))
	for accountID, candidate := range accountMap {
		accounts = append(accounts, candidate.Account)
		viewByID[accountID] = candidate.View
	}
	routeable, filtered := gettokensrouting.BuildRouteablePool(accounts, groups, gettokensrouting.ChannelRoutingConfig{
		Channel:            cfg.Channel,
		RouteMode:          cfg.RouteMode,
		OrderedAccountIDs:  cfg.OrderedAccountIDs,
		ChannelGroupStates: cfg.ChannelGroupStates,
	}, gettokensrouting.ChannelRouteRequest{Tried: tried})
	out := make([]explainRuntimeCandidate, 0, len(routeable))
	for _, item := range routeable {
		view := viewByID[item.Account.ID]
		groupID, groupOrder, _, _ := effectiveGroupForExplain(item.Account, groupLookup, cfg.ChannelGroupStates)
		out = append(out, explainRuntimeCandidate{
			Account: item.Account,
			View:    view,
			GroupID: groupID,
			Key: routeSortKey{
				GroupOrder:   groupOrder,
				AccountOrder: item.Account.RouteOrder,
				ChannelOrder: lookupRank(order, item.Account.ID),
				AccountID:    item.Account.ID,
			},
		})
	}
	return out, mapRuntimeExplainFiltered(filtered)
}

func applyRequestedModelRuntimeExplain(candidates []explainRuntimeCandidate, filtered []ChannelRoutingExplainFilteredAccount, requestedModel string) ([]explainRuntimeCandidate, []ChannelRoutingExplainFilteredAccount) {
	if requestedModel == "" {
		return candidates, filtered
	}
	kept := make([]explainRuntimeCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if len(candidate.View.ModelSet) == 0 {
			kept = append(kept, candidate)
			continue
		}
		if _, ok := candidate.View.ModelSet[requestedModel]; ok {
			kept = append(kept, candidate)
			continue
		}
		filtered = append(filtered, ChannelRoutingExplainFilteredAccount{ID: candidate.Account.ID, Reason: "runtime-model-unavailable"})
	}
	return kept, filtered
}

func applyProjectCandidatePoolRuntimeExplain(candidates []explainRuntimeCandidate, filtered []ChannelRoutingExplainFilteredAccount, channel string, input ChannelRoutingExplainRequest, rules []ProjectCandidatePoolRule) ([]explainRuntimeCandidate, []ChannelRoutingExplainFilteredAccount, *ChannelRoutingExplainProjectCandidate) {
	info := &ChannelRoutingExplainProjectCandidate{
		ProjectKey:           strings.TrimSpace(input.ProjectKey),
		ProjectName:          strings.TrimSpace(input.ProjectName),
		ProjectKeySource:     strings.TrimSpace(input.ProjectKeySource),
		ProjectKeyConfidence: strings.TrimSpace(input.ProjectKeyConfidence),
	}
	matchKeys := normalizeProjectCandidatePoolIDs(input.ProjectMatchKeys)
	if len(matchKeys) == 0 && info.ProjectKey != "" {
		matchKeys = []string{info.ProjectKey}
	}
	if strings.EqualFold(info.ProjectKeyConfidence, "ambiguous") {
		info.Evaluated = true
		info.Reason = "project-candidate-pool:not-evaluated:ambiguous-project"
		return candidates, filtered, info
	}
	if len(matchKeys) == 0 {
		return candidates, filtered, nil
	}
	info.Evaluated = true
	matches := matchingProjectCandidatePoolRules(rules, channel, matchKeys)
	switch len(matches) {
	case 0:
		info.Reason = "project-candidate-pool:not-matched"
		return candidates, filtered, info
	case 1:
		rule := matches[0]
		info.Activated = true
		info.RuleID = rule.ID
		info.AllowAccountIDs = append([]string(nil), rule.AllowAccountIDs...)
		info.BeforeCandidateCount = len(candidates)
		allowedSet := idSet(rule.AllowAccountIDs)
		kept := make([]explainRuntimeCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if _, ok := allowedSet[candidate.Account.ID]; ok {
				kept = append(kept, candidate)
				continue
			}
			filtered = append(filtered, ChannelRoutingExplainFilteredAccount{ID: candidate.Account.ID, Reason: "project-candidate-pool:filtered"})
			info.FilteredAccountIDs = append(info.FilteredAccountIDs, candidate.Account.ID)
		}
		info.AfterCandidateCount = len(kept)
		if len(kept) == 0 {
			info.Reason = "project-candidate-pool:no-routeable-account"
			return kept, filtered, info
		}
		info.Reason = "project-candidate-pool:matched"
		return kept, filtered, info
	default:
		info.Activated = true
		info.Reason = "project-candidate-pool:conflict"
		info.BeforeCandidateCount = len(candidates)
		info.FilteredAccountIDs = make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			filtered = append(filtered, ChannelRoutingExplainFilteredAccount{ID: candidate.Account.ID, Reason: "project-candidate-pool:conflict"})
			info.FilteredAccountIDs = append(info.FilteredAccountIDs, candidate.Account.ID)
		}
		info.AfterCandidateCount = 0
		return nil, filtered, info
	}
}

func mapRuntimeExplainCandidates(candidates []explainRuntimeCandidate) []ChannelRoutingExplainCandidate {
	out := make([]ChannelRoutingExplainCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, ChannelRoutingExplainCandidate{
			ID:             candidate.Account.ID,
			DisplayName:    candidate.View.DisplayName,
			Provider:       candidate.View.Provider,
			RouteOrder:     candidate.Account.RouteOrder,
			GroupID:        candidate.GroupID,
			GroupOrder:     candidate.Key.GroupOrder,
			ChannelOrder:   candidate.Key.ChannelOrder,
			ActiveSessions: candidate.Account.ActiveSessions,
			RouteIDs:       append([]string(nil), candidate.View.RouteIDs...),
		})
	}
	return out
}

func mapRuntimeExplainFiltered(items []gettokensrouting.FilteredAccount) []ChannelRoutingExplainFilteredAccount {
	out := make([]ChannelRoutingExplainFilteredAccount, 0, len(items))
	for _, item := range items {
		out = append(out, ChannelRoutingExplainFilteredAccount{
			ID:     strings.TrimSpace(item.AccountID),
			Reason: strings.TrimSpace(item.Reason),
		})
	}
	return out
}

func mapRouteSortKey(candidate explainRuntimeCandidate) routeSortKey {
	return candidate.Key
}

func orderRuntimeExplainCandidates(candidates []explainRuntimeCandidate, mode gettokensrouting.ChannelRouteMode) []explainRuntimeCandidate {
	if mode != gettokensrouting.ChannelRouteModeBalanced || len(candidates) < 2 {
		return candidates
	}
	out := append([]explainRuntimeCandidate(nil), candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Account.ActiveSessions != out[j].Account.ActiveSessions {
			return out[i].Account.ActiveSessions < out[j].Account.ActiveSessions
		}
		return lessExplainRouteSortKey(mapRouteSortKey(out[i]), mapRouteSortKey(out[j]))
	})
	return out
}

func lessExplainRouteSortKey(left, right routeSortKey) bool {
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

func effectiveGroupForExplain(account gettokensrouting.AccountSnapshot, groups map[string]gettokensrouting.AccountGroupSnapshot, channelStates map[string]gettokensrouting.ChannelGroupState) (string, int, bool, string) {
	groupID, groupOrder, ok := effectiveGroupForExplainAccount(account, groups, channelStates)
	if !ok {
		return "", 0, false, "group-disabled-or-missing"
	}
	return groupID, groupOrder, true, ""
}

func normalizeChannelRoutingExplainConfig(cfg channelRoutingPolicyConfig) channelRoutingPolicyConfig {
	cfg.Channel = strings.TrimSpace(strings.ToLower(cfg.Channel))
	if cfg.ChannelGroupStates == nil {
		cfg.ChannelGroupStates = map[string]gettokensrouting.ChannelGroupState{}
	}
	cfg.RouteMode = normalizeChannelRouteMode(cfg.RouteMode)
	cfg.OrderedAccountIDs = normalizeIDs(cfg.OrderedAccountIDs)
	return cfg
}

func normalizeChannelRouteMode(mode gettokensrouting.ChannelRouteMode) gettokensrouting.ChannelRouteMode {
	switch mode {
	case gettokensrouting.ChannelRouteModeBalanced:
		return mode
	default:
		return gettokensrouting.ChannelRouteModeSequential
	}
}

func normalizeIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
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

func channelRoutingExplainSnapshotVersion(cfg channelRoutingPolicyConfig) string {
	payload := []string{cfg.Channel, string(cfg.RouteMode), strings.Join(normalizeIDs(cfg.OrderedAccountIDs), ",")}
	sum := strings.Join(payload, "|")
	return strconv.Itoa(len(sum)) + ":" + strconv.Itoa(len(cfg.AccountGroups))
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

func effectiveGroupForExplainAccount(account gettokensrouting.AccountSnapshot, groups map[string]gettokensrouting.AccountGroupSnapshot, channelStates map[string]gettokensrouting.ChannelGroupState) (string, int, bool) {
	groupIDs := normalizeIDs(account.GroupIDs)
	if len(groupIDs) == 0 {
		return "", 0, true
	}
	bestID := ""
	bestOrder := 0
	found := false
	for _, groupID := range groupIDs {
		id, order, ok := effectiveGroupOrderForExplain(groupID, groups, channelStates)
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

func effectiveGroupOrderForExplain(groupID string, groups map[string]gettokensrouting.AccountGroupSnapshot, channelStates map[string]gettokensrouting.ChannelGroupState) (string, int, bool) {
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

func authSupportsChannel(auth *coreauth.Auth, channel string) bool {
	provider := strings.TrimSpace(strings.ToLower(auth.Provider))
	switch strings.TrimSpace(strings.ToLower(channel)) {
	case "claude":
		return provider == "claude" || provider == "anthropic" || strings.TrimSpace(auth.Attributes["format_base_url:anthropic"]) != ""
	default:
		if provider == "claude" || provider == "anthropic" {
			return false
		}
		return true
	}
}

func appendUniqueString(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, existing := range items {
		if existing == value {
			return items
		}
	}
	return append(items, value)
}

func findRuntimeExplainCandidate(candidates []explainRuntimeCandidate, accountID string) *explainRuntimeCandidate {
	for index := range candidates {
		if candidates[index].Account.ID == accountID {
			return &candidates[index]
		}
	}
	return nil
}

func findRuntimeExplainFilteredReason(filtered []ChannelRoutingExplainFilteredAccount, accountID string) (string, bool) {
	for _, item := range filtered {
		if item.ID == accountID {
			return item.Reason, true
		}
	}
	return "", false
}
