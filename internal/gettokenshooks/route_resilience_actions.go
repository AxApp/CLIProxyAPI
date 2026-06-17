package gettokenshooks

import (
	"net/http"
	"sort"
	"strings"

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
	Error                string                        `json:"error,omitempty"`
	NotImplementedReason string                        `json:"notImplementedReason,omitempty"`
}

type RouteResilienceActionEvidence struct {
	BlockCount     int                           `json:"blockCount"`
	Blocks         []RouteResilienceState        `json:"blocks,omitempty"`
	DroppedReasons []ChannelRoutingDroppedReason `json:"droppedReasons,omitempty"`
}

func ConfigureRouteResilienceActionRoutes(group *gin.RouterGroup, store *AccountRouteGuardStore) {
	if group == nil {
		return
	}
	group.POST("/gettokens/route-resilience/actions", func(c *gin.Context) {
		handleRouteResilienceAction(c, store)
	})
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
		target := routeResilienceActionTargetAuth(input)
		before := routeResilienceActionEvidence(store, target, input.Model)
		c.JSON(http.StatusNotImplemented, RouteResilienceActionResponse{
			OK:                   false,
			Authority:            "sidecar",
			Action:               input.Action,
			Status:               routeResilienceActionStatusNotImplemented,
			AccountKey:           input.AccountKey,
			AuthID:               input.AuthID,
			Model:                input.Model,
			Before:               before,
			After:                before,
			DroppedReasons:       append([]ChannelRoutingDroppedReason(nil), before.DroppedReasons...),
			NotImplementedReason: "current gettokenshooks management layer does not own bounded reconcile or routeability service permissions",
		})
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
		c.JSON(http.StatusOK, response)
		return
	}
	after := routeResilienceActionEvidence(store, target, input.Model)
	response.OK = true
	response.Status = routeResilienceActionStatusApplied
	response.After = after
	response.AuditID = randomID("route-audit")
	response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), after.DroppedReasons...)
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
		c.JSON(http.StatusBadRequest, response)
		return
	}
	sources, rejected := routeResilienceActionRequestedClearSources(input.Sources)
	if len(rejected) > 0 {
		response.Status = routeResilienceActionStatusRejected
		response.Error = "clear_transient_lockout can only clear auth-error, upstream-rate-limit, and upstream-error"
		response.DroppedSources = rejected
		response.DroppedReasons = append([]ChannelRoutingDroppedReason(nil), before.DroppedReasons...)
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
	c.JSON(http.StatusOK, response)
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
