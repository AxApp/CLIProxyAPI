package gettokenshooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type channelRoutingRoutePolicy struct{}

type channelRoutingPolicyStore struct {
	Channels map[string]channelRoutingPolicyConfig `json:"channels"`
}

type channelRoutingPolicyConfig struct {
	Channel                      string                                        `json:"channel"`
	RouteMode                    gettokensrouting.ChannelRouteMode             `json:"routeMode"`
	OrderedAccountIDs            []string                                      `json:"orderedAccountIDs"`
	AccountGroups                []channelRoutingPolicyAccountGroup            `json:"accountGroups,omitempty"`
	ChannelGroupStates           map[string]gettokensrouting.ChannelGroupState `json:"channelGroupStates"`
	ProjectBindings              []gettokensrouting.ProjectBinding             `json:"projectBindings"`
	ProjectModeFallbackRouteMode gettokensrouting.ChannelRouteMode             `json:"projectModeFallbackRouteMode"`
	FallbackMode                 gettokensrouting.ChannelFallbackMode          `json:"fallbackMode"`
}

type channelRoutingPolicyAccountGroup struct {
	ID         string   `json:"id"`
	Enabled    bool     `json:"enabled"`
	RouteOrder int      `json:"routeOrder,omitempty"`
	AccountIDs []string `json:"accountIDs"`
}

var channelRoutingActiveSessionsByAuthID = currentChannelRoutingActiveSessionsByAuthID

func (channelRoutingRoutePolicy) RoutePolicyStage() gettokensrouting.PolicyStage {
	return gettokensrouting.PolicyStagePoolScope
}

func (channelRoutingRoutePolicy) RewriteCandidates(_ context.Context, req coreauth.RoutePolicyRequest) coreauth.RoutePolicyDecision {
	channel := channelRoutingChannelForRequest(req)
	if channel == "" || len(req.Candidates) == 0 {
		return coreauth.RoutePolicyDecision{}
	}
	cfg, ok := loadChannelRoutingPolicyConfig(channel)
	if !ok {
		return coreauth.RoutePolicyDecision{}
	}
	accounts, groups := channelRoutingSnapshots(req.Candidates, cfg)
	decision := gettokensrouting.DecideChannelRoute(accounts, groups, gettokensrouting.ChannelRoutingConfig{
		Channel:                      channel,
		RouteMode:                    cfg.RouteMode,
		OrderedAccountIDs:            cfg.OrderedAccountIDs,
		ChannelGroupStates:           cfg.ChannelGroupStates,
		ProjectBindings:              cfg.ProjectBindings,
		ProjectModeFallbackRouteMode: cfg.ProjectModeFallbackRouteMode,
		FallbackMode:                 cfg.FallbackMode,
	}, gettokensrouting.ChannelRouteRequest{
		Tried: req.Tried,
	})
	if decision.SelectedID == "" {
		return coreauth.RoutePolicyDecision{}
	}
	return coreauth.RoutePolicyDecision{
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
	cfg, ok := store.Channels[channel]
	if !ok {
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

func channelRoutingPolicyConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gettokens-data", "channel-routing", "config.json"), nil
}

func channelRoutingChannelForRequest(req coreauth.RoutePolicyRequest) string {
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

func currentChannelRoutingActiveSessionsByAuthID() map[string]int {
	snapshot := CurrentLiveSessionsSnapshot()
	out := map[string]int{}
	for _, session := range snapshot.Sessions {
		if session.Status != "active" && session.Status != "streaming" {
			continue
		}
		authID := strings.TrimSpace(session.AuthID)
		if authID == "" {
			continue
		}
		out[authID]++
	}
	return out
}
