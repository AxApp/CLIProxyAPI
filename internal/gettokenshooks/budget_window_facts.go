package gettokenshooks

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	BudgetWindowKindDaily    = "daily"
	BudgetWindowKindMultiDay = "multi-day"
	BudgetWindowKindBounded  = "bounded"

	BudgetWindowMetricTokens   = "tokens"
	BudgetWindowMetricRequests = "requests"

	BudgetWindowSemanticsCalendar = "calendar"

	BudgetWindowFactSourceUsageAggregator = "usage-aggregator"
	BudgetWindowRecoverySourceWindowEnd   = "window-end"
)

type BudgetWindowDefinition struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Semantics string    `json:"semantics,omitempty"`
	Days      int       `json:"days,omitempty"`
	Metric    string    `json:"metric"`
	Limit     float64   `json:"limit"`
	Timezone  string    `json:"timezone,omitempty"`
	StartsAt  time.Time `json:"startsAt,omitempty"`
	EndsAt    time.Time `json:"endsAt,omitempty"`
	Enabled   bool      `json:"enabled"`
}

type BudgetWindowFactsOptions struct {
	Now          time.Time
	Calibrations []AccountQuotaUsageCalibration
}

func BuildBudgetWindowFacts(
	ctx context.Context,
	store *rateLimitStore,
	accountKey string,
	definitions []BudgetWindowDefinition,
	options BudgetWindowFactsOptions,
) ([]QuotaWindowFacts, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("rate limit store is not initialized")
	}
	accountKey = strings.TrimSpace(accountKey)
	if accountKey == "" {
		return nil, fmt.Errorf("account key is required")
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	out := make([]QuotaWindowFacts, 0, len(definitions))
	for _, definition := range definitions {
		if !definition.Enabled {
			continue
		}
		window, err := buildBudgetWindowFact(ctx, store, accountKey, definition, options.Calibrations, now)
		if err != nil {
			return nil, err
		}
		out = append(out, window)
	}
	return out, nil
}

func buildBudgetWindowFact(
	ctx context.Context,
	store *rateLimitStore,
	accountKey string,
	definition BudgetWindowDefinition,
	calibrations []AccountQuotaUsageCalibration,
	now time.Time,
) (QuotaWindowFacts, error) {
	windowID := strings.TrimSpace(definition.ID)
	if windowID == "" {
		return QuotaWindowFacts{}, fmt.Errorf("budget window id is required")
	}
	metric := normalizeBudgetWindowMetric(definition.Metric)
	if metric == "" {
		return QuotaWindowFacts{}, fmt.Errorf("budget window %s metric is required", windowID)
	}
	startsAt, endsAt, timezone, kind, err := budgetWindowBounds(definition, now)
	if err != nil {
		return QuotaWindowFacts{}, fmt.Errorf("budget window %s: %w", windowID, err)
	}
	rawUsed, err := queryBudgetWindowUsage(ctx, store, accountKey, metric, startsAt, endsAt)
	if err != nil {
		return QuotaWindowFacts{}, fmt.Errorf("budget window %s usage: %w", windowID, err)
	}
	calibrationDelta := budgetWindowCalibrationDelta(accountKey, windowID, metric, startsAt, endsAt, calibrations, now)
	limit := math.Max(definition.Limit, 0)
	calibratedUsed := rawUsed + calibrationDelta
	if calibratedUsed < 0 {
		calibratedUsed = 0
	}
	if limit > 0 && calibratedUsed > limit {
		calibratedUsed = limit
	}
	remaining := math.Max(limit-calibratedUsed, 0)
	window := QuotaWindowFacts{
		WindowID:          windowID,
		Kind:              kind,
		Metric:            metric,
		Timezone:          timezone,
		StartsAt:          startsAt,
		EndsAt:            endsAt,
		ObservedUsed:      calibratedUsed,
		ObservedLimit:     limit,
		ObservedRemaining: remaining,
		RawUsed:           rawUsed,
		CalibrationDelta:  calibrationDelta,
		CalibratedUsed:    calibratedUsed,
		GeneratedAt:       now,
		Source:            BudgetWindowFactSourceUsageAggregator,
		RecoverySource:    BudgetWindowRecoverySourceWindowEnd,
		Status:            QuotaFactFresh,
	}
	if limit > 0 {
		window.ObservedUsedPercent = clampQuotaValue((calibratedUsed/limit)*100, 0, 100)
		window.ObservedRemainingPercent = clampQuotaValue((remaining/limit)*100, 0, 100)
	}
	return window, nil
}

func budgetWindowBounds(definition BudgetWindowDefinition, now time.Time) (time.Time, time.Time, string, string, error) {
	kind := normalizeBudgetWindowKind(definition.Kind)
	switch kind {
	case BudgetWindowKindDaily:
		location, timezone, err := budgetWindowLocation(definition.Timezone)
		if err != nil {
			return time.Time{}, time.Time{}, "", "", err
		}
		start := localDayStart(now.In(location))
		end := start.AddDate(0, 0, 1)
		return start.UTC(), end.UTC(), timezone, kind, nil
	case BudgetWindowKindMultiDay:
		location, timezone, err := budgetWindowLocation(definition.Timezone)
		if err != nil {
			return time.Time{}, time.Time{}, "", "", err
		}
		days := definition.Days
		if days <= 0 {
			return time.Time{}, time.Time{}, "", "", fmt.Errorf("multi-day window requires positive days")
		}
		semantics := strings.ToLower(strings.TrimSpace(definition.Semantics))
		if semantics == "" {
			semantics = BudgetWindowSemanticsCalendar
		}
		if semantics != BudgetWindowSemanticsCalendar {
			return time.Time{}, time.Time{}, "", "", fmt.Errorf("unsupported multi-day semantics %q", definition.Semantics)
		}
		todayStart := localDayStart(now.In(location))
		start := todayStart.AddDate(0, 0, -days+1)
		end := todayStart.AddDate(0, 0, 1)
		return start.UTC(), end.UTC(), timezone, kind, nil
	case BudgetWindowKindBounded:
		start := definition.StartsAt.UTC()
		end := definition.EndsAt.UTC()
		if start.IsZero() || end.IsZero() {
			return time.Time{}, time.Time{}, "", "", fmt.Errorf("bounded window requires startsAt and endsAt")
		}
		if !end.After(start) {
			return time.Time{}, time.Time{}, "", "", fmt.Errorf("bounded window endsAt must be after startsAt")
		}
		timezone := strings.TrimSpace(definition.Timezone)
		return start, end, timezone, kind, nil
	default:
		return time.Time{}, time.Time{}, "", "", fmt.Errorf("unsupported budget window kind %q", definition.Kind)
	}
}

func queryBudgetWindowUsage(ctx context.Context, store *rateLimitStore, accountKey string, metric string, startsAt time.Time, endsAt time.Time) (float64, error) {
	startMs := startsAt.UTC().UnixMilli()
	endMs := endsAt.UTC().UnixMilli()
	switch metric {
	case BudgetWindowMetricTokens:
		var total sql.NullFloat64
		err := store.db.QueryRowContext(
			ctx,
			`SELECT COALESCE(SUM(total_tokens), 0) FROM usage_attribution_events
			  WHERE account_key = ?
			    AND completed_at_unix_ms >= ?
			    AND completed_at_unix_ms < ?
			    AND completed_at_unix_ms > 0
			    AND failed = 0`,
			accountKey,
			startMs,
			endMs,
		).Scan(&total)
		if err != nil {
			return 0, err
		}
		return total.Float64, nil
	case BudgetWindowMetricRequests:
		var count int64
		err := store.db.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM usage_attribution_events
			  WHERE account_key = ?
			    AND completed_at_unix_ms >= ?
			    AND completed_at_unix_ms < ?
			    AND completed_at_unix_ms > 0
			    AND failed = 0`,
			accountKey,
			startMs,
			endMs,
		).Scan(&count)
		if err != nil {
			return 0, err
		}
		return float64(count), nil
	default:
		return 0, fmt.Errorf("unsupported budget window metric %q", metric)
	}
}

func budgetWindowCalibrationDelta(
	accountKey string,
	windowID string,
	metric string,
	startsAt time.Time,
	endsAt time.Time,
	calibrations []AccountQuotaUsageCalibration,
	now time.Time,
) float64 {
	var delta float64
	for _, calibration := range calibrations {
		if !budgetWindowCalibrationApplies(calibration, accountKey, windowID, now) {
			continue
		}
		if normalizeQuotaUsageCalibrationMode(calibration.Mode) != "delta" {
			continue
		}
		calibrationMetric := normalizeBudgetWindowMetric(calibration.Metric)
		if calibrationMetric != "" && calibrationMetric != metric {
			continue
		}
		createdAt := calibration.CreatedAt
		if createdAt.IsZero() {
			createdAt = now
		}
		createdAt = createdAt.UTC()
		if createdAt.Before(startsAt.UTC()) || !createdAt.Before(endsAt.UTC()) {
			continue
		}
		delta += calibration.Value
	}
	return delta
}

func budgetWindowCalibrationApplies(calibration AccountQuotaUsageCalibration, accountKey string, windowID string, now time.Time) bool {
	if strings.TrimSpace(calibration.AccountKey) != strings.TrimSpace(accountKey) {
		return false
	}
	if strings.TrimSpace(calibration.WindowKey) != strings.TrimSpace(windowID) {
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

func normalizeBudgetWindowKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "daily", "day":
		return BudgetWindowKindDaily
	case "multi-day", "multi_day", "multiday", "calendar-days", "calendar_days":
		return BudgetWindowKindMultiDay
	case "bounded", "fixed", "range":
		return BudgetWindowKindBounded
	default:
		return strings.ToLower(strings.TrimSpace(kind))
	}
}

func normalizeBudgetWindowMetric(metric string) string {
	switch strings.ToLower(strings.TrimSpace(metric)) {
	case "", "tokens", "token":
		return BudgetWindowMetricTokens
	case "requests", "request":
		return BudgetWindowMetricRequests
	default:
		return strings.ToLower(strings.TrimSpace(metric))
	}
}

func budgetWindowLocation(timezone string) (*time.Location, string, error) {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		return nil, "", fmt.Errorf("timezone is required")
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, "", fmt.Errorf("load timezone %q: %w", timezone, err)
	}
	return location, timezone, nil
}

func localDayStart(localNow time.Time) time.Time {
	year, month, day := localNow.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, localNow.Location())
}
