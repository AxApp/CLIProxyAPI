package gettokenshooks

import (
	"bufio"
	"encoding/json"
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
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	RouteResilienceActionClearTransientLockout = "clear_transient_lockout"
	RouteResilienceActionRerunBoundedReconcile = "rerun_bounded_reconcile"
	RouteResilienceActionRecheckRouteability   = "recheck_routeability"

	routeResilienceActionStatusApplied        = "applied"
	routeResilienceActionStatusDryRun         = "dry_run"
	routeResilienceActionStatusRejected       = "rejected"
	routeResilienceActionStatusNotImplemented = "not_implemented"
)

var routeResilienceClearableTransientSources = map[string]struct{}{
	AccountRouteGuardSourceAuthError:            {},
	AccountRouteGuardSourceUpstreamRateLimit:    {},
	AccountRouteGuardSourceUpstreamTransientErr: {},
}

type RouteResilienceActionRequest struct {
	Action         string   `json:"action"`
	AccountKey     string   `json:"accountKey,omitempty"`
	AuthID         string   `json:"authId,omitempty"`
	Model          string   `json:"model,omitempty"`
	Sources        []string `json:"sources,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	DryRun         bool     `json:"dryRun,omitempty"`
	IdempotencyKey string   `json:"idempotencyKey,omitempty"`
}

type RouteResilienceActionResponse struct {
	OK                   bool                          `json:"ok"`
	Authority            string                        `json:"authority"`
	Action               string                        `json:"action"`
	Status               string                        `json:"status"`
	AccountKey           string                        `json:"accountKey,omitempty"`
	AuthID               string                        `json:"authId,omitempty"`
	Model                string                        `json:"model,omitempty"`
	Before               RouteResilienceActionEvidence `json:"before"`
	After                RouteResilienceActionEvidence `json:"after"`
	AuditID              string                        `json:"auditId,omitempty"`
	DroppedSources       []string                      `json:"droppedSources,omitempty"`
	DroppedReasons       []ChannelRoutingDroppedReason `json:"droppedReasons,omitempty"`
	TracerOnly           bool                          `json:"tracerOnly,omitempty"`
	ReconcileRuns        int                           `json:"reconcileRuns"`
	LedgerError          string                        `json:"ledgerError,omitempty"`
	Error                string                        `json:"error,omitempty"`
	NotImplementedReason string                        `json:"notImplementedReason,omitempty"`
}

type RouteResilienceActionEvidence struct {
	BlockCount     int                           `json:"blockCount"`
	Blocks         []RouteResilienceState        `json:"blocks,omitempty"`
	DroppedReasons []ChannelRoutingDroppedReason `json:"droppedReasons,omitempty"`
}

type RouteResilienceActionHistoryItem struct {
	Action           string    `json:"action"`
	Target           string    `json:"target"`
	Status           string    `json:"status"`
	AuditID          string    `json:"auditId,omitempty"`
	AccountKey       string    `json:"accountKey,omitempty"`
	AuthID           string    `json:"authId,omitempty"`
	Model            string    `json:"model,omitempty"`
	BeforeBlockCount int       `json:"beforeBlockCount"`
	AfterBlockCount  int       `json:"afterBlockCount"`
	CreatedAt        time.Time `json:"createdAt"`
	TracerOnly       bool      `json:"tracerOnly,omitempty"`
	ReconcileRuns    int       `json:"reconcileRuns"`
}

type RouteResilienceActionHistoryResponse struct {
	OK        bool                               `json:"ok"`
	Authority string                             `json:"authority"`
	Items     []RouteResilienceActionHistoryItem `json:"items"`
}

type RouteResilienceActionHistoryFilter struct {
	Action     string
	Status     string
	Target     string
	AccountKey string
	AuthID     string
	Model      string
	Limit      int
}

type routeResilienceActionHistoryStore struct {
	mu             sync.Mutex
	items          []RouteResilienceActionHistoryItem
	limit          int
	maxEntries     int
	ledgerPath     string
	appendLedger   func(string, RouteResilienceActionHistoryItem) error
	truncateLedger func(string, int) error
}

var defaultRouteResilienceActionHistoryStore = &routeResilienceActionHistoryStore{
	limit:      200,
	maxEntries: 200,
	ledgerPath: defaultRouteResilienceActionLedgerPath(),
}

func ConfigureRouteResilienceActionRoutes(group *gin.RouterGroup, store *AccountRouteGuardStore) {
	if group == nil {
		return
	}
	group.POST("/gettokens/route-resilience/actions", func(c *gin.Context) {
		handleRouteResilienceAction(c, store)
	})
	group.GET("/gettokens/route-resilience/actions/history", handleRouteResilienceActionHistory)
}

func handleRouteResilienceAction(c *gin.Context, store *AccountRouteGuardStore) {
	var input RouteResilienceActionRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, RouteResilienceActionResponse{
			OK:        false,
			Authority: "sidecar",
			Status:    routeResilienceActionStatusRejected,
			Error:     err.Error(),
		})
		return
	}
	input = normalizeRouteResilienceActionRequest(input)
	switch input.Action {
	case RouteResilienceActionClearTransientLockout:
		handleRouteResilienceClearTransientLockout(c, store, input)
	case RouteResilienceActionRecheckRouteability:
		handleRouteResilienceRecheckRouteability(c, store, input)
	case RouteResilienceActionRerunBoundedReconcile:
		handleRouteResilienceRerunBoundedReconcile(c, store, input)
	default:
		c.JSON(http.StatusBadRequest, RouteResilienceActionResponse{
			OK:        false,
			Authority: "sidecar",
			Action:    input.Action,
			Status:    routeResilienceActionStatusRejected,
			Error:     "unsupported route resilience action",
		})
	}
}

func handleRouteResilienceRecheckRouteability(c *gin.Context, store *AccountRouteGuardStore, input RouteResilienceActionRequest) {
	target := routeResilienceActionTargetAuth(input)
	before := routeResilienceActionEvidence(store, target, input.Model)
	response := RouteResilienceActionResponse{
		OK:             false,
		Authority:      "sidecar",
		Action:         input.Action,
		AccountKey:     input.AccountKey,
		AuthID:         input.AuthID,
		Model:          input.Model,
		Before:         before,
		After:          before,
		DroppedReasons: append([]ChannelRoutingDroppedReason(nil), before.DroppedReasons...),
		TracerOnly:     true,
		ReconcileRuns:  0,
	}
	if input.AccountKey == "" && input.AuthID == "" {
		response.Status = routeResilienceActionStatusRejected
		response.Error = "accountKey or authId is required"
		c.JSON(http.StatusBadRequest, response)
		return
	}
	if input.DryRun {
		response.OK = true
		response.Status = routeResilienceActionStatusDryRun
		response = routeResilienceActionRecordHistory(input, response)
		c.JSON(http.StatusOK, response)
		return
	}
	after := routeResilienceActionEvidence(store, target, input.Model)
	response.OK = true
	response.Status = routeResilienceActionStatusApplied
	response.After = after
	response.AuditID = randomID("route-audit")
	response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), after.DroppedReasons...)
	response = routeResilienceActionRecordHistory(input, response)
	c.JSON(http.StatusOK, response)
}

func handleRouteResilienceRerunBoundedReconcile(c *gin.Context, store *AccountRouteGuardStore, input RouteResilienceActionRequest) {
	target := routeResilienceActionTargetAuth(input)
	before := routeResilienceActionEvidence(store, target, input.Model)
	response := RouteResilienceActionResponse{
		OK:             false,
		Authority:      "sidecar",
		Action:         input.Action,
		AccountKey:     input.AccountKey,
		AuthID:         input.AuthID,
		Model:          input.Model,
		Before:         before,
		After:          before,
		DroppedReasons: append([]ChannelRoutingDroppedReason(nil), before.DroppedReasons...),
		TracerOnly:     true,
		ReconcileRuns:  0,
	}
	if input.AccountKey == "" && input.AuthID == "" {
		response.Status = routeResilienceActionStatusRejected
		response.Error = "accountKey or authId is required"
		c.JSON(http.StatusBadRequest, response)
		return
	}

	// Bounded reconcile is currently a sidecar tracer boundary: one target-scoped
	// re-evaluation sample, no external service calls, and no retry loop.
	after := routeResilienceActionEvidence(store, target, input.Model)
	response.After = after
	response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), after.DroppedReasons...)
	response.ReconcileRuns = 1
	if input.DryRun {
		response.OK = true
		response.Status = routeResilienceActionStatusDryRun
		response = routeResilienceActionRecordHistory(input, response)
		c.JSON(http.StatusOK, response)
		return
	}

	response.OK = true
	response.Status = routeResilienceActionStatusApplied
	response.AuditID = randomID("route-audit")
	response = routeResilienceActionRecordHistory(input, response)
	c.JSON(http.StatusOK, response)
}

func handleRouteResilienceClearTransientLockout(c *gin.Context, store *AccountRouteGuardStore, input RouteResilienceActionRequest) {
	target := routeResilienceActionTargetAuth(input)
	before := routeResilienceActionEvidence(store, target, input.Model)
	response := RouteResilienceActionResponse{
		OK:         false,
		Authority:  "sidecar",
		Action:     input.Action,
		AccountKey: input.AccountKey,
		AuthID:     input.AuthID,
		Model:      input.Model,
		Before:     before,
		After:      before,
	}
	if input.AccountKey == "" && input.AuthID == "" {
		response.Status = routeResilienceActionStatusRejected
		response.Error = "accountKey or authId is required"
		response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), before.DroppedReasons...)
		c.JSON(http.StatusBadRequest, response)
		return
	}
	if input.Reason == "" {
		response.Status = routeResilienceActionStatusRejected
		response.Error = "reason is required"
		response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), before.DroppedReasons...)
		response = routeResilienceActionRecordHistory(input, response)
		c.JSON(http.StatusBadRequest, response)
		return
	}
	sources, rejected := routeResilienceActionRequestedClearSources(input.Sources)
	if len(rejected) > 0 {
		response.Status = routeResilienceActionStatusRejected
		response.Error = "clear_transient_lockout can only clear auth-error, upstream-rate-limit, and upstream-error"
		response.DroppedSources = rejected
		response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), before.DroppedReasons...)
		response = routeResilienceActionRecordHistory(input, response)
		c.JSON(http.StatusBadRequest, response)
		return
	}
	afterBlocks := routeResilienceActionPreviewAfterClear(before.Blocks, sources)
	after := routeResilienceActionEvidenceFromBlocks(input, afterBlocks)
	response.After = after
	response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), after.DroppedReasons...)
	if input.DryRun {
		response.OK = true
		response.Status = routeResilienceActionStatusDryRun
		response = routeResilienceActionRecordHistory(input, response)
		c.JSON(http.StatusOK, response)
		return
	}
	store = routeResilienceActionStore(store)
	for _, source := range sources {
		for _, key := range routeResilienceActionTargetClearKeys(input) {
			store.ClearAuth(source, key)
		}
	}
	after = routeResilienceActionEvidence(store, target, input.Model)
	response.OK = true
	response.Status = routeResilienceActionStatusApplied
	response.After = after
	response.AuditID = randomID("route-audit")
	response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), after.DroppedReasons...)
	response = routeResilienceActionRecordHistory(input, response)
	c.JSON(http.StatusOK, response)
}

func handleRouteResilienceActionHistory(c *gin.Context) {
	filter := RouteResilienceActionHistoryFilter{
		Action:     strings.TrimSpace(c.Query("action")),
		Status:     strings.TrimSpace(c.Query("status")),
		Target:     strings.TrimSpace(c.Query("target")),
		AccountKey: strings.TrimSpace(c.Query("accountKey")),
		AuthID:     strings.TrimSpace(c.Query("authId")),
		Model:      strings.TrimSpace(c.Query("model")),
		Limit:      100,
	}
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"ok": false, "authority": "sidecar", "error": "limit must be a non-negative integer"})
			return
		}
		filter.Limit = limit
	}
	c.JSON(http.StatusOK, RouteResilienceActionHistoryResponse{
		OK:        true,
		Authority: "sidecar",
		Items:     ListRouteResilienceActionHistory(filter),
	})
}

func normalizeRouteResilienceActionRequest(input RouteResilienceActionRequest) RouteResilienceActionRequest {
	input.Action = strings.TrimSpace(input.Action)
	input.AccountKey = strings.TrimSpace(input.AccountKey)
	input.AuthID = strings.TrimSpace(input.AuthID)
	input.Model = strings.TrimSpace(input.Model)
	input.Reason = strings.TrimSpace(input.Reason)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	normalizedSources := make([]string, 0, len(input.Sources))
	for _, source := range input.Sources {
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		normalizedSources = append(normalizedSources, source)
	}
	input.Sources = normalizedSources
	return input
}

func routeResilienceActionRequestedClearSources(requested []string) ([]string, []string) {
	if len(requested) == 0 {
		sources := make([]string, 0, len(routeResilienceClearableTransientSources))
		for source := range routeResilienceClearableTransientSources {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		return sources, nil
	}
	sources := []string{}
	rejected := []string{}
	seen := map[string]struct{}{}
	for _, source := range requested {
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		if _, exists := seen[source]; exists {
			continue
		}
		seen[source] = struct{}{}
		if _, ok := routeResilienceClearableTransientSources[source]; !ok {
			rejected = append(rejected, source)
			continue
		}
		sources = append(sources, source)
	}
	sort.Strings(sources)
	sort.Strings(rejected)
	return sources, rejected
}

func routeResilienceActionTargetAuth(input RouteResilienceActionRequest) *coreauth.Auth {
	id := firstNonEmptyRouteGuardString(input.AuthID, input.AccountKey)
	return &coreauth.Auth{
		ID:         id,
		AccountKey: input.AccountKey,
		Provider:   "codex",
	}
}

func routeResilienceActionTargetClearKeys(input RouteResilienceActionRequest) []string {
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
	add(input.AuthID)
	add(input.AccountKey)
	return keys
}

func routeResilienceActionEvidence(store *AccountRouteGuardStore, auth *coreauth.Auth, model string) RouteResilienceActionEvidence {
	store = routeResilienceActionStore(store)
	blocks := store.ActiveBlocksForAuth(auth)
	persisted := activePersistedChannelRuntimeBlocksForCandidates([]*coreauth.Auth{auth})
	if len(persisted) > 0 {
		for _, extra := range persisted {
			blocks = append(blocks, extra...)
		}
	}
	model = strings.TrimSpace(model)
	if model != "" {
		blocksByID := filterAccountRouteGuardBlocksForRequestedModel(map[string][]AccountRouteGuardBlock{"target": blocks}, model)
		blocks = blocksByID["target"]
	}
	blocks = routeResilienceActionDedupeBlocks(blocks)
	input := RouteResilienceActionRequest{AccountKey: auth.AccountKey, AuthID: auth.ID, Model: model}
	return routeResilienceActionEvidenceFromBlocks(input, routeResilienceStatesFromBlocks(blocks))
}

func routeResilienceActionEvidenceFromBlocks(input RouteResilienceActionRequest, blocks []RouteResilienceState) RouteResilienceActionEvidence {
	dropped := make([]ChannelRoutingDroppedReason, 0, len(blocks))
	accountID := strings.TrimSpace(input.AccountKey)
	if accountID == "" && strings.HasPrefix(input.AuthID, "acct_") {
		accountID = strings.TrimSpace(input.AuthID)
	}
	for _, block := range blocks {
		if reason, ok := channelRoutingDroppedReasonFromRouteResilienceState(accountID, block); ok {
			dropped = append(dropped, reason)
		}
	}
	return RouteResilienceActionEvidence{
		BlockCount:     len(blocks),
		Blocks:         append([]RouteResilienceState(nil), blocks...),
		DroppedReasons: dropped,
	}
}

func routeResilienceActionPreviewAfterClear(blocks []RouteResilienceState, clearSources []string) []RouteResilienceState {
	clearSet := map[string]struct{}{}
	for _, source := range clearSources {
		clearSet[source] = struct{}{}
	}
	after := make([]RouteResilienceState, 0, len(blocks))
	for _, block := range blocks {
		if _, clear := clearSet[strings.TrimSpace(block.Source)]; clear {
			continue
		}
		after = append(after, block)
	}
	return after
}

func routeResilienceActionDedupeBlocks(blocks []AccountRouteGuardBlock) []AccountRouteGuardBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]AccountRouteGuardBlock, 0, len(blocks))
	seen := map[string]struct{}{}
	for _, block := range blocks {
		block = normalizeAccountRouteGuardBlock(block)
		key := strings.Join([]string{
			strings.TrimSpace(block.Source),
			string(normalizeRouteResilienceScope(block.FailureScope, block.Source)),
			accountRouteGuardBlockKey(block),
			strings.TrimSpace(block.Model),
		}, "\x00")
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, block)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return accountRouteGuardBlockKey(out[i]) < accountRouteGuardBlockKey(out[j])
	})
	return out
}

func routeResilienceActionStore(store *AccountRouteGuardStore) *AccountRouteGuardStore {
	if store != nil {
		return store
	}
	return defaultAccountRouteGuardStore
}

func routeResilienceActionRecordHistory(input RouteResilienceActionRequest, response RouteResilienceActionResponse) RouteResilienceActionResponse {
	if defaultRouteResilienceActionHistoryStore == nil {
		return response
	}
	item := RouteResilienceActionHistoryItem{
		Action:           response.Action,
		Target:           routeResilienceActionHistoryTarget(input),
		Status:           response.Status,
		AuditID:          response.AuditID,
		AccountKey:       response.AccountKey,
		AuthID:           response.AuthID,
		Model:            response.Model,
		BeforeBlockCount: response.Before.BlockCount,
		AfterBlockCount:  response.After.BlockCount,
		CreatedAt:        time.Now().UTC(),
		TracerOnly:       response.TracerOnly,
		ReconcileRuns:    response.ReconcileRuns,
	}
	if item.Action == "" || item.Target == "" || item.Status == "" {
		return response
	}
	if err := defaultRouteResilienceActionHistoryStore.record(item); err != nil {
		response.LedgerError = err.Error()
	}
	return response
}

func ListRouteResilienceActionHistory(filter RouteResilienceActionHistoryFilter) []RouteResilienceActionHistoryItem {
	if defaultRouteResilienceActionHistoryStore == nil {
		return nil
	}
	filter = normalizeRouteResilienceActionHistoryFilter(filter)
	return defaultRouteResilienceActionHistoryStore.list(filter)
}

func (s *routeResilienceActionHistoryStore) record(item RouteResilienceActionHistoryItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit <= 0 {
		s.limit = 200
	}
	if s.maxEntries <= 0 {
		s.maxEntries = 200
	}
	var ledgerErr error
	if s.ledgerPath != "" {
		appendLedger := s.appendLedger
		if appendLedger == nil {
			appendLedger = appendRouteResilienceActionLedgerItem
		}
		truncateLedger := s.truncateLedger
		if truncateLedger == nil {
			truncateLedger = truncateRouteResilienceActionLedger
		}
		if err := appendLedger(s.ledgerPath, item); err != nil {
			ledgerErr = fmt.Errorf("append route resilience action ledger: %w", err)
		} else if err := truncateLedger(s.ledgerPath, s.maxEntries); err != nil {
			ledgerErr = fmt.Errorf("truncate route resilience action ledger: %w", err)
		}
	}
	s.items = append([]RouteResilienceActionHistoryItem{item}, s.items...)
	memoryLimit := s.limit
	if s.maxEntries > 0 && s.maxEntries < memoryLimit {
		memoryLimit = s.maxEntries
	}
	if len(s.items) > memoryLimit {
		s.items = s.items[:memoryLimit]
	}
	return ledgerErr
}

func (s *routeResilienceActionHistoryStore) list(filter RouteResilienceActionHistoryFilter) []RouteResilienceActionHistoryItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	if filter.Limit == 0 {
		return []RouteResilienceActionHistoryItem{}
	}
	if s.ledgerPath != "" {
		if items, err := readRouteResilienceActionLedger(s.ledgerPath, filter); err == nil {
			return items
		}
	}
	out := make([]RouteResilienceActionHistoryItem, 0, len(s.items))
	for _, item := range s.items {
		if !routeResilienceActionHistoryItemMatchesFilter(item, filter) {
			continue
		}
		out = append(out, item)
		if len(out) >= filter.Limit {
			break
		}
	}
	return out
}

func (s *routeResilienceActionHistoryStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = nil
}

func (s *routeResilienceActionHistoryStore) setLedgerPath(path string) func() {
	s.mu.Lock()
	previousPath := s.ledgerPath
	s.ledgerPath = strings.TrimSpace(path)
	s.items = nil
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.ledgerPath = previousPath
		s.items = nil
		s.mu.Unlock()
	}
}

func (s *routeResilienceActionHistoryStore) setMaxEntries(maxEntries int) func() {
	s.mu.Lock()
	previousMaxEntries := s.maxEntries
	s.maxEntries = maxEntries
	if s.maxEntries <= 0 {
		s.maxEntries = 200
	}
	if len(s.items) > s.maxEntries {
		s.items = s.items[:s.maxEntries]
	}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.maxEntries = previousMaxEntries
		s.items = nil
		s.mu.Unlock()
	}
}

func (s *routeResilienceActionHistoryStore) setAppendLedgerFunc(appendLedger func(string, RouteResilienceActionHistoryItem) error) func() {
	s.mu.Lock()
	previousAppendLedger := s.appendLedger
	s.appendLedger = appendLedger
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.appendLedger = previousAppendLedger
		s.items = nil
		s.mu.Unlock()
	}
}

func (s *routeResilienceActionHistoryStore) setTruncateLedgerFunc(truncateLedger func(string, int) error) func() {
	s.mu.Lock()
	previousTruncateLedger := s.truncateLedger
	s.truncateLedger = truncateLedger
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.truncateLedger = previousTruncateLedger
		s.items = nil
		s.mu.Unlock()
	}
}

func normalizeRouteResilienceActionHistoryFilter(filter RouteResilienceActionHistoryFilter) RouteResilienceActionHistoryFilter {
	filter.Action = strings.TrimSpace(filter.Action)
	filter.Status = strings.TrimSpace(filter.Status)
	filter.Target = strings.TrimSpace(filter.Target)
	filter.AccountKey = strings.TrimSpace(filter.AccountKey)
	filter.AuthID = strings.TrimSpace(filter.AuthID)
	filter.Model = strings.TrimSpace(filter.Model)
	if filter.Limit < 0 {
		filter.Limit = 100
	}
	return filter
}

func routeResilienceActionHistoryTarget(input RouteResilienceActionRequest) string {
	parts := []string{}
	if input.AccountKey != "" {
		parts = append(parts, "accountKey="+input.AccountKey)
	}
	if input.AuthID != "" {
		parts = append(parts, "authId="+input.AuthID)
	}
	if input.Model != "" {
		parts = append(parts, "model="+input.Model)
	}
	return strings.Join(parts, "|")
}

func SetRouteResilienceActionLedgerPathFromConfig(configPath string) error {
	path, err := routeResilienceActionLedgerPathFromConfig(configPath)
	if err != nil {
		return err
	}
	if defaultRouteResilienceActionHistoryStore == nil {
		return nil
	}
	defaultRouteResilienceActionHistoryStore.setLedgerPath(path)
	return nil
}

func defaultRouteResilienceActionLedgerPath() string {
	path, err := routeResilienceActionLedgerPathFromConfig("")
	if err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "gettokens", "route-resilience-actions.jsonl")
}

func routeResilienceActionLedgerPathFromConfig(configPath string) (string, error) {
	if dir := strings.TrimSpace(configPath); dir != "" {
		if strings.EqualFold(filepath.Base(dir), "config.yaml") || strings.EqualFold(filepath.Base(dir), "config.yml") {
			dir = filepath.Dir(dir)
		}
		return filepath.Join(dir, "route-resilience", "actions.jsonl"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	if strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("resolve route resilience action ledger path: empty cache/temp dir")
	}
	return filepath.Join(base, "gettokens", "route-resilience-actions.jsonl"), nil
}

func appendRouteResilienceActionLedgerItem(path string, item RouteResilienceActionHistoryItem) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	return encoder.Encode(item)
}

func truncateRouteResilienceActionLedger(path string, maxEntries int) error {
	path = strings.TrimSpace(path)
	if path == "" || maxEntries <= 0 {
		return nil
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := make([]string, 0, maxEntries)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) > maxEntries {
			copy(lines, lines[len(lines)-maxEntries:])
			lines = lines[:maxEntries]
		}
	}
	closeErr := file.Close()
	if err := scanner.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func readRouteResilienceActionLedger(path string, filter RouteResilienceActionHistoryFilter) ([]RouteResilienceActionHistoryItem, error) {
	filter = normalizeRouteResilienceActionHistoryFilter(filter)
	if filter.Limit == 0 {
		return []RouteResilienceActionHistoryItem{}, nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	items := []RouteResilienceActionHistoryItem{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item RouteResilienceActionHistoryItem
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		if !routeResilienceActionHistoryItemMatchesFilter(item, filter) {
			continue
		}
		items = append(items, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit < 0 {
		limit = 100
	}
	out := make([]RouteResilienceActionHistoryItem, 0, len(items))
	for index := len(items) - 1; index >= 0; index-- {
		out = append(out, items[index])
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func routeResilienceActionHistoryItemMatchesFilter(item RouteResilienceActionHistoryItem, filter RouteResilienceActionHistoryFilter) bool {
	if filter.Action != "" && item.Action != filter.Action {
		return false
	}
	if filter.Status != "" && item.Status != filter.Status {
		return false
	}
	if filter.Target != "" && item.Target != filter.Target {
		return false
	}
	if filter.AccountKey != "" && item.AccountKey != filter.AccountKey {
		return false
	}
	if filter.AuthID != "" && item.AuthID != filter.AuthID {
		return false
	}
	if filter.Model != "" && item.Model != filter.Model {
		return false
	}
	return true
}
