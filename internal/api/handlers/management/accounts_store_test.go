package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
)

func TestOpenAccountStoreReusesInitializedStoreWhenExternalWriterHoldsLock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(dbPath)

	store, err := h.openAccountStore(context.Background())
	if err != nil {
		t.Fatalf("openAccountStore initial: %v", err)
	}
	defer store.Close()

	locker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite locker: %v", err)
	}
	defer locker.Close()
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin external write transaction: %v", err)
	}
	defer locker.Exec("ROLLBACK")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	reused, err := h.openAccountStore(ctx)
	if err != nil {
		t.Fatalf("openAccountStore with external writer lock: %v", err)
	}
	if reused != store {
		t.Fatal("openAccountStore should reuse the initialized store instead of opening and ensuring schema again")
	}
}

func TestListAccountsReopensCachedStoreAfterRecoverableReadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(dbPath)

	store, err := h.openAccountStore(ctx)
	if err != nil {
		t.Fatalf("openAccountStore: %v", err)
	}
	created, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "Primary",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:  "sk-primary",
			BaseURL: "https://api.example.com/v1",
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Simulate a cached account-store connection that entered an unrecoverable
	// driver state. Read endpoints should invalidate it and reopen the store
	// instead of requiring a full sidecar restart.
	if err := h.accountStore.Close(); err != nil {
		t.Fatalf("close cached account store: %v", err)
	}

	router := gin.New()
	router.GET("/v0/management/accounts", h.ListAccounts)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v0/management/accounts", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("accounts status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Accounts []accountstore.AccountRecord `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal accounts: %v", err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].AccountKey != created.AccountKey {
		t.Fatalf("accounts after reopen = %+v, want only %s", body.Accounts, created.AccountKey)
	}
}

func TestWriteAccountStoreErrorClassifiesRecoverableIOError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/error", func(c *gin.Context) {
		writeAccountStoreError(c, fmt.Errorf("query accounts: disk I/O error (522)"))
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/error", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error       string `json:"error"`
		Code        string `json:"code"`
		Recoverable bool   `json:"recoverable"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Code != "account_store_io_error" || !body.Recoverable {
		t.Fatalf("body = %+v, want recoverable account_store_io_error", body)
	}
	if !strings.Contains(body.Error, "disk I/O error (522)") {
		t.Fatalf("error = %q", body.Error)
	}
}

func TestAccountStoreDiagnosticsReportsReadRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(dbPath)

	store, err := h.openAccountStore(ctx)
	if err != nil {
		t.Fatalf("openAccountStore: %v", err)
	}
	if _, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "Primary",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:  "sk-primary",
			BaseURL: "https://api.example.com/v1",
		},
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := h.accountStore.Close(); err != nil {
		t.Fatalf("close cached account store: %v", err)
	}

	router := gin.New()
	router.GET("/v0/management/accounts", h.ListAccounts)
	router.GET("/v0/management/gettokens/account-store-diagnostics", h.GetAccountStoreDiagnostics)

	accountsRecorder := httptest.NewRecorder()
	router.ServeHTTP(accountsRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/accounts", nil))
	if accountsRecorder.Code != http.StatusOK {
		t.Fatalf("accounts status = %d body=%s", accountsRecorder.Code, accountsRecorder.Body.String())
	}

	diagRecorder := httptest.NewRecorder()
	router.ServeHTTP(diagRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/account-store-diagnostics", nil))
	if diagRecorder.Code != http.StatusOK {
		t.Fatalf("diagnostics status = %d body=%s", diagRecorder.Code, diagRecorder.Body.String())
	}
	var diag struct {
		PathBasename string `json:"path_basename"`
		Open         bool   `json:"open"`
		Recovery     struct {
			Count       int    `json:"count"`
			Endpoint    string `json:"last_endpoint"`
			Recovered   bool   `json:"last_recovered"`
			Error       string `json:"last_error"`
			RecoveredAt int64  `json:"last_recovered_at_unix_ms"`
		} `json:"read_recovery"`
	}
	if err := json.Unmarshal(diagRecorder.Body.Bytes(), &diag); err != nil {
		t.Fatalf("unmarshal diagnostics: %v", err)
	}
	if diag.PathBasename != "accounts-v1.sqlite" || !diag.Open {
		t.Fatalf("diagnostics path/open = %q/%v", diag.PathBasename, diag.Open)
	}
	if diag.Recovery.Count != 1 || diag.Recovery.Endpoint != "accounts" || !diag.Recovery.Recovered {
		t.Fatalf("diagnostics recovery = %+v", diag.Recovery)
	}
	if !strings.Contains(diag.Recovery.Error, "database is closed") {
		t.Fatalf("diagnostics recovery error = %q", diag.Recovery.Error)
	}
	if diag.Recovery.RecoveredAt <= 0 {
		t.Fatalf("diagnostics recovered_at missing: %+v", diag.Recovery)
	}
}

func TestAccountMigrationDryRunAndCommitEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatalf("MkdirAll authDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "codex-pro.json"), []byte(`{"type":"codex","access_token":"token"}`), 0600); err != nil {
		t.Fatalf("WriteFile auth: %v", err)
	}

	dbPath := filepath.Join(root, "accounts-v1.sqlite")
	codexStoreDir := filepath.Join(root, "codex-api-keys")
	if err := os.MkdirAll(codexStoreDir, 0700); err != nil {
		t.Fatalf("MkdirAll codexStoreDir: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir: authDir,
		CodexKey: []config.CodexKey{{
			LocalID: "codex-api-key:config-a",
			APIKey:  "sk-config",
			BaseURL: "https://api.example.com/v1",
		}},
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "deepseek",
			BaseURL: "https://api.deepseek.com",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
				APIKey: "sk-ds",
			}},
		}},
	}, nil)
	h.SetAccountStorePath(dbPath)
	applyCalls := 0
	h.SetAccountStoreApplyHook(func(context.Context) error {
		applyCalls++
		return nil
	})

	router := gin.New()
	router.POST("/v0/management/account-migration/dry-run", h.DryRunAccountMigration)
	router.POST("/v0/management/account-migration/commit", h.CommitAccountMigration)
	router.POST("/v0/management/account-migration/delete-legacy-sources", h.DeleteLegacyAccountSources)
	router.GET("/v0/management/accounts", h.ListAccounts)

	migrationBody := []byte(`{"codex_api_key_store_dirs":["` + codexStoreDir + `"]}`)
	dryRunRecorder := httptest.NewRecorder()
	router.ServeHTTP(dryRunRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/account-migration/dry-run", bytes.NewReader(migrationBody)))
	if dryRunRecorder.Code != http.StatusOK {
		t.Fatalf("dry-run status = %d body=%s", dryRunRecorder.Code, dryRunRecorder.Body.String())
	}
	var dryRun struct {
		Candidates []struct {
			AccountKey string `json:"account_key"`
			Kind       string `json:"kind"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(dryRunRecorder.Body.Bytes(), &dryRun); err != nil {
		t.Fatalf("unmarshal dry-run: %v", err)
	}
	if got, want := len(dryRun.Candidates), 3; got != want {
		t.Fatalf("dry-run candidates = %d, want %d: %s", got, want, dryRunRecorder.Body.String())
	}

	commitRecorder := httptest.NewRecorder()
	router.ServeHTTP(commitRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/account-migration/commit", bytes.NewReader(migrationBody)))
	if commitRecorder.Code != http.StatusOK {
		t.Fatalf("commit status = %d body=%s", commitRecorder.Code, commitRecorder.Body.String())
	}
	var commit struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal(commitRecorder.Body.Bytes(), &commit); err != nil {
		t.Fatalf("unmarshal commit: %v", err)
	}
	if commit.Imported != 3 || commit.Skipped != 0 {
		t.Fatalf("commit imported/skipped = %d/%d, want 3/0", commit.Imported, commit.Skipped)
	}

	accountsRecorder := httptest.NewRecorder()
	router.ServeHTTP(accountsRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/accounts", nil))
	if accountsRecorder.Code != http.StatusOK {
		t.Fatalf("accounts status = %d body=%s", accountsRecorder.Code, accountsRecorder.Body.String())
	}
	var accounts struct {
		Accounts []struct {
			AccountKey string `json:"account_key"`
			Kind       string `json:"kind"`
			Apply      string `json:"runtime_apply_status"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(accountsRecorder.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("unmarshal accounts: %v", err)
	}
	if got, want := len(accounts.Accounts), 3; got != want {
		t.Fatalf("accounts = %d, want %d: %s", got, want, accountsRecorder.Body.String())
	}
	for _, account := range accounts.Accounts {
		if account.Apply != "applied" {
			t.Fatalf("%s runtime apply = %q, want applied", account.AccountKey, account.Apply)
		}
	}
	if applyCalls != 1 {
		t.Fatalf("applyCalls after commit/list = %d, want 1", applyCalls)
	}

	commitAgainRecorder := httptest.NewRecorder()
	router.ServeHTTP(commitAgainRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/account-migration/commit", bytes.NewReader(migrationBody)))
	if commitAgainRecorder.Code != http.StatusOK {
		t.Fatalf("commit again status = %d body=%s", commitAgainRecorder.Code, commitAgainRecorder.Body.String())
	}
	if err := json.Unmarshal(commitAgainRecorder.Body.Bytes(), &commit); err != nil {
		t.Fatalf("unmarshal commit again: %v", err)
	}
	if commit.Imported != 0 || commit.Skipped != 3 {
		t.Fatalf("commit again imported/skipped = %d/%d, want 0/3", commit.Imported, commit.Skipped)
	}

	deleteLegacyRecorder := httptest.NewRecorder()
	deleteBody := []byte(`{"backup_dir":"` + filepath.Join(root, "backup") + `"}`)
	router.ServeHTTP(deleteLegacyRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/account-migration/delete-legacy-sources", bytes.NewReader(deleteBody)))
	if deleteLegacyRecorder.Code != http.StatusOK {
		t.Fatalf("delete legacy status = %d body=%s", deleteLegacyRecorder.Code, deleteLegacyRecorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(authDir, "codex-pro.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy auth file still exists or stat failed unexpectedly: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "backup")); err != nil || len(entries) == 0 {
		t.Fatalf("backup entries missing: entries=%v err=%v", entries, err)
	}
}

func TestAccountsCRUDEndpointsPreserveAccountKeyOnPatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	dbPath := filepath.Join(root, "accounts-v1.sqlite")
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	h.SetAccountStorePath(dbPath)
	applyCalls := 0
	h.SetAccountStoreApplyHook(func(context.Context) error {
		applyCalls++
		return nil
	})
	statusCalls := 0
	var statusHookDisabled bool
	h.SetAccountStoreStatusHook(func(_ context.Context, account accountstore.AccountRecord) error {
		statusCalls++
		statusHookDisabled = account.Disabled
		return nil
	})

	router := gin.New()
	router.POST("/v0/management/accounts", h.CreateAccount)
	router.GET("/v0/management/accounts/:account_key", h.GetAccount)
	router.PATCH("/v0/management/accounts/:account_key", h.PatchAccount)
	router.PATCH("/v0/management/accounts/:account_key/status", h.PatchAccountStatus)
	router.PATCH("/v0/management/accounts/:account_key/priority", h.PatchAccountPriority)
	router.DELETE("/v0/management/accounts/:account_key", h.DeleteAccount)

	createBody := []byte(`{
		"kind":"codex-api-key",
		"title":"Primary",
		"provider":"codex",
		"priority":1,
		"credential":{
			"api_key":"sk-old",
			"base_url":"https://api.example.com/v1",
			"prefix":"team-a",
			"websockets":true
		}
	}`)
	createRecorder := httptest.NewRecorder()
	router.ServeHTTP(createRecorder, httptest.NewRequest(http.MethodPost, "/v0/management/accounts", bytes.NewReader(createBody)))
	if createRecorder.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		AccountKey string `json:"account_key"`
		Revision   int    `json:"revision"`
		Apply      string `json:"runtime_apply_status"`
		Credential struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
		} `json:"codex_api_key"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}
	if created.AccountKey == "" || created.Credential.APIKey != "sk-old" {
		t.Fatalf("unexpected create response: %s", createRecorder.Body.String())
	}
	if created.Apply != "applied" || applyCalls != 1 {
		t.Fatalf("create runtime apply = %q calls=%d", created.Apply, applyCalls)
	}

	patchBody := []byte(`{
		"kind":"codex-api-key",
		"title":"Primary Updated",
		"provider":"codex",
		"priority":2,
		"disabled":true,
		"credential":{
			"api_key":"sk-new",
			"base_url":"https://api2.example.com/v1",
			"prefix":"team-b",
			"websockets":true
		}
	}`)
	patchRecorder := httptest.NewRecorder()
	router.ServeHTTP(patchRecorder, httptest.NewRequest(http.MethodPatch, "/v0/management/accounts/"+created.AccountKey, bytes.NewReader(patchBody)))
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("patch status = %d body=%s", patchRecorder.Code, patchRecorder.Body.String())
	}
	var patched struct {
		AccountKey string `json:"account_key"`
		Revision   int    `json:"revision"`
		Disabled   bool   `json:"disabled"`
		Priority   int    `json:"priority"`
		Apply      string `json:"runtime_apply_status"`
		Credential struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
		} `json:"codex_api_key"`
	}
	if err := json.Unmarshal(patchRecorder.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if patched.AccountKey != created.AccountKey {
		t.Fatalf("patch changed account key from %q to %q", created.AccountKey, patched.AccountKey)
	}
	if patched.Revision != created.Revision+1 {
		t.Fatalf("patch revision = %d, want %d", patched.Revision, created.Revision+1)
	}
	if patched.Credential.APIKey != "sk-new" || patched.Credential.BaseURL != "https://api2.example.com/v1" {
		t.Fatalf("patch credential not updated: %s", patchRecorder.Body.String())
	}
	if !patched.Disabled || patched.Priority != 2 {
		t.Fatalf("patch card fields not updated: disabled=%v priority=%d", patched.Disabled, patched.Priority)
	}
	if patched.Apply != "applied" || applyCalls != 2 {
		t.Fatalf("patch runtime apply = %q calls=%d", patched.Apply, applyCalls)
	}

	statusRecorder := httptest.NewRecorder()
	router.ServeHTTP(statusRecorder, httptest.NewRequest(http.MethodPatch, "/v0/management/accounts/"+created.AccountKey+"/status", bytes.NewReader([]byte(`{"disabled":false}`))))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status status = %d body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPatched struct {
		Disabled bool   `json:"disabled"`
		Apply    string `json:"runtime_apply_status"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPatched); err != nil {
		t.Fatalf("unmarshal status patch: %v", err)
	}
	if statusPatched.Disabled {
		t.Fatalf("status patch disabled = true, want false")
	}
	if statusPatched.Apply != "applied" {
		t.Fatalf("status patch runtime apply = %q, want previous applied state", statusPatched.Apply)
	}
	if applyCalls != 2 {
		t.Fatalf("status patch should not trigger runtime apply, calls=%d want 2", applyCalls)
	}
	if statusCalls != 1 || statusHookDisabled {
		t.Fatalf("status hook calls=%d disabled=%v, want one enabled status sync", statusCalls, statusHookDisabled)
	}

	priorityRecorder := httptest.NewRecorder()
	router.ServeHTTP(priorityRecorder, httptest.NewRequest(http.MethodPatch, "/v0/management/accounts/"+created.AccountKey+"/priority", bytes.NewReader([]byte(`{"priority":7}`))))
	if priorityRecorder.Code != http.StatusOK {
		t.Fatalf("priority status = %d body=%s", priorityRecorder.Code, priorityRecorder.Body.String())
	}

	deleteRecorder := httptest.NewRecorder()
	router.ServeHTTP(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/v0/management/accounts/"+created.AccountKey, nil))
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}

	getRecorder := httptest.NewRecorder()
	router.ServeHTTP(getRecorder, httptest.NewRequest(http.MethodGet, "/v0/management/accounts/"+created.AccountKey, nil))
	if getRecorder.Code != http.StatusNotFound {
		t.Fatalf("get deleted status = %d, want 404 body=%s", getRecorder.Code, getRecorder.Body.String())
	}
}
