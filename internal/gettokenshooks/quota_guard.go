package gettokenshooks

import (
	"net/http"
	"sort"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// AccountQuotaRuntimeState is the sidecar-owned quota snapshot used to derive
// routing guard blocks. Freshness and degradation are evaluated before routing.
type AccountQuotaRuntimeState struct {
	AccountKey     string
	AuthIDs        []string
	LookupKeys     []string
	Source         string
	Fresh          bool
	Stale          bool
	Degraded       bool
	DegradedReason string
	EvaluatedAt    time.Time
	ExpiresAt      time.Time
	Windows        []AccountQuotaWindowState
}

type AccountQuotaWindowState struct {
	Key       string
	Kind      string
	Remaining float64
	Limit     float64
	ResetAt   time.Time
	Exhausted bool
}

// QuotaEmptyRouteGuardBlocks converts fresh exhausted quota windows into
// route-guard blocks. Stale, degraded, unknown, or reset-uncertain quota data is
// intentionally optimistic and does not hard-block routing.
func QuotaEmptyRouteGuardBlocks(states []AccountQuotaRuntimeState, now time.Time) []AccountRouteGuardBlock {
	if len(states) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	blocks := make([]AccountRouteGuardBlock, 0, len(states))
	for _, state := range states {
		block, ok := quotaEmptyRouteGuardBlock(state, now)
		if !ok {
			continue
		}
		blocks = append(blocks, block)
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		left := accountRouteGuardBlockKey(blocks[i])
		right := accountRouteGuardBlockKey(blocks[j])
		if left == right {
			return blocks[i].AccountKey < blocks[j].AccountKey
		}
		return left < right
	})
	return blocks
}

func QuotaEmptyRouteGuardBlocksFromAuths(auths []*coreauth.Auth, now time.Time) []AccountRouteGuardBlock {
	if len(auths) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	states := make([]AccountQuotaRuntimeState, 0, len(auths))
	for _, auth := range auths {
		state, ok := quotaRuntimeStateFromAuth(auth, now)
		if !ok {
			continue
		}
		states = append(states, state)
	}
	return QuotaEmptyRouteGuardBlocks(states, now)
}

func (s *AccountRouteGuardStore) SyncQuotaEmptyAuth(auth *coreauth.Auth, now time.Time) {
	if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	blocks := QuotaEmptyRouteGuardBlocksFromAuths([]*coreauth.Auth{auth}, now)
	if len(blocks) == 0 {
		s.ClearAuth(AccountRouteGuardSourceQuotaEmpty, auth.ID)
		return
	}
	for _, block := range blocks {
		s.MarkBlocked(block)
	}
}

func SyncQuotaEmptyAuth(auth *coreauth.Auth) {
	defaultAccountRouteGuardStore.SyncQuotaEmptyAuth(auth, time.Now().UTC())
}

func quotaEmptyRouteGuardBlockForResult(result coreauth.Result, now time.Time) (AccountRouteGuardBlock, bool) {
	authID := strings.TrimSpace(result.AuthID)
	if authID == "" || result.Error == nil || !isQuotaEmptyResultError(result.Error) {
		return AccountRouteGuardBlock{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	if result.RetryAfter == nil || *result.RetryAfter <= 0 {
		return AccountRouteGuardBlock{}, false
	}
	resetAt := now.Add(*result.RetryAfter).UTC()
	return AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceQuotaEmpty,
		AuthID:     authID,
		AccountKey: "auth-id:" + authID,
		Reason:     "quota empty: upstream quota; reset at " + resetAt.Format(time.RFC3339),
		ExpiresAt:  resetAt,
		UpdatedAt:  now,
	}, true
}

func quotaEmptyRouteGuardBlock(state AccountQuotaRuntimeState, now time.Time) (AccountRouteGuardBlock, bool) {
	accountKey := strings.TrimSpace(state.AccountKey)
	authIDs := normalizeQuotaGuardIDs(state.AuthIDs)
	if accountKey == "" && len(authIDs) == 0 {
		return AccountRouteGuardBlock{}, false
	}
	if !state.Fresh || state.Stale || state.Degraded {
		return AccountRouteGuardBlock{}, false
	}
	if !state.ExpiresAt.IsZero() && !state.ExpiresAt.After(now) {
		return AccountRouteGuardBlock{}, false
	}
	windows, latestReset := exhaustedQuotaWindowsWithReset(state.Windows, now)
	if len(windows) == 0 || latestReset.IsZero() {
		return AccountRouteGuardBlock{}, false
	}
	updatedAt := state.EvaluatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	return AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceQuotaEmpty,
		AuthID:     firstQuotaGuardAuthID(authIDs),
		AccountKey: accountKey,
		LookupKeys: quotaGuardLookupKeys(authIDs, state.LookupKeys),
		Reason:     quotaEmptyGuardReason(windows, latestReset),
		ExpiresAt:  latestReset.UTC(),
		UpdatedAt:  updatedAt.UTC(),
	}, true
}

func quotaRuntimeStateFromAuth(auth *coreauth.Auth, now time.Time) (AccountQuotaRuntimeState, bool) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return AccountQuotaRuntimeState{}, false
	}
	if !auth.Quota.Exceeded || auth.Quota.NextRecoverAt.IsZero() || !auth.Quota.NextRecoverAt.After(now) {
		return AccountQuotaRuntimeState{}, false
	}
	evaluatedAt := auth.UpdatedAt
	if evaluatedAt.IsZero() {
		evaluatedAt = now
	}
	windowKey := strings.TrimSpace(auth.Quota.Reason)
	if windowKey == "" || strings.EqualFold(windowKey, "quota") {
		windowKey = "runtime-quota"
	}
	return AccountQuotaRuntimeState{
		AccountKey:  strings.TrimSpace(auth.AccountKey),
		AuthIDs:     []string{strings.TrimSpace(auth.ID)},
		LookupKeys:  accountRouteGuardIdentityKeysForAuth(auth),
		Source:      "runtime-auth-quota",
		Fresh:       true,
		EvaluatedAt: evaluatedAt,
		Windows: []AccountQuotaWindowState{{
			Key:       windowKey,
			Kind:      "quota",
			Remaining: 0,
			ResetAt:   auth.Quota.NextRecoverAt,
			Exhausted: true,
		}},
	}, true
}

func exhaustedQuotaWindowsWithReset(windows []AccountQuotaWindowState, now time.Time) ([]AccountQuotaWindowState, time.Time) {
	out := []AccountQuotaWindowState{}
	latestReset := time.Time{}
	for _, window := range windows {
		if !quotaWindowExhausted(window) {
			continue
		}
		resetAt := window.ResetAt
		if resetAt.IsZero() || !resetAt.After(now) {
			continue
		}
		window.ResetAt = resetAt.UTC()
		out = append(out, window)
		if latestReset.IsZero() || resetAt.After(latestReset) {
			latestReset = resetAt
		}
	}
	return out, latestReset.UTC()
}

func quotaWindowExhausted(window AccountQuotaWindowState) bool {
	return window.Exhausted || window.Remaining <= 0
}

func quotaEmptyGuardReason(windows []AccountQuotaWindowState, latestReset time.Time) string {
	names := make([]string, 0, len(windows))
	seen := map[string]struct{}{}
	for _, window := range windows {
		name := quotaWindowName(window)
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	reason := "quota empty"
	if len(names) > 0 {
		reason += ": " + strings.Join(names, ", ")
	}
	if !latestReset.IsZero() {
		reason += "; reset at " + latestReset.UTC().Format(time.RFC3339)
	}
	return reason
}

func quotaWindowName(window AccountQuotaWindowState) string {
	if key := strings.TrimSpace(window.Key); key != "" {
		return key
	}
	if kind := strings.TrimSpace(window.Kind); kind != "" {
		return kind
	}
	return "quota"
}

func quotaGuardLookupKeys(authIDs []string, extra []string) []string {
	keys := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}
	for _, authID := range authIDs {
		add(authID)
		add("auth-id:" + authID)
	}
	for _, key := range extra {
		add(key)
	}
	return keys
}

func normalizeQuotaGuardIDs(ids []string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func firstQuotaGuardAuthID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func isQuotaEmptyResultError(err *coreauth.Error) bool {
	if err == nil {
		return false
	}
	status := err.StatusCode()
	if status != http.StatusPaymentRequired && status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Code + " " + err.Message))
	return strings.Contains(text, "quota") ||
		strings.Contains(text, "usage_limit") ||
		strings.Contains(text, "usage limit") ||
		strings.Contains(text, "insufficient_quota") ||
		strings.Contains(text, "capacity exhausted") ||
		strings.Contains(text, "exhausted your capacity")
}
