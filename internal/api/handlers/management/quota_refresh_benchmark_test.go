package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
)

const (
	quotaRefreshBenchmarkAccountCount      = 1652
	quotaRefreshScaleBenchmarkAccountCount = 4000
)

func BenchmarkQuotaRefreshBatch1652Accounts(b *testing.B) {
	gin.SetMode(gin.TestMode)
	h, keys := setupQuotaRefreshBenchmarkStore(b, quotaRefreshBenchmarkAccountCount)
	router := gin.New()
	router.POST("/v0/management/gettokens/quota-refresh-batch", h.RefreshAccountQuotaBatch)
	body := mustQuotaRefreshBatchBody(b, keys)

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch", bytes.NewReader(body))
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			b.Fatalf("batch refresh status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func BenchmarkQuotaRefreshBatch4000Accounts(b *testing.B) {
	gin.SetMode(gin.TestMode)
	h, keys := setupQuotaRefreshBenchmarkStore(b, quotaRefreshScaleBenchmarkAccountCount)
	router := gin.New()
	router.POST("/v0/management/gettokens/quota-refresh-batch", h.RefreshAccountQuotaBatch)
	body := mustQuotaRefreshBatchBody(b, keys)

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch", bytes.NewReader(body))
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			b.Fatalf("batch refresh status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

func BenchmarkQuotaRefreshBatchTargetAccounts(b *testing.B) {
	for _, targetCount := range []int{1, 10, 100, quotaRefreshBenchmarkAccountCount, quotaRefreshScaleBenchmarkAccountCount} {
		totalCount := quotaRefreshBenchmarkAccountCount
		if targetCount > quotaRefreshBenchmarkAccountCount {
			totalCount = quotaRefreshScaleBenchmarkAccountCount
		}
		b.Run(fmt.Sprintf("targets_%d_total_%d", targetCount, totalCount), func(b *testing.B) {
			gin.SetMode(gin.TestMode)
			h, keys := setupQuotaRefreshBenchmarkStore(b, totalCount)
			router := gin.New()
			router.POST("/v0/management/gettokens/quota-refresh-batch", h.RefreshAccountQuotaBatch)
			body := mustQuotaRefreshBatchBody(b, keys[:targetCount])

			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh-batch", bytes.NewReader(body))
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					b.Fatalf("batch refresh status = %d body=%s", recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}

func BenchmarkQuotaRefreshSingleLoop1652Accounts(b *testing.B) {
	gin.SetMode(gin.TestMode)
	h, keys := setupQuotaRefreshBenchmarkStore(b, quotaRefreshBenchmarkAccountCount)
	router := gin.New()
	router.POST("/v0/management/gettokens/quota-refresh/:account_key", h.RefreshAccountQuota)
	body := []byte(`{"include_billing":true}`)

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		for _, key := range keys {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v0/management/gettokens/quota-refresh/"+key, bytes.NewReader(body))
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadGateway {
				b.Fatalf("single refresh status = %d body=%s", recorder.Code, recorder.Body.String())
			}
		}
	}
}

func setupQuotaRefreshBenchmarkStore(b *testing.B, count int) (*Handler, []string) {
	b.Helper()
	ctx := context.Background()
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(filepath.Join(b.TempDir(), "accounts-v1.sqlite"))
	store, err := h.openAccountStore(ctx)
	if err != nil {
		b.Fatalf("open account store: %v", err)
	}
	keys := make([]string, 0, count)
	for index := 0; index < count; index++ {
		account, err := store.CreateAccount(ctx, accountstore.AccountWrite{
			Kind:             accountstore.KindCodexAPIKey,
			Title:            fmt.Sprintf("benchmark-%04d", index),
			Provider:         "codex",
			CredentialSource: accountstore.SourceSidecarManagementAPI,
			Priority:         -index,
			CodexAPIKey: &accountstore.CodexAPIKeyCredential{
				APIKey:       fmt.Sprintf("sk-benchmark-%04d", index),
				BaseURL:      "https://api.example.com/v1",
				QuotaEnabled: false,
			},
		})
		if err != nil {
			b.Fatalf("create account %d: %v", index, err)
		}
		keys = append(keys, account.AccountKey)
	}
	return h, keys
}

func mustQuotaRefreshBatchBody(b *testing.B, keys []string) []byte {
	b.Helper()
	body, err := json.Marshal(quotaRefreshBatchRequest{
		AccountKeys:    keys,
		IncludeBilling: true,
		Concurrency:    4,
	})
	if err != nil {
		b.Fatalf("marshal batch body: %v", err)
	}
	return body
}
