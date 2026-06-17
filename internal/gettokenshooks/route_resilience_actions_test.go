package gettokenshooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRouteResilienceActionClearsOnlyTransientGuardForTargetAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	other := routeResilienceActionTestAuth("auth-other", "acct_00000000-0000-4000-8000-000000000222")
	seedRouteResilienceActionBlocks(store, target)
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceUpstreamTransientErr,
		AuthID:     other.ID,
		AccountKey: other.AccountKey,
		Reason:     "other upstream error",
	})

	router := routeResilienceActionTestRouter(store)
	response := performRouteResilienceActionRequest(t, router, `{
		"action": "clear_transient_lockout",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"sources": ["auth-error", "upstream-rate-limit", "upstream-error"],
		"reason": "operator verified recovery"
	}`)

	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Authority != "sidecar" || payload.Action != "clear_transient_lockout" || payload.Status != "applied" {
		t.Fatalf("payload status = %#v, want sidecar applied clear action", payload)
	}
	if payload.Before.BlockCount != 6 || payload.After.BlockCount != 3 {
		t.Fatalf("before/after block count = %d/%d, want 6/3", payload.Before.BlockCount, payload.After.BlockCount)
	}
	if len(payload.DroppedReasons) != 3 {
		t.Fatalf("droppedReasons = %#v, want remaining manual/rate/quota evidence", payload.DroppedReasons)
	}
	remainingTarget := store.ActiveBlocksForAuth(target)
	if routeResilienceActionHasSource(remainingTarget, AccountRouteGuardSourceAuthError) ||
		routeResilienceActionHasSource(remainingTarget, AccountRouteGuardSourceUpstreamRateLimit) ||
		routeResilienceActionHasSource(remainingTarget, AccountRouteGuardSourceUpstreamTransientErr) {
		t.Fatalf("remaining target blocks = %#v, transient sources should be cleared", remainingTarget)
	}
	if !routeResilienceActionHasSource(remainingTarget, AccountRouteGuardSourceManualDisabled) ||
		!routeResilienceActionHasSource(remainingTarget, AccountRouteGuardSourceRateLimit) ||
		!routeResilienceActionHasSource(remainingTarget, AccountRouteGuardSourceQuotaEmpty) {
		t.Fatalf("remaining target blocks = %#v, manual/rate/quota should remain", remainingTarget)
	}
	if otherBlocks := store.ActiveBlocksForAuth(other); len(otherBlocks) != 1 || otherBlocks[0].Source != AccountRouteGuardSourceUpstreamTransientErr {
		t.Fatalf("other account blocks = %#v, want untouched upstream-error", otherBlocks)
	}
}

func TestRouteResilienceActionRejectsManualDisabledAndQuotaSources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)

	router := routeResilienceActionTestRouter(store)
	response := performRouteResilienceActionRawRequest(router, `{
		"action": "clear_transient_lockout",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"sources": ["manual-disabled", "quota-empty", "rate-limit"],
		"reason": "operator asked for forbidden clear"
	}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
	}
	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != "rejected" {
		t.Fatalf("status = %q, want rejected", payload.Status)
	}
	if len(payload.DroppedSources) != 3 {
		t.Fatalf("droppedSources = %#v, want forbidden source list", payload.DroppedSources)
	}
	if blocks := store.ActiveBlocksForAuth(target); len(blocks) != 6 {
		t.Fatalf("blocks after rejected clear = %#v, want unchanged six blocks", blocks)
	}
}

func TestRouteResilienceActionRejectsMissingPreciseTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := routeResilienceActionTestRouter(NewAccountRouteGuardStore())

	response := performRouteResilienceActionRawRequest(router, `{
		"action": "clear_transient_lockout",
		"sources": ["auth-error"],
		"reason": "missing target"
	}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
	}
	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Authority != "sidecar" || payload.Status != "rejected" {
		t.Fatalf("payload = %#v, want sidecar rejected", payload)
	}
}

func TestRouteResilienceActionDryRunPreviewsWithoutWritingStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)

	router := routeResilienceActionTestRouter(store)
	response := performRouteResilienceActionRequest(t, router, `{
		"action": "clear_transient_lockout",
		"authId": "auth-target",
		"sources": ["auth-error"],
		"reason": "preview only",
		"dryRun": true
	}`)

	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != "dry_run" {
		t.Fatalf("status = %q, want dry_run", payload.Status)
	}
	if payload.Before.BlockCount != 6 || payload.After.BlockCount != 5 {
		t.Fatalf("before/after block count = %d/%d, want 6/5 dry-run preview", payload.Before.BlockCount, payload.After.BlockCount)
	}
	if blocks := store.ActiveBlocksForAuth(target); len(blocks) != 6 || !routeResilienceActionHasSource(blocks, AccountRouteGuardSourceAuthError) {
		t.Fatalf("blocks after dry-run = %#v, want unchanged auth-error block", blocks)
	}
}

func TestRouteResilienceActionRecheckRouteabilityDryRunSamplesGuardAndPersistedEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := writeRouteGuardChannelRoutingConfig(t, `{
  "channels": {},
  "runtimeStates": {
    "acct_00000000-0000-4000-8000-000000000111": {
      "accountID": "acct_00000000-0000-4000-8000-000000000111",
      "updatedAt": "2026-06-17T09:00:00Z",
      "sources": {
        "auth-error": {
          "source": "auth-error",
          "scope": "account",
          "reason": "persisted token expired",
          "model": "gpt-5",
          "updatedAt": "2026-06-17T09:00:00Z"
        }
      }
    }
  }
}`)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:       AccountRouteGuardSourceQuotaEmpty,
		FailureScope: RouteResilienceScopeModel,
		AuthID:       target.ID,
		AccountKey:   target.AccountKey,
		Model:        "gpt-5",
		Reason:       "model quota empty",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:       AccountRouteGuardSourceUpstreamTransientErr,
		FailureScope: RouteResilienceScopeModel,
		AuthID:       target.ID,
		AccountKey:   target.AccountKey,
		Model:        "gpt-4.1",
		Reason:       "other model upstream error",
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	})
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	router := routeResilienceActionTestRouter(store)
	response := performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5",
		"dryRun": true
	}`)

	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Authority != "sidecar" || payload.Action != RouteResilienceActionRecheckRouteability || payload.Status != routeResilienceActionStatusDryRun {
		t.Fatalf("payload = %#v, want sidecar dry-run recheck", payload)
	}
	if payload.AuditID != "" {
		t.Fatalf("auditId = %q, want empty for dry-run", payload.AuditID)
	}
	if payload.Before.BlockCount != 2 || payload.After.BlockCount != 2 {
		t.Fatalf("before/after block count = %d/%d, want in-memory + persisted gpt-5 evidence only", payload.Before.BlockCount, payload.After.BlockCount)
	}
	if !routeResilienceActionHasDroppedSource(payload.DroppedReasons, AccountRouteGuardSourceQuotaEmpty) ||
		!routeResilienceActionHasDroppedSource(payload.DroppedReasons, AccountRouteGuardSourceAuthError) {
		t.Fatalf("droppedReasons = %#v, want quota-empty and persisted auth-error", payload.DroppedReasons)
	}
	if routeResilienceActionHasDroppedSource(payload.DroppedReasons, AccountRouteGuardSourceUpstreamTransientErr) {
		t.Fatalf("droppedReasons = %#v, should not include other-model upstream-error", payload.DroppedReasons)
	}
	if payload.ReconcileRuns != 0 || !payload.TracerOnly {
		t.Fatalf("reconcileRuns/tracerOnly = %d/%v, want no bounded reconcile", payload.ReconcileRuns, payload.TracerOnly)
	}
	if blocks := store.ActiveBlocksForAuth(target); len(blocks) != 2 {
		t.Fatalf("store blocks after dry-run = %#v, want unchanged two in-memory blocks", blocks)
	}
}

func TestRouteResilienceActionRecheckRouteabilityAppliedAuditsWithoutReconcile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AuthID:     target.ID,
		AccountKey: target.AccountKey,
		Reason:     "window saturated",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})

	router := routeResilienceActionTestRouter(store)
	response := performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)

	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != routeResilienceActionStatusApplied || payload.Authority != "sidecar" {
		t.Fatalf("payload = %#v, want sidecar applied recheck", payload)
	}
	if payload.AuditID == "" {
		t.Fatalf("auditId should be present for applied recheck: %#v", payload)
	}
	if payload.Before.BlockCount != 1 || payload.After.BlockCount != 1 || len(payload.DroppedReasons) != 1 {
		t.Fatalf("payload evidence = %#v, want same before/after dropped reason", payload)
	}
	if payload.ReconcileRuns != 0 || !payload.TracerOnly {
		t.Fatalf("reconcileRuns/tracerOnly = %d/%v, want tracer without reconcile", payload.ReconcileRuns, payload.TracerOnly)
	}
	if blocks := store.ActiveBlocksForAuth(target); len(blocks) != 1 || blocks[0].Source != AccountRouteGuardSourceRateLimit {
		t.Fatalf("store blocks after applied recheck = %#v, want unchanged rate-limit", blocks)
	}
}

func TestRouteResilienceActionRecheckRouteabilityRejectsMissingPreciseTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := routeResilienceActionTestRouter(NewAccountRouteGuardStore())

	response := performRouteResilienceActionRawRequest(router, `{
		"action": "recheck_routeability",
		"model": "gpt-5"
	}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
	}
	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != routeResilienceActionStatusRejected || payload.Error == "" {
		t.Fatalf("payload = %#v, want rejected missing target", payload)
	}
}

func TestRouteResilienceActionUnsupportedActionsArePlannedNotImplemented(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := routeResilienceActionTestRouter(NewAccountRouteGuardStore())

	response := performRouteResilienceActionRawRequest(router, `{
		"action": "rerun_bounded_reconcile",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"reason": "operator requested bounded reconcile"
	}`)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body=%s, want 501", response.Code, response.Body.String())
	}
	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != "not_implemented" || payload.Authority != "sidecar" {
		t.Fatalf("payload = %#v, want sidecar not_implemented", payload)
	}
}

func TestRouteResilienceActionRegisteredOnGetTokensManagementRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureGetTokensManagementRoutes(group, nil, nil)

	response := performRouteResilienceActionRawRequest(router, `{
		"action": "clear_transient_lockout",
		"sources": ["auth-error"],
		"reason": "registration smoke"
	}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want registered handler validation error instead of 404", response.Code, response.Body.String())
	}
	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Authority != "sidecar" || payload.Action != RouteResilienceActionClearTransientLockout {
		t.Fatalf("payload = %#v, want route resilience handler response", payload)
	}
}

func routeResilienceActionTestRouter(store *AccountRouteGuardStore) http.Handler {
	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureRouteResilienceActionRoutes(group, store)
	return router
}

func performRouteResilienceActionRequest(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := performRouteResilienceActionRawRequest(router, body)
	if response.Code < http.StatusOK || response.Code >= http.StatusMultipleChoices {
		t.Fatalf("POST route resilience action returned %d: %s", response.Code, response.Body.String())
	}
	return response
}

func performRouteResilienceActionRawRequest(router http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/route-resilience/actions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func decodeRouteResilienceActionResponse(t *testing.T, response *httptest.ResponseRecorder, out *RouteResilienceActionResponse) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
		t.Fatalf("json.Unmarshal: %v body=%s", err, response.Body.String())
	}
}

func routeResilienceActionTestAuth(authID string, accountKey string) *coreauth.Auth {
	return &coreauth.Auth{ID: authID, AccountKey: accountKey, Provider: "codex"}
}

func seedRouteResilienceActionBlocks(store *AccountRouteGuardStore, auth *coreauth.Auth) {
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "disabled by user",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "operator rate limit",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceQuotaEmpty,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "quota empty",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceAuthError,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "auth failed",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceUpstreamRateLimit,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "upstream 429",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceUpstreamTransientErr,
		AuthID:     auth.ID,
		AccountKey: auth.AccountKey,
		Reason:     "upstream 5xx",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})
}

func routeResilienceActionHasSource(blocks []AccountRouteGuardBlock, source string) bool {
	for _, block := range blocks {
		if block.Source == source {
			return true
		}
	}
	return false
}

func routeResilienceActionHasDroppedSource(reasons []ChannelRoutingDroppedReason, source string) bool {
	for _, reason := range reasons {
		if reason.Source == source {
			return true
		}
	}
	return false
}
