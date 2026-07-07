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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
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

	defaultRateLimitEvaluationInterval  = 30 * time.Second
	defaultRateLimitReservationTTL      = 30 * time.Minute
	postUsageRateLimitCompletionTimeout = 5 * time.Second

	rateLimitReservationStatusPending   = "pending"
	rateLimitReservationStatusCommitted = "committed"
	rateLimitReservationStatusReleased  = "released"
	rateLimitReservationStatusExpired   = "expired"

	rateLimitSchema = `
CREATE TABLE IF NOT EXISTS rate_limit_rules (
  id TEXT PRIMARY KEY,
  account_key TEXT NOT NULL,
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

CREATE TABLE IF NOT EXISTS rate_limit_events (
  id TEXT PRIMARY KEY,
  account_key TEXT NOT NULL,
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
  ON rate_limit_events(account_key, triggered_at_unix_ms);

CREATE TABLE IF NOT EXISTS rate_limit_reservations (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL DEFAULT '',
  account_key TEXT NOT NULL,
  rule_id TEXT NOT NULL,
  window TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at_unix_ms INTEGER NOT NULL,
  updated_at_unix_ms INTEGER NOT NULL,
  expires_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_rate_limit_reservations_account_rule
  ON rate_limit_reservations(account_key, rule_id, status, created_at_unix_ms);
CREATE INDEX IF NOT EXISTS idx_rate_limit_reservations_request
  ON rate_limit_reservations(request_id, account_key);`
)

type RateLimitStrategyMeta struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	SupportedWindows []string `json:"supported_windows"`
}

type RateLimitRule struct {
	ID         string `json:"id"`
	AccountKey string `json:"account_key"`
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
	LimitValue   int64         `json:"limit_value"`
	WindowStart  string        `json:"window_start,omitempty"`
	WindowEnd    string        `json:"window_end,omitempty"`
	NextReset    string        `json:"next_reset,omitempty"`
}

type RateLimitSourceState struct {
	Source      string `json:"source"`
	Reason      string `json:"reason,omitempty"`
	RuleID      string `json:"rule_id,omitempty"`
	Strategy    string `json:"strategy,omitempty"`
	Window      string `json:"window,omitempty"`
	UsageValue  int64  `json:"usage_value,omitempty"`
	LimitValue  int64  `json:"limit_value,omitempty"`
	WindowStart string `json:"window_start,omitempty"`
	WindowEnd   string `json:"window_end,omitempty"`
	NextReset   string `json:"next_reset,omitempty"`
}

type RateLimitState struct {
	AccountKey      string                 `json:"account_key"`
	Blocked         bool                   `json:"blocked"`
	BlockReason     string                 `json:"block_reason"`
	Sources         []RateLimitSourceState `json:"sources"`
	Rules           []RateLimitRuleState   `json:"rules"`
	UpdatedAt       string                 `json:"updated_at"`
	LastEvaluatedAt string                 `json:"last_evaluated_at,omitempty"`
	NextReset       string                 `json:"next_reset,omitempty"`
	Stale           bool                   `json:"stale,omitempty"`
	DegradedReason  string                 `json:"degraded_reason,omitempty"`
}

type RateLimitEvent struct {
	ID          string `json:"id"`
	AccountKey  string `json:"account_key"`
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

type rateLimitBlockEvent struct {
	State     RateLimitState
	RuleState RateLimitRuleState
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

	evalMu        sync.Mutex
	reservationMu sync.Mutex
	mu            sync.RWMutex
	byKey         map[string]RateLimitState
	byAcct        map[string]RateLimitState
	byLookup      map[string]RateLimitState
	stopOnce      sync.Once
	stopCh        chan struct{}
}

var (
	rateLimitMu                   sync.RWMutex
	defaultRateLimitStore         *rateLimitStore
	defaultRateLimitEval          *RateLimitEvaluator
	installRateLimitAdmissionOnce sync.Once
	defaultRateLimitRegistry      = NewRateLimitStrategyRegistry(
		rateLimitTokenWindowStrategy{},
		rateLimitRequestWindowStrategy{},
	)
)

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
	var total sql.NullInt64
	err := store.db.QueryRowContext(
		ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM usage_attribution_events
		  WHERE completed_at_unix_ms >= ?
		    AND account_key = ?
		    AND failed = 0`,
		since,
		rule.AccountKey,
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
	var count int64
	err := store.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM usage_attribution_events
		  WHERE completed_at_unix_ms >= ?
		    AND account_key = ?`,
		since,
		rule.AccountKey,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	reserved, err := store.activeRequestReservationCount(ctx, rule, now)
	if err != nil {
		return 0, err
	}
	return count + reserved, nil
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
	InstallRateLimitAdmissionPolicy()

	rateLimitMu.Lock()
	if defaultRateLimitEval != nil {
		defaultRateLimitEval.Stop()
	}
	defaultRateLimitStore = store
	defaultRateLimitEval = evaluator
	rateLimitMu.Unlock()

	log.Infof("gettokenshooks: rate limit store ready at %s", dbPath)
	return nil
}

func InstallRateLimitAdmissionPolicy() {
	installRateLimitAdmissionOnce.Do(func() {
		gettokensrouting.RegisterAdmissionPolicy(rateLimitAdmissionPolicy(nil))
	})
}

func refreshRateLimitAfterUsage(ctx context.Context, accountKey string) error {
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return nil
	}
	rateLimitMu.RLock()
	evaluator := defaultRateLimitEval
	rateLimitMu.RUnlock()
	if evaluator == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return evaluator.EvaluateAccountNow(ctx, accountKey)
}

func completeRateLimitAfterUsage(ctx context.Context, event usageAttributionEvent) error {
	accountKey := strings.TrimSpace(event.AccountKey)
	if accountKey == "" {
		return nil
	}
	rateLimitMu.RLock()
	evaluator := defaultRateLimitEval
	rateLimitMu.RUnlock()
	if evaluator == nil {
		return nil
	}
	// Usage hooks run after the client request has completed, so the request context
	// may already be canceled while local reservation cleanup still needs to finish.
	postUsageCtx, cancel := context.WithTimeout(context.Background(), postUsageRateLimitCompletionTimeout)
	defer cancel()
	if err := evaluator.releaseReservationsForUsage(postUsageCtx, event); err != nil {
		return err
	}
	return evaluator.EvaluateAccountNow(postUsageCtx, accountKey)
}

func rateLimitAdmissionPolicy(evaluator *RateLimitEvaluator) gettokensrouting.AdmissionPolicy {
	return gettokensrouting.AdmissionPolicy{
		Name: "rate-limit-admission",
		Admit: func(ctx context.Context, routeCtx gettokensrouting.RouteContext, candidate gettokensrouting.RouteCandidate) gettokensrouting.AdmissionDecision {
			eval := evaluator
			if eval == nil {
				rateLimitMu.RLock()
				eval = defaultRateLimitEval
				rateLimitMu.RUnlock()
			}
			if eval == nil {
				return gettokensrouting.AdmissionDecision{}
			}
			auth := authFromRouteCandidate(candidate)
			if auth == nil {
				return gettokensrouting.AdmissionDecision{}
			}
			return eval.admitRequestWindow(ctx, auth)
		},
	}
}

func authFromRouteCandidate(candidate gettokensrouting.RouteCandidate) *coreauth.Auth {
	if auth, ok := candidate.Value.(*coreauth.Auth); ok && auth != nil {
		return auth.Clone()
	}
	id := strings.TrimSpace(candidate.ID)
	if id == "" {
		return nil
	}
	return &coreauth.Auth{ID: id}
}

func (e *RateLimitEvaluator) admitRequestWindow(ctx context.Context, auth *coreauth.Auth) gettokensrouting.AdmissionDecision {
	if e == nil || e.store == nil || auth == nil {
		return gettokensrouting.AdmissionDecision{}
	}
	accountKey := strings.TrimSpace(auth.AccountKey)
	if accountKey == "" {
		return gettokensrouting.AdmissionDecision{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := e.now().UTC()
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	reservationIDs, denyReason, err := e.createRequestWindowReservations(ctx, accountKey, requestID, now)
	if err != nil {
		log.WithError(err).WithField("account_key", accountKey).Warn("gettokenshooks: rate limit admission failed")
		return gettokensrouting.AdmissionDecision{
			Active: true,
			Allow:  false,
			Reason: "rate limit admission failed",
		}
	}
	if denyReason != "" {
		return gettokensrouting.AdmissionDecision{
			Active: true,
			Allow:  false,
			Reason: denyReason,
		}
	}
	if len(reservationIDs) == 0 {
		return gettokensrouting.AdmissionDecision{}
	}
	if err := e.EvaluateAccountNow(context.Background(), accountKey); err != nil {
		_ = e.store.updateReservationsStatus(context.Background(), reservationIDs, rateLimitReservationStatusReleased, e.now().UTC())
		log.WithError(err).WithField("account_key", accountKey).Warn("gettokenshooks: refresh rate limit after admission failed")
		return gettokensrouting.AdmissionDecision{
			Active: true,
			Allow:  false,
			Reason: "rate limit admission refresh failed",
		}
	}
	lease := gettokensrouting.NewAdmissionLease(func(context.Context) {
		if err := e.store.updateReservationsStatus(context.Background(), reservationIDs, rateLimitReservationStatusCommitted, e.now().UTC()); err != nil {
			log.WithError(err).WithField("account_key", accountKey).Warn("gettokenshooks: commit rate limit reservation failed")
			return
		}
		if err := e.EvaluateAccountNow(context.Background(), accountKey); err != nil {
			log.WithError(err).WithField("account_key", accountKey).Warn("gettokenshooks: refresh rate limit after reservation commit failed")
		}
	}, func(context.Context) {
		if err := e.store.updateReservationsStatus(context.Background(), reservationIDs, rateLimitReservationStatusReleased, e.now().UTC()); err != nil {
			log.WithError(err).WithField("account_key", accountKey).Warn("gettokenshooks: release rate limit reservation failed")
			return
		}
		if err := e.EvaluateAccountNow(context.Background(), accountKey); err != nil {
			log.WithError(err).WithField("account_key", accountKey).Warn("gettokenshooks: refresh rate limit after reservation release failed")
		}
	})
	return gettokensrouting.AdmissionDecision{
		Active: true,
		Allow:  true,
		Lease:  lease,
	}
}

func (e *RateLimitEvaluator) createRequestWindowReservations(ctx context.Context, accountKey string, requestID string, now time.Time) ([]string, string, error) {
	rules, err := e.store.listEnabledRequestWindowBlockRulesForAccount(accountKey)
	if err != nil {
		return nil, "", err
	}
	if len(rules) == 0 {
		return nil, "", nil
	}

	e.reservationMu.Lock()
	defer e.reservationMu.Unlock()
	if err := e.store.expireReservations(ctx, now); err != nil {
		return nil, "", err
	}
	rules, err = e.store.listEnabledRequestWindowBlockRulesForAccount(accountKey)
	if err != nil {
		return nil, "", err
	}
	if len(rules) == 0 {
		return nil, "", nil
	}
	strategy := rateLimitRequestWindowStrategy{}
	for _, rule := range rules {
		usage, err := strategy.UsageForRule(ctx, e.store, rule, now)
		if err != nil {
			return nil, "", err
		}
		if rule.LimitValue > 0 && usage >= rule.LimitValue {
			return nil, strategy.FormatReason(rule), nil
		}
	}
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		id := randomID("rlres")
		if err := e.store.insertReservation(ctx, id, requestID, rule, now, defaultRateLimitReservationTTL); err != nil {
			_ = e.store.updateReservationsStatus(context.Background(), ids, rateLimitReservationStatusReleased, now)
			return nil, "", err
		}
		ids = append(ids, id)
	}
	return ids, "", nil
}

func (e *RateLimitEvaluator) releaseReservationsForUsage(ctx context.Context, event usageAttributionEvent) error {
	if e == nil || e.store == nil {
		return nil
	}
	return e.store.releaseReservationsForUsage(ctx, event, e.now().UTC())
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
	if err := dropDeprecatedRateLimitIdentityTables(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(usageAttributionSchema + "\n" + rateLimitSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &rateLimitStore{db: db}, nil
}

func dropDeprecatedRateLimitIdentityTables(db *sql.DB) error {
	deprecatedColumn := "match" + "_key"
	for _, table := range []string{"rate_limit_events", "rate_limit_rules"} {
		hasDeprecatedColumn, err := sqliteTableHasColumn(db, table, deprecatedColumn)
		if err != nil {
			return err
		}
		if !hasDeprecatedColumn {
			continue
		}
		if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			return fmt.Errorf("drop legacy %s: %w", table, err)
		}
	}
	return nil
}

func sqliteTableHasColumn(db *sql.DB, table string, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("scan %s schema: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate %s schema: %w", table, err)
	}
	return false, nil
}

func ensureParentDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

func (s *rateLimitStore) insertUsageAttributionEvent(event usageAttributionEvent) error {
	return (&usageAttributionStore{db: s.db, disablePrune: true}).insert(event)
}

func (s *rateLimitStore) upsertRule(rule RateLimitRule, now time.Time) error {
	_, err := s.upsertRuleWithRegistry(rule, now, defaultRateLimitRegistry)
	return err
}

func (s *rateLimitStore) upsertRuleWithRegistry(rule RateLimitRule, now time.Time, registry *RateLimitStrategyRegistry) (RateLimitRule, error) {
	if s == nil || s.db == nil {
		return RateLimitRule{}, errors.New("rate limit store is not initialized")
	}
	rule = normalizeRateLimitRule(rule)
	if err := validateRateLimitRuleWithRegistry(rule, registry); err != nil {
		return RateLimitRule{}, err
	}
	nowMs := now.UTC().UnixMilli()
	if rule.CreatedAt <= 0 {
		rule.CreatedAt = nowMs
	}
	rule.UpdatedAt = nowMs
	_, err := s.db.Exec(
		`INSERT INTO rate_limit_rules (
			 id, account_key, strategy, window, limit_value, action,
			 enabled, label, created_at_unix_ms, updated_at_unix_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			 account_key = excluded.account_key,
			 strategy = excluded.strategy,
			 window = excluded.window,
			 limit_value = excluded.limit_value,
		 action = excluded.action,
		 enabled = excluded.enabled,
		 label = excluded.label,
		 updated_at_unix_ms = excluded.updated_at_unix_ms`,
		rule.ID,
		rule.AccountKey,
		rule.Strategy,
		rule.Window,
		rule.LimitValue,
		rule.Action,
		boolToInt(rule.Enabled),
		rule.Label,
		rule.CreatedAt,
		rule.UpdatedAt,
	)
	if err != nil {
		return RateLimitRule{}, err
	}
	return rule, nil
}

func (s *rateLimitStore) deleteRule(id string) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	_, err := s.db.Exec(`DELETE FROM rate_limit_rules WHERE id = ?`, strings.TrimSpace(id))
	return err
}

func (s *rateLimitStore) getRule(id string) (RateLimitRule, bool, error) {
	if s == nil || s.db == nil {
		return RateLimitRule{}, false, errors.New("rate limit store is not initialized")
	}
	row := s.db.QueryRow(`SELECT id, account_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules WHERE id = ?`, strings.TrimSpace(id))
	var rule RateLimitRule
	var enabled int64
	if err := row.Scan(&rule.ID, &rule.AccountKey, &rule.Strategy, &rule.Window, &rule.LimitValue, &rule.Action, &enabled, &rule.Label, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RateLimitRule{}, false, nil
		}
		return RateLimitRule{}, false, err
	}
	rule.Enabled = enabled != 0
	return normalizeRateLimitRule(rule), true, nil
}

func (s *rateLimitStore) replaceRule(rule RateLimitRule) error {
	return s.replaceRuleWithRegistry(rule, defaultRateLimitRegistry)
}

func (s *rateLimitStore) replaceRuleWithRegistry(rule RateLimitRule, registry *RateLimitStrategyRegistry) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	rule = normalizeRateLimitRule(rule)
	if err := validateRateLimitRuleWithRegistry(rule, registry); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO rate_limit_rules (
			 id, account_key, strategy, window, limit_value, action,
			 enabled, label, created_at_unix_ms, updated_at_unix_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
			 account_key = excluded.account_key,
			 strategy = excluded.strategy,
			 window = excluded.window,
			 limit_value = excluded.limit_value,
			 action = excluded.action,
			 enabled = excluded.enabled,
			 label = excluded.label,
			 created_at_unix_ms = excluded.created_at_unix_ms,
			 updated_at_unix_ms = excluded.updated_at_unix_ms`,
		rule.ID,
		rule.AccountKey,
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

func (s *rateLimitStore) listRules(accountKey string) ([]RateLimitRule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("rate limit store is not initialized")
	}
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(accountKey) == "" {
		rows, err = s.db.Query(`SELECT id, account_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules ORDER BY account_key, window, strategy`)
	} else {
		rows, err = s.db.Query(`SELECT id, account_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules WHERE account_key = ? ORDER BY window, strategy`, strings.TrimSpace(accountKey))
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
	rows, err := s.db.Query(`SELECT id, account_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules WHERE enabled = 1 ORDER BY account_key, window, strategy`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRateLimitRules(rows)
}

func (s *rateLimitStore) listEnabledRulesForAccount(accountKey string) ([]RateLimitRule, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("rate limit store is not initialized")
	}
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return []RateLimitRule{}, nil
	}
	rows, err := s.db.Query(`SELECT id, account_key, strategy, window, limit_value, action, enabled, label, created_at_unix_ms, updated_at_unix_ms FROM rate_limit_rules WHERE enabled = 1 AND account_key = ? ORDER BY window, strategy`, accountKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRateLimitRules(rows)
}

func (s *rateLimitStore) listEnabledRequestWindowBlockRulesForAccount(accountKey string) ([]RateLimitRule, error) {
	rules, err := s.listEnabledRulesForAccount(accountKey)
	if err != nil {
		return nil, err
	}
	out := make([]RateLimitRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Strategy == RateLimitStrategyRequestWindow && rule.Action == RateLimitActionBlock {
			out = append(out, rule)
		}
	}
	return out, nil
}

func scanRateLimitRules(rows *sql.Rows) ([]RateLimitRule, error) {
	out := []RateLimitRule{}
	for rows.Next() {
		var rule RateLimitRule
		var enabled int64
		if err := rows.Scan(&rule.ID, &rule.AccountKey, &rule.Strategy, &rule.Window, &rule.LimitValue, &rule.Action, &enabled, &rule.Label, &rule.CreatedAt, &rule.UpdatedAt); err != nil {
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

func (s *rateLimitStore) expireReservations(ctx context.Context, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	nowMs := now.UTC().UnixMilli()
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE rate_limit_reservations
		    SET status = ?, updated_at_unix_ms = ?
		  WHERE status IN (?, ?)
		    AND expires_at_unix_ms <= ?`,
		rateLimitReservationStatusExpired,
		nowMs,
		rateLimitReservationStatusPending,
		rateLimitReservationStatusCommitted,
		nowMs,
	)
	return err
}

func (s *rateLimitStore) activeRequestReservationCount(ctx context.Context, rule RateLimitRule, now time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("rate limit store is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	since := rateLimitRuleWindowStart(rule.Window, now).UnixMilli()
	nowMs := now.UTC().UnixMilli()
	var count int64
	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*)
		   FROM rate_limit_reservations AS reservation
		  WHERE reservation.account_key = ?
		    AND reservation.rule_id = ?
		    AND reservation.status IN (?, ?)
		    AND reservation.created_at_unix_ms >= ?
		    AND reservation.expires_at_unix_ms > ?
		    AND (
		      reservation.request_id = ''
		      OR NOT EXISTS (
		        SELECT 1
		          FROM usage_attribution_events AS usage
		         WHERE usage.request_id = reservation.request_id
		           AND usage.account_key = reservation.account_key
		      )
		    )`,
		rule.AccountKey,
		rule.ID,
		rateLimitReservationStatusPending,
		rateLimitReservationStatusCommitted,
		since,
		nowMs,
	).Scan(&count)
	return count, err
}

func (s *rateLimitStore) insertReservation(ctx context.Context, id string, requestID string, rule RateLimitRule, now time.Time, ttl time.Duration) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("reservation id is required")
	}
	if ttl <= 0 {
		ttl = defaultRateLimitReservationTTL
	}
	now = now.UTC()
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO rate_limit_reservations (
		     id, request_id, account_key, rule_id, window, status,
		     created_at_unix_ms, updated_at_unix_ms, expires_at_unix_ms
		   ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		strings.TrimSpace(requestID),
		rule.AccountKey,
		rule.ID,
		rule.Window,
		rateLimitReservationStatusPending,
		now.UnixMilli(),
		now.UnixMilli(),
		now.Add(ttl).UnixMilli(),
	)
	return err
}

func (s *rateLimitStore) updateReservationsStatus(ctx context.Context, ids []string, status string, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	status = strings.TrimSpace(status)
	if status == "" || len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `UPDATE rate_limit_reservations SET status = ?, updated_at_unix_ms = ? WHERE id = ? AND status IN (?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	nowMs := now.UTC().UnixMilli()
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, status, nowMs, id, rateLimitReservationStatusPending, rateLimitReservationStatusCommitted); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *rateLimitStore) releaseReservationsForUsage(ctx context.Context, event usageAttributionEvent, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	requestID := strings.TrimSpace(event.RequestID)
	accountKey := strings.TrimSpace(event.AccountKey)
	if requestID == "" || accountKey == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE rate_limit_reservations
		    SET status = ?, updated_at_unix_ms = ?
		  WHERE request_id = ?
		    AND account_key = ?
		    AND status IN (?, ?)`,
		rateLimitReservationStatusReleased,
		now.UTC().UnixMilli(),
		requestID,
		accountKey,
		rateLimitReservationStatusPending,
		rateLimitReservationStatusCommitted,
	)
	return err
}

func (s *rateLimitStore) insertEvent(state RateLimitState, ruleState RateLimitRuleState, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("rate limit store is not initialized")
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO rate_limit_events (
			 id, account_key, rule_id, strategy, window, action,
			 usage_value, limit_value, blocked, reason, triggered_at_unix_ms
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		randomID("rle"),
		state.AccountKey,
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
			`SELECT id, account_key, rule_id, strategy, window, action,
			        usage_value, limit_value, blocked, reason, triggered_at_unix_ms
			   FROM rate_limit_events
			  ORDER BY triggered_at_unix_ms DESC
			  LIMIT ?`,
			limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, account_key, rule_id, strategy, window, action,
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
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	now := e.now().UTC()
	if err := e.store.expireReservations(ctx, now); err != nil {
		return err
	}
	rules, err := e.store.listEnabledRules()
	if err != nil {
		return err
	}
	stateMap, err := e.evaluateRules(ctx, rules, now)
	if err != nil {
		return err
	}

	e.mu.Lock()
	previous := e.byKey
	events := collectNewRateLimitBlockEvents(previous, stateMap)
	e.replaceStatesLocked(stateMap)
	e.mu.Unlock()
	ReplaceAccountRouteGuardSource(AccountRouteGuardSourceRateLimit, rateLimitRouteGuardBlocks(stateMap))
	e.persistBlockEvents(events, now)
	return nil
}

func (e *RateLimitEvaluator) EvaluateAccountNow(ctx context.Context, accountKey string) error {
	if e == nil || e.store == nil {
		return errors.New("rate limit evaluator is not initialized")
	}
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return nil
	}
	e.evalMu.Lock()
	defer e.evalMu.Unlock()
	now := e.now().UTC()
	if err := e.store.expireReservations(ctx, now); err != nil {
		return err
	}
	rules, err := e.store.listEnabledRulesForAccount(accountKey)
	if err != nil {
		return err
	}
	stateMap, err := e.evaluateRules(ctx, rules, now)
	if err != nil {
		return err
	}

	targetKey := rateLimitStateKey(accountKey)
	e.mu.Lock()
	previous := e.byKey
	next := make(map[string]RateLimitState, len(e.byKey)+len(stateMap))
	for key, state := range e.byKey {
		if key == targetKey {
			continue
		}
		next[key] = state
	}
	for key, state := range stateMap {
		next[key] = state
	}
	events := collectNewRateLimitBlockEvents(previous, stateMap)
	e.replaceStatesLocked(next)
	guardStates := cloneRateLimitStateMap(e.byKey)
	e.mu.Unlock()
	ReplaceAccountRouteGuardSource(AccountRouteGuardSourceRateLimit, rateLimitRouteGuardBlocks(guardStates))
	e.persistBlockEvents(events, now)
	return nil
}

func (e *RateLimitEvaluator) evaluateRules(ctx context.Context, rules []RateLimitRule, now time.Time) (map[string]RateLimitState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stateMap := map[string]RateLimitState{}
	for _, rule := range rules {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		strategy, ok := e.registry.Get(rule.Strategy)
		if !ok {
			return nil, fmt.Errorf("unsupported rate limit strategy %q", rule.Strategy)
		}
		usage, err := strategy.UsageForRule(ctx, e.store, rule, now)
		if err != nil {
			return nil, err
		}
		stateKey := rateLimitStateKey(rule.AccountKey)
		state := stateMap[stateKey]
		if state.AccountKey == "" {
			state.AccountKey = rule.AccountKey
			state.UpdatedAt = now.Format(time.RFC3339)
			state.LastEvaluatedAt = state.UpdatedAt
		}
		ruleState := evaluateRateLimitRule(strategy, rule, usage, now)
		state.Rules = append(state.Rules, ruleState)
		if ruleState.Exceeded && rule.Action == RateLimitActionBlock {
			state.Blocked = true
			source := rateLimitSourceStateFromRuleState(ruleState)
			state.Sources = append(state.Sources, source)
			if state.BlockReason == "" {
				state.BlockReason = ruleState.Reason
			}
			state.NextReset = earliestRateLimitTimestamp(state.NextReset, source.NextReset)
		}
		stateMap[stateKey] = state
	}
	return stateMap, nil
}

func collectNewRateLimitBlockEvents(previous map[string]RateLimitState, states map[string]RateLimitState) []rateLimitBlockEvent {
	out := []rateLimitBlockEvent{}
	for key, state := range states {
		if !state.Blocked || previous[key].Blocked {
			continue
		}
		for _, ruleState := range state.Rules {
			if ruleState.Exceeded && ruleState.Rule.Action == RateLimitActionBlock {
				out = append(out, rateLimitBlockEvent{State: state, RuleState: ruleState})
			}
		}
	}
	return out
}

func (e *RateLimitEvaluator) persistBlockEvents(events []rateLimitBlockEvent, now time.Time) {
	if e == nil || e.store == nil {
		return
	}
	for _, event := range events {
		if err := e.store.insertEvent(event.State, event.RuleState, now); err != nil {
			log.WithError(err).Warn("gettokenshooks: rate limit event persist failed")
		}
	}
}

func (e *RateLimitEvaluator) replaceStatesLocked(states map[string]RateLimitState) {
	byKey := map[string]RateLimitState{}
	byAcct := map[string]RateLimitState{}
	byLookup := map[string]RateLimitState{}
	for key, state := range states {
		sort.SliceStable(state.Rules, func(i, j int) bool {
			return state.Rules[i].Rule.ID < state.Rules[j].Rule.ID
		})
		sort.SliceStable(state.Sources, func(i, j int) bool {
			if state.Sources[i].Source != state.Sources[j].Source {
				return state.Sources[i].Source < state.Sources[j].Source
			}
			return state.Sources[i].RuleID < state.Sources[j].RuleID
		})
		if state.Rules == nil {
			state.Rules = []RateLimitRuleState{}
		}
		if state.Sources == nil {
			state.Sources = []RateLimitSourceState{}
		}
		byKey[key] = state
		if existing, ok := byAcct[state.AccountKey]; !ok || (!existing.Blocked && state.Blocked) {
			byAcct[state.AccountKey] = state
		}
		indexRateLimitLookup(byLookup, state)
	}
	e.byKey = byKey
	e.byAcct = byAcct
	e.byLookup = byLookup
}

func cloneRateLimitStateMap(states map[string]RateLimitState) map[string]RateLimitState {
	out := make(map[string]RateLimitState, len(states))
	for key, state := range states {
		out[key] = cloneRateLimitState(state)
	}
	return out
}

func cloneRateLimitState(state RateLimitState) RateLimitState {
	state.Rules = append([]RateLimitRuleState(nil), state.Rules...)
	state.Sources = append([]RateLimitSourceState(nil), state.Sources...)
	return state
}

func evaluateRateLimitRule(strategy RateLimitStrategy, rule RateLimitRule, usage int64, now time.Time) RateLimitRuleState {
	exceeded := rule.LimitValue > 0 && usage >= rule.LimitValue
	pct := 0.0
	if rule.LimitValue > 0 {
		pct = float64(usage) / float64(rule.LimitValue) * 100
	}
	reason := ""
	if exceeded {
		reason = strategy.FormatReason(rule)
	}
	windowStart, windowEnd, nextReset := rateLimitRuleWindowBounds(rule.Window, now)
	return RateLimitRuleState{
		Rule:         rule,
		Exceeded:     exceeded,
		Reason:       reason,
		UsagePct:     pct,
		CurrentUsage: usage,
		LimitValue:   rule.LimitValue,
		WindowStart:  formatRateLimitTime(windowStart),
		WindowEnd:    formatRateLimitTime(windowEnd),
		NextReset:    formatRateLimitTime(nextReset),
	}
}

func rateLimitSourceStateFromRuleState(ruleState RateLimitRuleState) RateLimitSourceState {
	return RateLimitSourceState{
		Source:      AccountRouteGuardSourceRateLimit,
		Reason:      ruleState.Reason,
		RuleID:      ruleState.Rule.ID,
		Strategy:    ruleState.Rule.Strategy,
		Window:      ruleState.Rule.Window,
		UsageValue:  ruleState.CurrentUsage,
		LimitValue:  ruleState.LimitValue,
		WindowStart: ruleState.WindowStart,
		WindowEnd:   ruleState.WindowEnd,
		NextReset:   ruleState.NextReset,
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

func rateLimitRuleWindowBounds(window string, now time.Time) (time.Time, time.Time, time.Time) {
	now = now.UTC()
	if strings.EqualFold(strings.TrimSpace(window), RateLimitWindowCalendarDay) {
		localNow := now.Local()
		localStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location())
		start := localStart.UTC()
		end := localStart.Add(24 * time.Hour).UTC()
		return start, end, end
	}
	duration := parseUsageAttributionDuration(window, defaultUsageAttributionWindow)
	if duration <= 0 {
		duration = defaultUsageAttributionWindow
	}
	return now.Add(-duration), now, now.Add(duration)
}

func formatRateLimitTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func earliestRateLimitTimestamp(current string, candidate string) string {
	current = strings.TrimSpace(current)
	candidate = strings.TrimSpace(candidate)
	if current == "" {
		return candidate
	}
	if candidate == "" {
		return current
	}
	currentTime, currentErr := time.Parse(time.RFC3339, current)
	candidateTime, candidateErr := time.Parse(time.RFC3339, candidate)
	if currentErr != nil || candidateErr != nil {
		return current
	}
	if candidateTime.Before(currentTime) {
		return candidate
	}
	return current
}

func (e *RateLimitEvaluator) StateForAccount(accountKey string) (RateLimitState, bool) {
	if e == nil {
		return RateLimitState{}, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	state, ok := e.byAcct[strings.TrimSpace(accountKey)]
	if !ok {
		return RateLimitState{}, false
	}
	return cloneRateLimitState(state), true
}

func (e *RateLimitEvaluator) States() []RateLimitState {
	if e == nil {
		return []RateLimitState{}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]RateLimitState, 0, len(e.byKey))
	for _, state := range e.byKey {
		out = append(out, cloneRateLimitState(state))
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
}

func (e *RateLimitEvaluator) replaceStatesForTest(states []RateLimitState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.byKey = map[string]RateLimitState{}
	e.byAcct = map[string]RateLimitState{}
	e.byLookup = map[string]RateLimitState{}
	for _, state := range states {
		key := rateLimitStateKey(state.AccountKey)
		e.byKey[key] = state
		e.byAcct[state.AccountKey] = state
		indexRateLimitLookup(e.byLookup, state)
	}
	ReplaceAccountRouteGuardSource(AccountRouteGuardSourceRateLimit, rateLimitRouteGuardBlocks(e.byKey))
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
		var expiresAt time.Time
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(state.NextReset)); err == nil {
			expiresAt = parsed.UTC()
		}
		blocks = append(blocks, AccountRouteGuardBlock{
			Source:     AccountRouteGuardSourceRateLimit,
			AccountKey: state.AccountKey,
			Reason:     state.BlockReason,
			ExpiresAt:  expiresAt,
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
	add(auth.AccountKey)
	return keys
}

func rateLimitStateKey(accountKey string) string {
	return strings.TrimSpace(accountKey)
}

func normalizeRateLimitRule(rule RateLimitRule) RateLimitRule {
	rule.ID = strings.TrimSpace(rule.ID)
	rule.AccountKey = strings.TrimSpace(rule.AccountKey)
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
	return validateRateLimitRuleWithRegistry(rule, defaultRateLimitRegistry)
}

func validateRateLimitRuleWithRegistry(rule RateLimitRule, registry *RateLimitStrategyRegistry) error {
	if rule.AccountKey == "" {
		return errors.New("account_key is required")
	}
	if !accountstore.IsAccountKey(rule.AccountKey) {
		return fmt.Errorf("account_key must be an acct_* account card key, got %q", rule.AccountKey)
	}
	if registry == nil {
		registry = defaultRateLimitRegistry
	}
	strategy, ok := registry.Get(rule.Strategy)
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
		_, evaluator, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": evaluator.registry.List()})
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
		rule = normalizeRateLimitRule(rule)
		previous, existed, err := store.getRule(rule.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rule, err = store.upsertRuleWithRegistry(rule, time.Now().UTC(), evaluator.registry)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := evaluator.EvaluateNow(c.Request.Context()); err != nil {
			rollbackRateLimitRuleChange(store, evaluator, previous, existed, rule.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
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
		rule = normalizeRateLimitRule(rule)
		previous, existed, err := store.getRule(rule.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rule, err = store.upsertRuleWithRegistry(rule, time.Now().UTC(), evaluator.registry)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := evaluator.EvaluateNow(c.Request.Context()); err != nil {
			rollbackRateLimitRuleChange(store, evaluator, previous, existed, rule.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rules, _ := store.listRules(rule.AccountKey)
		c.JSON(http.StatusOK, gin.H{"items": rules})
	})
	group.DELETE("/gettokens/rate-limit-rules/:id", func(c *gin.Context) {
		store, evaluator, ok := currentRateLimitRuntime(c)
		if !ok {
			return
		}
		ruleID := strings.TrimSpace(c.Param("id"))
		previous, existed, err := store.getRule(ruleID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := store.deleteRule(ruleID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := evaluator.EvaluateNow(c.Request.Context()); err != nil {
			rollbackRateLimitRuleChange(store, evaluator, previous, existed, ruleID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
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
			c.JSON(http.StatusOK, RateLimitState{AccountKey: accountKey, Sources: []RateLimitSourceState{}, Rules: []RateLimitRuleState{}})
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

func rollbackRateLimitRuleChange(store *rateLimitStore, evaluator *RateLimitEvaluator, previous RateLimitRule, existed bool, ruleID string) {
	if store == nil {
		return
	}
	registry := defaultRateLimitRegistry
	if evaluator != nil && evaluator.registry != nil {
		registry = evaluator.registry
	}
	if existed {
		if err := store.replaceRuleWithRegistry(previous, registry); err != nil {
			log.WithError(err).WithField("rule_id", previous.ID).Warn("gettokenshooks: rollback rate limit rule failed")
		}
	} else if err := store.deleteRule(ruleID); err != nil {
		log.WithError(err).WithField("rule_id", ruleID).Warn("gettokenshooks: rollback new rate limit rule failed")
	}
	if evaluator != nil {
		if err := evaluator.EvaluateNow(context.Background()); err != nil {
			log.WithError(err).Warn("gettokenshooks: restore rate limit evaluator after rollback failed")
		}
	}
}

func ConfigureGetTokensManagementRoutes(group *gin.RouterGroup, handler *handlers.BaseAPIHandler, cfg *config.Config) {
	ConfigureUsageAttributionRoutes(group, handler, cfg)
	ConfigureQuotaRuntimeRoutes(group, handler, cfg)
	ConfigureRateLimitRoutes(group, handler, cfg)
	ConfigureQuotaThresholdRuleRoutes(group, handler, cfg)
	ConfigureBudgetWindowDefinitionRoutes(group, handler, cfg)
	ConfigureProjectCandidatePoolRoutes(group, handler, cfg)
	ConfigureChannelRoutingExplainRoutes(group, handler, cfg)
	ConfigureChannelRoutingDecisionRoutes(group, handler, cfg)
	ConfigureRouteResilienceActionRoutes(group, nil)
	ConfigureLiveSessionRoutes(group, handler, cfg)
	ConfigureDoctorDiagnosticsRoutes(group, handler, cfg)
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
