package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestOpenAIQuotaResetQueryUsesAuthFileCredentialAndCodexHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath string
	var gotAuthorization string
	var gotAccountID string
	var gotOriginator string
	var gotLanguage string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("chatgpt-account-id")
		gotOriginator = r.Header.Get("originator")
		gotLanguage = r.Header.Get("oai-language")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"account_id\":\"chatgpt-acct-1\",\"plan_type\":\"pro\",\"rate_limit_reset_credits\":{\"available_count\":2},\"rate_limit\":{\"primary_window\":{\"used_percent\":100,\"limit_window_seconds\":18000,\"reset_after_seconds\":60}}}"))
	}))
	defer upstream.Close()

	restore := overrideOpenAIQuotaResetUpstreamForTest(upstream.URL+"/backend-api/wham/usage", upstream.URL+"/backend-api/wham/rate-limit-reset-credits/consume")
	defer restore()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))
	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.GET("/v0/management/gettokens/openai-quota-reset/:account_key", h.GetOpenAIQuotaResetCredit)

	accountKey := createOpenAIQuotaResetAuthFileAccount(t, router)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/openai-quota-reset/"+accountKey, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("query status = %d body=%s", response.Code, response.Body.String())
	}

	body := decodeOpenAIQuotaResetMap(t, response.Body.Bytes())
	if stringFromMap(body, "account_key") != accountKey || intFromMap(body, "available_count") != 2 || stringFromMap(body, "plan_type") != "pro" || stringFromMap(body, "status") != "success" {
		t.Fatalf("response = %#v, want account/success/pro/2", body)
	}
	if gotPath != "/backend-api/wham/usage" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuthorization != "Bearer access-token" {
		t.Fatalf("Authorization = %q, want bearer access-token", gotAuthorization)
	}
	if gotAccountID != "chatgpt-acct-1" {
		t.Fatalf("chatgpt-account-id = %q", gotAccountID)
	}
	if gotOriginator != "Codex Desktop" || gotLanguage != "zh-CN" {
		t.Fatalf("codex headers originator=%q language=%q", gotOriginator, gotLanguage)
	}
}

func TestOpenAIQuotaResetConsumeRejectsWhenNoCreditsWithoutCallingConsume(t *testing.T) {
	gin.SetMode(gin.TestMode)

	consumeCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = w.Write([]byte("{\"rate_limit_reset_credits\":{\"available_count\":0}}"))
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			consumeCalls++
			_, _ = w.Write([]byte("{\"code\":\"ok\",\"windows_reset\":1}"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	restore := overrideOpenAIQuotaResetUpstreamForTest(upstream.URL+"/backend-api/wham/usage", upstream.URL+"/backend-api/wham/rate-limit-reset-credits/consume")
	defer restore()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))
	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/openai-quota-reset/:account_key/consume", h.ConsumeOpenAIQuotaResetCredit)

	accountKey := createOpenAIQuotaResetAuthFileAccount(t, router)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/openai-quota-reset/"+accountKey+"/consume", bytes.NewReader([]byte("{}"))))
	if response.Code != http.StatusConflict {
		t.Fatalf("consume status = %d body=%s", response.Code, response.Body.String())
	}
	if consumeCalls != 0 {
		t.Fatalf("consume upstream calls = %d, want 0", consumeCalls)
	}
	if !strings.Contains(response.Body.String(), "reset_credit_unavailable") {
		t.Fatalf("response body = %s, want reset_credit_unavailable code", response.Body.String())
	}
}

func TestOpenAIQuotaResetConsumeReturnsResultAndRefreshedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	usageCalls := 0
	var gotRedeemRequestID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls++
			available := 1
			if usageCalls > 1 {
				available = 0
			}
			_, _ = w.Write([]byte("{\"plan_type\":\"plus\",\"rate_limit_reset_credits\":{\"available_count\":" + jsonInt(int64(available)) + "}}"))
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotRedeemRequestID = strings.TrimSpace(req["redeem_request_id"])
			_, _ = w.Write([]byte("{\"code\":\"success\",\"windows_reset\":2,\"credit\":{\"id\":\"credit-1\",\"status\":\"redeemed\",\"redeemed_at\":\"2026-06-18T04:24:50Z\"}}"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	restore := overrideOpenAIQuotaResetUpstreamForTest(upstream.URL+"/backend-api/wham/usage", upstream.URL+"/backend-api/wham/rate-limit-reset-credits/consume")
	defer restore()

	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(t.TempDir(), "accounts-v1.sqlite"))
	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.POST("/v0/management/gettokens/openai-quota-reset/:account_key/consume", h.ConsumeOpenAIQuotaResetCredit)

	accountKey := createOpenAIQuotaResetAuthFileAccount(t, router)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/openai-quota-reset/"+accountKey+"/consume", bytes.NewReader([]byte("{}"))))
	if response.Code != http.StatusOK {
		t.Fatalf("consume status = %d body=%s", response.Code, response.Body.String())
	}
	if gotRedeemRequestID == "" {
		t.Fatal("redeem_request_id was not sent")
	}
	body := decodeOpenAIQuotaResetMap(t, response.Body.Bytes())
	if stringFromMap(body, "status") != "success" || stringFromMap(body, "code") != "success" || intFromMap(body, "windows_reset") != 2 || intFromMap(body, "available_count") != 0 {
		t.Fatalf("response = %#v, want success/code/windows/reset remaining", body)
	}
	credit, _ := body["credit"].(map[string]interface{})
	if stringFromMap(credit, "status") != "redeemed" || stringFromMap(credit, "redeemed_at") == "" {
		t.Fatalf("credit = %#v", credit)
	}
	if usageCalls != 2 {
		t.Fatalf("usage calls = %d, want preflight + refresh", usageCalls)
	}
}

func createOpenAIQuotaResetAuthFileAccount(t *testing.T, router *gin.Engine) string {
	t.Helper()
	authJSON := "{\"type\":\"codex\",\"access_token\":\"access-token\",\"account_id\":\"chatgpt-acct-1\",\"plan_type\":\"pro\",\"email\":\"user@example.com\"}"
	body := []byte("{\"kind\":\"auth-file\",\"title\":\"Codex Pro\",\"provider\":\"codex\",\"credential\":{\"source_file_name\":\"codex-pro.json\",\"auth_json\":" + jsonString(authJSON) + ",\"auth_type\":\"codex\",\"email\":\"user@example.com\",\"plan_type\":\"pro\"}}")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v0/management/accounts", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("create account status = %d body=%s", response.Code, response.Body.String())
	}
	created := decodeOpenAIQuotaResetMap(t, response.Body.Bytes())
	accountKey := stringFromMap(created, "account_key")
	if accountKey == "" {
		t.Fatal("created account key is empty")
	}
	return accountKey
}

func decodeOpenAIQuotaResetMap(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return decoded
}

func stringFromMap(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func intFromMap(values map[string]interface{}, key string) int {
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func jsonString(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}
