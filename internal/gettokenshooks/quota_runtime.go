package gettokenshooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	QuotaRuntimeStatusSuccess  = "success"
	QuotaRuntimeStatusError    = "error"
	QuotaRuntimeStatusStale    = "stale"
	QuotaRuntimeStatusDegraded = "degraded"
)

const (
	QuotaFactStateAvailable   = "available"
	QuotaFactStateNoQuota     = "no_quota"
	QuotaFactStateUnknown     = "unknown"
	QuotaFactStateStale       = "stale"
	QuotaFactStateDenied      = "denied"
	QuotaFactStateUnsupported = "unsupported"

	QuotaFactFreshnessFresh   = "fresh"
	QuotaFactFreshnessStale   = "stale"
	QuotaFactFreshnessUnknown = "unknown"

	QuotaFactConfidenceHigh   = "high"
	QuotaFactConfidenceMedium = "medium"
	QuotaFactConfidenceLow    = "low"
	QuotaFactConfidenceNone   = "none"

	QuotaFactRiskNone     = "none"
	QuotaFactRiskWarning  = "warning"
	QuotaFactRiskBlocking = "blocking"
	QuotaFactRiskDenied   = "denied"
	QuotaFactRiskUnknown  = "unknown"
)

var (
	quotaFactBearerSecretPattern = regexp.MustCompile(`(?i)bearer\s+[^\s,;]+`)
	quotaFactAPIKeySecretPattern = regexp.MustCompile(`(?i)(api[_ -]?key|authorization|cookie|token)\s*[:=]\s*[^\s,;]+`)
	quotaFactSKSecretPattern     = regexp.MustCompile(`sk-[A-Za-z0-9][A-Za-z0-9_-]{4,}`)
)

type QuotaRuntimeWindow struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	RemainingPercent *int     `json:"remaining_percent,omitempty"`
	UsedTokens       *float64 `json:"used_tokens,omitempty"`
	LimitTokens      *float64 `json:"limit_tokens,omitempty"`
	RemainingTokens  *float64 `json:"remaining_tokens,omitempty"`
	ResetLabel       string   `json:"reset_label,omitempty"`
	ResetAtUnix      int64    `json:"reset_at_unix,omitempty"`
}

type QuotaRuntimeBilling struct {
	IsAvailable  bool                      `json:"is_available"`
	BalanceInfos []QuotaRuntimeBalanceInfo `json:"balance_infos"`
}

type QuotaRuntimeBalanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

type QuotaRuntimeSourceState struct {
	Source    string `json:"source"`
	Reason    string `json:"reason,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	NextReset string `json:"next_reset,omitempty"`
}

type QuotaRuntimeFact struct {
	State        string   `json:"state"`
	Source       string   `json:"source"`
	Freshness    string   `json:"freshness"`
	Confidence   string   `json:"confidence"`
	Risk         string   `json:"risk"`
	Explanation  string   `json:"explanation"`
	ObservedAt   string   `json:"observed_at,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	EvidenceRefs []string `json:"evidence_refs"`
}

type QuotaRuntimeState struct {
	AccountKey      string                    `json:"account_key"`
	Source          string                    `json:"source,omitempty"`
	Status          string                    `json:"status"`
	PlanType        string                    `json:"plan_type,omitempty"`
	Windows         []QuotaRuntimeWindow      `json:"windows"`
	Billing         *QuotaRuntimeBilling      `json:"billing,omitempty"`
	UpdatedAt       string                    `json:"updated_at,omitempty"`
	LastEvaluatedAt string                    `json:"last_evaluated_at,omitempty"`
	Stale           bool                      `json:"stale,omitempty"`
	DegradedReason  string                    `json:"degraded_reason,omitempty"`
	Blocked         bool                      `json:"blocked"`
	BlockReason     string                    `json:"block_reason,omitempty"`
	Sources         []QuotaRuntimeSourceState `json:"sources"`
	Fact            *QuotaRuntimeFact         `json:"fact,omitempty"`
}

func (state QuotaRuntimeState) MarshalJSON() ([]byte, error) {
	type quotaRuntimeStateJSON QuotaRuntimeState
	return json.Marshal(struct {
		quotaRuntimeStateJSON
		QuotaFact *QuotaRuntimeFact `json:"quotaFact,omitempty"`
	}{
		quotaRuntimeStateJSON: quotaRuntimeStateJSON(state),
		QuotaFact:             cloneQuotaRuntimeFact(state.Fact),
	})
}

type QuotaRuntimeStore struct {
	mu     sync.RWMutex
	states map[string]QuotaRuntimeState
	guard  *AccountRouteGuardStore
}

var defaultQuotaRuntimeStore = NewQuotaRuntimeStore(DefaultAccountRouteGuardStore())

func NewQuotaRuntimeStore(guard *AccountRouteGuardStore) *QuotaRuntimeStore {
	if guard == nil {
		guard = DefaultAccountRouteGuardStore()
	}
	return &QuotaRuntimeStore{
		states: map[string]QuotaRuntimeState{},
		guard:  guard,
	}
}

func DefaultQuotaRuntimeStore() *QuotaRuntimeStore {
	return defaultQuotaRuntimeStore
}

func (s *QuotaRuntimeStore) Upsert(input QuotaRuntimeState, now time.Time) (QuotaRuntimeState, error) {
	if s == nil {
		return QuotaRuntimeState{}, fmt.Errorf("quota runtime store is not initialized")
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	state, err := normalizeQuotaRuntimeState(input, now)
	if err != nil {
		return QuotaRuntimeState{}, err
	}
	s.syncGuard(state, now)
	state = s.withGuardState(state)

	s.mu.Lock()
	s.states[state.AccountKey] = cloneQuotaRuntimeState(state)
	s.mu.Unlock()
	return state, nil
}

func (s *QuotaRuntimeStore) StateForAccount(accountKey string) (QuotaRuntimeState, bool) {
	if s == nil {
		return QuotaRuntimeState{}, false
	}
	accountKey = strings.TrimSpace(accountKey)
	s.mu.RLock()
	state, ok := s.states[accountKey]
	s.mu.RUnlock()
	if !ok {
		return QuotaRuntimeState{}, false
	}
	return s.withGuardState(cloneQuotaRuntimeState(state)), true
}

func (s *QuotaRuntimeStore) StatesForAccounts(accountKeys []string) []QuotaRuntimeState {
	if s == nil {
		return nil
	}
	now := time.Now().UTC()
	states := make([]QuotaRuntimeState, 0, len(accountKeys))
	s.mu.RLock()
	for _, accountKey := range accountKeys {
		accountKey = strings.TrimSpace(accountKey)
		if accountKey == "" {
			continue
		}
		if state, ok := s.states[accountKey]; ok {
			states = append(states, cloneQuotaRuntimeState(state))
			continue
		}
		states = append(states, quotaRuntimeStateWithFact(QuotaRuntimeState{
			AccountKey: accountKey,
			Status:     QuotaRuntimeStatusStale,
			Windows:    []QuotaRuntimeWindow{},
			Sources:    []QuotaRuntimeSourceState{},
		}, now))
	}
	s.mu.RUnlock()
	for index := range states {
		states[index] = s.withGuardState(states[index])
	}
	return states
}

func (s *QuotaRuntimeStore) States() []QuotaRuntimeState {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	states := make([]QuotaRuntimeState, 0, len(s.states))
	for _, state := range s.states {
		states = append(states, cloneQuotaRuntimeState(state))
	}
	s.mu.RUnlock()
	for index := range states {
		states[index] = s.withGuardState(states[index])
	}
	sort.SliceStable(states, func(i, j int) bool {
		return states[i].AccountKey < states[j].AccountKey
	})
	return states
}

func (s *QuotaRuntimeStore) syncGuard(state QuotaRuntimeState, now time.Time) {
	if s == nil || s.guard == nil || strings.TrimSpace(state.AccountKey) == "" {
		return
	}
	if !strings.EqualFold(state.Status, QuotaRuntimeStatusSuccess) || state.Stale || quotaRuntimeStateIsDegraded(state) {
		return
	}
	s.guard.ClearAuth(AccountRouteGuardSourceQuotaEmpty, state.AccountKey)
	blocks := quotaRuntimeRouteGuardBlocks(state, now)
	for _, block := range blocks {
		s.guard.MarkBlocked(block)
	}
}

func (s *QuotaRuntimeStore) withGuardState(state QuotaRuntimeState) QuotaRuntimeState {
	state.Sources = []QuotaRuntimeSourceState{}
	state.Blocked = false
	state.BlockReason = ""
	if s == nil || s.guard == nil || strings.TrimSpace(state.AccountKey) == "" {
		return quotaRuntimeStateWithExistingFact(state, time.Now().UTC())
	}
	blocks := s.guard.ActiveBlocksForAuth(accountRouteGuardAuthForAccountKey(state.AccountKey))
	for _, block := range blocks {
		source := QuotaRuntimeSourceState{
			Source: strings.TrimSpace(block.Source),
			Reason: strings.TrimSpace(block.Reason),
		}
		if !block.ExpiresAt.IsZero() {
			source.ExpiresAt = block.ExpiresAt.UTC().Format(time.RFC3339)
			source.NextReset = source.ExpiresAt
		}
		state.Sources = append(state.Sources, source)
	}
	state.Blocked = len(state.Sources) > 0
	if state.Blocked {
		reasons := make([]string, 0, len(state.Sources))
		for _, source := range state.Sources {
			if source.Reason != "" {
				reasons = append(reasons, source.Reason)
			}
		}
		state.BlockReason = strings.Join(reasons, "; ")
	}
	return quotaRuntimeStateWithExistingFact(state, time.Now().UTC())
}

func quotaRuntimeRouteGuardBlocks(state QuotaRuntimeState, now time.Time) []AccountRouteGuardBlock {
	windows := make([]AccountQuotaWindowState, 0, len(state.Windows))
	for _, window := range state.Windows {
		remaining, ok := quotaRuntimeWindowRemaining(window)
		if !ok {
			continue
		}
		windows = append(windows, AccountQuotaWindowState{
			Key:       quotaRuntimeWindowKey(window),
			Remaining: remaining,
			ResetAt:   quotaRuntimeWindowResetAt(window),
			Exhausted: remaining <= 0,
		})
	}
	return QuotaEmptyRouteGuardBlocks([]AccountQuotaRuntimeState{{
		AccountKey:     state.AccountKey,
		Source:         firstNonEmptyQuotaRuntimeString(state.Source, "quota-runtime"),
		Fresh:          strings.EqualFold(state.Status, QuotaRuntimeStatusSuccess),
		Stale:          state.Stale,
		Degraded:       quotaRuntimeStateIsDegraded(state),
		DegradedReason: state.DegradedReason,
		EvaluatedAt:    parseQuotaRuntimeTimestamp(state.LastEvaluatedAt, now),
		Windows:        windows,
	}}, now)
}

func normalizeQuotaRuntimeState(input QuotaRuntimeState, now time.Time) (QuotaRuntimeState, error) {
	state := cloneQuotaRuntimeState(input)
	state.AccountKey = strings.TrimSpace(state.AccountKey)
	if !accountstore.IsAccountKey(state.AccountKey) {
		return QuotaRuntimeState{}, fmt.Errorf("invalid account_key %q", state.AccountKey)
	}
	state.Source = strings.TrimSpace(state.Source)
	if state.Source == "" {
		state.Source = "quota-runtime"
	}
	state.Status = strings.TrimSpace(state.Status)
	if state.Status == "" {
		state.Status = QuotaRuntimeStatusSuccess
	}
	switch state.Status {
	case QuotaRuntimeStatusSuccess, QuotaRuntimeStatusError, QuotaRuntimeStatusStale, QuotaRuntimeStatusDegraded:
	default:
		return QuotaRuntimeState{}, fmt.Errorf("unsupported quota status %q", state.Status)
	}
	state.PlanType = strings.TrimSpace(state.PlanType)
	state.DegradedReason = strings.TrimSpace(state.DegradedReason)
	state.Stale = state.Stale || strings.EqualFold(state.Status, QuotaRuntimeStatusStale)
	if state.UpdatedAt == "" {
		state.UpdatedAt = now.Format(time.RFC3339)
	}
	if state.LastEvaluatedAt == "" {
		state.LastEvaluatedAt = now.Format(time.RFC3339)
	}
	if state.Windows == nil {
		state.Windows = []QuotaRuntimeWindow{}
	}
	if state.Billing != nil && state.Billing.BalanceInfos == nil {
		state.Billing.BalanceInfos = []QuotaRuntimeBalanceInfo{}
	}
	state.Sources = []QuotaRuntimeSourceState{}
	state.Blocked = false
	state.BlockReason = ""
	state = quotaRuntimeStateWithFact(state, now)
	return state, nil
}

func quotaRuntimeWindowRemaining(window QuotaRuntimeWindow) (float64, bool) {
	if window.RemainingPercent != nil {
		return float64(*window.RemainingPercent), true
	}
	if window.RemainingTokens != nil {
		return *window.RemainingTokens, true
	}
	return 0, false
}

func quotaRuntimeWindowKey(window QuotaRuntimeWindow) string {
	if value := strings.TrimSpace(window.ID); value != "" {
		return value
	}
	if value := strings.TrimSpace(window.Label); value != "" {
		return value
	}
	return "quota"
}

func quotaRuntimeWindowResetAt(window QuotaRuntimeWindow) time.Time {
	if window.ResetAtUnix <= 0 {
		return time.Time{}
	}
	return time.Unix(window.ResetAtUnix, 0).UTC()
}

func quotaRuntimeStateIsDegraded(state QuotaRuntimeState) bool {
	return state.Status == QuotaRuntimeStatusError ||
		state.Status == QuotaRuntimeStatusDegraded ||
		strings.TrimSpace(state.DegradedReason) != ""
}

func accountRouteGuardAuthForAccountKey(accountKey string) *coreauth.Auth {
	return &coreauth.Auth{AccountKey: strings.TrimSpace(accountKey)}
}

func parseQuotaRuntimeTimestamp(value string, fallback time.Time) time.Time {
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
		return parsed.UTC()
	}
	return fallback.UTC()
}

func firstNonEmptyQuotaRuntimeString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func EnsureQuotaRuntimeFact(state QuotaRuntimeState, now time.Time) QuotaRuntimeState {
	return quotaRuntimeStateWithFact(state, now)
}

func BuildQuotaRuntimeFact(state QuotaRuntimeState, now time.Time) QuotaRuntimeFact {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	source := firstNonEmptyQuotaRuntimeString(state.Source, "quota-runtime")
	observedAt := quotaRuntimeFactObservedAt(state, now)
	evidenceRefs := quotaRuntimeFactEvidenceRefs(state, source)
	base := QuotaRuntimeFact{
		Source:       source,
		ObservedAt:   observedAt.UTC().Format(time.RFC3339),
		EvidenceRefs: evidenceRefs,
	}

	reason := quotaRuntimeFactReason(state)
	if quotaRuntimeFactUnsupported(reason) {
		base.State = QuotaFactStateUnsupported
		base.Freshness = QuotaFactFreshnessUnknown
		base.Confidence = QuotaFactConfidenceNone
		base.Risk = QuotaFactRiskUnknown
		base.Explanation = quotaRuntimeFactExplanation("Quota check is unsupported or not configured", reason)
		return base
	}
	if quotaRuntimeFactDenied(state, reason) {
		base.State = QuotaFactStateDenied
		base.Freshness = quotaRuntimeFactFreshnessForDenied(state)
		base.Confidence = QuotaFactConfidenceHigh
		base.Risk = QuotaFactRiskDenied
		base.Explanation = quotaRuntimeFactExplanation("Provider denied quota check", reason)
		return base
	}
	if quotaRuntimeStateIsStaleFact(state) {
		if !quotaRuntimeFactHasDisplayData(state) {
			base.State = QuotaFactStateUnknown
			base.Freshness = QuotaFactFreshnessUnknown
			base.Confidence = QuotaFactConfidenceNone
			base.Risk = QuotaFactRiskUnknown
			base.Explanation = "Quota runtime has no current quota evidence."
			return base
		}
		base.State = QuotaFactStateStale
		base.Freshness = QuotaFactFreshnessStale
		base.Confidence = QuotaFactConfidenceLow
		base.Risk = QuotaFactRiskWarning
		base.Explanation = quotaRuntimeFactExplanation("Quota runtime is using stale or degraded cached quota", reason)
		if expiresAt := latestQuotaRuntimeWindowReset(state.Windows, now); !expiresAt.IsZero() {
			base.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
		}
		return base
	}

	windowFacts := quotaRuntimeWindowFacts(state.Windows, now)
	if windowFacts.known > 0 && windowFacts.exhausted == windowFacts.known && !windowFacts.latestExhaustedReset.IsZero() {
		base.State = QuotaFactStateNoQuota
		base.Freshness = QuotaFactFreshnessFresh
		base.Confidence = QuotaFactConfidenceHigh
		base.Risk = QuotaFactRiskBlocking
		base.ExpiresAt = windowFacts.latestExhaustedReset.UTC().Format(time.RFC3339)
		base.Explanation = "Known quota windows are exhausted until " + base.ExpiresAt + "."
		return base
	}
	if windowFacts.positive > 0 || quotaRuntimeBillingHasEvidence(state.Billing) {
		base.State = QuotaFactStateAvailable
		base.Freshness = QuotaFactFreshnessFresh
		base.Confidence = QuotaFactConfidenceHigh
		base.Risk = QuotaFactRiskNone
		base.Explanation = "Quota runtime has fresh available quota evidence."
		if expiresAt := latestQuotaRuntimeWindowReset(state.Windows, now); !expiresAt.IsZero() {
			base.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
		}
		return base
	}

	base.State = QuotaFactStateUnknown
	base.Freshness = QuotaFactFreshnessUnknown
	base.Confidence = QuotaFactConfidenceNone
	base.Risk = QuotaFactRiskUnknown
	base.Explanation = "Quota runtime has no current quota evidence."
	return base
}

type quotaRuntimeWindowFactSummary struct {
	known                int
	positive             int
	exhausted            int
	latestExhaustedReset time.Time
}

func quotaRuntimeStateWithFact(state QuotaRuntimeState, now time.Time) QuotaRuntimeState {
	if state.Fact != nil {
		state.Fact = normalizeProvidedQuotaRuntimeFact(state.Fact, state, now)
		return state
	}
	fact := BuildQuotaRuntimeFact(state, now)
	state.Fact = &fact
	return state
}

func quotaRuntimeStateWithExistingFact(state QuotaRuntimeState, now time.Time) QuotaRuntimeState {
	if state.Fact == nil {
		return state
	}
	state.Fact = normalizeProvidedQuotaRuntimeFact(state.Fact, state, now)
	return state
}

func normalizeProvidedQuotaRuntimeFact(input *QuotaRuntimeFact, state QuotaRuntimeState, now time.Time) *QuotaRuntimeFact {
	if input == nil {
		return nil
	}
	fact := *input
	fact.State = strings.TrimSpace(fact.State)
	fact.Source = strings.TrimSpace(fact.Source)
	if fact.Source == "" {
		fact.Source = firstNonEmptyQuotaRuntimeString(state.Source, "quota-runtime")
	}
	fact.Freshness = strings.TrimSpace(fact.Freshness)
	fact.Confidence = strings.TrimSpace(fact.Confidence)
	fact.Risk = strings.TrimSpace(fact.Risk)
	fact.Explanation = sanitizeQuotaRuntimeFactExplanation(fact.Explanation)
	fact.ObservedAt = strings.TrimSpace(fact.ObservedAt)
	if fact.ObservedAt == "" {
		if now.IsZero() {
			now = time.Now()
		}
		fact.ObservedAt = quotaRuntimeFactObservedAt(state, now.UTC()).Format(time.RFC3339)
	}
	fact.ExpiresAt = strings.TrimSpace(fact.ExpiresAt)
	fact.EvidenceRefs = uniqueQuotaRuntimeFactRefs(fact.EvidenceRefs)
	if fact.EvidenceRefs == nil {
		fact.EvidenceRefs = []string{}
	}
	return &fact
}

func quotaRuntimeFactObservedAt(state QuotaRuntimeState, now time.Time) time.Time {
	if parsed := parseQuotaRuntimeTimestamp(state.LastEvaluatedAt, time.Time{}); !parsed.IsZero() {
		return parsed.UTC()
	}
	if parsed := parseQuotaRuntimeTimestamp(state.UpdatedAt, time.Time{}); !parsed.IsZero() {
		return parsed.UTC()
	}
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func quotaRuntimeFactReason(state QuotaRuntimeState) string {
	return firstNonEmptyQuotaRuntimeString(state.DegradedReason, state.BlockReason, state.Status)
}

func quotaRuntimeFactUnsupported(reason string) bool {
	text := strings.ToLower(strings.TrimSpace(reason))
	return strings.Contains(text, "unsupported") ||
		strings.Contains(text, "does not support quota") ||
		strings.Contains(text, "quota curl is not configured") ||
		strings.Contains(text, "quota refresh is not configured") ||
		strings.Contains(text, "account kind does not support")
}

func quotaRuntimeFactDenied(state QuotaRuntimeState, reason string) bool {
	text := strings.ToLower(strings.TrimSpace(state.Status + " " + reason))
	return strings.Contains(text, "status 401") ||
		strings.Contains(text, "status 402") ||
		strings.Contains(text, "status 403") ||
		strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "forbidden") ||
		strings.Contains(text, "permission denied") ||
		strings.Contains(text, "access denied") ||
		strings.Contains(text, "not authorized") ||
		strings.Contains(text, "invalid_grant") ||
		strings.Contains(text, "invalid auth") ||
		strings.Contains(text, "token_invalidated") ||
		strings.Contains(text, "deactivated_workspace")
}

func quotaRuntimeFactFreshnessForDenied(state QuotaRuntimeState) string {
	if state.Stale && quotaRuntimeFactHasDisplayData(state) {
		return QuotaFactFreshnessStale
	}
	return QuotaFactFreshnessFresh
}

func quotaRuntimeStateIsStaleFact(state QuotaRuntimeState) bool {
	return state.Stale ||
		strings.EqualFold(state.Status, QuotaRuntimeStatusStale) ||
		strings.EqualFold(state.Status, QuotaRuntimeStatusDegraded) ||
		quotaRuntimeStateIsDegraded(state)
}

func quotaRuntimeWindowFacts(windows []QuotaRuntimeWindow, now time.Time) quotaRuntimeWindowFactSummary {
	summary := quotaRuntimeWindowFactSummary{}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for _, window := range windows {
		remaining, ok := quotaRuntimeWindowRemaining(window)
		if !ok {
			continue
		}
		summary.known++
		if remaining > 0 {
			summary.positive++
			continue
		}
		summary.exhausted++
		resetAt := quotaRuntimeWindowResetAt(window)
		if resetAt.IsZero() || !resetAt.After(now) {
			continue
		}
		if summary.latestExhaustedReset.IsZero() || resetAt.After(summary.latestExhaustedReset) {
			summary.latestExhaustedReset = resetAt.UTC()
		}
	}
	return summary
}

func quotaRuntimeFactHasDisplayData(state QuotaRuntimeState) bool {
	return strings.TrimSpace(state.PlanType) != "" ||
		len(state.Windows) > 0 ||
		quotaRuntimeBillingHasEvidence(state.Billing)
}

func quotaRuntimeBillingHasEvidence(billing *QuotaRuntimeBilling) bool {
	return billing != nil && (billing.IsAvailable || len(billing.BalanceInfos) > 0)
}

func latestQuotaRuntimeWindowReset(windows []QuotaRuntimeWindow, now time.Time) time.Time {
	latest := time.Time{}
	for _, window := range windows {
		resetAt := quotaRuntimeWindowResetAt(window)
		if resetAt.IsZero() || !resetAt.After(now) {
			continue
		}
		if latest.IsZero() || resetAt.After(latest) {
			latest = resetAt.UTC()
		}
	}
	return latest
}

func quotaRuntimeFactEvidenceRefs(state QuotaRuntimeState, source string) []string {
	refs := []string{}
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return
		}
		for _, existing := range refs {
			if existing == ref {
				return
			}
		}
		refs = append(refs, ref)
	}
	if source != "" {
		add("source:" + source)
	}
	for _, window := range state.Windows {
		add("quota.window:" + quotaRuntimeWindowKey(window))
	}
	if quotaRuntimeBillingHasEvidence(state.Billing) {
		add("billing:available")
	}
	for _, sourceState := range state.Sources {
		if sourceState.Source != "" {
			add("route_guard:" + sourceState.Source)
		}
	}
	sort.Strings(refs)
	return refs
}

func uniqueQuotaRuntimeFactRefs(values []string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, value := range values {
		ref := strings.TrimSpace(value)
		if ref == "" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func quotaRuntimeFactExplanation(prefix string, reason string) string {
	reason = sanitizeQuotaRuntimeFactExplanation(reason)
	if reason == "" {
		return prefix + "."
	}
	return prefix + ": " + reason
}

func sanitizeQuotaRuntimeFactExplanation(value string) string {
	text := strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	text = quotaFactBearerSecretPattern.ReplaceAllString(text, "Bearer [redacted]")
	text = quotaFactAPIKeySecretPattern.ReplaceAllString(text, "$1=[redacted]")
	text = quotaFactSKSecretPattern.ReplaceAllString(text, "sk-[redacted]")
	if len(text) > 240 {
		text = strings.TrimSpace(text[:240]) + "..."
	}
	return text
}

func cloneQuotaRuntimeState(state QuotaRuntimeState) QuotaRuntimeState {
	state.Windows = append([]QuotaRuntimeWindow(nil), state.Windows...)
	if state.Billing != nil {
		billing := *state.Billing
		billing.BalanceInfos = append([]QuotaRuntimeBalanceInfo(nil), billing.BalanceInfos...)
		state.Billing = &billing
	}
	state.Sources = append([]QuotaRuntimeSourceState(nil), state.Sources...)
	if state.Fact != nil {
		state.Fact = cloneQuotaRuntimeFact(state.Fact)
	}
	return state
}

func cloneQuotaRuntimeFact(input *QuotaRuntimeFact) *QuotaRuntimeFact {
	if input == nil {
		return nil
	}
	fact := *input
	fact.EvidenceRefs = append([]string(nil), input.EvidenceRefs...)
	return &fact
}

func ConfigureQuotaRuntimeRoutes(group *gin.RouterGroup, _ *handlers.BaseAPIHandler, _ *config.Config) {
	configureQuotaRuntimeRoutes(group, defaultQuotaRuntimeStore)
}

func configureQuotaRuntimeRoutes(group *gin.RouterGroup, store *QuotaRuntimeStore) {
	if group == nil {
		return
	}
	group.GET("/gettokens/quota-status", func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota runtime store is not initialized"})
			return
		}
		accountKeys := quotaRuntimeRequestedAccountKeys(c)
		if len(accountKeys) > 1 || strings.TrimSpace(c.Query("account_keys")) != "" {
			c.JSON(http.StatusOK, gin.H{"items": store.StatesForAccounts(accountKeys)})
			return
		}
		if len(accountKeys) == 1 {
			accountKey := accountKeys[0]
			if state, exists := store.StateForAccount(accountKey); exists {
				c.JSON(http.StatusOK, state)
				return
			}
			c.JSON(http.StatusOK, quotaRuntimeStateWithFact(QuotaRuntimeState{AccountKey: accountKey, Status: QuotaRuntimeStatusStale, Windows: []QuotaRuntimeWindow{}, Sources: []QuotaRuntimeSourceState{}}, time.Now().UTC()))
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": store.States()})
	})
	group.PUT("/gettokens/quota-status/:account_key", func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota runtime store is not initialized"})
			return
		}
		var state QuotaRuntimeState
		if err := c.ShouldBindJSON(&state); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		state.AccountKey = strings.TrimSpace(c.Param("account_key"))
		next, err := store.Upsert(state, time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, next)
	})
}

func quotaRuntimeRequestedAccountKeys(c *gin.Context) []string {
	if c == nil || c.Request == nil {
		return nil
	}
	query := c.Request.URL.Query()
	values := append([]string{}, query["account_key"]...)
	values = append(values, query["account_keys"]...)
	keys := make([]string, 0, len(values))
	for _, value := range values {
		for _, key := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(key); trimmed != "" {
				keys = append(keys, trimmed)
			}
		}
	}
	return keys
}
