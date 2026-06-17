package gettokenshooks

import (
	"encoding/json"
	"fmt"
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
	if state.Fact == nil || state.Fact.State != "no_quota" || state.Fact.Risk != "blocking" {
		t.Fatalf("fact = %#v, want no_quota blocking fact", state.Fact)
	}
	if state.Fact.ExpiresAt == "" {
		t.Fatalf("fact = %#v, want reset expiry", state.Fact)
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
	if state.Fact == nil || state.Fact.State != "stale" || state.Fact.Freshness != "stale" {
		t.Fatalf("fact = %#v, want stale cached fact", state.Fact)
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
	if getState.Fact == nil || getState.Fact.State != "no_quota" || getState.Fact.Source != "quota-curl" {
		t.Fatalf("GET fact = %#v, want no_quota quota-curl fact", getState.Fact)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(get.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw GET state: %v", err)
	}
	if _, ok := raw["quotaFact"]; !ok {
		t.Fatalf("raw GET state keys = %#v, want explicit quotaFact", raw)
	}
	var quotaFact QuotaRuntimeFact
	if err := json.Unmarshal(raw["quotaFact"], &quotaFact); err != nil {
		t.Fatalf("decode quotaFact: %v", err)
	}
	if quotaFact.State != "no_quota" || quotaFact.Source != "quota-curl" {
		t.Fatalf("quotaFact = %#v, want copied runtime fact", quotaFact)
	}
}

func TestQuotaRuntimeFactBuilderClassifiesRuntimeStates(t *testing.T) {
	now := time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC)
	remainingZero := 0
	remainingHealthy := 34

	tests := []struct {
		name      string
		state     QuotaRuntimeState
		wantState string
		wantFresh string
		wantRisk  string
	}{
		{
			name: "fresh exhausted known window",
			state: QuotaRuntimeState{
				AccountKey: "acct_00000000-0000-4000-8000-000000000301",
				Status:     QuotaRuntimeStatusSuccess,
				Source:     "quota-curl",
				Windows: []QuotaRuntimeWindow{{
					ID:               "five-hour",
					RemainingPercent: &remainingZero,
					ResetAtUnix:      now.Add(time.Hour).Unix(),
				}},
			},
			wantState: "no_quota",
			wantFresh: "fresh",
			wantRisk:  "blocking",
		},
		{
			name: "fresh positive window",
			state: QuotaRuntimeState{
				AccountKey: "acct_00000000-0000-4000-8000-000000000302",
				Status:     QuotaRuntimeStatusSuccess,
				Source:     "quota-curl",
				Windows: []QuotaRuntimeWindow{{
					ID:               "five-hour",
					RemainingPercent: &remainingHealthy,
					ResetAtUnix:      now.Add(time.Hour).Unix(),
				}},
			},
			wantState: "available",
			wantFresh: "fresh",
			wantRisk:  "none",
		},
		{
			name: "missing window evidence",
			state: QuotaRuntimeState{
				AccountKey: "acct_00000000-0000-4000-8000-000000000303",
				Status:     QuotaRuntimeStatusSuccess,
				Source:     "quota-runtime",
				Windows:    []QuotaRuntimeWindow{},
			},
			wantState: "unknown",
			wantFresh: "unknown",
			wantRisk:  "unknown",
		},
		{
			name: "degraded cache",
			state: QuotaRuntimeState{
				AccountKey:     "acct_00000000-0000-4000-8000-000000000304",
				Status:         QuotaRuntimeStatusDegraded,
				Source:         "quota-curl",
				Stale:          true,
				DegradedReason: "quota refresh failed",
				Windows: []QuotaRuntimeWindow{{
					ID:               "five-hour",
					RemainingPercent: &remainingHealthy,
				}},
			},
			wantState: "stale",
			wantFresh: "stale",
			wantRisk:  "warning",
		},
		{
			name: "provider denied",
			state: QuotaRuntimeState{
				AccountKey:     "acct_00000000-0000-4000-8000-000000000305",
				Status:         QuotaRuntimeStatusError,
				Source:         "quota-curl",
				DegradedReason: "quota request failed with status 403",
				Windows:        []QuotaRuntimeWindow{},
			},
			wantState: "denied",
			wantFresh: "fresh",
			wantRisk:  "denied",
		},
		{
			name: "unsupported quota probe",
			state: QuotaRuntimeState{
				AccountKey:     "acct_00000000-0000-4000-8000-000000000307",
				Status:         QuotaRuntimeStatusError,
				Source:         "quota-curl",
				DegradedReason: "account kind does not support quota refresh",
				Windows:        []QuotaRuntimeWindow{},
			},
			wantState: "unsupported",
			wantFresh: "unknown",
			wantRisk:  "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fact := BuildQuotaRuntimeFact(tt.state, now)
			if fact.State != tt.wantState || fact.Freshness != tt.wantFresh || fact.Risk != tt.wantRisk {
				t.Fatalf("fact = %#v, want state=%s freshness=%s risk=%s", fact, tt.wantState, tt.wantFresh, tt.wantRisk)
			}
			if fact.ObservedAt == "" || fact.Explanation == "" {
				t.Fatalf("fact = %#v, want observed_at and explanation", fact)
			}
		})
	}
}

func TestQuotaRuntimePreservesProvidedFact(t *testing.T) {
	store := NewQuotaRuntimeStore(NewAccountRouteGuardStore())
	now := time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC)
	state, err := store.Upsert(QuotaRuntimeState{
		AccountKey: "acct_00000000-0000-4000-8000-000000000306",
		Status:     QuotaRuntimeStatusSuccess,
		Windows:    []QuotaRuntimeWindow{},
		Fact: &QuotaRuntimeFact{
			State:       "unsupported",
			Source:      "provider-quota-curl",
			Freshness:   "unknown",
			Confidence:  "none",
			Risk:        "unknown",
			Explanation: "quota probe is not configured",
		},
	}, now)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if state.Fact == nil || state.Fact.State != "unsupported" || state.Fact.Explanation != "quota probe is not configured" {
		t.Fatalf("fact = %#v, want provided fact preserved", state.Fact)
	}
}

func TestQuotaRuntimeRoutesGetStatusesByKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewQuotaRuntimeStore(NewAccountRouteGuardStore())
	now := time.Now().UTC()
	keys := []string{
		"acct_00000000-0000-4000-8000-000000000201",
		"acct_00000000-0000-4000-8000-000000000202",
	}
	for index, key := range keys {
		if _, err := store.Upsert(QuotaRuntimeState{
			AccountKey: key,
			Status:     QuotaRuntimeStatusSuccess,
			PlanType:   fmt.Sprintf("plan-%d", index),
			Windows:    []QuotaRuntimeWindow{},
		}, now); err != nil {
			t.Fatalf("Upsert %s: %v", key, err)
		}
	}
	router := gin.New()
	configureQuotaRuntimeRoutes(router.Group("/v0/management"), store)

	request := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-status?account_key="+keys[1]+"&account_key=acct_missing&account_key="+keys[0], nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []QuotaRuntimeState `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Items) != 3 {
		t.Fatalf("items = %#v, want 3", response.Items)
	}
	if response.Items[0].AccountKey != keys[1] || response.Items[0].PlanType != "plan-1" {
		t.Fatalf("first item = %#v, want second requested key", response.Items[0])
	}
	if response.Items[1].AccountKey != "acct_missing" || response.Items[1].Status != QuotaRuntimeStatusStale {
		t.Fatalf("missing item = %#v, want stale missing state", response.Items[1])
	}
	if response.Items[1].Fact == nil || response.Items[1].Fact.State != "unknown" {
		t.Fatalf("missing fact = %#v, want unknown fact", response.Items[1].Fact)
	}
	if response.Items[2].AccountKey != keys[0] || response.Items[2].PlanType != "plan-0" {
		t.Fatalf("third item = %#v, want first requested key", response.Items[2])
	}
}

func TestQuotaRuntimeRoutesGetStatusesByCommaKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewQuotaRuntimeStore(NewAccountRouteGuardStore())
	key := "acct_00000000-0000-4000-8000-000000000203"
	if _, err := store.Upsert(QuotaRuntimeState{
		AccountKey: key,
		Status:     QuotaRuntimeStatusSuccess,
		PlanType:   "team",
		Windows:    []QuotaRuntimeWindow{},
	}, time.Now().UTC()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	router := gin.New()
	configureQuotaRuntimeRoutes(router.Group("/v0/management"), store)

	request := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-status?account_keys="+key+",acct_missing", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []QuotaRuntimeState `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Items) != 2 || response.Items[0].AccountKey != key || response.Items[1].Status != QuotaRuntimeStatusStale {
		t.Fatalf("items = %#v, want stored then stale", response.Items)
	}
}

func BenchmarkQuotaRuntimeStatusSnapshot1652Accounts(b *testing.B) {
	benchmarkQuotaRuntimeStatusSnapshot(b, 1652)
}

func BenchmarkQuotaRuntimeStatusTarget1Of1652Accounts(b *testing.B) {
	benchmarkQuotaRuntimeStatusByKeys(b, 1652, 1)
}

func BenchmarkQuotaRuntimeStatusTarget10Of1652Accounts(b *testing.B) {
	benchmarkQuotaRuntimeStatusByKeys(b, 1652, 10)
}

func BenchmarkQuotaRuntimeStatusTarget100Of1652Accounts(b *testing.B) {
	benchmarkQuotaRuntimeStatusByKeys(b, 1652, 100)
}

func BenchmarkQuotaRuntimeStatusTarget1652Of1652Accounts(b *testing.B) {
	benchmarkQuotaRuntimeStatusByKeys(b, 1652, 1652)
}

func benchmarkQuotaRuntimeStatusSnapshot(b *testing.B, totalAccounts int) {
	gin.SetMode(gin.TestMode)
	store := seedQuotaRuntimeBenchmarkStore(b, totalAccounts)
	router := gin.New()
	configureQuotaRuntimeRoutes(router.Group("/v0/management"), store)

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-status", nil))
		if recorder.Code != http.StatusOK {
			b.Fatalf("GET status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func benchmarkQuotaRuntimeStatusByKeys(b *testing.B, totalAccounts int, targetAccounts int) {
	gin.SetMode(gin.TestMode)
	store := seedQuotaRuntimeBenchmarkStore(b, totalAccounts)
	router := gin.New()
	configureQuotaRuntimeRoutes(router.Group("/v0/management"), store)
	query := strings.Builder{}
	for index := 0; index < targetAccounts; index++ {
		if index > 0 {
			query.WriteByte('&')
		}
		query.WriteString("account_key=")
		query.WriteString(fmt.Sprintf("acct_00000000-0000-4000-8000-%012d", index))
	}
	url := "/v0/management/gettokens/quota-status?" + query.String()

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, url, nil))
		if recorder.Code != http.StatusOK {
			b.Fatalf("GET status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func seedQuotaRuntimeBenchmarkStore(b *testing.B, totalAccounts int) *QuotaRuntimeStore {
	b.Helper()
	store := NewQuotaRuntimeStore(NewAccountRouteGuardStore())
	remaining := 87
	now := time.Now().UTC()
	for index := 0; index < totalAccounts; index++ {
		_, err := store.Upsert(QuotaRuntimeState{
			AccountKey: fmt.Sprintf("acct_00000000-0000-4000-8000-%012d", index),
			Source:     "quota-curl",
			Status:     QuotaRuntimeStatusSuccess,
			PlanType:   "team",
			Windows: []QuotaRuntimeWindow{{
				ID:               "five-hour",
				Label:            "5H",
				RemainingPercent: &remaining,
				ResetAtUnix:      now.Add(time.Hour).Unix(),
			}},
			Sources: []QuotaRuntimeSourceState{},
		}, now)
		if err != nil {
			b.Fatalf("upsert quota state %d: %v", index, err)
		}
	}
	return store
}
