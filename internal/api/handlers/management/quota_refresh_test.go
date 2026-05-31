package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
)

func TestQuotaRefreshCodexAPIKeyAccountWritesRuntimeGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotAuthorization string
	resetAt := time.Now().UTC().Add(time.Hour).Unix()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"pro",
			"rate_limit":{
				"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":` + jsonInt(resetAt) + `}
			}
		}`))
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/quota-refresh/:account_key", h.RefreshAccountQuota)

	createBody := []byte(`{
		"kind":"codex-api-key",
		"title":"Quota Runtime",
		"provider":"codex",
		"credential":{
			"api_key":"sk-sidecar",
			"base_url":"` + upstream.URL + `",
			"quota_enabled":true,
			"quota_curl":"curl -sS \"{{baseUrl}}/usage\" -H \"Authorization: Bearer {{apiKey}}\""
		}
	}`)
	createRecorder := httptest.NewRecorder()
	router.ServeHTTP(createRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/accounts", bytes.NewReader(createBody)))
	if createRecorder.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		AccountKey string `json:"account_key"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}

	refreshRecorder := httptest.NewRecorder()
	router.ServeHTTP(refreshRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh/"+created.AccountKey, bytes.NewReader([]byte(`{"include_billing":false}`))))
	if refreshRecorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", refreshRecorder.Code, refreshRecorder.Body.String())
	}
	var state gettokenshooks.QuotaRuntimeState
	if err := json.Unmarshal(refreshRecorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("unmarshal refresh: %v", err)
	}
	if gotAuthorization != "Bearer sk-sidecar" {
		t.Fatalf("Authorization = %q, want Bearer sk-sidecar", gotAuthorization)
	}
	if state.AccountKey != created.AccountKey || state.Status != gettokenshooks.QuotaRuntimeStatusSuccess {
		t.Fatalf("state identity/status = %#v", state)
	}
	if len(state.Windows) != 1 || state.Windows[0].RemainingPercent == nil || *state.Windows[0].RemainingPercent != 0 {
		t.Fatalf("windows = %#v, want exhausted quota window", state.Windows)
	}
	if !state.Blocked || len(state.Sources) != 1 || state.Sources[0].Source != gettokenshooks.AccountRouteGuardSourceQuotaEmpty {
		t.Fatalf("guard state = blocked:%v sources:%#v, want quota-empty", state.Blocked, state.Sources)
	}
	if state.Sources[0].ExpiresAt == "" || state.Sources[0].NextReset == "" {
		t.Fatalf("quota-empty source missing reset fields: %#v", state.Sources[0])
	}
}

func TestQuotaDraftTestRejectsShellFeaturesWithoutRuntimeWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	router := gin.New()
	router.POST("/v0/management/gettokens/quota-test", h.TestQuotaCurl)

	accountKey := "acct_00000000-0000-4000-8000-00000000a111"
	body := []byte(`{
		"account_key":"` + accountKey + `",
		"api_key":"sk-test",
		"base_url":"https://quota.example.com",
		"quota_curl":"curl https://quota.example.com/usage | jq ."
	}`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-test", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("quota-test status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("shell")) && !bytes.Contains(recorder.Body.Bytes(), []byte("管道")) {
		t.Fatalf("quota-test error should mention shell feature rejection: %s", recorder.Body.String())
	}
	if _, ok := gettokenshooks.DefaultQuotaRuntimeStore().StateForAccount(accountKey); ok {
		t.Fatalf("quota-test draft failure must not write runtime state for %s", accountKey)
	}
}

func TestBillingDraftParsesOpenRouterWithoutRuntimeWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"total_credits":12.5,"total_usage":4.25}}`))
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	router := gin.New()
	router.POST("/v0/management/gettokens/billing-test", h.TestBillingCurl)

	accountKey := "acct_00000000-0000-4000-8000-00000000b111"
	body := []byte(`{
		"account_key":"` + accountKey + `",
		"api_key":"sk-billing",
		"base_url":"` + upstream.URL + `",
		"billing_curl":"curl -sS \"{{baseUrl}}/credits\" -H \"Authorization: Bearer {{apiKey}}\""
	}`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/billing-test", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("billing-test status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var state gettokenshooks.QuotaRuntimeState
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("unmarshal billing-test: %v", err)
	}
	if gotAuthorization != "Bearer sk-billing" {
		t.Fatalf("Authorization = %q, want Bearer sk-billing", gotAuthorization)
	}
	if state.AccountKey != accountKey || state.Source != quotaBillingDraftTestSource || state.Status != gettokenshooks.QuotaRuntimeStatusSuccess {
		t.Fatalf("state identity/source/status = %#v", state)
	}
	if len(state.Windows) != 0 {
		t.Fatalf("billing-test windows = %#v, want empty", state.Windows)
	}
	if state.Billing == nil || !state.Billing.IsAvailable || len(state.Billing.BalanceInfos) != 1 {
		t.Fatalf("billing = %#v, want one available balance", state.Billing)
	}
	balance := state.Billing.BalanceInfos[0]
	if balance.Currency != "USD" || balance.TotalBalance != "12.50" || balance.GrantedBalance != "8.25" || balance.ToppedUpBalance != "12.50" {
		t.Fatalf("balance = %#v, want OpenRouter credits mapped", balance)
	}
	if _, ok := gettokenshooks.DefaultQuotaRuntimeStore().StateForAccount(accountKey); ok {
		t.Fatalf("billing-test draft must not write runtime state for %s", accountKey)
	}
}

func jsonInt(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
