package gettokenshooks

import (
	"encoding/json"
	"fmt"
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
	Used      float64
	Remaining float64
	Limit     float64
	ResetAt   time.Time
	Exhausted bool
}

type AccountQuotaThresholdRule struct {
	ID               string                     `json:"id"`
	AccountKey       string                     `json:"account_key"`
	WindowKey        string                     `json:"window_key"`
	Metric           string                     `json:"metric"`
	Comparator       string                     `json:"comparator,omitempty"`
	ThresholdPercent float64                    `json:"threshold_percent"`
	Condition        *AccountQuotaRuleCondition `json:"condition,omitempty"`
	Enabled          bool                       `json:"enabled"`
}

type AccountQuotaRuleCondition struct {
	All        []AccountQuotaRuleCondition `json:"all,omitempty"`
	Any        []AccountQuotaRuleCondition `json:"any,omitempty"`
	Not        *AccountQuotaRuleCondition  `json:"not,omitempty"`
	Fact       string                      `json:"fact,omitempty"`
	WindowKey  string                      `json:"window_key,omitempty"`
	Metric     string                      `json:"metric,omitempty"`
	Comparator string                      `json:"comparator,omitempty"`
	Value      float64                     `json:"value,omitempty"`
}

func (rule *AccountQuotaThresholdRule) UnmarshalJSON(data []byte) error {
	type alias AccountQuotaThresholdRule
	var decoded struct {
		alias
		AccountKeyCamel       string   `json:"accountKey,omitempty"`
		WindowKeyCamel        string   `json:"windowKey,omitempty"`
		ThresholdPercentCamel *float64 `json:"thresholdPercent,omitempty"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	next := AccountQuotaThresholdRule(decoded.alias)
	if decoded.AccountKeyCamel != "" {
		next.AccountKey = decoded.AccountKeyCamel
	}
	if decoded.WindowKeyCamel != "" {
		next.WindowKey = decoded.WindowKeyCamel
	}
	if decoded.ThresholdPercentCamel != nil {
		next.ThresholdPercent = *decoded.ThresholdPercentCamel
	}
	*rule = next
	return nil
}

type AccountQuotaUsageCalibration struct {
	ID         string     `json:"id"`
	AccountKey string     `json:"account_key"`
	WindowKey  string     `json:"window_key"`
	Metric     string     `json:"metric"`
	Mode       string     `json:"mode"`
	Value      float64    `json:"value"`
	CreatedAt  time.Time  `json:"created_at,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
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

func QuotaThresholdRouteGuardBlocks(states []AccountQuotaRuntimeState, rules []AccountQuotaThresholdRule, now time.Time) []AccountRouteGuardBlock {
	if len(states) == 0 || len(rules) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	blocks := []AccountRouteGuardBlock{}
	for _, state := range states {
		for _, rule := range rules {
			block, ok := quotaThresholdRouteGuardBlock(state, rule, now)
			if !ok {
				continue
			}
			blocks = append(blocks, block)
		}
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		left := accountRouteGuardBlockKey(blocks[i])
		right := accountRouteGuardBlockKey(blocks[j])
		if left == right {
			return blocks[i].Reason < blocks[j].Reason
		}
		return left < right
	})
	return blocks
}

func ApplyQuotaUsageCalibrations(state AccountQuotaRuntimeState, calibrations []AccountQuotaUsageCalibration, now time.Time) AccountQuotaRuntimeState {
	if len(calibrations) == 0 || len(state.Windows) == 0 {
		return state
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	accountKey := strings.TrimSpace(state.AccountKey)
	if accountKey == "" {
		return state
	}
	state.Windows = append([]AccountQuotaWindowState(nil), state.Windows...)
	for windowIndex := range state.Windows {
		window := state.Windows[windowIndex]
		if window.Limit <= 0 {
			continue
		}
		observedUsed := quotaWindowObservedUsed(window)
		effectiveUsed := observedUsed
		for _, calibration := range calibrations {
			if !quotaUsageCalibrationApplies(calibration, accountKey, window.Key, now) {
				continue
			}
			switch normalizeQuotaUsageCalibrationMode(calibration.Mode) {
			case "set-effective":
				if calibration.Value > effectiveUsed {
					effectiveUsed = calibration.Value
				}
			default:
				effectiveUsed += calibration.Value
			}
		}
		effectiveUsed = clampQuotaValue(effectiveUsed, 0, window.Limit)
		window.Used = effectiveUsed
		window.Remaining = clampQuotaValue(window.Limit-effectiveUsed, 0, window.Limit)
		window.Exhausted = window.Remaining <= 0
		state.Windows[windowIndex] = window
	}
	return state
}

func quotaThresholdRouteGuardBlock(state AccountQuotaRuntimeState, rule AccountQuotaThresholdRule, now time.Time) (AccountRouteGuardBlock, bool) {
	accountKey := strings.TrimSpace(state.AccountKey)
	if accountKey == "" {
		return AccountRouteGuardBlock{}, false
	}
	decision, match := evaluateQuotaThresholdRuleDecision(state, rule, now)
	if !decision.Decision.Denied || decision.Decision.Action != "block" || match.Window.ResetAt.IsZero() {
		return AccountRouteGuardBlock{}, false
	}
	updatedAt := state.EvaluatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	authIDs := normalizeQuotaGuardIDs(state.AuthIDs)
	return AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceQuotaThreshold,
		AuthID:     firstQuotaGuardAuthID(authIDs),
		AccountKey: accountKey,
		LookupKeys: quotaGuardLookupKeys(authIDs, state.LookupKeys),
		Reason:     decision.Decision.Reason,
		ExpiresAt:  match.Window.ResetAt.UTC(),
		UpdatedAt:  updatedAt.UTC(),
	}, true
}

type quotaThresholdRuleEvaluation struct {
	Window     AccountQuotaWindowState
	Metric     string
	Comparator string
	Actual     float64
	Threshold  float64
	Trace      string
}

func quotaThresholdRuleMatch(state AccountQuotaRuntimeState, rule AccountQuotaThresholdRule, now time.Time) (quotaThresholdRuleEvaluation, bool) {
	if rule.Condition != nil {
		return quotaRuleConditionMatched(state, *rule.Condition, now)
	}
	window, ok := quotaThresholdWindow(state.Windows, rule.WindowKey, now)
	if !ok {
		return quotaThresholdRuleEvaluation{}, false
	}
	metric := normalizeQuotaRuleMetric(rule.Metric)
	actual, ok := quotaRuleMetricValue(window, metric)
	if !ok {
		return quotaThresholdRuleEvaluation{}, false
	}
	comparator := normalizeQuotaThresholdComparator(rule.Comparator, metric)
	threshold := normalizeQuotaRuleThreshold(metric, rule.ThresholdPercent)
	if !quotaThresholdMatched(actual, comparator, threshold) {
		return quotaThresholdRuleEvaluation{}, false
	}
	return quotaThresholdRuleEvaluation{
		Window:     window,
		Metric:     metric,
		Comparator: comparator,
		Actual:     actual,
		Threshold:  threshold,
		Trace:      "legacy-threshold",
	}, true
}

func quotaRuleConditionMatched(state AccountQuotaRuntimeState, condition AccountQuotaRuleCondition, now time.Time) (quotaThresholdRuleEvaluation, bool) {
	if len(condition.All) > 0 {
		var first quotaThresholdRuleEvaluation
		for index, child := range condition.All {
			match, ok := quotaRuleConditionMatched(state, child, now)
			if !ok {
				return quotaThresholdRuleEvaluation{}, false
			}
			if index == 0 || first.Window.ResetAt.IsZero() && !match.Window.ResetAt.IsZero() {
				first = match
			}
		}
		if first.Window.ResetAt.IsZero() {
			return quotaThresholdRuleEvaluation{}, false
		}
		first.Trace = "all(" + first.Trace + ")"
		return first, true
	}
	if len(condition.Any) > 0 {
		for _, child := range condition.Any {
			match, ok := quotaRuleConditionMatched(state, child, now)
			if ok && !match.Window.ResetAt.IsZero() {
				match.Trace = "any(" + match.Trace + ")"
				return match, true
			}
		}
		return quotaThresholdRuleEvaluation{}, false
	}
	if condition.Not != nil {
		_, ok := quotaRuleConditionMatched(state, *condition.Not, now)
		if ok {
			return quotaThresholdRuleEvaluation{}, false
		}
		return quotaThresholdRuleEvaluation{Trace: "not"}, true
	}
	return quotaRuleLeafConditionMatched(state, condition, now)
}

func quotaRuleLeafConditionMatched(state AccountQuotaRuntimeState, condition AccountQuotaRuleCondition, now time.Time) (quotaThresholdRuleEvaluation, bool) {
	window, ok := quotaThresholdWindow(state.Windows, condition.WindowKey, now)
	if !ok {
		return quotaThresholdRuleEvaluation{}, false
	}
	metric := normalizeQuotaRuleMetric(condition.Metric)
	actual, ok := quotaRuleMetricValue(window, metric)
	if !ok {
		return quotaThresholdRuleEvaluation{}, false
	}
	comparator := normalizeQuotaThresholdComparator(condition.Comparator, metric)
	threshold := normalizeQuotaRuleThreshold(metric, condition.Value)
	if !quotaThresholdMatched(actual, comparator, threshold) {
		return quotaThresholdRuleEvaluation{}, false
	}
	return quotaThresholdRuleEvaluation{
		Window:     window,
		Metric:     metric,
		Comparator: comparator,
		Actual:     actual,
		Threshold:  threshold,
		Trace:      fmt.Sprintf("%s %s %.2f", metric, comparator, threshold),
	}, true
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

func quotaUsageCalibrationApplies(calibration AccountQuotaUsageCalibration, accountKey string, windowKey string, now time.Time) bool {
	if strings.TrimSpace(calibration.AccountKey) != strings.TrimSpace(accountKey) {
		return false
	}
	if strings.TrimSpace(calibration.WindowKey) != strings.TrimSpace(windowKey) {
		return false
	}
	if metric := strings.ToLower(strings.TrimSpace(calibration.Metric)); metric != "" && metric != "tokens" {
		return false
	}
	if calibration.RevokedAt != nil && !calibration.RevokedAt.IsZero() && !calibration.RevokedAt.After(now) {
		return false
	}
	if !calibration.ExpiresAt.IsZero() && !calibration.ExpiresAt.After(now) {
		return false
	}
	if !calibration.CreatedAt.IsZero() && calibration.CreatedAt.After(now) {
		return false
	}
	return true
}

func normalizeQuotaUsageCalibrationMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "set-effective", "set_effective", "set":
		return "set-effective"
	default:
		return "delta"
	}
}

func quotaWindowObservedUsed(window AccountQuotaWindowState) float64 {
	if window.Used > 0 {
		return window.Used
	}
	if window.Limit > 0 {
		return clampQuotaValue(window.Limit-window.Remaining, 0, window.Limit)
	}
	return 0
}

func clampQuotaValue(value float64, min float64, max float64) float64 {
	if value < min {
		return min
	}
	if max > min && value > max {
		return max
	}
	return value
}

func quotaThresholdWindow(windows []AccountQuotaWindowState, windowKey string, now time.Time) (AccountQuotaWindowState, bool) {
	windowKey = strings.TrimSpace(windowKey)
	if windowKey == "" {
		return AccountQuotaWindowState{}, false
	}
	for _, window := range windows {
		if strings.TrimSpace(window.Key) != windowKey {
			continue
		}
		resetAt := window.ResetAt
		if resetAt.IsZero() || !resetAt.After(now) {
			return AccountQuotaWindowState{}, false
		}
		window.ResetAt = resetAt.UTC()
		return window, true
	}
	return AccountQuotaWindowState{}, false
}

func quotaThresholdPercent(window AccountQuotaWindowState, metric string) (float64, bool) {
	return quotaRuleMetricValue(window, metric)
}

func quotaRuleMetricValue(window AccountQuotaWindowState, metric string) (float64, bool) {
	switch normalizeQuotaRuleMetric(metric) {
	case "remaining-percent":
		if window.Limit <= 0 {
			return 0, false
		}
		return clampQuotaThresholdPercent((window.Remaining / window.Limit) * 100), true
	case "used-percent":
		if window.Limit <= 0 {
			return 0, false
		}
		used := window.Used
		if used <= 0 {
			used = window.Limit - window.Remaining
		}
		return clampQuotaThresholdPercent((used / window.Limit) * 100), true
	case "remaining":
		return window.Remaining, true
	case "used":
		used := window.Used
		if used <= 0 && window.Limit > 0 {
			used = window.Limit - window.Remaining
		}
		return used, true
	default:
		return 0, false
	}
}

func normalizeQuotaThresholdMetric(metric string) string {
	return normalizeQuotaRuleMetric(metric)
}

func normalizeQuotaRuleMetric(metric string) string {
	switch strings.ToLower(strings.TrimSpace(metric)) {
	case "used-percent", "used_percent":
		return "used-percent"
	case "remaining", "remaining-tokens", "remaining_tokens":
		return "remaining"
	case "used", "used-tokens", "used_tokens":
		return "used"
	default:
		return "remaining-percent"
	}
}

func normalizeQuotaThresholdComparator(comparator string, metric string) string {
	comparator = strings.TrimSpace(comparator)
	if comparator == "<" || comparator == "<=" || comparator == ">" || comparator == ">=" {
		return comparator
	}
	switch normalizeQuotaRuleMetric(metric) {
	case "used-percent", "used":
		return ">="
	default:
		return "<="
	}
}

func normalizeQuotaRuleThreshold(metric string, value float64) float64 {
	switch normalizeQuotaRuleMetric(metric) {
	case "remaining-percent", "used-percent":
		return clampQuotaThresholdPercent(value)
	default:
		return value
	}
}

func quotaThresholdMatched(actual float64, comparator string, threshold float64) bool {
	switch comparator {
	case "<":
		return actual < threshold
	case "<=":
		return actual <= threshold
	case ">":
		return actual > threshold
	case ">=":
		return actual >= threshold
	default:
		return actual <= threshold
	}
}

func clampQuotaThresholdPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func quotaThresholdGuardReason(rule AccountQuotaThresholdRule, match quotaThresholdRuleEvaluation) string {
	ruleID := strings.TrimSpace(rule.ID)
	if ruleID == "" {
		ruleID = "quota-threshold"
	}
	unit := ""
	if strings.Contains(match.Metric, "percent") {
		unit = "%"
	}
	trace := strings.TrimSpace(match.Trace)
	if trace == "" {
		trace = "threshold"
	}
	return fmt.Sprintf("quota threshold: rule=%s window=%s metric=%s actual=%.2f%s %s %.2f%s; trace=%s; reset at %s", ruleID, quotaWindowName(match.Window), match.Metric, match.Actual, unit, match.Comparator, match.Threshold, unit, trace, match.Window.ResetAt.UTC().Format(time.RFC3339))
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
