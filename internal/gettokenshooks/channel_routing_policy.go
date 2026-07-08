package gettokenshooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type channelRoutingPolicyStore struct {
	Channels            map[string]json.RawMessage                     `json:"channels"`
	Events              json.RawMessage                                `json:"events,omitempty"`
	NextEventID         int                                            `json:"nextEventID,omitempty"`
	RuntimeStates       map[string]persistedChannelAccountRuntimeState `json:"runtimeStates,omitempty"`
	QuotaThresholdRules []AccountQuotaThresholdRule                    `json:"quotaThresholdRules,omitempty"`
}

type channelRoutingPolicyConfig struct {
	Channel            string                                        `json:"channel"`
	RouteMode          gettokensrouting.ChannelRouteMode             `json:"routeMode"`
	OrderedAccountIDs  []string                                      `json:"orderedAccountIDs"`
	AccountGroups      []channelRoutingPolicyAccountGroup            `json:"accountGroups,omitempty"`
	ChannelGroupStates map[string]gettokensrouting.ChannelGroupState `json:"channelGroupStates"`
}

type channelRoutingPolicyAccountGroup struct {
	ID         string   `json:"id"`
	Enabled    bool     `json:"enabled"`
	RouteOrder int      `json:"routeOrder,omitempty"`
	AccountIDs []string `json:"accountIDs"`
}

var channelRoutingActiveSessionsByAuthID = currentLiveSessionActiveAuthCounts

var channelRoutingPolicyConfigPathState struct {
	sync.RWMutex
	path string
}

func channelRoutingPolicy() gettokensrouting.Policy {
	return gettokensrouting.Policy{
		Stage:   gettokensrouting.PolicyStagePoolScope,
		Name:    "channel-routing",
		Rewrite: rewriteChannelRoutingCandidates,
	}
}

func rewriteChannelRoutingCandidates(_ context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
	channel := channelRoutingChannelForRequest(req)
	if channel == "" || len(req.Candidates) == 0 {
		return gettokensrouting.PolicyDecision{}
	}
	cfg, ok := loadChannelRoutingPolicyConfig(channel)
	if !ok {
		return gettokensrouting.PolicyDecision{}
	}
	accounts, groups := channelRoutingSnapshots(authCandidatesFromRouteContext(req), cfg)
	decision := gettokensrouting.DecideChannelRoute(accounts, groups, gettokensrouting.ChannelRoutingConfig{
		Channel:            channel,
		RouteMode:          cfg.RouteMode,
		OrderedAccountIDs:  cfg.OrderedAccountIDs,
		ChannelGroupStates: cfg.ChannelGroupStates,
	}, gettokensrouting.ChannelRouteRequest{
		Tried: req.Tried,
	})
	if decision.SelectedID == "" {
		return gettokensrouting.PolicyDecision{}
	}
	return gettokensrouting.PolicyDecision{
		OrderIDs: channelRoutingOrderIDs(decision),
		Reason:   "channel-routing:" + channel + ":" + string(cfg.RouteMode),
	}
}

func loadChannelRoutingPolicyConfig(channel string) (channelRoutingPolicyConfig, bool) {
	path, err := channelRoutingPolicyConfigPath()
	if err != nil {
		return channelRoutingPolicyConfig{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return channelRoutingPolicyConfig{}, false
	}
	var store channelRoutingPolicyStore
	if err := json.Unmarshal(body, &store); err != nil {
		return channelRoutingPolicyConfig{}, false
	}
	raw, ok := store.Channels[channel]
	if !ok {
		return channelRoutingPolicyConfig{}, false
	}
	var cfg channelRoutingPolicyConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return channelRoutingPolicyConfig{}, false
	}
	if strings.TrimSpace(cfg.Channel) == "" {
		cfg.Channel = channel
	}
	if cfg.ChannelGroupStates == nil {
		cfg.ChannelGroupStates = map[string]gettokensrouting.ChannelGroupState{}
	}
	return cfg, true
}

func loadQuotaThresholdRulesFromChannelRoutingConfig() []AccountQuotaThresholdRule {
	store, err := readChannelRoutingPolicyStore()
	if err != nil {
		return nil
	}
	if len(store.QuotaThresholdRules) == 0 {
		return nil
	}
	out := make([]AccountQuotaThresholdRule, 0, len(store.QuotaThresholdRules))
	for _, rule := range store.QuotaThresholdRules {
		if strings.TrimSpace(rule.ID) == "" || strings.TrimSpace(rule.AccountKey) == "" || !quotaThresholdRuleHasFactTarget(rule) {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func readChannelRoutingPolicyStore() (channelRoutingPolicyStore, error) {
	path, err := channelRoutingPolicyConfigPath()
	if err != nil {
		return channelRoutingPolicyStore{}, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return channelRoutingPolicyStore{
				Channels:            map[string]json.RawMessage{},
				QuotaThresholdRules: []AccountQuotaThresholdRule{},
			}, nil
		}
		return channelRoutingPolicyStore{}, err
	}
	var store channelRoutingPolicyStore
	if err := json.Unmarshal(body, &store); err != nil {
		return channelRoutingPolicyStore{}, err
	}
	if store.Channels == nil {
		store.Channels = map[string]json.RawMessage{}
	}
	if store.QuotaThresholdRules == nil {
		store.QuotaThresholdRules = []AccountQuotaThresholdRule{}
	}
	return store, nil
}

func saveChannelRoutingPolicyStore(store channelRoutingPolicyStore) error {
	path, err := channelRoutingPolicyConfigPath()
	if err != nil {
		return err
	}
	if store.Channels == nil {
		store.Channels = map[string]json.RawMessage{}
	}
	if store.QuotaThresholdRules == nil {
		store.QuotaThresholdRules = []AccountQuotaThresholdRule{}
	}
	sort.SliceStable(store.QuotaThresholdRules, func(i, j int) bool {
		left := store.QuotaThresholdRules[i]
		right := store.QuotaThresholdRules[j]
		if left.AccountKey != right.AccountKey {
			return left.AccountKey < right.AccountKey
		}
		if left.WindowKey != right.WindowKey {
			return left.WindowKey < right.WindowKey
		}
		return left.ID < right.ID
	})
	body, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func ConfigureQuotaThresholdRuleRoutes(group *gin.RouterGroup, handler *handlers.BaseAPIHandler, _ *config.Config) {
	if group == nil {
		return
	}
	group.POST("/route-guard/rules/simulate", func(c *gin.Context) {
		var request struct {
			RuleIDs []string                    `json:"ruleIds,omitempty"`
			Rule    *AccountQuotaThresholdRule  `json:"rule,omitempty"`
			Rules   []AccountQuotaThresholdRule `json:"rules,omitempty"`
			Facts   SimulationFacts             `json:"facts"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store, err := readChannelRoutingPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		draftRules := append([]AccountQuotaThresholdRule(nil), request.Rules...)
		if request.Rule != nil {
			draftRules = append(draftRules, *request.Rule)
		}
		rules, err := simulationQuotaThresholdRules(store.QuotaThresholdRules, request.RuleIDs, draftRules)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, SimulateQuotaThresholdRules(rules, request.Facts))
	})
	group.GET("/gettokens/quota-threshold-rules", func(c *gin.Context) {
		store, err := readChannelRoutingPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": listQuotaThresholdRules(store, c.Query("account_key"))})
	})
	group.POST("/gettokens/quota-threshold-rules", func(c *gin.Context) {
		var rule AccountQuotaThresholdRule
		if err := c.ShouldBindJSON(&rule); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store, err := readChannelRoutingPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" {
			rule.ID = randomID("quota-threshold-rule")
		}
		normalized, err := normalizeQuotaThresholdRule(rule)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := validateQuotaThresholdRule(normalized, store.QuotaThresholdRules); err != nil {
			writeQuotaThresholdRuleValidationError(c, err)
			return
		}
		store.QuotaThresholdRules = upsertQuotaThresholdRule(store.QuotaThresholdRules, normalized)
		if err := saveChannelRoutingPolicyStore(store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		bumpQuotaThresholdRuleEpoch(handler)
		c.JSON(http.StatusOK, gin.H{"items": listQuotaThresholdRules(store, normalized.AccountKey)})
	})
	group.PUT("/gettokens/quota-threshold-rules/:id", func(c *gin.Context) {
		var rule AccountQuotaThresholdRule
		if err := c.ShouldBindJSON(&rule); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store, err := readChannelRoutingPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rule.ID = strings.TrimSpace(c.Param("id"))
		normalized, err := normalizeQuotaThresholdRule(rule)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := validateQuotaThresholdRule(normalized, store.QuotaThresholdRules); err != nil {
			writeQuotaThresholdRuleValidationError(c, err)
			return
		}
		store.QuotaThresholdRules = upsertQuotaThresholdRule(store.QuotaThresholdRules, normalized)
		if err := saveChannelRoutingPolicyStore(store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		bumpQuotaThresholdRuleEpoch(handler)
		c.JSON(http.StatusOK, gin.H{"items": listQuotaThresholdRules(store, normalized.AccountKey)})
	})
	group.DELETE("/gettokens/quota-threshold-rules/:id", func(c *gin.Context) {
		store, err := readChannelRoutingPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		store.QuotaThresholdRules = deleteQuotaThresholdRule(store.QuotaThresholdRules, c.Param("id"))
		if err := saveChannelRoutingPolicyStore(store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		bumpQuotaThresholdRuleEpoch(handler)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
}

func listQuotaThresholdRules(store channelRoutingPolicyStore, accountKey string) []AccountQuotaThresholdRule {
	accountKey = strings.TrimSpace(accountKey)
	out := make([]AccountQuotaThresholdRule, 0, len(store.QuotaThresholdRules))
	for _, rule := range store.QuotaThresholdRules {
		if accountKey != "" && strings.TrimSpace(rule.AccountKey) != accountKey {
			continue
		}
		normalized, err := normalizeQuotaThresholdRule(rule)
		if err != nil {
			continue
		}
		out = append(out, normalized)
	}
	return out
}

func simulationQuotaThresholdRules(rules []AccountQuotaThresholdRule, ruleIDs []string, draftRules []AccountQuotaThresholdRule) ([]AccountQuotaThresholdRule, error) {
	wanted := map[string]struct{}{}
	for _, id := range ruleIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		wanted[id] = struct{}{}
	}
	out := make([]AccountQuotaThresholdRule, 0, len(rules))
	found := map[string]struct{}{}
	for _, rule := range rules {
		if len(wanted) > 0 {
			if _, ok := wanted[strings.TrimSpace(rule.ID)]; !ok {
				continue
			}
		}
		normalized, err := normalizeQuotaThresholdRule(rule)
		if err != nil {
			return nil, err
		}
		if err := validateQuotaThresholdRule(normalized, out); err != nil {
			return nil, err
		}
		out = append(out, normalized)
		found[normalized.ID] = struct{}{}
	}
	for index, rule := range draftRules {
		if strings.TrimSpace(rule.ID) == "" {
			rule.ID = fmt.Sprintf("simulation-draft-%d", index+1)
		}
		normalized, err := normalizeQuotaThresholdRule(rule)
		if err != nil {
			return nil, err
		}
		if err := validateQuotaThresholdRule(normalized, nil); err != nil {
			return nil, err
		}
		out = append(out, normalized)
		found[normalized.ID] = struct{}{}
	}
	for id := range wanted {
		if _, ok := found[id]; !ok {
			return nil, fmt.Errorf("rule %q not found", id)
		}
	}
	return out, nil
}

func normalizeQuotaThresholdRule(rule AccountQuotaThresholdRule) (AccountQuotaThresholdRule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	if rule.ID == "" {
		return AccountQuotaThresholdRule{}, errors.New("id is required")
	}
	rule.AccountKey = strings.TrimSpace(rule.AccountKey)
	rule.WindowKey = strings.TrimSpace(rule.WindowKey)
	rule.Metric = strings.ToLower(strings.TrimSpace(rule.Metric))
	if rule.Metric == "" {
		rule.Metric = "remaining-percent"
	}
	if rule.Condition != nil {
		condition, err := normalizeQuotaRuleCondition(*rule.Condition)
		if err != nil {
			return AccountQuotaThresholdRule{}, err
		}
		rule.Condition = &condition
	}
	rule.Comparator = strings.TrimSpace(rule.Comparator)
	if rule.Comparator == "" {
		switch rule.Metric {
		case "used-percent":
			rule.Comparator = ">="
		default:
			rule.Comparator = "<="
		}
	}
	return rule, nil
}

func validateQuotaThresholdRule(rule AccountQuotaThresholdRule, existing []AccountQuotaThresholdRule) error {
	if rule.AccountKey == "" {
		return errors.New("account_key is required")
	}
	if rule.Condition == nil && rule.WindowKey == "" {
		return errors.New("window_key is required")
	}
	if rule.Condition != nil {
		if err := validateQuotaRuleCondition(*rule.Condition); err != nil {
			return err
		}
	} else {
		switch rule.Metric {
		case "remaining-percent", "used-percent", "remaining", "used":
		default:
			return fmt.Errorf("unsupported metric %q", rule.Metric)
		}
		switch rule.Comparator {
		case "<=", ">=", "<", ">":
		default:
			return fmt.Errorf("unsupported comparator %q", rule.Comparator)
		}
		if (rule.Metric == "remaining-percent" || rule.Metric == "used-percent") && (rule.ThresholdPercent < 0 || rule.ThresholdPercent > 100) {
			return fmt.Errorf("threshold_percent must be between 0 and 100")
		}
	}
	for _, candidate := range existing {
		if strings.TrimSpace(candidate.ID) == rule.ID {
			continue
		}
		normalized, err := normalizeQuotaThresholdRule(candidate)
		if err != nil {
			continue
		}
		if normalized.Enabled && rule.Enabled && quotaThresholdRuleConflictKey(normalized) == quotaThresholdRuleConflictKey(rule) {
			return quotaThresholdRuleConflictError{
				RuleID:     normalized.ID,
				AccountKey: rule.AccountKey,
				Conflict:   quotaThresholdRuleConflictKey(rule),
			}
		}
	}
	return nil
}

func quotaThresholdRuleHasFactTarget(rule AccountQuotaThresholdRule) bool {
	if strings.TrimSpace(rule.WindowKey) != "" {
		return true
	}
	return quotaRuleConditionHasFactTarget(rule.Condition)
}

func quotaRuleConditionHasFactTarget(condition *AccountQuotaRuleCondition) bool {
	if condition == nil {
		return false
	}
	if strings.TrimSpace(condition.WindowKey) != "" {
		return true
	}
	for index := range condition.All {
		if quotaRuleConditionHasFactTarget(&condition.All[index]) {
			return true
		}
	}
	for index := range condition.Any {
		if quotaRuleConditionHasFactTarget(&condition.Any[index]) {
			return true
		}
	}
	if condition.Not != nil {
		return quotaRuleConditionHasFactTarget(condition.Not)
	}
	return false
}

type quotaThresholdRuleConflictError struct {
	RuleID     string
	AccountKey string
	Conflict   string
}

func (e quotaThresholdRuleConflictError) Error() string {
	return fmt.Sprintf("conflicting enabled quota threshold rule %q for account_key %q", e.RuleID, e.AccountKey)
}

func writeQuotaThresholdRuleValidationError(c *gin.Context, err error) {
	var conflict quotaThresholdRuleConflictError
	if errors.As(err, &conflict) {
		c.JSON(http.StatusConflict, gin.H{
			"error": err.Error(),
			"conflicts": []gin.H{{
				"rule_id":     conflict.RuleID,
				"account_key": conflict.AccountKey,
				"conflict":    conflict.Conflict,
			}},
		})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

func normalizeQuotaRuleCondition(condition AccountQuotaRuleCondition) (AccountQuotaRuleCondition, error) {
	condition.Fact = strings.TrimSpace(condition.Fact)
	condition.WindowKey = strings.TrimSpace(condition.WindowKey)
	condition.Metric = strings.ToLower(strings.TrimSpace(condition.Metric))
	condition.Comparator = strings.TrimSpace(condition.Comparator)
	if condition.Metric == "" && len(condition.All) == 0 && len(condition.Any) == 0 && condition.Not == nil {
		condition.Metric = "remaining-percent"
	}
	if condition.Comparator == "" && condition.Metric != "" {
		condition.Comparator = normalizeQuotaThresholdComparator("", condition.Metric)
	}
	for index := range condition.All {
		next, err := normalizeQuotaRuleCondition(condition.All[index])
		if err != nil {
			return AccountQuotaRuleCondition{}, err
		}
		condition.All[index] = next
	}
	for index := range condition.Any {
		next, err := normalizeQuotaRuleCondition(condition.Any[index])
		if err != nil {
			return AccountQuotaRuleCondition{}, err
		}
		condition.Any[index] = next
	}
	if condition.Not != nil {
		next, err := normalizeQuotaRuleCondition(*condition.Not)
		if err != nil {
			return AccountQuotaRuleCondition{}, err
		}
		condition.Not = &next
	}
	return condition, nil
}

func validateQuotaRuleCondition(condition AccountQuotaRuleCondition) error {
	branches := 0
	if len(condition.All) > 0 {
		branches++
		for _, child := range condition.All {
			if err := validateQuotaRuleCondition(child); err != nil {
				return err
			}
		}
	}
	if len(condition.Any) > 0 {
		branches++
		for _, child := range condition.Any {
			if err := validateQuotaRuleCondition(child); err != nil {
				return err
			}
		}
	}
	if condition.Not != nil {
		branches++
		if err := validateQuotaRuleCondition(*condition.Not); err != nil {
			return err
		}
	}
	if branches > 0 {
		if strings.TrimSpace(condition.WindowKey) != "" || strings.TrimSpace(condition.Metric) != "" {
			return errors.New("condition group cannot also define a leaf comparison")
		}
		return nil
	}
	if condition.WindowKey == "" {
		return errors.New("condition.window_key is required")
	}
	switch condition.Metric {
	case "remaining-percent", "used-percent", "remaining", "used":
	default:
		return fmt.Errorf("unsupported condition metric %q", condition.Metric)
	}
	switch condition.Comparator {
	case "<=", ">=", "<", ">":
	default:
		return fmt.Errorf("unsupported condition comparator %q", condition.Comparator)
	}
	if (condition.Metric == "remaining-percent" || condition.Metric == "used-percent") && (condition.Value < 0 || condition.Value > 100) {
		return fmt.Errorf("condition value must be between 0 and 100 for percent metrics")
	}
	return nil
}

func quotaThresholdRuleConflictKey(rule AccountQuotaThresholdRule) string {
	if rule.Condition != nil {
		body, err := json.Marshal(rule.Condition)
		if err == nil {
			return rule.AccountKey + ":condition:" + string(body)
		}
	}
	return strings.Join([]string{rule.AccountKey, rule.WindowKey, rule.Metric}, ":")
}

func upsertQuotaThresholdRule(rules []AccountQuotaThresholdRule, rule AccountQuotaThresholdRule) []AccountQuotaThresholdRule {
	out := make([]AccountQuotaThresholdRule, 0, len(rules)+1)
	replaced := false
	for _, existing := range rules {
		if strings.TrimSpace(existing.ID) == rule.ID {
			out = append(out, rule)
			replaced = true
			continue
		}
		out = append(out, existing)
	}
	if !replaced {
		out = append(out, rule)
	}
	return out
}

func deleteQuotaThresholdRule(rules []AccountQuotaThresholdRule, id string) []AccountQuotaThresholdRule {
	id = strings.TrimSpace(id)
	out := make([]AccountQuotaThresholdRule, 0, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.ID) == id {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func bumpQuotaThresholdRuleEpoch(handler *handlers.BaseAPIHandler) {
	defaultQuotaRuntimeStore.RefreshQuotaThresholdGuards(time.Now().UTC())
	if handler == nil || handler.AuthManager == nil {
		return
	}
	handler.AuthManager.BumpSessionAffinityPoolEpoch()
}

func SetChannelRoutingPolicyConfigPathFromConfig(configPath string) {
	path, err := channelRoutingPolicyConfigPathFromConfig(configPath)
	if err != nil {
		return
	}
	setAccountRouteGuardAccountStorePathFromConfig(configPath)
	channelRoutingPolicyConfigPathState.Lock()
	channelRoutingPolicyConfigPathState.path = path
	channelRoutingPolicyConfigPathState.Unlock()
	_ = hydrateAccountRouteGuardStoreFromPersistedRuntimeStates(defaultAccountRouteGuardStore, time.Now().UTC())
}

func channelRoutingPolicyConfigPath() (string, error) {
	channelRoutingPolicyConfigPathState.RLock()
	path := channelRoutingPolicyConfigPathState.path
	channelRoutingPolicyConfigPathState.RUnlock()
	if strings.TrimSpace(path) != "" {
		return path, nil
	}
	return channelRoutingPolicyConfigPathFromConfig("")
}

func channelRoutingPolicyConfigPathFromConfig(configPath string) (string, error) {
	if dir := strings.TrimSpace(configPath); dir != "" {
		if strings.EqualFold(filepath.Base(dir), "config.yaml") || strings.EqualFold(filepath.Base(dir), "config.yml") {
			dir = filepath.Dir(dir)
		}
		return filepath.Join(dir, "channel-routing", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gettokens-data", "channel-routing", "config.json"), nil
}

func channelRoutingChannelForRequest(req gettokensrouting.RouteContext) string {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	switch provider {
	case "codex", "claude":
		return provider
	}
	channels := map[string]struct{}{}
	for _, provider := range req.Providers {
		switch strings.ToLower(strings.TrimSpace(provider)) {
		case "codex":
			channels["codex"] = struct{}{}
		case "claude":
			channels["claude"] = struct{}{}
		}
	}
	if len(channels) == 1 {
		for channel := range channels {
			return channel
		}
	}
	return ""
}

func channelRoutingSnapshots(candidates []*coreauth.Auth, cfg channelRoutingPolicyConfig) ([]gettokensrouting.AccountSnapshot, []gettokensrouting.AccountGroupSnapshot) {
	sessionCounts := channelRoutingActiveSessionsByAuthID()
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
	accounts := make([]gettokensrouting.AccountSnapshot, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		id := strings.TrimSpace(candidate.ID)
		accounts = append(accounts, gettokensrouting.AccountSnapshot{
			ID:             id,
			Enabled:        !candidate.Disabled && candidate.Status != coreauth.StatusDisabled,
			Requestable:    !candidate.Unavailable && candidate.Status != coreauth.StatusError,
			RouteOrder:     channelRoutingAuthRouteOrder(candidate),
			GroupIDs:       append([]string(nil), accountGroups[id]...),
			ActiveSessions: sessionCounts[id],
		})
	}
	return accounts, groups
}

func channelRoutingAuthRouteOrder(auth *coreauth.Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	for _, key := range []string{"routeOrder", "route_order", "priority"} {
		raw := strings.TrimSpace(auth.Attributes[key])
		if raw == "" {
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err == nil {
			return parsed
		}
	}
	return 0
}

func channelRoutingOrderIDs(decision gettokensrouting.ChannelRouteDecision) []string {
	out := make([]string, 0, len(decision.Candidates))
	seen := make(map[string]struct{}, len(decision.Candidates))
	selected := strings.TrimSpace(decision.SelectedID)
	if selected != "" {
		out = append(out, selected)
		seen[selected] = struct{}{}
	}
	for _, candidate := range decision.Candidates {
		id := strings.TrimSpace(candidate.Account.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		out = append(out, id)
		seen[id] = struct{}{}
	}
	return out
}
