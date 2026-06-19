package gettokenshooks

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type QuotaFactStatus string

const (
	QuotaFactFresh    QuotaFactStatus = "fresh"
	QuotaFactStale    QuotaFactStatus = "stale"
	QuotaFactDegraded QuotaFactStatus = "degraded"
	QuotaFactMissing  QuotaFactStatus = "missing"
)

type SimulationFacts struct {
	Now          time.Time              `json:"now"`
	Request      RouteRequestFacts      `json:"request"`
	Accounts     []AccountFacts         `json:"accounts"`
	RuntimeState RuntimeGuardStateFacts `json:"runtimeState,omitempty"`
}

type RouteRequestFacts struct {
	Channel string `json:"channel,omitempty"`
	Model   string `json:"model,omitempty"`
	Project string `json:"project,omitempty"`
}

type RuntimeGuardStateFacts struct {
	Sources map[string][]string `json:"sources,omitempty"`
}

type AccountFacts struct {
	AccountID         string             `json:"accountId"`
	QuotaWindow       *QuotaWindowFacts  `json:"quotaWindow,omitempty"`
	QuotaWindows      []QuotaWindowFacts `json:"quotaWindows,omitempty"`
	CalibrationLedger []CalibrationFact  `json:"calibrationLedger,omitempty"`
}

type QuotaWindowFacts struct {
	WindowID                 string          `json:"windowId"`
	Kind                     string          `json:"kind,omitempty"`
	Metric                   string          `json:"metric,omitempty"`
	Timezone                 string          `json:"timezone,omitempty"`
	StartsAt                 time.Time       `json:"startsAt"`
	EndsAt                   time.Time       `json:"endsAt"`
	ObservedUsed             float64         `json:"observedUsed"`
	ObservedLimit            float64         `json:"observedLimit"`
	ObservedRemaining        float64         `json:"observedRemaining"`
	ObservedUsedPercent      float64         `json:"observedUsedPercent,omitempty"`
	ObservedRemainingPercent float64         `json:"observedRemainingPercent,omitempty"`
	RawUsed                  float64         `json:"rawUsed,omitempty"`
	CalibrationDelta         float64         `json:"calibrationDelta,omitempty"`
	CalibratedUsed           float64         `json:"calibratedUsed,omitempty"`
	GeneratedAt              time.Time       `json:"generatedAt,omitempty"`
	Source                   string          `json:"source,omitempty"`
	RecoverySource           string          `json:"recoverySource,omitempty"`
	Status                   QuotaFactStatus `json:"status"`
}

type CalibrationFact struct {
	ID         string     `json:"id"`
	AccountID  string     `json:"accountId,omitempty"`
	AccountKey string     `json:"account_key,omitempty"`
	WindowID   string     `json:"windowId,omitempty"`
	WindowKey  string     `json:"window_key,omitempty"`
	Metric     string     `json:"metric,omitempty"`
	Mode       string     `json:"mode,omitempty"`
	Value      float64    `json:"value,omitempty"`
	CreatedAt  time.Time  `json:"createdAt,omitempty"`
	ExpiresAt  time.Time  `json:"expiresAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

type SimulationResult struct {
	Now      time.Time              `json:"now"`
	Accounts []AccountDecisionTrace `json:"accounts"`
	Summary  SimulationSummary      `json:"summary"`
}

type SimulationSummary struct {
	TotalAccounts      int `json:"totalAccounts"`
	BlockedAccounts    int `json:"blockedAccounts"`
	DiagnosticAccounts int `json:"diagnosticAccounts"`
	AllowedAccounts    int `json:"allowedAccounts"`
}

type AccountDecisionTrace struct {
	AccountID   string             `json:"accountId"`
	Decision    RouteGuardDecision `json:"decision"`
	MatchedRule *MatchedRuleTrace  `json:"matchedRule,omitempty"`
	ReasonTrace []ReasonTraceStep  `json:"reasonTrace"`
	RecoveryAt  *time.Time         `json:"recoveryAt,omitempty"`
	ExpiresAt   *time.Time         `json:"expiresAt,omitempty"`
}

type RouteGuardDecision struct {
	Denied     bool   `json:"denied"`
	Action     string `json:"action"`
	DenySource string `json:"denySource,omitempty"`
	Reason     string `json:"reason"`
}

type MatchedRuleTrace struct {
	RuleID   string `json:"ruleId"`
	RuleName string `json:"ruleName,omitempty"`
	Kind     string `json:"kind"`
}

type ReasonTraceStep struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func SimulateQuotaThresholdRules(rules []AccountQuotaThresholdRule, facts SimulationFacts) SimulationResult {
	now := facts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	result := SimulationResult{
		Now:      now,
		Accounts: make([]AccountDecisionTrace, 0, len(facts.Accounts)),
	}
	for _, account := range facts.Accounts {
		state, calibrationTrace := accountQuotaRuntimeStateFromSimulationFacts(account, now)
		trace := simulateQuotaThresholdAccount(state, rules, now)
		if len(calibrationTrace) > 0 {
			trace.ReasonTrace = append(calibrationTrace, trace.ReasonTrace...)
		}
		result.Accounts = append(result.Accounts, trace)
	}
	sort.SliceStable(result.Accounts, func(i, j int) bool {
		return result.Accounts[i].AccountID < result.Accounts[j].AccountID
	})
	result.Summary = summarizeSimulationAccounts(result.Accounts)
	return result
}

func simulateQuotaThresholdAccount(state AccountQuotaRuntimeState, rules []AccountQuotaThresholdRule, now time.Time) AccountDecisionTrace {
	accountKey := strings.TrimSpace(state.AccountKey)
	trace := AccountDecisionTrace{
		AccountID: accountKey,
		Decision: RouteGuardDecision{
			Denied: false,
			Action: "allow",
			Reason: "no quota-threshold rule matched",
		},
	}
	for _, rule := range rules {
		decision, match := evaluateQuotaThresholdRuleDecision(state, rule, now)
		if len(decision.ReasonTrace) > 0 {
			trace.ReasonTrace = append(trace.ReasonTrace, decision.ReasonTrace...)
		}
		if decision.MatchedRule == nil && !quotaThresholdRuleTargetsAccount(rule, accountKey) {
			continue
		}
		if decision.Decision.Action == "allow" && !match.Window.ResetAt.IsZero() {
			continue
		}
		if decision.Decision.Action != "allow" || decision.MatchedRule != nil {
			return decision
		}
	}
	if len(trace.ReasonTrace) == 0 {
		trace.ReasonTrace = append(trace.ReasonTrace, ReasonTraceStep{
			Code:    "routeguard.action.allow",
			Message: "no quota-threshold rule matched this account",
		})
	}
	return trace
}

func evaluateQuotaThresholdRuleDecision(state AccountQuotaRuntimeState, rule AccountQuotaThresholdRule, now time.Time) (AccountDecisionTrace, quotaThresholdRuleEvaluation) {
	accountKey := strings.TrimSpace(state.AccountKey)
	trace := AccountDecisionTrace{
		AccountID: accountKey,
		Decision:  RouteGuardDecision{Denied: false, Action: "allow", Reason: "quota-threshold rule did not match"},
	}
	if !rule.Enabled {
		return trace, quotaThresholdRuleEvaluation{}
	}
	if !quotaThresholdRuleTargetsAccount(rule, accountKey) {
		return trace, quotaThresholdRuleEvaluation{}
	}
	status := quotaFactStatusFromRuntimeState(state)
	trace.ReasonTrace = append(trace.ReasonTrace, ReasonTraceStep{
		Code:    "quota.window.loaded",
		Message: fmt.Sprintf("quota fact status is %s", status),
		Data: map[string]any{
			"status": string(status),
		},
	})
	match, steps, missingFact := evaluateQuotaThresholdRuleMatchTrace(state, rule, now)
	trace.ReasonTrace = append(trace.ReasonTrace, steps...)
	if missingFact {
		trace.Decision = RouteGuardDecision{
			Denied: false,
			Action: "diagnostic",
			Reason: "quota fact missing; no hard block created",
		}
		return trace, quotaThresholdRuleEvaluation{}
	}
	if match.Window.ResetAt.IsZero() {
		trace.Decision = RouteGuardDecision{
			Denied: false,
			Action: "allow",
			Reason: "quota-threshold rule did not match",
		}
		return trace, quotaThresholdRuleEvaluation{}
	}
	trace.MatchedRule = &MatchedRuleTrace{
		RuleID: strings.TrimSpace(rule.ID),
		Kind:   "quota-threshold",
	}
	expiresAt := match.Window.ResetAt.UTC()
	trace.ExpiresAt = &expiresAt
	trace.RecoveryAt = &expiresAt
	if status != QuotaFactFresh {
		trace.Decision = RouteGuardDecision{
			Denied: false,
			Action: "diagnostic",
			Reason: fmt.Sprintf("quota-threshold matched but quota fact is %s; no hard block created", status),
		}
		trace.ReasonTrace = append(trace.ReasonTrace, ReasonTraceStep{
			Code:    "routeguard.action.diagnostic",
			Message: "quota-threshold match recorded as diagnostic because quota fact is not fresh",
			Data:    map[string]any{"status": string(status)},
		})
		return trace, match
	}
	reason := quotaThresholdGuardReason(rule, match)
	trace.Decision = RouteGuardDecision{
		Denied:     true,
		Action:     "block",
		DenySource: AccountRouteGuardSourceQuotaThreshold,
		Reason:     reason,
	}
	trace.ReasonTrace = append(trace.ReasonTrace, ReasonTraceStep{
		Code:    "routeguard.action.block",
		Message: "account denied by quota-threshold rule",
		Data: map[string]any{
			"denySource": AccountRouteGuardSourceQuotaThreshold,
			"ruleId":     strings.TrimSpace(rule.ID),
		},
	})
	return trace, match
}

func evaluateQuotaThresholdRuleMatchTrace(state AccountQuotaRuntimeState, rule AccountQuotaThresholdRule, now time.Time) (quotaThresholdRuleEvaluation, []ReasonTraceStep, bool) {
	if rule.Condition != nil {
		return evaluateQuotaRuleConditionTrace(state, *rule.Condition, now)
	}
	condition := AccountQuotaRuleCondition{
		Fact:       "quota.window",
		WindowKey:  rule.WindowKey,
		Metric:     rule.Metric,
		Comparator: rule.Comparator,
		Value:      rule.ThresholdPercent,
	}
	return evaluateQuotaRuleConditionTrace(state, condition, now)
}

func evaluateQuotaRuleConditionTrace(state AccountQuotaRuntimeState, condition AccountQuotaRuleCondition, now time.Time) (quotaThresholdRuleEvaluation, []ReasonTraceStep, bool) {
	if len(condition.All) > 0 {
		steps := []ReasonTraceStep{}
		var first quotaThresholdRuleEvaluation
		missing := false
		matchedAll := true
		for index, child := range condition.All {
			match, childSteps, childMissing := evaluateQuotaRuleConditionTrace(state, child, now)
			steps = append(steps, childSteps...)
			if childMissing {
				missing = true
			}
			if match.Window.ResetAt.IsZero() {
				matchedAll = false
				continue
			}
			if index == 0 || first.Window.ResetAt.IsZero() {
				first = match
			}
		}
		steps = append(steps, ReasonTraceStep{
			Code:    "condition.all.evaluated",
			Message: fmt.Sprintf("all condition matched=%t", matchedAll && !first.Window.ResetAt.IsZero()),
			Data:    map[string]any{"matched": matchedAll && !first.Window.ResetAt.IsZero()},
		})
		if !matchedAll || first.Window.ResetAt.IsZero() {
			return quotaThresholdRuleEvaluation{}, steps, missing
		}
		first.Trace = "all(" + strings.TrimPrefix(first.Trace, "all(")
		if !strings.HasSuffix(first.Trace, ")") {
			first.Trace += ")"
		}
		return first, steps, missing
	}
	if len(condition.Any) > 0 {
		steps := []ReasonTraceStep{}
		missing := false
		var first quotaThresholdRuleEvaluation
		for _, child := range condition.Any {
			match, childSteps, childMissing := evaluateQuotaRuleConditionTrace(state, child, now)
			steps = append(steps, childSteps...)
			if childMissing {
				missing = true
			}
			if first.Window.ResetAt.IsZero() && !match.Window.ResetAt.IsZero() {
				first = match
			}
		}
		matched := !first.Window.ResetAt.IsZero()
		steps = append(steps, ReasonTraceStep{Code: "condition.any.evaluated", Message: fmt.Sprintf("any condition matched=%t", matched), Data: map[string]any{"matched": matched}})
		if !matched {
			return quotaThresholdRuleEvaluation{}, steps, missing
		}
		first.Trace = "any(" + strings.TrimPrefix(first.Trace, "any(")
		if !strings.HasSuffix(first.Trace, ")") {
			first.Trace += ")"
		}
		return first, steps, missing
	}
	if condition.Not != nil {
		match, steps, missing := evaluateQuotaRuleConditionTrace(state, *condition.Not, now)
		matched := match.Window.ResetAt.IsZero() && !missing
		steps = append(steps, ReasonTraceStep{Code: "condition.not.evaluated", Message: fmt.Sprintf("not condition matched=%t", matched), Data: map[string]any{"matched": matched}})
		if matched {
			return quotaThresholdRuleEvaluation{Trace: "not"}, steps, false
		}
		return quotaThresholdRuleEvaluation{}, steps, missing
	}
	return evaluateQuotaRuleLeafConditionTrace(state, condition, now)
}

func evaluateQuotaRuleLeafConditionTrace(state AccountQuotaRuntimeState, condition AccountQuotaRuleCondition, now time.Time) (quotaThresholdRuleEvaluation, []ReasonTraceStep, bool) {
	window, ok := quotaThresholdWindow(state.Windows, condition.WindowKey, now)
	if !ok {
		windowKey := strings.TrimSpace(condition.WindowKey)
		return quotaThresholdRuleEvaluation{}, []ReasonTraceStep{{
			Code:    "quota.window.missing",
			Message: "quota window is missing or expired",
			Data:    map[string]any{"windowId": windowKey},
		}}, true
	}
	metric := normalizeQuotaRuleMetric(condition.Metric)
	actual, ok := quotaRuleMetricValue(window, metric)
	if !ok {
		return quotaThresholdRuleEvaluation{}, []ReasonTraceStep{{
			Code:    "quota.metric.unavailable",
			Message: "quota metric is unavailable",
			Data:    map[string]any{"metric": metric, "windowId": window.Key},
		}}, true
	}
	comparator := normalizeQuotaThresholdComparator(condition.Comparator, metric)
	threshold := normalizeQuotaRuleThreshold(metric, condition.Value)
	matched := quotaThresholdMatched(actual, comparator, threshold)
	step := ReasonTraceStep{
		Code:    "quota.metric.compared",
		Message: fmt.Sprintf("%s %.2f %s %.2f matched=%t", metric, actual, comparator, threshold, matched),
		Data: map[string]any{
			"metric":   metric,
			"actual":   actual,
			"operator": comparator,
			"expected": threshold,
			"matched":  matched,
			"windowId": window.Key,
		},
	}
	if !matched {
		return quotaThresholdRuleEvaluation{}, []ReasonTraceStep{step}, false
	}
	return quotaThresholdRuleEvaluation{
		Window:     window,
		Metric:     metric,
		Comparator: comparator,
		Actual:     actual,
		Threshold:  threshold,
		Trace:      fmt.Sprintf("%s %s %.2f", metric, comparator, threshold),
	}, []ReasonTraceStep{step}, false
}

func accountQuotaRuntimeStateFromSimulationFacts(account AccountFacts, now time.Time) (AccountQuotaRuntimeState, []ReasonTraceStep) {
	accountKey := strings.TrimSpace(account.AccountID)
	windowFacts := accountQuotaWindowFacts(account)
	state, windowTrace := accountQuotaRuntimeStateFromQuotaWindowFacts(accountKey, windowFacts, now)
	if len(windowTrace) > 0 {
		return state, windowTrace
	}
	calibrations := calibrationFactsToUsageCalibrations(accountKey, account.CalibrationLedger, now)
	trace := quotaCalibrationTraceSteps(accountKey, account.CalibrationLedger, now)
	state = ApplyQuotaUsageCalibrations(state, calibrations, now)
	return state, trace
}

func accountQuotaRuntimeStateFromQuotaWindowFacts(accountKey string, windowFacts []QuotaWindowFacts, now time.Time) (AccountQuotaRuntimeState, []ReasonTraceStep) {
	accountKey = strings.TrimSpace(accountKey)
	state := AccountQuotaRuntimeState{AccountKey: accountKey, EvaluatedAt: now}
	if len(windowFacts) == 0 {
		return state, []ReasonTraceStep{{
			Code:    "quota.window.missing",
			Message: "account has no quota window fact",
		}}
	}
	status := accountQuotaFactStatus(windowFacts)
	state.Fresh = status == QuotaFactFresh
	state.Stale = status == QuotaFactStale
	state.Degraded = status == QuotaFactDegraded
	windows := make([]AccountQuotaWindowState, 0, len(windowFacts))
	for _, windowFact := range windowFacts {
		window := quotaWindowStateFromFacts(windowFact)
		if window.Key == "" {
			continue
		}
		windows = append(windows, window)
		if state.ExpiresAt.IsZero() || (!window.ResetAt.IsZero() && window.ResetAt.After(state.ExpiresAt)) {
			state.ExpiresAt = window.ResetAt
		}
	}
	if len(windows) == 0 {
		return state, []ReasonTraceStep{{
			Code:    "quota.window.missing",
			Message: "account has no usable quota window fact",
		}}
	}
	state.Windows = windows
	return state, nil
}

func accountQuotaWindowFacts(account AccountFacts) []QuotaWindowFacts {
	out := make([]QuotaWindowFacts, 0, len(account.QuotaWindows)+1)
	if account.QuotaWindow != nil {
		out = append(out, *account.QuotaWindow)
	}
	out = append(out, account.QuotaWindows...)
	return out
}

func accountQuotaFactStatus(windows []QuotaWindowFacts) QuotaFactStatus {
	status := QuotaFactFresh
	for _, window := range windows {
		next := normalizeQuotaFactStatus(window.Status)
		switch next {
		case QuotaFactDegraded:
			return QuotaFactDegraded
		case QuotaFactStale:
			status = QuotaFactStale
		case QuotaFactMissing:
			if status == QuotaFactFresh {
				status = QuotaFactMissing
			}
		}
	}
	return status
}

func quotaWindowStateFromFacts(windowFact QuotaWindowFacts) AccountQuotaWindowState {
	kind := strings.TrimSpace(windowFact.Kind)
	if kind == "" {
		kind = "tokens"
	}
	window := AccountQuotaWindowState{
		Key:       strings.TrimSpace(windowFact.WindowID),
		Kind:      kind,
		Used:      windowFact.ObservedUsed,
		Remaining: windowFact.ObservedRemaining,
		Limit:     windowFact.ObservedLimit,
		ResetAt:   windowFact.EndsAt.UTC(),
	}
	if window.Used <= 0 && window.Limit > 0 {
		window.Used = clampQuotaValue(window.Limit-window.Remaining, 0, window.Limit)
	}
	return window
}

func calibrationFactsToUsageCalibrations(defaultAccountKey string, facts []CalibrationFact, now time.Time) []AccountQuotaUsageCalibration {
	out := make([]AccountQuotaUsageCalibration, 0, len(facts))
	for _, fact := range facts {
		accountKey := strings.TrimSpace(fact.AccountID)
		if accountKey == "" {
			accountKey = strings.TrimSpace(fact.AccountKey)
		}
		if accountKey == "" {
			accountKey = defaultAccountKey
		}
		windowKey := strings.TrimSpace(fact.WindowID)
		if windowKey == "" {
			windowKey = strings.TrimSpace(fact.WindowKey)
		}
		createdAt := fact.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		out = append(out, AccountQuotaUsageCalibration{
			ID:         strings.TrimSpace(fact.ID),
			AccountKey: accountKey,
			WindowKey:  windowKey,
			Metric:     strings.TrimSpace(fact.Metric),
			Mode:       strings.TrimSpace(fact.Mode),
			Value:      fact.Value,
			CreatedAt:  createdAt.UTC(),
			ExpiresAt:  fact.ExpiresAt.UTC(),
			RevokedAt:  fact.RevokedAt,
		})
	}
	return out
}

func quotaCalibrationTraceSteps(accountKey string, facts []CalibrationFact, now time.Time) []ReasonTraceStep {
	calibrations := calibrationFactsToUsageCalibrations(accountKey, facts, now)
	steps := make([]ReasonTraceStep, 0, len(calibrations))
	for _, calibration := range calibrations {
		applies := quotaUsageCalibrationApplies(calibration, accountKey, calibration.WindowKey, now)
		code := "quota.calibration.applied"
		message := "calibration participates in effective usage"
		if !applies {
			code = "quota.calibration.ignored"
			message = "calibration ignored for effective usage"
		}
		steps = append(steps, ReasonTraceStep{
			Code:    code,
			Message: message,
			Data: map[string]any{
				"calibrationId": calibration.ID,
				"windowId":      calibration.WindowKey,
				"mode":          normalizeQuotaUsageCalibrationMode(calibration.Mode),
				"value":         calibration.Value,
				"applies":       applies,
			},
		})
	}
	return steps
}

func quotaFactStatusFromRuntimeState(state AccountQuotaRuntimeState) QuotaFactStatus {
	if state.Degraded {
		return QuotaFactDegraded
	}
	if state.Stale {
		return QuotaFactStale
	}
	if state.Fresh {
		return QuotaFactFresh
	}
	return QuotaFactMissing
}

func normalizeQuotaFactStatus(status QuotaFactStatus) QuotaFactStatus {
	switch QuotaFactStatus(strings.ToLower(strings.TrimSpace(string(status)))) {
	case QuotaFactStale:
		return QuotaFactStale
	case QuotaFactDegraded:
		return QuotaFactDegraded
	case QuotaFactMissing:
		return QuotaFactMissing
	default:
		return QuotaFactFresh
	}
}

func quotaThresholdRuleTargetsAccount(rule AccountQuotaThresholdRule, accountKey string) bool {
	return strings.TrimSpace(rule.AccountKey) == strings.TrimSpace(accountKey)
}

func summarizeSimulationAccounts(accounts []AccountDecisionTrace) SimulationSummary {
	summary := SimulationSummary{TotalAccounts: len(accounts)}
	for _, account := range accounts {
		switch account.Decision.Action {
		case "block":
			summary.BlockedAccounts++
		case "diagnostic":
			summary.DiagnosticAccounts++
		default:
			summary.AllowedAccounts++
		}
	}
	return summary
}
