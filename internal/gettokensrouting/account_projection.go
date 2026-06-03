package gettokensrouting

import (
	"sort"
	"strconv"
	"strings"
)

const (
	RuntimeAccountReasonDetached       = "account-detached"
	RuntimeAccountReasonDisabled       = "account-disabled"
	RuntimeAccountReasonStatusDisabled = "status-disabled"
	RuntimeAccountReasonStatusError    = "status-error"
	RuntimeAccountReasonUnavailable    = "account-unavailable"
)

type RuntimeAccountGuardBlock struct {
	Source string
	Reason string
}

type RuntimeAccountInput struct {
	AuthID      string
	AccountKey  string
	Provider    string
	Disabled    bool
	Status      string
	Unavailable bool
	Attributes  map[string]string
}

type RuntimeAccountProjection struct {
	AccountKey      string
	AuthID          string
	Provider        string
	Present         bool
	Enabled         bool
	Requestable     bool
	CoarseAvailable bool
	FilteredReasons []string
	ActiveSessions  int
	RouteOrder      int
}

type RuntimeAccountProjectionOptions struct {
	GuardBlocksByAuthID map[string][]RuntimeAccountGuardBlock
	ActiveSessionsByID  map[string]int
}

type RuntimeAccountProjectionSnapshot struct {
	ByAuthID     map[string]RuntimeAccountProjection
	ByAccountKey map[string]RuntimeAccountProjection
}

func BuildRuntimeAccountProjectionSnapshot(inputs []RuntimeAccountInput, opts RuntimeAccountProjectionOptions) RuntimeAccountProjectionSnapshot {
	snapshot := RuntimeAccountProjectionSnapshot{
		ByAuthID:     map[string]RuntimeAccountProjection{},
		ByAccountKey: map[string]RuntimeAccountProjection{},
	}
	for _, input := range inputs {
		projection, ok := RuntimeAccountProjectionForInput(input, opts)
		if !ok {
			continue
		}
		if projection.AuthID != "" {
			snapshot.ByAuthID[projection.AuthID] = projection
		}
		if projection.AccountKey != "" {
			snapshot.ByAccountKey[projection.AccountKey] = projection
		}
	}
	return snapshot
}

func RuntimeAccountProjectionForInput(input RuntimeAccountInput, opts RuntimeAccountProjectionOptions) (RuntimeAccountProjection, bool) {
	authID := strings.TrimSpace(input.AuthID)
	if authID == "" {
		return RuntimeAccountProjection{}, false
	}
	projection := RuntimeAccountProjection{
		AuthID:         authID,
		AccountKey:     strings.TrimSpace(input.AccountKey),
		Provider:       strings.TrimSpace(input.Provider),
		Present:        true,
		Enabled:        true,
		Requestable:    true,
		ActiveSessions: opts.ActiveSessionsByID[authID],
		RouteOrder:     runtimeAccountRouteOrder(input.Attributes),
	}
	status := strings.ToLower(strings.TrimSpace(input.Status))
	if input.Disabled {
		projection.Enabled = false
		projection.FilteredReasons = appendUniqueReason(projection.FilteredReasons, RuntimeAccountReasonDisabled)
	}
	if status == "disabled" {
		projection.Enabled = false
		projection.FilteredReasons = appendUniqueReason(projection.FilteredReasons, RuntimeAccountReasonStatusDisabled)
	}
	if status == "error" {
		projection.Requestable = false
		projection.FilteredReasons = appendUniqueReason(projection.FilteredReasons, RuntimeAccountReasonStatusError)
	}
	if input.Unavailable {
		projection.Requestable = false
		projection.FilteredReasons = appendUniqueReason(projection.FilteredReasons, RuntimeAccountReasonUnavailable)
	}
	for _, block := range opts.GuardBlocksByAuthID[authID] {
		if source := strings.TrimSpace(block.Source); source != "" {
			projection.FilteredReasons = appendUniqueReason(projection.FilteredReasons, source)
		}
	}
	sort.Strings(projection.FilteredReasons)
	projection.CoarseAvailable = projection.Present && projection.Enabled && projection.Requestable && len(projection.FilteredReasons) == 0
	return projection, true
}

func DetachedRuntimeAccountProjection(authID string, accountKey string) RuntimeAccountProjection {
	return RuntimeAccountProjection{
		AuthID:          strings.TrimSpace(authID),
		AccountKey:      strings.TrimSpace(accountKey),
		Present:         false,
		Enabled:         false,
		Requestable:     false,
		CoarseAvailable: false,
		FilteredReasons: []string{RuntimeAccountReasonDetached},
	}
}

func (s RuntimeAccountProjectionSnapshot) Find(authID string, accountKey string) (RuntimeAccountProjection, bool) {
	authID = strings.TrimSpace(authID)
	accountKey = strings.TrimSpace(accountKey)
	if authID != "" {
		if projection, ok := s.ByAuthID[authID]; ok {
			return projection, true
		}
	}
	if accountKey != "" {
		if projection, ok := s.ByAccountKey[accountKey]; ok {
			return projection, true
		}
	}
	return RuntimeAccountProjection{}, false
}

func runtimeAccountRouteOrder(attrs map[string]string) int {
	if attrs == nil {
		return 0
	}
	for _, key := range []string{"routeOrder", "route_order", "priority"} {
		raw := strings.TrimSpace(attrs[key])
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

func appendUniqueReason(reasons []string, reason string) []string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return reasons
	}
	for _, existing := range reasons {
		if existing == reason {
			return reasons
		}
	}
	return append(reasons, reason)
}
