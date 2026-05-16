package gettokenshooks

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	usageAttributionSchema = `
CREATE TABLE IF NOT EXISTS usage_attribution_events (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL DEFAULT '',
  attempt_index INTEGER NOT NULL DEFAULT 0,
  started_at_unix_ms INTEGER NOT NULL,
  completed_at_unix_ms INTEGER NOT NULL,
  method TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  requested_model TEXT NOT NULL DEFAULT '',
  routed_model TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  attribution_key TEXT NOT NULL,
  attribution_kind TEXT NOT NULL,
  account_key TEXT NOT NULL DEFAULT '',
  credential_key TEXT NOT NULL DEFAULT '',
  auth_id TEXT NOT NULL DEFAULT '',
  auth_index TEXT NOT NULL DEFAULT '',
  auth_type TEXT NOT NULL DEFAULT '',
  source_hash TEXT NOT NULL DEFAULT '',
  api_key_hash TEXT NOT NULL DEFAULT '',
  status_code INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cached_input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  evidence_kind TEXT NOT NULL,
  evidence_ref TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_usage_attribution_account_time
  ON usage_attribution_events(account_key, completed_at_unix_ms);
CREATE INDEX IF NOT EXISTS idx_usage_attribution_key_time
  ON usage_attribution_events(attribution_key, completed_at_unix_ms);
CREATE INDEX IF NOT EXISTS idx_usage_attribution_model_time
  ON usage_attribution_events(requested_model, completed_at_unix_ms);
CREATE INDEX IF NOT EXISTS idx_usage_attribution_auth_index_time
  ON usage_attribution_events(auth_index, completed_at_unix_ms);`

	defaultUsageAttributionWindow = 24 * time.Hour
	defaultUsageAttributionBucket = time.Hour
)

type UsageAttributionOptions struct {
	ConfigFilePath string
	WritableBase   string
}

type usageAttributionEvent struct {
	ID                string
	RequestID         string
	AttemptIndex      int64
	StartedAtUnixMs   int64
	CompletedAtUnixMs int64
	Method            string
	Path              string
	RequestedModel    string
	RoutedModel       string
	Provider          string
	AttributionKey    string
	AttributionKind   string
	AccountKey        string
	CredentialKey     string
	AuthID            string
	AuthIndex         string
	AuthType          string
	SourceHash        string
	APIKeyHash        string
	StatusCode        int64
	Failed            bool
	LatencyMs         int64
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
	TotalTokens       int64
	EvidenceKind      string
	EvidenceRef       string
}

type UsageAttributionSummaryResponse struct {
	Window      string                 `json:"window"`
	Bucket      string                 `json:"bucket"`
	GeneratedAt string                 `json:"generatedAt"`
	Items       []UsageAttributionItem `json:"items"`
	Unresolved  []UsageAttributionItem `json:"unresolved,omitempty"`
}

type UsageAttributionItem struct {
	AttributionKey    string                   `json:"attributionKey"`
	AttributionKind   string                   `json:"attributionKind"`
	AccountKey        string                   `json:"accountKey"`
	CredentialKey     string                   `json:"credentialKey,omitempty"`
	Provider          string                   `json:"provider"`
	RequestedModels   []string                 `json:"requestedModels"`
	RequestCount      int64                    `json:"requestCount"`
	FailedCount       int64                    `json:"failedCount"`
	LatencyAverageMs  int64                    `json:"latencyAverageMs,omitempty"`
	InputTokens       int64                    `json:"inputTokens"`
	CachedInputTokens int64                    `json:"cachedInputTokens"`
	OutputTokens      int64                    `json:"outputTokens"`
	TotalTokens       int64                    `json:"totalTokens"`
	LastActivityAt    string                   `json:"lastActivityAt,omitempty"`
	Buckets           []UsageAttributionBucket `json:"buckets"`
}

type UsageAttributionBucket struct {
	Start             string `json:"start"`
	RequestCount      int64  `json:"requestCount"`
	FailedCount       int64  `json:"failedCount"`
	InputTokens       int64  `json:"inputTokens"`
	CachedInputTokens int64  `json:"cachedInputTokens"`
	OutputTokens      int64  `json:"outputTokens"`
	TotalTokens       int64  `json:"totalTokens"`
}

type usageAttributionStore struct {
	db *sql.DB
}

var (
	usageAttributionMu      sync.RWMutex
	defaultAttributionStore *usageAttributionStore
)

func InstallUsageAttributionHook(opts UsageAttributionOptions) error {
	dbPath, err := resolveUsageAttributionPath(opts)
	if err != nil {
		return fmt.Errorf("resolve usage attribution path: %w", err)
	}
	store, err := newUsageAttributionStore(dbPath)
	if err != nil {
		return fmt.Errorf("open usage attribution store: %w", err)
	}
	usageAttributionMu.Lock()
	defaultAttributionStore = store
	usageAttributionMu.Unlock()
	coreusage.RegisterPlugin(usageAttributionPlugin{store: store})
	log.Infof("gettokenshooks: usage attribution ledger ready at %s", dbPath)
	return nil
}

func resolveUsageAttributionPath(opts UsageAttributionOptions) (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("GETTOKENS_USAGE_ATTRIBUTION_SQLITE_PATH")); fromEnv != "" {
		return fromEnv, nil
	}
	if configPath := strings.TrimSpace(opts.ConfigFilePath); configPath != "" {
		return filepath.Join(filepath.Dir(configPath), "usage-attribution-v1.sqlite"), nil
	}
	if writableBase := strings.TrimSpace(opts.WritableBase); writableBase != "" {
		return filepath.Join(writableBase, "usage-attribution-v1.sqlite"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gettokens", "usage-attribution-v1.sqlite"), nil
}

func newUsageAttributionStore(path string) (*usageAttributionStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty sqlite path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(usageAttributionSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &usageAttributionStore{db: db}, nil
}

func (s *usageAttributionStore) insert(event usageAttributionEvent) error {
	if s == nil || s.db == nil {
		return errors.New("usage attribution store is not initialized")
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO usage_attribution_events (
		 id, request_id, attempt_index, started_at_unix_ms, completed_at_unix_ms, method, path,
		 requested_model, routed_model, provider, attribution_key, attribution_kind, account_key,
		 credential_key, auth_id, auth_index, auth_type, source_hash, api_key_hash, status_code,
		 failed, latency_ms, input_tokens, cached_input_tokens, output_tokens, total_tokens,
		 evidence_kind, evidence_ref
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID,
		event.RequestID,
		event.AttemptIndex,
		event.StartedAtUnixMs,
		event.CompletedAtUnixMs,
		event.Method,
		event.Path,
		event.RequestedModel,
		event.RoutedModel,
		event.Provider,
		event.AttributionKey,
		event.AttributionKind,
		event.AccountKey,
		event.CredentialKey,
		event.AuthID,
		event.AuthIndex,
		event.AuthType,
		event.SourceHash,
		event.APIKeyHash,
		event.StatusCode,
		boolToInt(event.Failed),
		event.LatencyMs,
		event.InputTokens,
		event.CachedInputTokens,
		event.OutputTokens,
		event.TotalTokens,
		event.EvidenceKind,
		event.EvidenceRef,
	)
	return err
}

func (s *usageAttributionStore) summary(window, bucket time.Duration, includeUnresolved bool) (UsageAttributionSummaryResponse, error) {
	if s == nil || s.db == nil {
		return UsageAttributionSummaryResponse{}, errors.New("usage attribution store is not initialized")
	}
	windowLabel := formatUsageAttributionWindow(window)
	if window == 0 {
		window = defaultUsageAttributionWindow
		windowLabel = defaultUsageAttributionWindow.String()
	}
	if bucket <= 0 {
		bucket = defaultUsageAttributionBucket
	}
	now := time.Now().UTC()
	since := int64(0)
	if window > 0 {
		since = now.Add(-window).UnixMilli()
	}
	rows, err := s.db.Query(
		`SELECT attribution_key, attribution_kind, account_key, credential_key, provider,
		        requested_model, completed_at_unix_ms, failed, latency_ms,
		        input_tokens, cached_input_tokens, output_tokens, total_tokens
		   FROM usage_attribution_events
		  WHERE completed_at_unix_ms >= ?
		  ORDER BY completed_at_unix_ms ASC`,
		since,
	)
	if err != nil {
		return UsageAttributionSummaryResponse{}, err
	}
	defer rows.Close()

	type aggregate struct {
		item         UsageAttributionItem
		models       map[string]struct{}
		latencySum   int64
		latencyCount int64
		buckets      map[int64]*UsageAttributionBucket
		lastUnixMs   int64
	}
	aggregates := map[string]*aggregate{}
	for rows.Next() {
		var attributionKey, attributionKind, accountKey, credentialKey, provider, requestedModel string
		var completedAtUnixMs, failed, latencyMs, inputTokens, cachedInputTokens, outputTokens, totalTokens int64
		if err := rows.Scan(
			&attributionKey,
			&attributionKind,
			&accountKey,
			&credentialKey,
			&provider,
			&requestedModel,
			&completedAtUnixMs,
			&failed,
			&latencyMs,
			&inputTokens,
			&cachedInputTokens,
			&outputTokens,
			&totalTokens,
		); err != nil {
			return UsageAttributionSummaryResponse{}, err
		}
		key := strings.TrimSpace(accountKey)
		if key == "" {
			key = strings.TrimSpace(attributionKey)
		}
		if key == "" {
			continue
		}
		agg := aggregates[key]
		if agg == nil {
			agg = &aggregate{
				item: UsageAttributionItem{
					AttributionKey:  attributionKey,
					AttributionKind: attributionKind,
					AccountKey:      accountKey,
					CredentialKey:   credentialKey,
					Provider:        provider,
				},
				models:  map[string]struct{}{},
				buckets: map[int64]*UsageAttributionBucket{},
			}
			aggregates[key] = agg
		}
		if requestedModel = strings.TrimSpace(requestedModel); requestedModel != "" {
			agg.models[requestedModel] = struct{}{}
		}
		agg.item.RequestCount++
		agg.item.FailedCount += failed
		agg.item.InputTokens += inputTokens
		agg.item.CachedInputTokens += cachedInputTokens
		agg.item.OutputTokens += outputTokens
		agg.item.TotalTokens += totalTokens
		if latencyMs > 0 {
			agg.latencySum += latencyMs
			agg.latencyCount++
		}
		if completedAtUnixMs > agg.lastUnixMs {
			agg.lastUnixMs = completedAtUnixMs
		}

		bucketStart := floorUnixMilli(completedAtUnixMs, bucket)
		b := agg.buckets[bucketStart]
		if b == nil {
			b = &UsageAttributionBucket{Start: time.UnixMilli(bucketStart).UTC().Format(time.RFC3339)}
			agg.buckets[bucketStart] = b
		}
		b.RequestCount++
		b.FailedCount += failed
		b.InputTokens += inputTokens
		b.CachedInputTokens += cachedInputTokens
		b.OutputTokens += outputTokens
		b.TotalTokens += totalTokens
	}
	if err := rows.Err(); err != nil {
		return UsageAttributionSummaryResponse{}, err
	}

	out := UsageAttributionSummaryResponse{
		Window:      windowLabel,
		Bucket:      bucket.String(),
		GeneratedAt: now.Format(time.RFC3339),
	}
	for _, agg := range aggregates {
		if agg.latencyCount > 0 {
			agg.item.LatencyAverageMs = agg.latencySum / agg.latencyCount
		}
		if agg.lastUnixMs > 0 {
			agg.item.LastActivityAt = time.UnixMilli(agg.lastUnixMs).UTC().Format(time.RFC3339)
		}
		agg.item.RequestedModels = sortedStringSet(agg.models)
		agg.item.Buckets = sortedBuckets(agg.buckets)
		if strings.TrimSpace(agg.item.AccountKey) == "" {
			if includeUnresolved {
				out.Unresolved = append(out.Unresolved, agg.item)
			}
			continue
		}
		out.Items = append(out.Items, agg.item)
	}
	sortUsageAttributionItems(out.Items)
	sortUsageAttributionItems(out.Unresolved)
	return out, nil
}

type usageAttributionPlugin struct {
	store *usageAttributionStore
}

func (p usageAttributionPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p.store == nil {
		return
	}
	event := buildUsageAttributionEvent(ctx, record)
	if strings.TrimSpace(event.AttributionKey) == "" {
		return
	}
	if err := p.store.insert(event); err != nil {
		log.WithError(err).Warn("gettokenshooks: persist usage attribution failed")
	}
}

func buildUsageAttributionEvent(ctx context.Context, record coreusage.Record) usageAttributionEvent {
	startedAt := record.RequestedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	latencyMs := record.Latency.Milliseconds()
	if latencyMs < 0 {
		latencyMs = 0
	}
	completedAt := startedAt.Add(time.Duration(latencyMs) * time.Millisecond)
	if completedAt.Before(startedAt) || completedAt.IsZero() {
		completedAt = time.Now()
	}
	statusCode := int64(record.Fail.StatusCode)
	failed := record.Failed || statusCode >= http.StatusBadRequest
	if statusCode <= 0 {
		if failed {
			statusCode = http.StatusInternalServerError
		} else {
			statusCode = http.StatusOK
		}
	}

	attributionKey, attributionKind, evidenceRef := resolveUsageAttributionEvidence(record)
	sourceHash := ""
	if source := strings.TrimSpace(record.Source); source != "" {
		sourceHash = shortSHA256(source)
	}
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	endpoint := strings.TrimSpace(internallogging.GetEndpoint(ctx))
	method, path := splitUsageAttributionEndpoint(endpoint)
	event := usageAttributionEvent{
		RequestID:         requestID,
		StartedAtUnixMs:   startedAt.UTC().UnixMilli(),
		CompletedAtUnixMs: completedAt.UTC().UnixMilli(),
		Method:            method,
		Path:              path,
		RequestedModel:    firstNonEmptyString(record.Alias, record.Model),
		RoutedModel:       strings.TrimSpace(record.Model),
		Provider:          strings.TrimSpace(record.Provider),
		AttributionKey:    attributionKey,
		AttributionKind:   attributionKind,
		AuthID:            strings.TrimSpace(record.AuthID),
		AuthIndex:         strings.TrimSpace(record.AuthIndex),
		AuthType:          strings.TrimSpace(record.AuthType),
		SourceHash:        sourceHash,
		StatusCode:        statusCode,
		Failed:            failed,
		LatencyMs:         latencyMs,
		InputTokens:       record.Detail.InputTokens,
		CachedInputTokens: record.Detail.CachedTokens,
		OutputTokens:      record.Detail.OutputTokens,
		TotalTokens:       normalizeUsageAttributionTotal(record.Detail),
		EvidenceKind:      attributionKind,
		EvidenceRef:       evidenceRef,
	}
	event.ID = usageAttributionEventID(event)
	return event
}

func resolveUsageAttributionEvidence(record coreusage.Record) (string, string, string) {
	if authID := strings.TrimSpace(record.AuthID); authID != "" && shouldPreferUsageAttributionAuthID(record, authID) {
		return "auth-id:" + authID, "auth_id", authID
	}
	if authIndex := strings.TrimSpace(record.AuthIndex); authIndex != "" {
		return "auth-index:" + authIndex, "auth_index", authIndex
	}
	if authID := strings.TrimSpace(record.AuthID); authID != "" {
		return "auth-id:" + authID, "auth_id", authID
	}
	if source := strings.TrimSpace(record.Source); source != "" {
		hash := shortSHA256(source)
		return "source:" + hash, "source_hash", hash
	}
	if provider := strings.TrimSpace(record.Provider); provider != "" {
		return "provider:" + strings.ToLower(provider), "provider_fallback", provider
	}
	return "", "unresolved", ""
}

func shouldPreferUsageAttributionAuthID(record coreusage.Record, authID string) bool {
	normalizedAuthID := strings.ToLower(strings.TrimSpace(authID))
	if strings.HasPrefix(normalizedAuthID, "codex:apikey:") || strings.HasPrefix(normalizedAuthID, "openai-compatibility:") {
		return true
	}
	normalizedAuthType := strings.ToLower(strings.TrimSpace(record.AuthType))
	return normalizedAuthType == "api-key" ||
		normalizedAuthType == "api_key" ||
		normalizedAuthType == "apikey" ||
		normalizedAuthType == "openai-compatible"
}

func ConfigureUsageAttributionRoutes(group *gin.RouterGroup, _ *handlers.BaseAPIHandler, _ *config.Config) {
	if group == nil {
		return
	}
	group.GET("/gettokens/usage-attribution", func(c *gin.Context) {
		usageAttributionMu.RLock()
		store := defaultAttributionStore
		usageAttributionMu.RUnlock()
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage attribution store is not initialized"})
			return
		}
		window := parseUsageAttributionDuration(c.Query("window"), defaultUsageAttributionWindow)
		bucket := parseUsageAttributionDuration(c.Query("bucket"), defaultUsageAttributionBucket)
		includeUnresolved := parseUsageAttributionBool(c.Query("include_unresolved"))
		summary, err := store.summary(window, bucket, includeUnresolved)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, summary)
	})
}

func parseUsageAttributionDuration(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "all", "unbounded":
		return -1
	}
	if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
		return parsed
	}
	if strings.HasSuffix(raw, "d") {
		value, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err == nil && value > 0 {
			return time.Duration(value) * 24 * time.Hour
		}
	}
	return fallback
}

func formatUsageAttributionWindow(window time.Duration) string {
	if window < 0 {
		return "all"
	}
	return window.String()
}

func parseUsageAttributionBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func floorUnixMilli(value int64, bucket time.Duration) int64 {
	bucketMs := bucket.Milliseconds()
	if bucketMs <= 0 {
		return value
	}
	return (value / bucketMs) * bucketMs
}

func sortedStringSet(input map[string]struct{}) []string {
	out := make([]string, 0, len(input))
	for item := range input {
		out = append(out, item)
	}
	sortStrings(out)
	return out
}

func sortedBuckets(input map[int64]*UsageAttributionBucket) []UsageAttributionBucket {
	keys := make([]int64, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sortInt64s(keys)
	out := make([]UsageAttributionBucket, 0, len(keys))
	for _, key := range keys {
		if input[key] != nil {
			out = append(out, *input[key])
		}
	}
	return out
}

func sortUsageAttributionItems(items []UsageAttributionItem) {
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j].AccountKey < items[i].AccountKey {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}

func sortStrings(items []string) {
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j] < items[i] {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}

func sortInt64s(items []int64) {
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			if items[j] < items[i] {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}

func usageAttributionEventID(event usageAttributionEvent) string {
	seed := fmt.Sprintf(
		"%s|%s|%d|%s|%s|%d|%d|%d|%d",
		event.RequestID,
		event.AttributionKey,
		event.CompletedAtUnixMs,
		event.RequestedModel,
		event.RoutedModel,
		event.InputTokens,
		event.OutputTokens,
		event.TotalTokens,
		boolToInt(event.Failed),
	)
	return shortSHA256(seed)
}

func normalizeUsageAttributionTotal(detail coreusage.Detail) int64 {
	if detail.TotalTokens != 0 {
		return detail.TotalTokens
	}
	total := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	if total != 0 {
		return total
	}
	return detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens + detail.CachedTokens
}

func splitUsageAttributionEndpoint(endpoint string) (string, string) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", ""
	}
	parts := strings.SplitN(endpoint, " ", 2)
	if len(parts) != 2 {
		return "", endpoint
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func shortSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
