package gettokenshooks

import (
	"fmt"
	"net/http"
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
		states = append(states, QuotaRuntimeState{
			AccountKey: accountKey,
			Status:     QuotaRuntimeStatusStale,
			Windows:    []QuotaRuntimeWindow{},
			Sources:    []QuotaRuntimeSourceState{},
		})
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
		return state
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
	return state
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

func cloneQuotaRuntimeState(state QuotaRuntimeState) QuotaRuntimeState {
	state.Windows = append([]QuotaRuntimeWindow(nil), state.Windows...)
	if state.Billing != nil {
		billing := *state.Billing
		billing.BalanceInfos = append([]QuotaRuntimeBalanceInfo(nil), billing.BalanceInfos...)
		state.Billing = &billing
	}
	state.Sources = append([]QuotaRuntimeSourceState(nil), state.Sources...)
	return state
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
			c.JSON(http.StatusOK, QuotaRuntimeState{AccountKey: accountKey, Status: QuotaRuntimeStatusStale, Windows: []QuotaRuntimeWindow{}, Sources: []QuotaRuntimeSourceState{}})
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
