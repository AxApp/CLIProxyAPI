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

func TestQuotaRuntimeStoreUpsertFeedsQuotaEmptyGuard(t *testing.T) {
	guard := NewAccountRouteGuardStore()
	store := NewQuotaRuntimeStore(guard)
	now := time.Now().UTC()
	remaining := 0

	state, err := store.Upsert(QuotaRuntimeState{
		AccountKey: "acct_00000000-0000-4000-8000-000000000101",
		Source:     "quota-curl",
		Status:     QuotaRuntimeStatusSuccess,
		PlanType:   "pro",
		Windows: []QuotaRuntimeWindow{{
			ID:               "five-hour",
			Label:            "5H",
			RemainingPercent: &remaining,
			ResetAtUnix:      now.Add(time.Hour).Unix(),
		}},
	}, now)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !state.Blocked || len(state.Sources) != 1 || state.Sources[0].Source != AccountRouteGuardSourceQuotaEmpty {
		t.Fatalf("state = %#v, want quota-empty blocked source", state)
	}
	if got := guard.DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         "codex-auth-1",
		AccountKey: state.AccountKey,
		Provider:   "codex",
	}}); len(got) != 1 || got[0] != "codex-auth-1" {
		t.Fatalf("guard deny ids = %#v, want quota-empty deny", got)
	}
}

func TestQuotaRuntimeStoreRecoveryClearsOnlyQuotaEmpty(t *testing.T) {
	guard := NewAccountRouteGuardStore()
	store := NewQuotaRuntimeStore(guard)
	now := time.Now().UTC()
	accountKey := "acct_00000000-0000-4000-8000-000000000102"
	remainingEmpty := 0
	remainingHealthy := 42
	guard.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AccountKey: accountKey,
		Reason:     "disabled by user",
	})

	if _, err := store.Upsert(QuotaRuntimeState{
		AccountKey: accountKey,
		Status:     QuotaRuntimeStatusSuccess,
		Windows: []QuotaRuntimeWindow{{
			ID:               "five-hour",
			Label:            "5H",
			RemainingPercent: &remainingEmpty,
			ResetAtUnix:      now.Add(time.Hour).Unix(),
		}},
	}, now); err != nil {
		t.Fatalf("Upsert exhausted: %v", err)
	}
	state, err := store.Upsert(QuotaRuntimeState{
		AccountKey: accountKey,
		Status:     QuotaRuntimeStatusSuccess,
		Windows: []QuotaRuntimeWindow{{
			ID:               "five-hour",
			Label:            "5H",
			RemainingPercent: &remainingHealthy,
			ResetAtUnix:      now.Add(time.Hour).Unix(),
		}},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Upsert recovered: %v", err)
	}
	if !state.Blocked || len(state.Sources) != 1 || state.Sources[0].Source != AccountRouteGuardSourceManualDisabled {
		t.Fatalf("state = %#v, want manual-disabled only after quota recovery", state)
	}
}

func TestQuotaRuntimeStoreRecoveryClearsAuthScopedQuotaEmptyByAccountKey(t *testing.T) {
	guard := NewAccountRouteGuardStore()
	store := NewQuotaRuntimeStore(guard)
	now := time.Now().UTC()
	accountKey := "acct_00000000-0000-4000-8000-000000000105"
	authID := "codex-auth-quota"
	remainingHealthy := 42
	guard.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceQuotaEmpty,
		AuthID:     authID,
		AccountKey: accountKey,
		Reason:     "quota empty: upstream quota",
		ExpiresAt:  now.Add(time.Hour),
	})

	state, err := store.Upsert(QuotaRuntimeState{
		AccountKey: accountKey,
		Status:     QuotaRuntimeStatusSuccess,
		Windows: []QuotaRuntimeWindow{{
			ID:               "five-hour",
			Label:            "5H",
			RemainingPercent: &remainingHealthy,
			ResetAtUnix:      now.Add(time.Hour).Unix(),
		}},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Upsert recovered: %v", err)
	}
	if state.Blocked || len(state.Sources) != 0 {
		t.Fatalf("state = %#v, want account-key recovery to clear auth-scoped quota-empty", state)
	}
	if got := guard.DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         authID,
		AccountKey: accountKey,
		Provider:   "codex",
	}}); len(got) != 0 {
		t.Fatalf("guard deny ids = %#v, want recovered auth selectable", got)
	}
}

func TestQuotaRuntimeStoreStaleStateDoesNotClearFreshQuotaEmptyGuard(t *testing.T) {
	guard := NewAccountRouteGuardStore()
	store := NewQuotaRuntimeStore(guard)
	now := time.Now().UTC()
	accountKey := "acct_00000000-0000-4000-8000-000000000104"
	remainingEmpty := 0
	remainingCached := 100

	if _, err := store.Upsert(QuotaRuntimeState{
		AccountKey: accountKey,
		Status:     QuotaRuntimeStatusSuccess,
		Windows: []QuotaRuntimeWindow{{
			ID:               "five-hour",
			Label:            "5H",
			RemainingPercent: &remainingEmpty,
			ResetAtUnix:      now.Add(time.Hour).Unix(),
		}},
	}, now); err != nil {
		t.Fatalf("Upsert exhausted: %v", err)
	}
	state, err := store.Upsert(QuotaRuntimeState{
		AccountKey: accountKey,
		Status:     QuotaRuntimeStatusStale,
		Windows: []QuotaRuntimeWindow{{
			ID:               "five-hour",
			Label:            "5H",
			RemainingPercent: &remainingCached,
			ResetAtUnix:      now.Add(time.Hour).Unix(),
		}},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Upsert stale: %v", err)
	}
	if !state.Blocked || len(state.Sources) != 1 || state.Sources[0].Source != AccountRouteGuardSourceQuotaEmpty {
		t.Fatalf("state = %#v, want stale state to preserve existing quota-empty until reset", state)
	}
}

func TestQuotaRuntimeRoutesPutAndGetStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := NewAccountRouteGuardStore()
	store := NewQuotaRuntimeStore(guard)
	router := gin.New()
	configureQuotaRuntimeRoutes(router.Group("/v0/management"), store)

	body := `{
		"source":"quota-curl",
		"status":"success",
		"plan_type":"plus",
		"windows":[{"id":"five-hour","label":"5H","remaining_percent":0,"reset_at_unix":1893456000}]
	}`
	put := httptest.NewRecorder()
	router.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/v0/management/gettokens/quota-status/acct_00000000-0000-4000-8000-000000000103", strings.NewReader(body)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", put.Code, put.Body.String())
	}
	var putState QuotaRuntimeState
	if err := json.Unmarshal(put.Body.Bytes(), &putState); err != nil {
		t.Fatalf("decode PUT state: %v", err)
	}
	if !putState.Blocked || putState.Sources[0].Source != AccountRouteGuardSourceQuotaEmpty {
		t.Fatalf("PUT state = %#v, want quota-empty blocked", putState)
	}

	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-status?account_key=acct_00000000-0000-4000-8000-000000000103", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", get.Code, get.Body.String())
	}
	var getState QuotaRuntimeState
	if err := json.Unmarshal(get.Body.Bytes(), &getState); err != nil {
		t.Fatalf("decode GET state: %v", err)
	}
	if getState.AccountKey != putState.AccountKey || !getState.Blocked {
		t.Fatalf("GET state = %#v, want stored blocked quota state", getState)
	}
}
