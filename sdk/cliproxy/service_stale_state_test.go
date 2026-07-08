package cliproxy

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	_ "modernc.org/sqlite"
)

func TestServiceApplyCoreAuthAddOrUpdate_DeleteReAddDoesNotInheritStaleRuntimeState(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	authID := "service-stale-state-auth"
	modelID := "stale-model"
	lastRefreshedAt := time.Date(2026, time.March, 1, 8, 0, 0, 0, time.UTC)
	nextRefreshAfter := lastRefreshedAt.Add(30 * time.Minute)

	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:               authID,
		Provider:         "claude",
		Status:           coreauth.StatusActive,
		LastRefreshedAt:  lastRefreshedAt,
		NextRefreshAfter: nextRefreshAfter,
		ModelStates: map[string]*coreauth.ModelState{
			modelID: {
				Quota: coreauth.QuotaState{BackoffLevel: 7},
			},
		},
	})

	service.applyCoreAuthRemoval(context.Background(), authID)

	disabled, ok := service.coreManager.GetByID(authID)
	if !ok || disabled == nil {
		t.Fatalf("expected disabled auth after removal")
	}
	if !disabled.Disabled || disabled.Status != coreauth.StatusDisabled {
		t.Fatalf("expected disabled auth after removal, got disabled=%v status=%v", disabled.Disabled, disabled.Status)
	}
	if disabled.LastRefreshedAt.IsZero() {
		t.Fatalf("expected disabled auth to still carry prior LastRefreshedAt for regression setup")
	}
	if disabled.NextRefreshAfter.IsZero() {
		t.Fatalf("expected disabled auth to still carry prior NextRefreshAfter for regression setup")
	}

	// Reconcile prunes unsupported model state during registration, so seed the
	// disabled snapshot explicitly before exercising delete -> re-add behavior.
	disabled.ModelStates = map[string]*coreauth.ModelState{
		modelID: {
			Quota: coreauth.QuotaState{BackoffLevel: 7},
		},
	}
	if _, err := service.coreManager.Update(context.Background(), disabled); err != nil {
		t.Fatalf("seed disabled auth stale ModelStates: %v", err)
	}

	disabled, ok = service.coreManager.GetByID(authID)
	if !ok || disabled == nil {
		t.Fatalf("expected disabled auth after stale state seeding")
	}
	if len(disabled.ModelStates) == 0 {
		t.Fatalf("expected disabled auth to carry seeded ModelStates for regression setup")
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
	})

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected re-added auth to be present")
	}
	if updated.Disabled {
		t.Fatalf("expected re-added auth to be active")
	}
	if !updated.LastRefreshedAt.IsZero() {
		t.Fatalf("expected LastRefreshedAt to reset on delete -> re-add, got %v", updated.LastRefreshedAt)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected NextRefreshAfter to reset on delete -> re-add, got %v", updated.NextRefreshAfter)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected ModelStates to reset on delete -> re-add, got %d entries", len(updated.ModelStates))
	}
	if models := registry.GetGlobalRegistry().GetModelsForClient(authID); len(models) == 0 {
		t.Fatalf("expected re-added auth to re-register models in global registry")
	}
}

func TestServiceRefreshAccountStoreAuthsWithoutWatcherRegistersCodexAPIKeyModels(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/accounts-v1.sqlite"
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	account, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "DB Codex",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:     "sk-db-codex",
			BaseURL:    "https://codex.example.com/v1",
			ModelsJSON: `[]`,
		},
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	service := &Service{
		cfg:         &config.Config{AccountStoreDB: dbPath},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	if err := service.refreshAccountStoreAuths(ctx); err != nil {
		t.Fatalf("refresh account-store auths: %v", err)
	}

	var authID string
	for _, auth := range service.coreManager.List() {
		if auth != nil && auth.AccountKey == account.AccountKey {
			authID = auth.ID
			break
		}
	}
	if authID == "" {
		t.Fatalf("expected account-store codex auth for %s to be registered", account.AccountKey)
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})
	if !GlobalModelRegistry().ClientSupportsModel(authID, "gpt-5.5") {
		t.Fatalf("account-store codex auth %s should support default Codex model gpt-5.5", authID)
	}
	refreshed, err := store.GetAccount(ctx, account.AccountKey)
	if err != nil {
		t.Fatalf("get account after refresh: %v", err)
	}
	if refreshed.RuntimeRouteabilityStatus != "registered_routeable" {
		t.Fatalf("runtime routeability status = %q, want registered_routeable", refreshed.RuntimeRouteabilityStatus)
	}
	if refreshed.RuntimeRegisteredModelsCount == 0 {
		t.Fatalf("runtime registered models count = %d, want > 0", refreshed.RuntimeRegisteredModelsCount)
	}
}

func TestServiceReconcileAccountStoreRouteabilityIsolatesMisclassifiedOpenRouterCodexKey(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/accounts-v1.sqlite"
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	accountKey := "acct_00000000-0000-4000-8000-000000000001"
	seedLegacyMisclassifiedCodexAPIKeyForService(t, dbPath, accountKey)

	service := &Service{
		cfg:         &config.Config{AccountStoreDB: dbPath},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	if err := service.refreshAccountStoreAuths(ctx); err != nil {
		t.Fatalf("refresh account-store auths: %v", err)
	}
	for _, auth := range service.coreManager.List() {
		if auth != nil && auth.AccountKey == accountKey {
			t.Fatalf("misclassified known openai-compatible codex-api-key should not be registered, got auth %+v", auth)
		}
	}
	if err := service.reconcileAccountStoreRouteability(ctx); err != nil {
		t.Fatalf("reconcile account-store routeability: %v", err)
	}
	refreshed, err := store.GetAccount(ctx, accountKey)
	if err != nil {
		t.Fatalf("get account after refresh: %v", err)
	}
	if refreshed.RuntimeRouteabilityStatus != "degraded" {
		t.Fatalf("runtime routeability status = %q, want degraded", refreshed.RuntimeRouteabilityStatus)
	}
	if refreshed.RuntimeFailureClass != "misclassified_openai_compatible_provider" {
		t.Fatalf("runtime failure class = %q, want misclassified_openai_compatible_provider", refreshed.RuntimeFailureClass)
	}
	if !strings.Contains(refreshed.RuntimeRouteabilityReason, "delete and recreate") {
		t.Fatalf("routeability reason = %q, want remediation", refreshed.RuntimeRouteabilityReason)
	}
	if refreshed.RuntimeRegisteredModelsCount != 0 {
		t.Fatalf("registered models count = %d, want 0", refreshed.RuntimeRegisteredModelsCount)
	}
}

func seedLegacyMisclassifiedCodexAPIKeyForService(t *testing.T, dbPath string, accountKey string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed db: %v", err)
	}
	defer db.Close()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`
INSERT INTO account_cards(account_key, kind, title, provider, credential_source, priority, disabled, revision, metadata_json, created_at_unix_ms, updated_at_unix_ms)
VALUES (?, 'codex-api-key', 'OpenRouter', 'codex', 'sidecar-management-api', 0, 0, 1, '{}', ?, ?)`,
		accountKey,
		now,
		now,
	); err != nil {
		t.Fatalf("insert legacy account card: %v", err)
	}
	if _, err := db.Exec(`
INSERT INTO codex_api_key_accounts(account_key, api_key, api_key_fingerprint, base_url, prefix, proxy_url, websockets, quota_curl, quota_enabled, billing_curl, billing_enabled, platform_cookie, curl_variables_json, format_base_urls_json, headers_json, models_json, excluded_models_json, updated_at_unix_ms)
VALUES (?, 'sk-legacy-openrouter', 'fingerprint', 'https://openrouter.ai/api', '', '', 1, '', 0, '', 0, '', '{}', '{"openai_chat":"https://openrouter.ai/api"}', '{}', '[{"name":"tencent/hy3:free","alias":""}]', '[]', ?)`,
		accountKey,
		now,
	); err != nil {
		t.Fatalf("insert legacy codex api key: %v", err)
	}
	if _, err := db.Exec(`
INSERT INTO account_runtime_apply_state(account_key, revision, status, last_error, applied_at_unix_ms, updated_at_unix_ms)
VALUES (?, 1, 'applied', '', ?, ?)`,
		accountKey,
		now,
		now,
	); err != nil {
		t.Fatalf("insert legacy runtime apply state: %v", err)
	}
}

func TestServiceRefreshAccountStoreAuthsWithoutWatcherPreservesRuntimeOnStoreReadError(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open account store: %v", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	account, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "Runtime Preserve",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:     "sk-runtime-preserve",
			BaseURL:    "https://codex.example.com/v1",
			ModelsJSON: `[]`,
		},
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close account store: %v", err)
	}

	service := &Service{
		cfg:         &config.Config{AccountStoreDB: dbPath},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	if err := service.refreshAccountStoreAuths(ctx); err != nil {
		t.Fatalf("initial refresh account-store auths: %v", err)
	}

	var authID string
	for _, auth := range service.coreManager.List() {
		if auth != nil && auth.AccountKey == account.AccountKey {
			authID = auth.ID
			break
		}
	}
	if authID == "" {
		t.Fatalf("expected account-store codex auth for %s to be registered", account.AccountKey)
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})
	if !GlobalModelRegistry().ClientSupportsModel(authID, "gpt-5.5") {
		t.Fatalf("account-store codex auth %s should support default Codex model gpt-5.5 before read error", authID)
	}

	service.cfg.AccountStoreDB = t.TempDir()
	if err := service.refreshAccountStoreAuths(ctx); err == nil {
		t.Fatalf("refreshAccountStoreAuths should report account-store read error")
	}

	auth, ok := service.coreManager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("expected runtime auth %s to survive account-store read error", authID)
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		t.Fatalf("runtime auth should not be disabled by account-store read error: disabled=%v status=%q", auth.Disabled, auth.Status)
	}
	if !GlobalModelRegistry().ClientSupportsModel(authID, "gpt-5.5") {
		t.Fatalf("runtime auth %s should keep registered Codex models after account-store read error", authID)
	}
}

func TestServiceApplyAccountStoreDeleteRemovesOnlyTargetAccounts(t *testing.T) {
	ctx := context.Background()
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	deletedAuth := &coreauth.Auth{
		ID:         "codex-delete-target",
		AccountKey: "acct_delete_target",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{
			"source":      "account-store:codex[target]",
			"auth_kind":   "apikey",
			"account_key": "acct_delete_target",
		},
	}
	keptAuth := &coreauth.Auth{
		ID:         "codex-delete-kept",
		AccountKey: "acct_delete_kept",
		Provider:   "codex",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{
			"source":      "account-store:codex[kept]",
			"auth_kind":   "apikey",
			"account_key": "acct_delete_kept",
		},
	}
	service.applyCoreAuthAddOrUpdate(ctx, deletedAuth)
	service.applyCoreAuthAddOrUpdate(ctx, keptAuth)
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(deletedAuth.ID)
		GlobalModelRegistry().UnregisterClient(keptAuth.ID)
	})
	if !GlobalModelRegistry().ClientSupportsModel(deletedAuth.ID, "gpt-5.5") {
		t.Fatalf("expected deleted setup auth to support gpt-5.5 before targeted delete")
	}
	if !GlobalModelRegistry().ClientSupportsModel(keptAuth.ID, "gpt-5.5") {
		t.Fatalf("expected kept setup auth to support gpt-5.5 before targeted delete")
	}

	if err := service.applyAccountStoreDelete(ctx, []string{deletedAuth.AccountKey}); err != nil {
		t.Fatalf("applyAccountStoreDelete: %v", err)
	}

	deleted, ok := service.coreManager.GetByID(deletedAuth.ID)
	if !ok || deleted == nil {
		t.Fatalf("expected deleted auth to remain as disabled tombstone")
	}
	if !deleted.Disabled || deleted.Status != coreauth.StatusDisabled {
		t.Fatalf("deleted auth state = disabled=%v status=%q, want disabled tombstone", deleted.Disabled, deleted.Status)
	}
	if GlobalModelRegistry().ClientSupportsModel(deletedAuth.ID, "gpt-5.5") {
		t.Fatalf("deleted auth should no longer support gpt-5.5 after targeted delete")
	}
	kept, ok := service.coreManager.GetByID(keptAuth.ID)
	if !ok || kept == nil {
		t.Fatalf("expected kept auth to remain present")
	}
	if kept.Disabled || kept.Status == coreauth.StatusDisabled {
		t.Fatalf("kept auth should remain active, got disabled=%v status=%q", kept.Disabled, kept.Status)
	}
	if !GlobalModelRegistry().ClientSupportsModel(keptAuth.ID, "gpt-5.5") {
		t.Fatalf("kept auth should keep gpt-5.5 after targeted delete")
	}
}

func TestServiceInitializeAccountStoreRuntimeMigratesLegacySchemaAndRegistersRuntimeAuths(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	createLegacyServiceAccountStoreDB(t, dbPath)

	service := &Service{
		cfg:         &config.Config{AccountStoreDB: dbPath},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	if err := service.initializeAccountStoreRuntime(ctx); err != nil {
		t.Fatalf("initializeAccountStoreRuntime: %v", err)
	}

	var authID string
	for _, auth := range service.coreManager.List() {
		if auth != nil && auth.AccountKey == "acct_dd2172ea-9dd9-458a-88bd-590cc55a468c" {
			authID = auth.ID
			break
		}
	}
	if authID == "" {
		t.Fatal("expected legacy account-store codex auth to be registered at startup")
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})
	if !GlobalModelRegistry().ClientSupportsModel(authID, "gpt-5.5") {
		t.Fatalf("startup account-store auth %s should support default Codex model gpt-5.5", authID)
	}

	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen migrated account store: %v", err)
	}
	defer store.Close()
	account, err := store.GetAccount(ctx, "acct_dd2172ea-9dd9-458a-88bd-590cc55a468c")
	if err != nil {
		t.Fatalf("get migrated account: %v", err)
	}
	if account.RuntimeRouteabilityStatus != "registered_routeable" {
		t.Fatalf("runtime routeability status = %q, want registered_routeable", account.RuntimeRouteabilityStatus)
	}
}

func createLegacyServiceAccountStoreDB(t *testing.T, dbPath string) {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open legacy sqlite: %v", err)
	}
	defer db.Close()

	statements := []string{
		`CREATE TABLE account_cards (
		  account_key TEXT PRIMARY KEY,
		  kind TEXT NOT NULL,
		  title TEXT NOT NULL DEFAULT '',
		  provider TEXT NOT NULL DEFAULT '',
		  credential_source TEXT NOT NULL DEFAULT '',
		  priority INTEGER NOT NULL DEFAULT 0,
		  disabled INTEGER NOT NULL DEFAULT 0,
		  revision INTEGER NOT NULL DEFAULT 1,
		  metadata_json TEXT NOT NULL DEFAULT '{}',
		  created_at_unix_ms INTEGER NOT NULL,
		  updated_at_unix_ms INTEGER NOT NULL,
		  deleted_at_unix_ms INTEGER
		)`,
		`CREATE TABLE codex_api_key_accounts (
		  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
		  api_key TEXT NOT NULL,
		  api_key_fingerprint TEXT NOT NULL DEFAULT '',
		  base_url TEXT NOT NULL,
		  prefix TEXT NOT NULL DEFAULT '',
		  proxy_url TEXT NOT NULL DEFAULT '',
		  websockets INTEGER NOT NULL DEFAULT 1,
		  quota_curl TEXT NOT NULL DEFAULT '',
		  quota_enabled INTEGER NOT NULL DEFAULT 0,
		  billing_curl TEXT NOT NULL DEFAULT '',
		  billing_enabled INTEGER NOT NULL DEFAULT 0,
		  format_base_urls_json TEXT NOT NULL DEFAULT '{}',
		  headers_json TEXT NOT NULL DEFAULT '{}',
		  models_json TEXT NOT NULL DEFAULT '[]',
		  excluded_models_json TEXT NOT NULL DEFAULT '[]',
		  updated_at_unix_ms INTEGER NOT NULL,
		  platform_cookie TEXT NOT NULL DEFAULT '',
		  curl_variables_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE account_runtime_apply_state (
		  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
		  revision INTEGER NOT NULL,
		  status TEXT NOT NULL,
		  last_error TEXT NOT NULL DEFAULT '',
		  applied_at_unix_ms INTEGER NOT NULL DEFAULT 0,
		  updated_at_unix_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE openai_compatible_accounts (
		  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
		  provider_name TEXT NOT NULL DEFAULT '',
		  runtime_provider_key TEXT NOT NULL,
		  base_url TEXT NOT NULL,
		  prefix TEXT NOT NULL DEFAULT '',
		  api_key_entries_json TEXT NOT NULL DEFAULT '[]',
		  headers_json TEXT NOT NULL DEFAULT '{}',
		  models_json TEXT NOT NULL DEFAULT '[]',
		  updated_at_unix_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE auth_file_accounts (
		  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
		  source_file_name TEXT NOT NULL DEFAULT '',
		  auth_json TEXT NOT NULL,
		  auth_fingerprint TEXT NOT NULL DEFAULT '',
		  auth_type TEXT NOT NULL DEFAULT '',
		  email TEXT NOT NULL DEFAULT '',
		  plan_type TEXT NOT NULL DEFAULT '',
		  status TEXT NOT NULL DEFAULT '',
		  status_message TEXT NOT NULL DEFAULT '',
		  modified_unix_ms INTEGER NOT NULL DEFAULT 0,
		  size_bytes INTEGER NOT NULL DEFAULT 0,
		  updated_at_unix_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE account_runtime_identities (
		  identity_key TEXT PRIMARY KEY,
		  account_key TEXT NOT NULL REFERENCES account_cards(account_key) ON DELETE CASCADE,
		  identity_kind TEXT NOT NULL,
		  created_at_unix_ms INTEGER NOT NULL,
		  updated_at_unix_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE account_migration_sources (
		  id TEXT PRIMARY KEY,
		  account_key TEXT NOT NULL REFERENCES account_cards(account_key) ON DELETE CASCADE,
		  source_kind TEXT NOT NULL,
		  source_path TEXT NOT NULL DEFAULT '',
		  source_key TEXT NOT NULL DEFAULT '',
		  source_fingerprint TEXT NOT NULL DEFAULT '',
		  imported_at_unix_ms INTEGER NOT NULL,
		  deleted_at_unix_ms INTEGER NOT NULL DEFAULT 0,
		  backup_path TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO account_cards (
		  account_key, kind, title, provider, credential_source, priority, disabled, revision, metadata_json, created_at_unix_ms, updated_at_unix_ms, deleted_at_unix_ms
		) VALUES (
		  'acct_dd2172ea-9dd9-458a-88bd-590cc55a468c', 'codex-api-key', '公司 1', 'codex', 'legacy-gettokens-codex-api-key', 1, 0, 3, '{}', 1780107740986, 1781490144984, NULL
		)`,
		`INSERT INTO codex_api_key_accounts (
		  account_key, api_key, api_key_fingerprint, base_url, prefix, proxy_url, websockets, quota_curl, quota_enabled, billing_curl, billing_enabled, format_base_urls_json, headers_json, models_json, excluded_models_json, updated_at_unix_ms, platform_cookie, curl_variables_json
		) VALUES (
		  'acct_dd2172ea-9dd9-458a-88bd-590cc55a468c', 'sk-legacy-company-1', 'legacy-company-1', 'http://cpa.host.dxy/v1', '', '', 0, '', 0, '', 0, '{}', '{}', '[]', '[]', 1781490144984, '', '{}'
		)`,
		`INSERT INTO account_runtime_apply_state (
		  account_key, revision, status, last_error, applied_at_unix_ms, updated_at_unix_ms
		) VALUES (
		  'acct_dd2172ea-9dd9-458a-88bd-590cc55a468c', 3, 'applied', '', 1781490144984, 1781490144984
		)`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec legacy schema statement failed: %v\nsql: %s", err, stmt)
		}
	}
}

func TestServiceReconcileAccountStoreRouteabilityRepairsMissingRegistryModels(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/accounts-v1.sqlite"
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	account, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "Repair Models",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:     "sk-repair-models",
			BaseURL:    "https://codex.example.com/v1",
			ModelsJSON: `[]`,
		},
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	service := &Service{
		cfg:         &config.Config{AccountStoreDB: dbPath},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	if err := service.refreshAccountStoreAuths(ctx); err != nil {
		t.Fatalf("initial refresh account-store auths: %v", err)
	}

	var authID string
	for _, auth := range service.coreManager.List() {
		if auth != nil && auth.AccountKey == account.AccountKey {
			authID = auth.ID
			break
		}
	}
	if authID == "" {
		t.Fatalf("expected registered auth for %s", account.AccountKey)
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	GlobalModelRegistry().UnregisterClient(authID)

	if err := service.reconcileAccountStoreRouteability(ctx); err != nil {
		t.Fatalf("reconcileAccountStoreRouteability: %v", err)
	}
	if !GlobalModelRegistry().ClientSupportsModel(authID, "gpt-5.5") {
		t.Fatalf("expected bounded reconcile to restore registry models for %s", authID)
	}
	refreshed, err := store.GetAccount(ctx, account.AccountKey)
	if err != nil {
		t.Fatalf("get account after repair: %v", err)
	}
	if refreshed.RuntimeRouteabilityStatus != "registered_routeable" {
		t.Fatalf("runtime routeability status = %q, want registered_routeable", refreshed.RuntimeRouteabilityStatus)
	}
	if refreshed.RuntimeRepairOutcome != "recovered" {
		t.Fatalf("runtime repair outcome = %q, want recovered", refreshed.RuntimeRepairOutcome)
	}
	if refreshed.RuntimeRepairAction != "resynthesize_refresh" {
		t.Fatalf("runtime repair action = %q, want resynthesize_refresh", refreshed.RuntimeRepairAction)
	}
	if refreshed.RuntimeRepairTriggerStatus != "applied_not_registered" {
		t.Fatalf("runtime repair trigger status = %q, want applied_not_registered", refreshed.RuntimeRepairTriggerStatus)
	}
	if refreshed.RuntimeRepairTriggerClass != "runtime_models_missing" {
		t.Fatalf("runtime repair trigger class = %q, want runtime_models_missing", refreshed.RuntimeRepairTriggerClass)
	}
	if refreshed.RuntimeFailureClass != "" {
		t.Fatalf("runtime failure class = %q, want empty after recovery", refreshed.RuntimeFailureClass)
	}
	if refreshed.LastRuntimeRepairAtUnixMs == 0 {
		t.Fatal("expected last runtime repair timestamp to be recorded")
	}
}

func TestServiceReconcileAccountStoreRouteabilityRepairsDegradedRuntimeAuth(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/accounts-v1.sqlite"
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	account, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "Repair Degraded",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:     "sk-repair-degraded",
			BaseURL:    "https://codex.example.com/v1",
			ModelsJSON: `[]`,
		},
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	service := &Service{
		cfg:         &config.Config{AccountStoreDB: dbPath},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	if err := service.refreshAccountStoreAuths(ctx); err != nil {
		t.Fatalf("initial refresh account-store auths: %v", err)
	}

	var authID string
	var seeded *coreauth.Auth
	for _, auth := range service.coreManager.List() {
		if auth != nil && auth.AccountKey == account.AccountKey {
			authID = auth.ID
			seeded = auth.Clone()
			break
		}
	}
	if authID == "" || seeded == nil {
		t.Fatalf("expected registered auth for %s", account.AccountKey)
	}
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	seeded.Status = coreauth.StatusError
	seeded.Unavailable = true
	seeded.StatusMessage = "stale upstream error"
	seeded.LastError = &coreauth.Error{Code: "upstream_error", Message: "stale upstream error"}
	if _, err := service.coreManager.Update(ctx, seeded); err != nil {
		t.Fatalf("seed degraded auth: %v", err)
	}

	if err := service.reconcileAccountStoreRouteability(ctx); err != nil {
		t.Fatalf("reconcileAccountStoreRouteability: %v", err)
	}
	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected repaired auth to exist")
	}
	if updated.Status != coreauth.StatusActive || updated.Unavailable || updated.LastError != nil || updated.StatusMessage != "" {
		t.Fatalf("updated auth state = status=%q unavailable=%v message=%q last_error=%v, want clean active", updated.Status, updated.Unavailable, updated.StatusMessage, updated.LastError)
	}
	refreshed, err := store.GetAccount(ctx, account.AccountKey)
	if err != nil {
		t.Fatalf("get account after repair: %v", err)
	}
	if refreshed.RuntimeRouteabilityStatus != "registered_routeable" {
		t.Fatalf("runtime routeability status = %q, want registered_routeable", refreshed.RuntimeRouteabilityStatus)
	}
	if refreshed.RuntimeRepairOutcome != "recovered" {
		t.Fatalf("runtime repair outcome = %q, want recovered", refreshed.RuntimeRepairOutcome)
	}
	if refreshed.RuntimeRepairAction != "resynthesize_refresh" {
		t.Fatalf("runtime repair action = %q, want resynthesize_refresh", refreshed.RuntimeRepairAction)
	}
	if refreshed.RuntimeRepairTriggerStatus != "degraded" {
		t.Fatalf("runtime repair trigger status = %q, want degraded", refreshed.RuntimeRepairTriggerStatus)
	}
	if refreshed.RuntimeRepairTriggerClass != "runtime_auth_unavailable" {
		t.Fatalf("runtime repair trigger class = %q, want runtime_auth_unavailable", refreshed.RuntimeRepairTriggerClass)
	}
	if refreshed.RuntimeFailureClass != "" {
		t.Fatalf("runtime failure class = %q, want empty after recovery", refreshed.RuntimeFailureClass)
	}
	if refreshed.LastRuntimeRepairAtUnixMs == 0 {
		t.Fatal("expected last runtime repair timestamp to be recorded")
	}
}

func TestServiceApplyCoreAuthAddOrUpdate_CredentialRefreshResetsStaleRuntimeState(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	authID := "service-credential-refresh-reset"
	modelID := "stale-model"
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	staleRetry := time.Now().Add(30 * time.Minute)
	if _, err := service.coreManager.Register(context.Background(), &coreauth.Auth{
		ID:               authID,
		Provider:         "codex",
		Status:           coreauth.StatusError,
		StatusMessage:    "token refresh failed",
		Unavailable:      true,
		LastError:        &coreauth.Error{Code: "unauthorized", HTTPStatus: 401, Message: "token refresh failed"},
		LastRefreshedAt:  time.Now().Add(-24 * time.Hour),
		NextRefreshAfter: staleRetry,
		Metadata: map[string]any{
			"refresh_token": "old-refresh-token",
			"access_token":  "old-access-token",
		},
		ModelStates: map[string]*coreauth.ModelState{
			modelID: {
				Status:         coreauth.StatusError,
				StatusMessage:  "stale quota",
				Unavailable:    true,
				NextRetryAfter: staleRetry,
				LastError:      &coreauth.Error{HTTPStatus: 429, Message: "quota"},
			},
		},
	}); err != nil {
		t.Fatalf("seed stale auth: %v", err)
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"refresh_token": "new-refresh-token",
			"access_token":  "new-access-token",
			"last_refresh":  time.Now().Format(time.RFC3339),
		},
	})

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected refreshed auth to be present")
	}
	if updated.Status != coreauth.StatusActive || updated.Unavailable || updated.StatusMessage != "" || updated.LastError != nil {
		t.Fatalf("refreshed auth state = status=%q unavailable=%v message=%q last_error=%v, want clean active", updated.Status, updated.Unavailable, updated.StatusMessage, updated.LastError)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected NextRefreshAfter to reset after credential refresh, got %v", updated.NextRefreshAfter)
	}
	if !updated.LastRefreshedAt.IsZero() {
		t.Fatalf("expected LastRefreshedAt to reset after credential refresh, got %v", updated.LastRefreshedAt)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected ModelStates to reset after credential refresh, got %d entries", len(updated.ModelStates))
	}
}

func TestServiceApplyCoreAuthAddOrUpdate_HealthyAuthClearsTransientRouteGuards(t *testing.T) {
	authID := "codex:apikey:3df2001c2d1b"
	accountKey := "acct_dd2172ea-9dd9-458a-88bd-590cc55a468c"
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceAuthError)
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceUpstreamRateLimit)
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceUpstreamTransientErr)
	t.Cleanup(func() {
		gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceAuthError)
		gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceUpstreamRateLimit)
		gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceUpstreamTransientErr)
		GlobalModelRegistry().UnregisterClient(authID)
	})

	gettokenshooks.MarkAccountRouteGuardBlocked(gettokenshooks.AccountRouteGuardBlock{
		Source:     gettokenshooks.AccountRouteGuardSourceAuthError,
		AuthID:     authID,
		AccountKey: accountKey,
		Reason:     "stale auth error",
	})
	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         authID,
		AccountKey: accountKey,
		Provider:   "codex",
	}}); len(got) != 1 || got[0] != authID {
		t.Fatalf("deny ids before healthy update = %#v, want stale auth blocked", got)
	}

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:         authID,
		AccountKey: accountKey,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	})

	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         authID,
		AccountKey: accountKey,
		Provider:   "codex",
	}}); len(got) != 0 {
		t.Fatalf("deny ids after healthy update = %#v, want transient route guards cleared", got)
	}
}

func TestForceHomeRuntimeConfigEnablesUsageStatistics(t *testing.T) {
	cfg := &config.Config{
		UsageStatisticsEnabled: false,
	}

	forceHomeRuntimeConfig(cfg)

	if !cfg.UsageStatisticsEnabled {
		t.Fatal("expected home runtime config to force usage statistics enabled")
	}
}

func TestServiceApplyCoreAuthAddOrUpdate_DisablingCodexAuthGuardsRouteAndClosesWebsocket(t *testing.T) {
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	t.Cleanup(func() {
		gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	authID := "codex-disable-route-guard-auth"
	var closed []string
	previousClose := closeCodexWebsocketSessionsForAuthID
	closeCodexWebsocketSessionsForAuthID = func(id string, reason string) {
		closed = append(closed, id+":"+reason)
	}
	t.Cleanup(func() {
		closeCodexWebsocketSessionsForAuthID = previousClose
		GlobalModelRegistry().UnregisterClient(authID)
	})

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	})
	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusDisabled,
		Disabled: true,
	})

	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{
		{ID: authID, Provider: "codex"},
		{ID: "other-codex-auth", Provider: "codex"},
	}); len(got) != 1 || got[0] != authID {
		t.Fatalf("manual route guard deny ids = %#v, want disabled auth", got)
	}
	if len(closed) != 1 || closed[0] != authID+":auth_disabled" {
		t.Fatalf("closed websocket sessions = %#v, want disabled auth close", closed)
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	})
	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("manual route guard deny ids after re-enable = %#v, want empty", got)
	}
}

func TestServiceApplyAccountStoreStatusChangeGuardsRouteAndClosesWebsocket(t *testing.T) {
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	t.Cleanup(func() {
		gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	authID := "account-store-status-codex-auth"
	accountKey := "acct_00000000-0000-4000-8000-000000000010"
	var closed []string
	previousClose := closeCodexWebsocketSessionsForAuthID
	closeCodexWebsocketSessionsForAuthID = func(id string, reason string) {
		closed = append(closed, id+":"+reason)
	}
	t.Cleanup(func() {
		closeCodexWebsocketSessionsForAuthID = previousClose
		GlobalModelRegistry().UnregisterClient(authID)
	})

	if _, err := service.coreManager.Register(context.Background(), &coreauth.Auth{
		ID:         authID,
		AccountKey: accountKey,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := service.applyAccountStoreStatusChange(context.Background(), accountstore.AccountRecord{
		AccountKey: accountKey,
		Disabled:   true,
	}); err != nil {
		t.Fatalf("disable status change: %v", err)
	}
	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{
		{ID: authID, AccountKey: accountKey, Provider: "codex"},
		{ID: "other-codex-auth", AccountKey: "acct_00000000-0000-4000-8000-000000000011", Provider: "codex"},
	}); len(got) != 1 || got[0] != authID {
		t.Fatalf("manual route guard deny ids = %#v, want disabled account auth", got)
	}
	if len(closed) != 1 || closed[0] != authID+":auth_disabled" {
		t.Fatalf("closed websocket sessions = %#v, want disabled auth close", closed)
	}
	if auth, ok := service.coreManager.GetByID(authID); !ok || auth == nil || !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("runtime auth after disable = %+v, want disabled route state", auth)
	}

	if err := service.applyAccountStoreStatusChange(context.Background(), accountstore.AccountRecord{
		AccountKey: accountKey,
		Disabled:   false,
	}); err != nil {
		t.Fatalf("enable status change: %v", err)
	}
	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, AccountKey: accountKey, Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("manual route guard deny ids after enable = %#v, want empty", got)
	}
	if auth, ok := service.coreManager.GetByID(authID); !ok || auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		t.Fatalf("runtime auth after enable = %+v, want active route state", auth)
	}
}

func TestApplyHomeOverlayForcesUsageStatisticsEnabled(t *testing.T) {
	baseCfg := &config.Config{}
	baseCfg.Home.Enabled = true
	service := &Service{cfg: baseCfg}

	service.applyHomeOverlay(&config.Config{
		UsageStatisticsEnabled: false,
	})

	if service.cfg == nil || !service.cfg.UsageStatisticsEnabled {
		t.Fatal("expected home overlay to force usage statistics enabled")
	}
	if !service.cfg.Home.Enabled {
		t.Fatal("expected home overlay to preserve local home settings")
	}
}
