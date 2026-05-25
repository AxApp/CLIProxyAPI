package gettokenshooks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	RateLimitStrategyTokenWindow   = "token-window"
	RateLimitStrategyRequestWindow = "request-window"

	RateLimitWindowCalendarDay = "calendar-day"

	RateLimitActionBlock = "block"
	RateLimitActionWarn  = "warn"

	defaultRateLimitEvaluationInterval = 30 * time.Second

	rateLimitSchema = `
CREATE TABLE IF NOT EXISTS rate_limit_rules (
  id TEXT PRIMARY KEY,
  account_key TEXT NOT NULL,
  match_key TEXT NOT NULL DEFAULT '',
  strategy TEXT NOT NULL,
  window TEXT NOT NULL,
  limit_value INTEGER NOT NULL,
  action TEXT NOT NULL DEFAULT 'block',
  enabled INTEGER NOT NULL DEFAULT 1,
  label TEXT NOT NULL DEFAULT '',
  created_at_unix_ms INTEGER NOT NULL,
  updated_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_rate_limit_rules_account
  ON rate_limit_rules(account_key);
CREATE INDEX IF NOT EXISTS idx_rate_limit_rules_match
  ON rate_limit_rules(match_key);

CREATE TABLE IF NOT EXISTS rate_limit_events (
  id TEXT PRIMARY KEY,
  account_key TEXT NOT NULL,
  match_key TEXT NOT NULL DEFAULT '',
  rule_id TEXT NOT NULL,
  strategy TEXT NOT NULL,
  window TEXT NOT NULL,
  action TEXT NOT NULL,
  usage_value INTEGER NOT NULL,
  limit_value INTEGER NOT NULL,
  blocked INTEGER NOT NULL,
  reason TEXT NOT NULL,
  triggered_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_rate_limit_events_account_time
  ON rate_limit_events(account_key, triggered_at_unix_ms);`
)

type RateLimitStrategyMeta struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	SupportedWindows []string `json:"supported_windows"`
}

type RateLimitRule struct {
	ID         string `json:"id"`
	AccountKey string `json:"account_key"`
	MatchKey   string `json:"match_key,omitempty"`
	Strategy   string `json:"strategy"`
	Window     string `json:"window"`
	LimitValue int64  `json:"limit_value"`
	Action     string `json:"action"`
	Enabled    bool   `json:"enabled"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
}

type RateLimitRuleState struct {
	Rule         RateLimitRule `json:"rule"`
	Exceeded     bool          `json:"exceeded"`
	Reason       string        `json:"reason"`
	UsagePct     float64       `json:"usage_pct"`
	CurrentUsage int64         `json:"current_usage"`
}

type RateLimitState struct {
	AccountKey  string               `json:"account_key"`
	MatchKey    string               `json:"match_key,omitempty"`
	Blocked     bool                 `json:"blocked"`
	BlockReason string               `json:"block_reason"`
	Rules       []RateLimitRuleState `json:"rules"`
	UpdatedAt   string               `json:"updated_at"`
}

type RateLimitEvent struct {
	ID          string `json:"id"`
	AccountKey  string `json:"account_key"`
	MatchKey    string `json:"match_key,omitempty"`
	RuleID      string `json:"rule_id"`
	Strategy    string `json:"strategy"`
	Window      string `json:"window"`
	Action      string `json:"action"`
	UsageValue  int64  `json:"usage_value"`
	LimitValue  int64  `json:"limit_value"`
	Blocked     bool   `json:"blocked"`
	Reason      string `json:"reason"`
	TriggeredAt int64  `json:"triggered_at"`
}

type RateLimitStrategy interface {
	ID() string
	Name() string
	SupportedWindows() []string
	UsageForRule(ctx context.Context, store *rateLimitStore, rule RateLimitRule, now time.Time) (int64, error)
	FormatReason(rule RateLimitRule) string
}

type RateLimitStrategyRegistry struct {
	order      []string
	strategies map[string]RateLimitStrategy
}

type rateLimitStore struct {
	db *sql.DB
}

type RateLimitEvaluatorOptions struct {
	Now      func() time.Time
	Interval time.Duration
	Registry *RateLimitStrategyRegistry
}

type RateLimitEvaluator struct {
	store    *rateLimitStore
	registry *RateLimitStrategyRegistry
	now      func() time.Time
	interval time.Duration

	mu       sync.RWMutex
	byKey    map[string]RateLimitState
	byAcct   map[string]RateLimitState
	byLookup map[string]RateLimitState
	stopOnce sync.Once
	stopCh   chan struct{}
}

var (
	rateLimitMu              sync.RWMutex
	defaultRateLimitStore    *rateLimitStore
	defaultRateLimitEval     *RateLimitEvaluator
	defaultRateLimitCleanup  func()
	defaultRateLimitRegistry = NewRateLimitStrategyRegistry(
		rateLimitTokenWindowStrategy{},
		rateLimitRequestWindowStrategy{},
	)
)

type rateLimitPolicy struct {
	evaluator *RateLimitEvaluator
}

func NewRateLimitStrategyRegistry(strategies ...RateLimitStrategy) *RateLimitStrategyRegistry {
	registry := &RateLimitStrategyRegistry{
		order:      []string{},
		strategies: map[string]RateLimitStrategy{},
	}
	for _, strategy := range strategies {
		registry.Register(strategy)
	}
	return registry
}

func (r *RateLimitStrategyRegistry) Register(strategy RateLimitStrategy) {
	if r == nil || strategy == nil {
		return
	}
	id := strings.TrimSpace(strategy.ID())
	if id == "" {
		return
	}
	if r.strategies == nil {
		r.strategies = map[string]RateLimitStrategy{}
	}
	if _, exists := r.strategies[id]; !exists {
		r.order = append(r.order, id)
	}
	r.strategies[id] = strategy
}

func (r *RateLimitStrategyRegistry) Get(id string) (RateLimitStrategy, bool) {
	if r == nil {
		return nil, false
	}
	strategy, ok := r.strategies[strings.TrimSpace(id)]
	return strategy, ok
}

func (r *RateLimitStrategyRegistry) List() []RateLimitStrategyMeta {
	if r == nil {
		return []RateLimitStrategyMeta{}
	}
	out := make([]RateLimitStrategyMeta, 0, len(r.order))
	for _, id := range r.order {
		strategy, ok := r.strategies[id]
		if !ok || strategy == nil {
			continue
		}
		out = append(out, RateLimitStrategyMeta{
			ID:               strategy.ID(),
			Name:             strategy.Name(),
			SupportedWindows: append([]string(nil), strategy.SupportedWindows()...),
		})
	}
	return out
}

type rateLimitTokenWindowStrategy struct{}

func (rateLimitTokenWindowStrategy) ID() string { return RateLimitStrategyTokenWindow }

func (rateLimitTokenWindowStrategy) Name() string { return "Token 窗口限流" }

func (rateLimitTokenWindowStrategy) SupportedWindows() []string {
	return []string{"1h", "6h", "12h", "24h", RateLimitWindowCalendarDay, "7d", "30d"}
}

func (rateLimitTokenWindowStrategy) UsageForRule(ctx context.Context, store *rateLimitStore, rule RateLimitRule, now time.Time) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	since := rateLimitRuleWindowStart(rule.Window, now).UnixMilli()
	matchKey := rateLimitRuleMatchKey(rule)
	var total sql.NullInt64
	err := store.db.QueryRowContext(
		ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM usage_attribution_events
		  WHERE completed_at_unix_ms >= ?
		    AND (account_key = ? OR attribution_key = ?)`,
		since,
		rule.AccountKey,
		matchKey,
	).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func (rateLimitTokenWindowStrategy) FormatReason(rule RateLimitRule) string {
	return rateLimitRuleWindow(rule) + " tokens 已满"
}

type rateLimitRequestWindowStrategy struct{}

func (rateLimitRequestWindowStrategy) ID() string { return RateLimitStrategyRequestWindow }

func (rateLimitRequestWindowStrategy) Name() string { return "请求窗口限流" }

func (rateLimitRequestWindowStrategy) SupportedWindows() []string {
	return []string{"1h", "6h", "12h", "24h", RateLimitWindowCalendarDay, "7d", "30d"}
}

func (rateLimitRequestWindowStrategy) UsageForRule(ctx context.Context, store *rateLimitStore, rule RateLimitRule, now time.Time) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	since := rateLimitRuleWindowStart(rule.Window, now).UnixMilli()
	matchKey := rateLimitRuleMatchKey(rule)
	var count int64
	err := store.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM usage_attribution_events
		  WHERE completed_at_unix_ms >= ?
		    AND (account_key = ? OR attribution_key = ?)`,
		since,
		rule.AccountKey,
		matchKey,
	).Scan(&count)
	return count, err
}

func (rateLimitRequestWindowStrategy) FormatReason(rule RateLimitRule) string {
	return rateLimitRuleWindow(rule) + " requests 已满"
}

func InstallRateLimitHook(opts UsageAttributionOptions) error {
	dbPath, err := resolveUsageAttributionPath(opts)
	if err != nil {
		return fmt.Errorf("resolve rate limit path: %w", err)
	}
	store, err := newRateLimitStore(dbPath)
	if err != nil {
		return fmt.Errorf("open rate limit store: %w", err)
	}
	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		log.WithError(err).Warn("gettokenshooks: initial rate limit evaluation failed")
	}
	evaluator.Start(context.Background())

	rateLimitMu.Lock()
	if defaultRateLimitEval != nil {
		defaultRateLimitEval.Stop()
	}
	if defaultRateLimitCleanup != nil {
		defaultRateLimitCleanup()
	}
	defaultRateLimitStore = store
	defaultRateLimitEval = evaluator
	defaultRateLimitCleanup = coreauth.RegisterRoutePolicy(rateLimitPolicy{evaluator: evaluator})
	rateLimitMu.Unlock()

	log.Infof("gettokenshooks: rate limit store ready at %s", dbPath)
	return nil
}

func newRateLimitStore(path string) (*rateLimitStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty sqlite path")
	}
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(usageAttributionSchema + "\n" + rateLimitSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &rateLimitStore{db: db}, nil
}

func ensureParentDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

func (s *rateLimitStore) insertUsageAttributionEvent(event usageAttributionEvent) error {
	return (&usageAttributionStore{db: s.db}).insert(event)
}

func (s *rateLimitStore) upsertRule(rule RateLimitRule, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	rule = normalizeRateLimitRule(rule)
	if err := validateRateLimitRule(rule); err != nil {
		return err
	}
	nowMs := now.UTC().UnixMilli()
	if rule.CreatedAt <= 0 {
		rule.CreatedAt = nowMs
	}
	rule.UpdatedAt = nowMs
	_, err := s.db.Exec(
		`INSERT INTO rate_limit_rules (
		 id, account_key, match_key, strategy, window, limit_value, action,
		 enabled, label, created_at_unix_ms, updated_at_unix_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		 account_key = excluded.account_key,
		 match_key = excluded.match_key,
		 strategy = excluded.strategy,
		 window = excluded.window,
		 limit_value = excluded.limit_value,
		 action = excluded.action,
		 enabled = excluded.enabled,
		 label = excluded.label,
		 updated_at_unix_ms = excluded.updated_at_unix_ms`,
		rule.ID,
		rule.AccountKey,
		rule.MatchKey,
		rule.Strategy,
		rule.Window,
		rule.LimitValue,
		rule.Action,
		boolToInt(rule.Enabled),
		rule.Label,
		rule.CreatedAt,
		rule.UpdatedAt,
	)
	return err
}

func (s *rateLimitStore) deleteRule(id string) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	_, err := s.db.Exec(`DELETE FROM rate_limit_rules WHERE id = ?`, strings.TrimSpace(id))
	return err
}

func (s *rateLimitStore) listRules(accountKey string) ([]RateLimitRule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("rate limit store is not initialized")
	}
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(accountKey) == "" {
		rows, err = s.db.Query(`SELECT id, account_key, match_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules ORDER BY account_key, window, strategy`)
	} else {
		rows, err = s.db.Query(`SELECT id, account_key, match_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules WHERE account_key = ? ORDER BY window, strategy`, strings.TrimSpace(accountKey))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRateLimitRules(rows)
}

func (s *rateLimitStore) listEnabledRules() ([]RateLimitRule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("rate limit store is not initialized")
	}
	rows, err := s.db.Query(`SELECT id, account_key, match_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules WHERE enabled = 1 ORDER BY account_key, window, strategy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRateLimitRules(rows)
}

func scanRateLimitRules(rows *sql.Rows) ([]RateLimitRule, error) {
	out := []RateLimitRule{}
	for rows.Next() {
		var rule RateLimitRule
		var enabled int64
		if err := rows.Scan(&rule.ID, &rule.AccountKey, &rule.MatchKey, &rule.Strategy, &rule.Window, &rule.LimitValue, &rule.Action, &enabled, &rule.Label, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
			return nil, err
		}
		rule.Enabled = enabled != 0
		out = append(out, normalizeRateLimitRule(rule))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *rateLimitStore) insertEvent(state RateLimitState, ruleState RateLimitRuleState, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO rate_limit_events (
		 id, account_key, match_key, rule_id, strategy, window, action,
		 usage_value, limit_value, blocked, reason, triggered_at_unix_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		randomID("rle"),
		state.AccountKey,
		state.MatchKey,
		ruleState.Rule.ID,
		ruleState.Rule.Strategy,
		ruleState.Rule.Window,
		ruleState.Rule.Action,
		ruleState.CurrentUsage,
		ruleState.Rule.LimitValue,
		boolToInt(state.Blocked),
		ruleState.Reason,
		now.UTC().UnixMilli(),
	)
	return err
}

func (s *rateLimitStore) listEvents(accountKey string, limit int) ([]RateLimitEvent, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("rate limit store is not initialized")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(accountKey) == "" {
		rows, err = s.db.Query(
			`SELECT id, account_key, match_key, rule_id, strategy, window, action,
			        usage_value, limit_value, blocked, reason, triggered_at_unix_ms
			   FROM rate_limit_events
			  ORDER BY triggered_at_unix_ms DESC
			  LIMIT ?`,
			limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, account_key, match_key, rule_id, strategy, window, action,
			        usage_value, limit_value, blocked, reason, triggered_at_unix_ms
			   FROM rate_limit_events
			  WHERE account_key = ?
			  ORDER BY triggered_at_unix_ms DESC
			  LIMIT ?`,
			strings.TrimSpace(accountKey),
			limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RateLimitEvent{}
	for rows.Next() {
		var event RateLimitEvent
		var blocked int64
		if err := rows.Scan(
			&event.ID,
			&event.AccountKey,
			&event.MatchKey,
			&event.RuleID,
			&event.Strategy,
			&event.Window,
			&event.Action,
			&event.UsageValue,
			&event.LimitValue,
			&blocked,
			&event.Reason,
			&event.TriggeredAt,
		); err != nil {
			return nil, err
		}
		event.Blocked = blocked != 0
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func NewRateLimitEvaluator(store *rateLimitStore, opts RateLimitEvaluatorOptions) *RateLimitEvaluator {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultRateLimitEvaluationInterval
	}
	registry := opts.Registry
	if registry == nil {
		registry = defaultRateLimitRegistry
	}
	return &RateLimitEvaluator{
		store:    store,
		registry: registry,
		now:      now,
		interval: interval,
		byKey:    map[string]RateLimitState{},
		byAcct:   map[string]RateLimitState{},
		byLookup: map[string]RateLimitState{},
		stopCh:   make(chan struct{}),
	}
}

func (e *RateLimitEvaluator) Start(ctx context.Context) {
	if e == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(e.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := e.EvaluateNow(ctx); err != nil {
					log.WithError(err).Warn("gettokenshooks: rate limit evaluation failed")
				}
			case <-e.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (e *RateLimitEvaluator) Stop() {
	if e == nil {
		return
	}
	e.stopOnce.Do(func() {
		close(e.stopCh)
	})
}

func (e *RateLimitEvaluator) EvaluateNow(ctx context.Context) error {
	if e == nil || e.store == nil {
		return errors.New("rate limit evaluator is not initialized")
	}
	rules, err := e.store.listEnabledRules()
	if err != nil {
		return err
	}
	now := e.now().UTC()
	stateMap := map[string]RateLimitState{}
	for _, rule := range rules {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		strategy, ok := e.registry.Get(rule.Strategy)
		if !ok {
			return fmt.Errorf("unsupported rate limit strategy %q", rule.Strategy)
		}
		usage, err := strategy.UsageForRule(ctx, e.store, rule, now)
		if err != nil {
			return err
		}
		matchKey := rateLimitRuleMatchKey(rule)
		stateKey := rateLimitStateKey(rule.AccountKey, matchKey)
		state := stateMap[stateKey]
		if state.AccountKey == "" {
			state.AccountKey = rule.AccountKey
			state.MatchKey = matchKey
			state.UpdatedAt = now.Format(time.RFC3339)
		}
		ruleState := evaluateRateLimitRule(strategy, rule, usage)
		state.Rules = append(state.Rules, ruleState)
		if ruleState.Exceeded && rule.Action == RateLimitActionBlock {
			state.Blocked = true
			if state.BlockReason == "" {
				state.BlockReason = ruleState.Reason
			}
		}
		stateMap[stateKey] = state
	}

	e.mu.Lock()
	previous := e.byKey
	byKey := map[string]RateLimitState{}
	byAcct := map[string]RateLimitState{}
	byLookup := map[string]RateLimitState{}
	for key, state := range stateMap {
		sort.SliceStable(state.Rules, func(i, j int) bool {
			return state.Rules[i].Rule.ID < state.Rules[j].Rule.ID
		})
		byKey[key] = state
		if existing, ok := byAcct[state.AccountKey]; !ok || (!existing.Blocked && state.Blocked) {
			byAcct[state.AccountKey] = state
		}
		indexRateLimitLookup(byLookup, state)
		if state.Blocked && !previous[key].Blocked {
			for _, ruleState := range state.Rules {
				if ruleState.Exceeded && ruleState.Rule.Action == RateLimitActionBlock {
					if err := e.store.insertEvent(state, ruleState, now); err != nil {
						log.WithError(err).Warn("gettokenshooks: rate limit event persist failed")
					}
				}
			}
		}
	}
	e.byKey = byKey
	e.byAcct = byAcct
	e.byLookup = byLookup
	e.mu.Unlock()
	ReplaceAccountRouteGuardSource(AccountRouteGuardSourceRateLimit, rateLimitRouteGuardBlocks(byKey))
	return nil
}

func evaluateRateLimitRule(strategy RateLimitStrategy, rule RateLimitRule, usage int64) RateLimitRuleState {
	exceeded := rule.LimitValue > 0 && usage >= rule.LimitValue
	pct := 0.0
	if rule.LimitValue > 0 {
		pct = float64(usage) / float64(rule.LimitValue) * 100
	}
	reason := ""
	if exceeded {
		reason = strategy.FormatReason(rule)
	}
	return RateLimitRuleState{
		Rule:         rule,
		Exceeded:     exceeded,
		Reason:       reason,
		UsagePct:     pct,
		CurrentUsage: usage,
	}
}

func rateLimitRuleWindow(rule RateLimitRule) string {
	window := strings.TrimSpace(rule.Window)
	if window == "" {
		return "window"
	}
	if strings.EqualFold(window, RateLimitWindowCalendarDay) {
		return "00:00-23:59"
	}
	return window
}

func rateLimitRuleWindowStart(window string, now time.Time) time.Time {
	if strings.EqualFold(strings.TrimSpace(window), RateLimitWindowCalendarDay) {
		localNow := now.Local()
		return time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location()).UTC()
	}
	duration := parseUsageAttributionDuration(window, defaultUsageAttributionWindow)
	if duration <= 0 {
		duration = defaultUsageAttributionWindow
	}
	return now.Add(-duration).UTC()
}

func rateLimitRuleMatchKey(rule RateLimitRule) string {
	matchKey := strings.TrimSpace(rule.MatchKey)
	if matchKey == "" {
		matchKey = strings.TrimSpace(rule.AccountKey)
	}
	return matchKey
}

func (e *RateLimitEvaluator) StateForAccount(accountKey string) (RateLimitState, bool) {
	if e == nil {
		return RateLimitState{}, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	state, ok := e.byAcct[strings.TrimSpace(accountKey)]
	return state, ok
}

func (e *RateLimitEvaluator) States() []RateLimitState {
	if e == nil {
		return []RateLimitState{}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]RateLimitState, 0, len(e.byKey))
	for _, state := range e.byKey {
		out = append(out, state)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AccountKey < out[j].AccountKey
	})
	return out
}

func (e *RateLimitEvaluator) DenyIDsForCandidates(candidates []*coreauth.Auth) []string {
	if e == nil || len(candidates) == 0 {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	deny := []string{}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		keys := candidateRateLimitKeys(candidate)
		for _, key := range keys {
			if state, ok := e.byLookup[key]; ok && state.Blocked {
				deny = append(deny, candidate.ID)
				break
			}
		}
	}
	return deny
}

func indexRateLimitLookup(index map[string]RateLimitState, state RateLimitState) {
	if index == nil {
		return
	}
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if existing, ok := index[key]; !ok || (!existing.Blocked && state.Blocked) {
			index[key] = state
		}
	}
	add(state.AccountKey)
	add(state.MatchKey)
}

func (e *RateLimitEvaluator) replaceStatesForTest(states []RateLimitState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.byKey = map[string]RateLimitState{}
	e.byAcct = map[string]RateLimitState{}
	e.byLookup = map[string]RateLimitState{}
	for _, state := range states {
		key := rateLimitStateKey(state.AccountKey, state.MatchKey)
		e.byKey[key] = state
		e.byAcct[state.AccountKey] = state
		indexRateLimitLookup(e.byLookup, state)
	}
	ReplaceAccountRouteGuardSource(AccountRouteGuardSourceRateLimit, rateLimitRouteGuardBlocks(e.byKey))
}

func (p rateLimitPolicy) RewriteCandidates(ctx context.Context, req coreauth.RoutePolicyRequest) coreauth.RoutePolicyDecision {
	if p.evaluator == nil {
		return coreauth.RoutePolicyDecision{}
	}
	deny := p.evaluator.DenyIDsForCandidates(req.Candidates)
	if len(deny) == 0 {
		return coreauth.RoutePolicyDecision{}
	}
	return coreauth.RoutePolicyDecision{DenyIDs: deny, Reason: "gettokens rate limit"}
}

func (p rateLimitPolicy) RoutePolicyStage() gettokensrouting.PolicyStage {
	return gettokensrouting.PolicyStageHardFilter
}

func rateLimitRouteGuardBlocks(states map[string]RateLimitState) []AccountRouteGuardBlock {
	if len(states) == 0 {
		return nil
	}
	blocks := make([]AccountRouteGuardBlock, 0, len(states))
	for _, state := range states {
		if !state.Blocked {
			continue
		}
		blocks = append(blocks, AccountRouteGuardBlock{
			Source:     AccountRouteGuardSourceRateLimit,
			AccountKey: state.AccountKey,
			MatchKey:   state.MatchKey,
			Reason:     state.BlockReason,
		})
	}
	return blocks
}

func candidateRateLimitKeys(auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
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
	add(auth.ID)
	add("auth-id:" + auth.ID)
	add(auth.Index)
	add("auth-index:" + auth.Index)
	add("provider:" + strings.ToLower(strings.TrimSpace(auth.Provider)))
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		add("auth-file:" + filepath.Base(fileName))
	}
	return keys
}

func rateLimitStateKey(accountKey string, matchKey string) string {
	accountKey = strings.TrimSpace(accountKey)
	matchKey = strings.TrimSpace(matchKey)
	if matchKey == "" {
		matchKey = accountKey
	}
	return accountKey + "\x00" + matchKey
}

func normalizeRateLimitRule(rule RateLimitRule) RateLimitRule {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.AccountKey = strings.TrimSpace(rule.AccountKey)
	rule.MatchKey = strings.TrimSpace(rule.MatchKey)
	rule.Strategy = strings.ToLower(strings.TrimSpace(rule.Strategy))
	rule.Window = strings.ToLower(strings.TrimSpace(rule.Window))
	rule.Action = strings.ToLower(strings.TrimSpace(rule.Action))
	rule.Label = strings.TrimSpace(rule.Label)
	if rule.ID == "" {
		rule.ID = randomID("rlr")
	}
	if rule.Action == "" {
		rule.Action = RateLimitActionBlock
	}
	if rule.Window == "" {
		rule.Window = "24h"
	}
	return rule
}

func validateRateLimitRule(rule RateLimitRule) error {
	if rule.AccountKey == "" {
		return errors.New("account_key is required")
	}
	strategy, ok := defaultRateLimitRegistry.Get(rule.Strategy)
	if !ok {
		return fmt.Errorf("unsupported strategy %q", rule.Strategy)
	}
	if !strings.EqualFold(rule.Window, RateLimitWindowCalendarDay) && parseUsageAttributionDuration(rule.Window, 0) <= 0 {
		return fmt.Errorf("unsupported window %q", rule.Window)
	}
	if !rateLimitStrategySupportsWindow(strategy, rule.Window) {
		return fmt.Errorf("unsupported window %q for strategy %q", rule.Window, rule.Strategy)
	}
	if rule.LimitValue <= 0 {
		return errors.New("limit_value must be positive")
	}
	switch rule.Action {
	case RateLimitActionBlock, RateLimitActionWarn:
	default:
		return fmt.Errorf("unsupported action %q", rule.Action)
	}
	return nil
}

func rateLimitStrategySupportsWindow(strategy RateLimitStrategy, window string) bool {
	if strategy == nil {
		return false
	}
	window = strings.TrimSpace(window)
	for _, supported := range strategy.SupportedWindows() {
		if strings.EqualFold(strings.TrimSpace(supported), window) {
			return true
		}
	}
	return false
}

func ListRateLimitStrategies() []RateLimitStrategyMeta {
	return defaultRateLimitRegistry.List()
}

func ConfigureRateLimitRoutes(group *gin.RouterGroup, _ *handlers.BaseAPIHandler, _ *config.Config) {
	if group == nil {
		return
	}
	group.GET("/gettokens/rate-limit-strategies", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"items": ListRateLimitStrategies()})
	})
	group.GET("/gettokens/rate-limit-rules", func(c *gin.Context) {
		store, _, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		rules, err := store.listRules(c.Query("account_key"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": rules})
	})
	group.POST("/gettokens/rate-limit-rules", func(c *gin.Context) {
		store, evaluator, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		var rule RateLimitRule
		if err := c.ShouldBindJSON(&rule); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := store.upsertRule(rule, time.Now().UTC()); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_ = evaluator.EvaluateNow(c.Request.Context())
		rules, _ := store.listRules(rule.AccountKey)
		c.JSON(http.StatusOK, gin.H{"items": rules})
	})
	group.PUT("/gettokens/rate-limit-rules/:id", func(c *gin.Context) {
		store, evaluator, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		var rule RateLimitRule
		if err := c.ShouldBindJSON(&rule); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		rule.ID = c.Param("id")
		if err := store.upsertRule(rule, time.Now().UTC()); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_ = evaluator.EvaluateNow(c.Request.Context())
		rules, _ := store.listRules(rule.AccountKey)
		c.JSON(http.StatusOK, gin.H{"items": rules})
	})
	group.DELETE("/gettokens/rate-limit-rules/:id", func(c *gin.Context) {
		store, evaluator, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		if err := store.deleteRule(c.Param("id")); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		_ = evaluator.EvaluateNow(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	group.GET("/gettokens/rate-limit-status", func(c *gin.Context) {
		_, evaluator, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		if accountKey := strings.TrimSpace(c.Query("account_key")); accountKey != "" {
			if state, exists := evaluator.StateForAccount(accountKey); exists {
				c.JSON(http.StatusOK, state)
				return
			}
			c.JSON(http.StatusOK, RateLimitState{AccountKey: accountKey, Rules: []RateLimitRuleState{}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": evaluator.States()})
	})
	group.GET("/gettokens/rate-limit-events", func(c *gin.Context) {
		store, _, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		limit := 50
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				limit = parsed
			}
		}
		events, err := store.listEvents(c.Query("account_key"), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": events})
	})
}

func ConfigureGetTokensManagementRoutes(group *gin.RouterGroup, handler *handlers.BaseAPIHandler, cfg *config.Config) {
	ConfigureUsageAttributionRoutes(group, handler, cfg)
	ConfigureRateLimitRoutes(group, handler, cfg)
	ConfigureLiveSessionRoutes(group, handler, cfg)
}

func currentRateLimitRuntime(c *gin.Context) (*rateLimitStore, *RateLimitEvaluator, bool) {
	rateLimitMu.RLock()
	store := defaultRateLimitStore
	evaluator := defaultRateLimitEval
	rateLimitMu.RUnlock()
	if store == nil || evaluator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "rate limit store is not initialized"})
		return nil, nil, false
	}
	return store, evaluator, true
}

func randomID(prefix string) string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(bytes[:])
}
