package management

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
	_ "modernc.org/sqlite"
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
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	h.SetAccountStorePath(dbPath)

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

func TestQuotaRefreshOpenAICompatibleAccountWritesBillingRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usage":
			_, _ = w.Write([]byte(`{"plan_type":"billing","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":3600,"reset_at":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `}}}`))
		case "/balance":
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"42.00","granted_balance":"12.00","topped_up_balance":"30.00"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	h.SetAccountStorePath(dbPath)

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/quota-refresh/:account_key", h.RefreshAccountQuota)

	createBody := []byte(`{
		"kind":"openai-compatible",
		"title":"DeepSeek",
		"provider":"deepseek",
		"openai_compatible":{
			"provider_name":"deepseek",
			"base_url":"` + upstream.URL + `",
			"api_key_entries_json":"[{\"api-key\":\"sk-openai-compatible\"}]",
			"quota_enabled":true,
			"quota_curl":"curl -sS \"{{baseUrl}}/usage\" -H \"Authorization: Bearer {{apiKey}}\"",
			"billing_enabled":true,
			"billing_curl":"curl -sS \"{{baseUrl}}/balance\" -H \"Authorization: Bearer {{apiKey}}\""
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
	router.ServeHTTP(refreshRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh/"+created.AccountKey, bytes.NewReader([]byte(`{"include_billing":true}`))))
	if refreshRecorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body=%s", refreshRecorder.Code, refreshRecorder.Body.String())
	}
	var state gettokenshooks.QuotaRuntimeState
	if err := json.Unmarshal(refreshRecorder.Body.Bytes(), &state); err != nil {
		t.Fatalf("unmarshal refresh: %v", err)
	}
	if gotAuthorization != "Bearer sk-openai-compatible" {
		t.Fatalf("Authorization = %q, want Bearer sk-openai-compatible", gotAuthorization)
	}
	if state.AccountKey != created.AccountKey || state.Status != gettokenshooks.QuotaRuntimeStatusSuccess {
		t.Fatalf("state identity/status = %#v", state)
	}
	if len(state.Windows) != 1 {
		t.Fatalf("windows = %#v, want quota window", state.Windows)
	}
	if state.Billing == nil || !state.Billing.IsAvailable || len(state.Billing.BalanceInfos) != 1 {
		t.Fatalf("billing = %#v, want one balance", state.Billing)
	}
	if state.Billing.BalanceInfos[0].TotalBalance != "42.00" {
		t.Fatalf("balance = %#v, want total 42.00", state.Billing.BalanceInfos[0])
	}
}

func TestQuotaRefreshBatchRefreshesSelectedAccountsWithPartialErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `}}}`))
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/quota-refresh-batch", h.RefreshAccountQuotaBatch)

	createAccount := func(title string, quotaEnabled bool) string {
		t.Helper()
		createBody := []byte(`{
			"kind":"codex-api-key",
			"title":"` + title + `",
			"provider":"codex",
			"credential":{
				"api_key":"sk-` + title + `",
				"base_url":"` + upstream.URL + `",
				"quota_enabled":` + jsonBool(quotaEnabled) + `,
				"quota_curl":"curl -sS \"{{baseUrl}}/usage\" -H \"Authorization: Bearer {{apiKey}}\""
			}
		}`)
		createRecorder := httptest.NewRecorder()
		router.ServeHTTP(createRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/accounts", bytes.NewReader(createBody)))
		if createRecorder.Code != http.StatusOK {
			t.Fatalf("create %s status = %d body=%s", title, createRecorder.Code, createRecorder.Body.String())
		}
		var created struct {
			AccountKey string `json:"account_key"`
		}
		if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
			t.Fatalf("unmarshal create %s: %v", title, err)
		}
		return created.AccountKey
	}

	okKey := createAccount("batch-ok", true)
	missingQuotaKey := createAccount("batch-missing-quota", false)

	refreshRecorder := httptest.NewRecorder()
	body := []byte(`{"account_keys":["` + okKey + `","` + missingQuotaKey + `"],"include_billing":false,"concurrency":4}`)
	router.ServeHTTP(refreshRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch", bytes.NewReader(body)))
	if refreshRecorder.Code != http.StatusOK {
		t.Fatalf("batch refresh status = %d body=%s", refreshRecorder.Code, refreshRecorder.Body.String())
	}
	var result quotaRefreshBatchResponse
	if err := json.Unmarshal(refreshRecorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal batch refresh: %v", err)
	}
	if result.Succeeded != 1 || result.Failed != 1 || len(result.Items) != 1 || len(result.Errors) != 1 {
		t.Fatalf("batch result = %#v, want one success and one error", result)
	}
	if result.Items[0].AccountKey != okKey || result.Items[0].Status != gettokenshooks.QuotaRuntimeStatusSuccess {
		t.Fatalf("batch success item = %#v", result.Items[0])
	}
	if result.Errors[0].AccountKey != missingQuotaKey || result.Errors[0].Error == "" {
		t.Fatalf("batch error item = %#v", result.Errors[0])
	}
}

func TestQuotaRefreshBatchUsesKeyScopedAccountQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `}}}`))
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	h.SetAccountStorePath(dbPath)

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/quota-refresh-batch", h.RefreshAccountQuotaBatch)

	createAccount := func(title string) string {
		t.Helper()
		createBody := []byte(`{
			"kind":"codex-api-key",
			"title":"` + title + `",
			"provider":"codex",
			"credential":{
				"api_key":"sk-` + title + `",
				"base_url":"` + upstream.URL + `",
				"quota_enabled":true,
				"quota_curl":"curl -sS \"{{baseUrl}}/usage\" -H \"Authorization: Bearer {{apiKey}}\""
			}
		}`)
		createRecorder := httptest.NewRecorder()
		router.ServeHTTP(createRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/accounts", bytes.NewReader(createBody)))
		if createRecorder.Code != http.StatusOK {
			t.Fatalf("create %s status = %d body=%s", title, createRecorder.Code, createRecorder.Body.String())
		}
		var created struct {
			AccountKey string `json:"account_key"`
		}
		if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
			t.Fatalf("unmarshal create %s: %v", title, err)
		}
		return created.AccountKey
	}

	targetKey := createAccount("target")
	brokenSiblingKey := createAccount("broken-sibling")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), "DELETE FROM codex_api_key_accounts WHERE account_key = ?", brokenSiblingKey); err != nil {
		t.Fatalf("delete sibling credential: %v", err)
	}

	refreshRecorder := httptest.NewRecorder()
	body := []byte(`{"account_keys":["` + targetKey + `"],"include_billing":false}`)
	router.ServeHTTP(refreshRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch", bytes.NewReader(body)))
	if refreshRecorder.Code != http.StatusOK {
		t.Fatalf("batch refresh status = %d body=%s", refreshRecorder.Code, refreshRecorder.Body.String())
	}
	var result quotaRefreshBatchResponse
	if err := json.Unmarshal(refreshRecorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal batch refresh: %v", err)
	}
	if result.Succeeded != 1 || result.Failed != 0 || len(result.Items) != 1 || result.Items[0].AccountKey != targetKey {
		t.Fatalf("batch result = %#v, want only requested target success", result)
	}
}

func TestQuotaRefreshBatchJobCompletesWithErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))

	router := gin.New()
	router.POST("/v0/management/gettokens/quota-refresh-batch/jobs", h.StartAccountQuotaBatchRefreshJob)
	router.GET("/v0/management/gettokens/quota-refresh-batch/jobs/:job_id", h.GetAccountQuotaBatchRefreshJob)

	missingKey := "acct_00000000-0000-4000-8000-00000000c001"
	startRecorder := httptest.NewRecorder()
	startBody := []byte(`{"account_keys":["` + missingKey + `"],"include_billing":true,"concurrency":4}`)
	router.ServeHTTP(startRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch/jobs", bytes.NewReader(startBody)))
	if startRecorder.Code != http.StatusAccepted {
		t.Fatalf("start job status = %d body=%s", startRecorder.Code, startRecorder.Body.String())
	}
	var started quotaRefreshBatchJobResponse
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &started); err != nil {
		t.Fatalf("unmarshal start job: %v", err)
	}
	if started.JobID == "" || started.Total != 1 {
		t.Fatalf("started job = %#v, want id and total", started)
	}
	if started.Status != quotaRefreshBatchJobStatusPending && started.Status != quotaRefreshBatchJobStatusRunning && started.Status != quotaRefreshBatchJobStatusFailed {
		t.Fatalf("started job status = %q", started.Status)
	}

	var snapshot quotaRefreshBatchJobResponse
	for attempt := 0; attempt < 50; attempt++ {
		getRecorder := httptest.NewRecorder()
		router.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-refresh-batch/jobs/"+started.JobID, nil))
		if getRecorder.Code != http.StatusOK {
			t.Fatalf("get job status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
		}
		if err := json.Unmarshal(getRecorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("unmarshal get job: %v", err)
		}
		if snapshot.Status == quotaRefreshBatchJobStatusFailed || snapshot.Status == quotaRefreshBatchJobStatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snapshot.Status != quotaRefreshBatchJobStatusFailed {
		t.Fatalf("job snapshot = %#v, want failed", snapshot)
	}
	if snapshot.Failed != 1 || len(snapshot.Errors) != 1 || snapshot.Errors[0].AccountKey != missingKey {
		t.Fatalf("job errors = failed:%d errors:%#v, want missing account error", snapshot.Failed, snapshot.Errors)
	}
	if snapshot.CompletedAt == "" {
		t.Fatalf("job completed_at is empty: %#v", snapshot)
	}
}

func TestQuotaRefreshBatchJobStartDoesNotWaitForSlowUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseUpstream)
		})
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
		default:
			close(upstreamStarted)
		}
		<-releaseUpstream
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `}}}`))
	}))
	defer upstream.Close()
	defer release()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/quota-refresh-batch/jobs", h.StartAccountQuotaBatchRefreshJob)
	router.GET("/v0/management/gettokens/quota-refresh-batch/jobs/:job_id", h.GetAccountQuotaBatchRefreshJob)

	createBody := []byte(`{
		"kind":"codex-api-key",
		"title":"slow-job",
		"provider":"codex",
		"credential":{
			"api_key":"sk-slow",
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

	startRecorder := httptest.NewRecorder()
	startBody := []byte(`{"account_keys":["` + created.AccountKey + `"],"include_billing":false,"concurrency":1}`)
	startedAt := time.Now()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(startRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch/jobs", bytes.NewReader(startBody)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("start job did not return before slow upstream completed")
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("start job elapsed = %s, want under 500ms", elapsed)
	}
	if startRecorder.Code != http.StatusAccepted {
		t.Fatalf("start job status = %d body=%s", startRecorder.Code, startRecorder.Body.String())
	}
	var started quotaRefreshBatchJobResponse
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &started); err != nil {
		t.Fatalf("unmarshal start job: %v", err)
	}
	if started.JobID == "" {
		t.Fatalf("started job = %#v, want job_id", started)
	}
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background job did not reach slow upstream")
	}

	var snapshot quotaRefreshBatchJobResponse
	for attempt := 0; attempt < 50; attempt++ {
		getRecorder := httptest.NewRecorder()
		router.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-refresh-batch/jobs/"+started.JobID, nil))
		if getRecorder.Code != http.StatusOK {
			t.Fatalf("get job status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
		}
		if err := json.Unmarshal(getRecorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("unmarshal get job: %v", err)
		}
		if snapshot.Status == quotaRefreshBatchJobStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snapshot.Status != quotaRefreshBatchJobStatusRunning {
		t.Fatalf("job snapshot before release = %#v, want running", snapshot)
	}

	release()
	for attempt := 0; attempt < 50; attempt++ {
		getRecorder := httptest.NewRecorder()
		router.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-refresh-batch/jobs/"+started.JobID, nil))
		if getRecorder.Code != http.StatusOK {
			t.Fatalf("get completed job status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
		}
		if err := json.Unmarshal(getRecorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("unmarshal completed job: %v", err)
		}
		if snapshot.Status == quotaRefreshBatchJobStatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snapshot.Status != quotaRefreshBatchJobStatusSucceeded || snapshot.Succeeded != 1 || len(snapshot.Items) != 1 {
		t.Fatalf("job snapshot after release = %#v, want success", snapshot)
	}
}

func TestQuotaRefreshBatchJobCancelsWhenTargetAccountIsDeleted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseUpstream)
		})
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
		default:
			close(upstreamStarted)
		}
		select {
		case <-r.Context().Done():
			return
		case <-releaseUpstream:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `}}}`))
	}))
	defer upstream.Close()
	defer release()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.DELETE("/v0/management/accounts/:account_key", h.DeleteAccount)
	router.POST("/v0/management/gettokens/quota-refresh-batch/jobs", h.StartAccountQuotaBatchRefreshJob)
	router.GET("/v0/management/gettokens/quota-refresh-batch/jobs/:job_id", h.GetAccountQuotaBatchRefreshJob)

	accountKey := createQuotaRefreshTestAccount(t, router, "delete-cancel-job", upstream.URL)
	started := startQuotaRefreshBatchJob(t, router, accountKey, 1)
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background job did not reach slow upstream")
	}

	deleteRecorder := httptest.NewRecorder()
	router.ServeHTTP(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/v0/management/accounts/"+accountKey, nil))
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}

	snapshot := waitForQuotaRefreshJobStatus(t, router, started.JobID, quotaRefreshBatchJobStatusCanceled)
	if snapshot.Running != 0 || snapshot.Pending != 0 {
		t.Fatalf("canceled job counters = running:%d pending:%d", snapshot.Running, snapshot.Pending)
	}
	if snapshot.CompletedAt == "" {
		t.Fatalf("canceled job completed_at is empty: %#v", snapshot)
	}
	if snapshot.Failed == 0 || len(snapshot.Errors) == 0 {
		t.Fatalf("canceled job should retain an error summary: %#v", snapshot)
	}
}

func TestQuotaRefreshBatchJobStoreCancelAllStopsRunningJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
		default:
			close(upstreamStarted)
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/quota-refresh-batch/jobs", h.StartAccountQuotaBatchRefreshJob)
	router.GET("/v0/management/gettokens/quota-refresh-batch/jobs/:job_id", h.GetAccountQuotaBatchRefreshJob)

	accountKey := createQuotaRefreshTestAccount(t, router, "shutdown-cancel-job", upstream.URL)
	started := startQuotaRefreshBatchJob(t, router, accountKey, 1)
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background job did not reach slow upstream")
	}

	canceled := h.cancelAllQuotaRefreshBatchJobs("server shutdown", time.Now().UTC())
	if canceled != 1 {
		t.Fatalf("cancelAll jobs = %d, want 1", canceled)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled by job store cancellation")
	}
	snapshot := waitForQuotaRefreshJobStatus(t, router, started.JobID, quotaRefreshBatchJobStatusCanceled)
	if snapshot.CompletedAt == "" {
		t.Fatalf("canceled job completed_at is empty: %#v", snapshot)
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

func createQuotaRefreshTestAccount(t *testing.T, router *gin.Engine, title string, upstreamURL string) string {
	t.Helper()
	createBody := []byte(`{
		"kind":"codex-api-key",
		"title":"` + title + `",
		"provider":"codex",
		"credential":{
			"api_key":"sk-` + title + `",
			"base_url":"` + upstreamURL + `",
			"quota_enabled":true,
			"quota_curl":"curl -sS \"{{baseUrl}}/usage\" -H \"Authorization: Bearer {{apiKey}}\""
		}
	}`)
	createRecorder := httptest.NewRecorder()
	router.ServeHTTP(createRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/accounts", bytes.NewReader(createBody)))
	if createRecorder.Code != http.StatusOK {
		t.Fatalf("create %s status = %d body=%s", title, createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		AccountKey string `json:"account_key"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create %s: %v", title, err)
	}
	return created.AccountKey
}

func startQuotaRefreshBatchJob(t *testing.T, router *gin.Engine, accountKey string, concurrency int) quotaRefreshBatchJobResponse {
	t.Helper()
	startRecorder := httptest.NewRecorder()
	startBody := []byte(`{"account_keys":["` + accountKey + `"],"include_billing":false,"concurrency":` + jsonInt(int64(concurrency)) + `}`)
	router.ServeHTTP(startRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch/jobs", bytes.NewReader(startBody)))
	if startRecorder.Code != http.StatusAccepted {
		t.Fatalf("start job status = %d body=%s", startRecorder.Code, startRecorder.Body.String())
	}
	var started quotaRefreshBatchJobResponse
	if err := json.Unmarshal(startRecorder.Body.Bytes(), &started); err != nil {
		t.Fatalf("unmarshal start job: %v", err)
	}
	if started.JobID == "" {
		t.Fatalf("started job = %#v, want job_id", started)
	}
	return started
}

func waitForQuotaRefreshJobStatus(t *testing.T, router *gin.Engine, jobID string, status string) quotaRefreshBatchJobResponse {
	t.Helper()
	var snapshot quotaRefreshBatchJobResponse
	for attempt := 0; attempt < 100; attempt++ {
		getRecorder := httptest.NewRecorder()
		router.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-refresh-batch/jobs/"+jobID, nil))
		if getRecorder.Code != http.StatusOK {
			t.Fatalf("get job status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
		}
		if err := json.Unmarshal(getRecorder.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("unmarshal get job: %v", err)
		}
		if snapshot.Status == status {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach status %q, last snapshot=%#v", jobID, status, snapshot)
	return snapshot
}

func jsonInt(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func jsonBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestQuotaDraftReplacesArbitraryCurlVariables(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotOrg string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrg = r.Header.Get("X-Organization-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"billing","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":3600,"reset_at":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `}}}`))
	}))
	defer upstream.Close()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	router := gin.New()
	router.POST("/v0/management/gettokens/quota-test", h.TestQuotaCurl)

	body := []byte(`{
		"api_key":"sk-test",
		"base_url":"` + upstream.URL + `",
		"quota_curl":"curl -sS \"{{baseUrl}}/usage\" -H \"Authorization: Bearer {{apiKey}}\" -H \"X-Organization-Id: {{organizationId}}\"",
		"curl_variables":{"organizationId":"org_123"}
	}`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-test", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("quota-test status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotOrg != "org_123" {
		t.Fatalf("X-Organization-Id = %q, want org_123", gotOrg)
	}
}
