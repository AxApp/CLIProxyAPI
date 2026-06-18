package gettokenshooks

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRouteResilienceActionClearsOnlyTransientGuardForTargetAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
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

	historyResponse := performRouteResilienceActionHistoryRequest(router, "?accountKey=acct_00000000-0000-4000-8000-000000000111&action=clear_transient_lockout")
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s, want 200", historyResponse.Code, historyResponse.Body.String())
	}
	var history RouteResilienceActionHistoryResponse
	decodeRouteResilienceActionHistoryResponse(t, historyResponse, &history)
	if !history.OK || history.Authority != "sidecar" || len(history.Items) != 1 {
		t.Fatalf("history = %#v, want one sidecar-owned clear item", history)
	}
	item := history.Items[0]
	if item.Action != RouteResilienceActionClearTransientLockout || item.Status != routeResilienceActionStatusApplied || item.AuditID != payload.AuditID {
		t.Fatalf("history item = %#v, want applied clear audit", item)
	}
	if item.BeforeBlockCount != 6 || item.AfterBlockCount != 3 || item.TracerOnly || item.ReconcileRuns != 0 || item.CreatedAt.IsZero() {
		t.Fatalf("history item = %#v, want clear block counts without tracer reconcile", item)
	}
}

func TestRouteResilienceActionRejectsManualDisabledAndQuotaSources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
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
	useRouteResilienceActionLedgerPathForTest(t)
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
	useRouteResilienceActionLedgerPathForTest(t)
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
	useRouteResilienceActionLedgerPathForTest(t)
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
	useRouteResilienceActionLedgerPathForTest(t)
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

	historyResponse := performRouteResilienceActionHistoryRequest(router, "?accountKey=acct_00000000-0000-4000-8000-000000000111&action=recheck_routeability")
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s, want 200", historyResponse.Code, historyResponse.Body.String())
	}
	var history RouteResilienceActionHistoryResponse
	decodeRouteResilienceActionHistoryResponse(t, historyResponse, &history)
	if !history.OK || history.Authority != "sidecar" || len(history.Items) != 1 {
		t.Fatalf("history = %#v, want one sidecar-owned recheck item", history)
	}
	item := history.Items[0]
	if item.Action != RouteResilienceActionRecheckRouteability || item.Target == "" || item.Status != routeResilienceActionStatusApplied {
		t.Fatalf("history item = %#v, want applied recheck with stable target", item)
	}
	if item.AuditID != payload.AuditID || item.BeforeBlockCount != 1 || item.AfterBlockCount != 1 {
		t.Fatalf("history item = %#v, want audit and block counts copied from response", item)
	}
	if !item.TracerOnly || item.ReconcileRuns != 0 || item.CreatedAt.IsZero() {
		t.Fatalf("history tracer fields = %#v, want tracerOnly, zero reconcile runs, createdAt", item)
	}
}

func TestRouteResilienceActionRecheckRouteabilityRejectsMissingPreciseTarget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
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

func TestRouteResilienceActionRerunBoundedReconcileIsBoundedTracerAndAudited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceUpstreamTransientErr,
		AuthID:     target.ID,
		AccountKey: target.AccountKey,
		Model:      "gpt-5",
		Reason:     "sample upstream failure",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})

	router := routeResilienceActionTestRouter(store)
	response := performRouteResilienceActionRequest(t, router, `{
		"action": "rerun_bounded_reconcile",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5",
		"reason": "operator requested bounded reconcile"
	}`)
	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Authority != "sidecar" || payload.Action != RouteResilienceActionRerunBoundedReconcile || payload.Status != routeResilienceActionStatusApplied {
		t.Fatalf("payload = %#v, want sidecar applied bounded reconcile tracer", payload)
	}
	if !payload.TracerOnly || payload.ReconcileRuns != 1 {
		t.Fatalf("tracerOnly/reconcileRuns = %v/%d, want one bounded sampling run", payload.TracerOnly, payload.ReconcileRuns)
	}
	if payload.AuditID == "" {
		t.Fatalf("auditId should be present for non-dry-run bounded reconcile: %#v", payload)
	}
	if payload.Before.BlockCount != 1 || payload.After.BlockCount != 1 || len(payload.DroppedReasons) != 1 {
		t.Fatalf("payload evidence = %#v, want sampled before/after without mutation", payload)
	}
	if blocks := store.ActiveBlocksForAuth(target); len(blocks) != 1 || blocks[0].Source != AccountRouteGuardSourceUpstreamTransientErr {
		t.Fatalf("store blocks after bounded tracer = %#v, want unchanged upstream block", blocks)
	}

	historyResponse := performRouteResilienceActionHistoryRequest(router, "?accountKey=acct_00000000-0000-4000-8000-000000000111&action=rerun_bounded_reconcile")
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s, want 200", historyResponse.Code, historyResponse.Body.String())
	}
	var history RouteResilienceActionHistoryResponse
	decodeRouteResilienceActionHistoryResponse(t, historyResponse, &history)
	if len(history.Items) != 1 {
		t.Fatalf("history = %#v, want one bounded reconcile item", history)
	}
	item := history.Items[0]
	if item.Action != RouteResilienceActionRerunBoundedReconcile || item.Status != routeResilienceActionStatusApplied || item.AuditID != payload.AuditID {
		t.Fatalf("history item = %#v, want applied bounded reconcile audit", item)
	}
	if !item.TracerOnly || item.ReconcileRuns != 1 || item.BeforeBlockCount != 1 || item.AfterBlockCount != 1 {
		t.Fatalf("history item = %#v, want bounded tracer counts", item)
	}
}

func TestRouteResilienceActionRerunBoundedReconcileDryRunDoesNotAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceQuotaEmpty,
		AuthID:     target.ID,
		AccountKey: target.AccountKey,
		Model:      "gpt-5",
		Reason:     "quota sample",
	})

	router := routeResilienceActionTestRouter(store)
	response := performRouteResilienceActionRequest(t, router, `{
		"action": "rerun_bounded_reconcile",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5",
		"dryRun": true
	}`)

	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != routeResilienceActionStatusDryRun || payload.AuditID != "" {
		t.Fatalf("payload = %#v, want dry_run without audit", payload)
	}
	if !payload.TracerOnly || payload.ReconcileRuns != 1 || payload.Before.BlockCount != 1 || payload.After.BlockCount != 1 {
		t.Fatalf("payload = %#v, want one bounded dry-run sampling pass", payload)
	}
}

func TestRouteResilienceActionHistoryReadsDurableJSONLLedger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ledgerPath := useRouteResilienceActionLedgerPathForTest(t)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)
	router := routeResilienceActionTestRouter(store)

	clearResponse := performRouteResilienceActionRequest(t, router, `{
		"action": "clear_transient_lockout",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"sources": ["auth-error"],
		"reason": "operator verified transient recovery"
	}`)
	var clearPayload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, clearResponse, &clearPayload)
	if clearPayload.AuditID == "" {
		t.Fatalf("clear auditId should be present: %#v", clearPayload)
	}

	recheckResponse := performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)
	var recheckPayload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, recheckResponse, &recheckPayload)
	if recheckPayload.AuditID == "" || !recheckPayload.TracerOnly || recheckPayload.ReconcileRuns != 0 {
		t.Fatalf("recheck payload = %#v, want audited tracer without reconcile", recheckPayload)
	}

	rerunResponse := performRouteResilienceActionRequest(t, router, `{
		"action": "rerun_bounded_reconcile",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5",
		"dryRun": true
	}`)
	var rerunPayload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, rerunResponse, &rerunPayload)
	if rerunPayload.AuditID != "" || !rerunPayload.TracerOnly || rerunPayload.ReconcileRuns != 1 {
		t.Fatalf("rerun dry-run payload = %#v, want bounded tracer without audit", rerunPayload)
	}

	rawLedger, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawLedger)), "\n")
	if len(lines) != 3 {
		t.Fatalf("ledger lines = %d content=%s, want three JSONL records", len(lines), string(rawLedger))
	}
	for _, line := range lines {
		var item RouteResilienceActionHistoryItem
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatalf("ledger line is not JSON: %v line=%q", err, line)
		}
		if item.Target == "" || item.CreatedAt.IsZero() {
			t.Fatalf("ledger item = %#v, want stable target and createdAt", item)
		}
	}

	resetRouteResilienceActionHistoryForTest()
	historyResponse := performRouteResilienceActionHistoryRequest(router, "?accountKey=acct_00000000-0000-4000-8000-000000000111&limit=10")
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s, want 200", historyResponse.Code, historyResponse.Body.String())
	}
	var history RouteResilienceActionHistoryResponse
	decodeRouteResilienceActionHistoryResponse(t, historyResponse, &history)
	if !history.OK || history.Authority != "sidecar" || len(history.Items) != 3 {
		t.Fatalf("history = %#v, want three records read back from JSONL ledger", history)
	}
	if history.Items[0].Action != RouteResilienceActionRerunBoundedReconcile ||
		history.Items[1].Action != RouteResilienceActionRecheckRouteability ||
		history.Items[2].Action != RouteResilienceActionClearTransientLockout {
		t.Fatalf("history order = %#v, want newest-first JSONL replay", history.Items)
	}
	if history.Items[0].AuditID != "" || history.Items[0].Status != routeResilienceActionStatusDryRun || history.Items[0].ReconcileRuns != 1 {
		t.Fatalf("rerun history = %#v, want dry-run bounded tracer", history.Items[0])
	}
	if history.Items[1].AuditID != recheckPayload.AuditID || !history.Items[1].TracerOnly || history.Items[1].ReconcileRuns != 0 {
		t.Fatalf("recheck history = %#v, want audited tracer without reconcile", history.Items[1])
	}
	if history.Items[2].AuditID != clearPayload.AuditID || history.Items[2].BeforeBlockCount != 6 || history.Items[2].AfterBlockCount != 5 {
		t.Fatalf("clear history = %#v, want clear audit and block counts", history.Items[2])
	}
}

func TestRouteResilienceActionHistoryLedgerTruncatesToMaxEntries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ledgerPath := useRouteResilienceActionLedgerPathForTest(t)
	setRouteResilienceActionLedgerMaxEntriesForTest(t, 2)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)
	router := routeResilienceActionTestRouter(store)

	performRouteResilienceActionRequest(t, router, `{
		"action": "clear_transient_lockout",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"sources": ["auth-error"],
		"reason": "operator verified transient recovery"
	}`)
	performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)
	performRouteResilienceActionRequest(t, router, `{
		"action": "rerun_bounded_reconcile",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5",
		"dryRun": true
	}`)

	rawLedger, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(rawLedger)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ledger lines = %d content=%s, want max entries truncation to two latest records", len(lines), string(rawLedger))
	}

	resetRouteResilienceActionHistoryForTest()
	historyResponse := performRouteResilienceActionHistoryRequest(router, "?limit=10")
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s, want 200", historyResponse.Code, historyResponse.Body.String())
	}
	var history RouteResilienceActionHistoryResponse
	decodeRouteResilienceActionHistoryResponse(t, historyResponse, &history)
	if len(history.Items) != 2 {
		t.Fatalf("history = %#v, want two max-bounded records", history)
	}
	if history.Items[0].Action != RouteResilienceActionRerunBoundedReconcile ||
		history.Items[1].Action != RouteResilienceActionRecheckRouteability {
		t.Fatalf("history order = %#v, want newest two records after truncation", history.Items)
	}
}

func TestRouteResilienceActionHistoryEndpointFiltersStatusTargetAndLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	other := routeResilienceActionTestAuth("auth-other", "acct_00000000-0000-4000-8000-000000000222")
	seedRouteResilienceActionBlocks(store, target)
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceUpstreamTransientErr,
		AuthID:     other.ID,
		AccountKey: other.AccountKey,
		Model:      "gpt-4.1",
		Reason:     "other target upstream failure",
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	})
	router := routeResilienceActionTestRouter(store)

	performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)
	performRouteResilienceActionRequest(t, router, `{
		"action": "rerun_bounded_reconcile",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5",
		"dryRun": true
	}`)
	performRouteResilienceActionRequest(t, router, `{
		"action": "rerun_bounded_reconcile",
		"accountKey": "acct_00000000-0000-4000-8000-000000000222",
		"authId": "auth-other",
		"model": "gpt-4.1"
	}`)

	stableTarget := "accountKey=acct_00000000-0000-4000-8000-000000000111|authId=auth-target|model=gpt-5"
	query := "?action=rerun_bounded_reconcile&status=dry_run&accountKey=acct_00000000-0000-4000-8000-000000000111&authId=auth-target&model=gpt-5&target=" + url.QueryEscape(stableTarget) + "&limit=5"
	historyResponse := performRouteResilienceActionHistoryRequest(router, query)
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s, want 200", historyResponse.Code, historyResponse.Body.String())
	}
	var history RouteResilienceActionHistoryResponse
	decodeRouteResilienceActionHistoryResponse(t, historyResponse, &history)
	if len(history.Items) != 1 {
		t.Fatalf("history = %#v, want one status/target-filtered item", history)
	}
	item := history.Items[0]
	if item.Action != RouteResilienceActionRerunBoundedReconcile || item.Status != routeResilienceActionStatusDryRun || item.Target != stableTarget {
		t.Fatalf("history item = %#v, want dry-run bounded reconcile for stable target", item)
	}

	emptyResponse := performRouteResilienceActionHistoryRequest(router, "?limit=0")
	if emptyResponse.Code != http.StatusOK {
		t.Fatalf("limit=0 status = %d body=%s, want 200", emptyResponse.Code, emptyResponse.Body.String())
	}
	var emptyHistory RouteResilienceActionHistoryResponse
	decodeRouteResilienceActionHistoryResponse(t, emptyResponse, &emptyHistory)
	if len(emptyHistory.Items) != 0 {
		t.Fatalf("limit=0 history = %#v, want explicit empty result", emptyHistory)
	}
}

func TestRouteResilienceActionDefaultLedgerPathAvoidsFormalConfigDir(t *testing.T) {
	path := filepath.ToSlash(defaultRouteResilienceActionLedgerPath())
	if strings.Contains(path, "/.config/gettokens") {
		t.Fatalf("default ledger path = %q, should not write the formal GetTokens config dir", path)
	}
	if !strings.HasSuffix(path, "/gettokens/route-resilience-actions.jsonl") {
		t.Fatalf("default ledger path = %q, want gettokens route action JSONL file", path)
	}
}

func TestRouteResilienceActionLedgerPathDerivesFromProfileConfigPath(t *testing.T) {
	profileDir := filepath.Join(t.TempDir(), "profiles", "dev")
	path, err := routeResilienceActionLedgerPathFromConfig(filepath.Join(profileDir, "config.yaml"))
	if err != nil {
		t.Fatalf("routeResilienceActionLedgerPathFromConfig: %v", err)
	}
	want := filepath.Join(profileDir, "route-resilience", "actions.jsonl")
	if path != want {
		t.Fatalf("ledger path = %q, want profile-aware %q", path, want)
	}

	dirPath, err := routeResilienceActionLedgerPathFromConfig(profileDir)
	if err != nil {
		t.Fatalf("routeResilienceActionLedgerPathFromConfig with dir: %v", err)
	}
	if dirPath != want {
		t.Fatalf("ledger path from profile dir = %q, want %q", dirPath, want)
	}

	restore := defaultRouteResilienceActionHistoryStore.setLedgerPath(filepath.Join(t.TempDir(), "previous.jsonl"))
	t.Cleanup(restore)
	if err := SetRouteResilienceActionLedgerPathFromConfig(filepath.Join(profileDir, "config.yaml")); err != nil {
		t.Fatalf("SetRouteResilienceActionLedgerPathFromConfig: %v", err)
	}
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)
	router := routeResilienceActionTestRouter(store)
	performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("profile-aware ledger path %q was not written: %v", want, err)
	}
}

func TestRouteResilienceActionStartupConfigPathWiresLedgerPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	profileDir := filepath.Join(t.TempDir(), "profiles", "dev")
	configPath := filepath.Join(profileDir, "config.yaml")
	want := filepath.Join(profileDir, "route-resilience", "actions.jsonl")
	restore := defaultRouteResilienceActionHistoryStore.setLedgerPath(filepath.Join(t.TempDir(), "previous.jsonl"))
	t.Cleanup(restore)

	InstallRoutingPoliciesWithConfigPath(configPath)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)
	router := routeResilienceActionTestRouter(store)
	performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)

	if _, err := os.Stat(want); err != nil {
		t.Fatalf("startup config ledger path %q was not written: %v", want, err)
	}
}

func TestRouteResilienceActionTestLedgerOverrideSurvivesStartupConfigPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	profileDir := filepath.Join(t.TempDir(), "profiles", "dev")
	profileLedger := filepath.Join(profileDir, "route-resilience", "actions.jsonl")

	InstallRoutingPoliciesWithConfigPath(filepath.Join(profileDir, "config.yaml"))
	overrideLedger := useRouteResilienceActionLedgerPathForTest(t)
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)
	router := routeResilienceActionTestRouter(store)
	performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)

	if _, err := os.Stat(overrideLedger); err != nil {
		t.Fatalf("test override ledger path %q was not written: %v", overrideLedger, err)
	}
	if _, err := os.Stat(profileLedger); !os.IsNotExist(err) {
		t.Fatalf("profile ledger path %q should not be written after test override, stat err=%v", profileLedger, err)
	}
}

func TestRouteResilienceActionResponseSurfacesLedgerAppendFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
	setRouteResilienceActionLedgerAppendFuncForTest(t, func(path string, item RouteResilienceActionHistoryItem) error {
		return errors.New("disk append denied")
	})
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)
	router := routeResilienceActionTestRouter(store)

	response := performRouteResilienceActionRequest(t, router, `{
		"action": "clear_transient_lockout",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"sources": ["auth-error"],
		"reason": "operator verified transient recovery"
	}`)

	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != routeResilienceActionStatusApplied || !payload.OK || payload.AuditID == "" {
		t.Fatalf("payload = %#v, want action still applied despite ledger append failure", payload)
	}
	if !strings.Contains(payload.LedgerError, "append route resilience action ledger") ||
		!strings.Contains(payload.LedgerError, "disk append denied") {
		t.Fatalf("ledgerError = %q, want append failure surfaced", payload.LedgerError)
	}
}

func TestRouteResilienceActionResponseSurfacesLedgerTruncateFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
	setRouteResilienceActionLedgerMaxEntriesForTest(t, 1)
	setRouteResilienceActionLedgerTruncateFuncForTest(t, func(path string, maxEntries int) error {
		return errors.New("truncate denied")
	})
	store := NewAccountRouteGuardStore()
	target := routeResilienceActionTestAuth("auth-target", "acct_00000000-0000-4000-8000-000000000111")
	seedRouteResilienceActionBlocks(store, target)
	router := routeResilienceActionTestRouter(store)

	response := performRouteResilienceActionRequest(t, router, `{
		"action": "recheck_routeability",
		"accountKey": "acct_00000000-0000-4000-8000-000000000111",
		"authId": "auth-target",
		"model": "gpt-5"
	}`)

	var payload RouteResilienceActionResponse
	decodeRouteResilienceActionResponse(t, response, &payload)
	if payload.Status != routeResilienceActionStatusApplied || !payload.OK || payload.AuditID == "" {
		t.Fatalf("payload = %#v, want action still applied despite ledger truncate failure", payload)
	}
	if !strings.Contains(payload.LedgerError, "truncate route resilience action ledger") ||
		!strings.Contains(payload.LedgerError, "truncate denied") {
		t.Fatalf("ledgerError = %q, want truncate failure surfaced", payload.LedgerError)
	}
}

func TestRouteResilienceActionRegisteredOnGetTokensManagementRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useRouteResilienceActionLedgerPathForTest(t)
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

func performRouteResilienceActionHistoryRequest(router http.Handler, query string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/route-resilience/actions/history"+query, nil)
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

func decodeRouteResilienceActionHistoryResponse(t *testing.T, response *httptest.ResponseRecorder, out *RouteResilienceActionHistoryResponse) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
		t.Fatalf("json.Unmarshal history: %v body=%s", err, response.Body.String())
	}
}

func resetRouteResilienceActionHistoryForTest() {
	defaultRouteResilienceActionHistoryStore.reset()
}

func useRouteResilienceActionLedgerPathForTest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "route-actions.jsonl")
	setRouteResilienceActionLedgerPathForTest(t, path)
	return path
}

func setRouteResilienceActionLedgerPathForTest(t *testing.T, path string) {
	t.Helper()
	restore := defaultRouteResilienceActionHistoryStore.setLedgerPath(path)
	t.Cleanup(restore)
}

func setRouteResilienceActionLedgerMaxEntriesForTest(t *testing.T, maxEntries int) {
	t.Helper()
	restore := defaultRouteResilienceActionHistoryStore.setMaxEntries(maxEntries)
	t.Cleanup(restore)
}

func setRouteResilienceActionLedgerAppendFuncForTest(t *testing.T, appendFunc func(string, RouteResilienceActionHistoryItem) error) {
	t.Helper()
	restore := defaultRouteResilienceActionHistoryStore.setAppendLedgerFunc(appendFunc)
	t.Cleanup(restore)
}

func setRouteResilienceActionLedgerTruncateFuncForTest(t *testing.T, truncateFunc func(string, int) error) {
	t.Helper()
	restore := defaultRouteResilienceActionHistoryStore.setTruncateLedgerFunc(truncateFunc)
	t.Cleanup(restore)
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
