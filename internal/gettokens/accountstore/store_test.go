package accountstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "modernc.org/sqlite"
)

func TestStoreEnsureSchemaCreatesTablesAndRestrictivePermissions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "accounts-v1.sqlite")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Dir(dbPath))
		if err != nil {
			t.Fatalf("stat db dir: %v", err)
		}
		if got := dirInfo.Mode().Perm(); got != 0700 {
			t.Fatalf("db dir mode = %o, want 0700", got)
		}

		dbInfo, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("stat db file: %v", err)
		}
		if got := dbInfo.Mode().Perm(); got != 0600 {
			t.Fatalf("db file mode = %o, want 0600", got)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	for _, table := range []string{
		"account_store_meta",
		"account_cards",
		"codex_api_key_accounts",
		"auth_file_accounts",
		"openai_compatible_accounts",
		"account_runtime_identities",
		"account_runtime_apply_state",
		"account_migration_sources",
	} {
		var name string
		err := db.QueryRowContext(
			context.Background(),
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?",
			table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %s not found: %v", table, err)
		}
	}

	var version string
	if err := db.QueryRowContext(context.Background(), "SELECT value FROM account_store_meta WHERE key='schema_version'").Scan(&version); err != nil {
		t.Fatalf("schema_version missing: %v", err)
	}
	if version != "1" {
		t.Fatalf("schema_version = %q, want 1", version)
	}
}

func TestStoreEnsureSchemaWaitsForConcurrentSQLiteWriter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	store.Close()

	locker, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	defer locker.Close()
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		t.Fatalf("set locker busy timeout: %v", err)
	}
	tx, err := locker.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin locker tx: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(), "UPDATE account_store_meta SET value=value WHERE key='schema_version'"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("hold write lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		next, err := Open(dbPath)
		if err != nil {
			done <- err
			return
		}
		defer next.Close()
		done <- next.EnsureSchema(context.Background())
	}()

	select {
	case err := <-done:
		_ = tx.Rollback()
		if err == nil {
			t.Fatal("EnsureSchema returned before the writer released the sqlite lock")
		}
		if isSQLiteBusyError(err) {
			t.Fatalf("EnsureSchema returned SQLITE_BUSY before waiting: %v", err)
		}
		t.Fatalf("EnsureSchema returned unexpected early error: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit locker tx: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureSchema after lock release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureSchema did not finish after sqlite lock was released")
	}
}

func isSQLiteBusyError(err error) bool {
	for err != nil {
		if strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked") {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func TestDryRunLegacyImportFindsLegacySourcesWithoutWritingOrDeleting(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	currentStoreDir := filepath.Join(root, "gettokens-data", "codex-api-keys")
	legacyStoreDir := filepath.Join(root, "gettokens", "codex-api-keys")
	mustMkdir(t, authDir)
	mustMkdir(t, currentStoreDir)
	mustMkdir(t, legacyStoreDir)

	authPath := filepath.Join(authDir, "codex-pro.json")
	mustWriteJSON(t, authPath, map[string]any{
		"type":         "codex",
		"email":        "user@example.com",
		"access_token": "access-token",
	})
	mustWriteJSON(t, filepath.Join(authDir, "claude.json"), map[string]any{"type": "claude"})

	mustWriteJSON(t, filepath.Join(currentStoreDir, "first.json"), map[string]any{
		"local-id":        "codex-api-key:local-first",
		"api-key":         "sk-shared",
		"label":           "First",
		"base-url":        "https://api.example.com/v1",
		"prefix":          "team-a",
		"proxy-url":       "http://proxy.local:8080",
		"quota-curl":      "curl https://quota.example.com",
		"quota-enabled":   true,
		"billing-curl":    "curl https://billing.example.com",
		"billing-enabled": true,
		"headers": map[string]string{
			"X-Team": "A",
		},
		"models": []map[string]string{{
			"name":  "gpt-5",
			"alias": "gpt-5",
		}},
	})
	mustWriteJSON(t, filepath.Join(legacyStoreDir, "second.json"), map[string]any{
		"local-id": "codex-api-key:local-second",
		"api-key":  "sk-legacy",
		"label":    "Second",
		"base-url": "https://legacy.example.com/v1",
	})

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				LocalID:  "codex-api-key:config-a",
				APIKey:   "sk-duplicate",
				BaseURL:  "https://config.example.com/v1",
				Prefix:   "dup",
				Priority: 3,
			},
			{
				LocalID:  "codex-api-key:config-b",
				APIKey:   "sk-duplicate",
				BaseURL:  "https://config.example.com/v1",
				Prefix:   "dup",
				Priority: 4,
				Disabled: true,
			},
		},
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "deepseek",
			BaseURL: "https://api.deepseek.com",
			Prefix:  "ds",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
				APIKey:   "sk-openai-compatible",
				ProxyURL: "http://proxy.local:9000",
			}},
			Headers: map[string]string{"X-Provider": "DeepSeek"},
			Models: []config.OpenAICompatibilityModel{{
				Name:  "deepseek-chat",
				Alias: "deepseek-chat",
			}},
		}},
	}

	report, err := DryRunLegacyImport(context.Background(), LegacySources{
		AuthDir:              authDir,
		CodexAPIKeyStoreDirs: []string{currentStoreDir, legacyStoreDir},
		Config:               cfg,
	})
	if err != nil {
		t.Fatalf("DryRunLegacyImport: %v", err)
	}

	if got, want := countKind(report.Candidates, KindAuthFile), 1; got != want {
		t.Fatalf("auth-file candidates = %d, want %d", got, want)
	}
	if got, want := countKind(report.Candidates, KindCodexAPIKey), 4; got != want {
		t.Fatalf("codex-api-key candidates = %d, want %d", got, want)
	}
	if got, want := countKind(report.Candidates, KindOpenAICompatible), 1; got != want {
		t.Fatalf("openai-compatible candidates = %d, want %d", got, want)
	}

	configCandidates := candidatesBySource(report.Candidates, SourceLegacyConfigCodexAPIKey)
	if got, want := len(configCandidates), 2; got != want {
		t.Fatalf("config codex-api-key candidates = %d, want %d", got, want)
	}
	if configCandidates[0].AccountKey == configCandidates[1].AccountKey {
		t.Fatalf("duplicate config entries must get distinct account keys: %q", configCandidates[0].AccountKey)
	}
	for _, candidate := range report.Candidates {
		if candidate.AccountKey == "" || !IsAccountKey(candidate.AccountKey) {
			t.Fatalf("candidate %s account_key = %q, want acct_<uuid>", candidate.LegacyID, candidate.AccountKey)
		}
	}

	if _, err := os.Stat(authPath); err != nil {
		t.Fatalf("dry-run deleted auth file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(currentStoreDir, "first.json")); err != nil {
		t.Fatalf("dry-run deleted codex api key file: %v", err)
	}
}

func TestDryRunLegacyImportInfersAuthFilePlanTypeFromFileNameFallback(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	mustMkdir(t, authDir)

	authPath := filepath.Join(authDir, "codex-demo@example.com-plus.json")
	mustWriteJSON(t, authPath, map[string]any{
		"type":  "codex",
		"email": "demo@example.com",
	})

	report, err := DryRunLegacyImport(context.Background(), LegacySources{AuthDir: authDir})
	if err != nil {
		t.Fatalf("DryRunLegacyImport: %v", err)
	}
	if got, want := len(report.Candidates), 1; got != want {
		t.Fatalf("candidates = %d, want %d", got, want)
	}
	if report.Candidates[0].AuthFile == nil {
		t.Fatalf("expected auth file credential: %#v", report.Candidates[0])
	}
	if got, want := report.Candidates[0].AuthFile.PlanType, "plus"; got != want {
		t.Fatalf("plan type = %q, want %q", got, want)
	}
}

func TestCommitImportWritesAccountsAndIsIdempotentByMigrationSource(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	report := &MigrationReport{
		GeneratedAtUnixMs: 1700000000000,
		Candidates: []ImportCandidate{
			{
				AccountKey:        "acct_00000000-0000-4000-8000-000000000001",
				Kind:              KindCodexAPIKey,
				Title:             "Config A",
				Provider:          "codex",
				CredentialSource:  SourceLegacyConfigCodexAPIKey,
				Priority:          10,
				LegacyID:          "codex-api-key:config-a",
				SourceKey:         "codex-api-key:config-a",
				SourceFingerprint: "fingerprint-config-a",
				CodexAPIKey: &CodexAPIKeyCredential{
					APIKey:             "sk-same",
					APIKeyFingerprint:  "fp-a",
					BaseURL:            "https://api.example.com/v1",
					Prefix:             "team-a/",
					Websockets:         true,
					HeadersJSON:        `{"X-Team":"A"}`,
					ModelsJSON:         `[{"name":"gpt-5","alias":"gpt-5"}]`,
					ExcludedModelsJSON: `["gpt-4"]`,
				},
			},
			{
				AccountKey:        "acct_00000000-0000-4000-8000-000000000002",
				Kind:              KindCodexAPIKey,
				Title:             "Config B",
				Provider:          "codex",
				CredentialSource:  SourceLegacyConfigCodexAPIKey,
				Priority:          11,
				Disabled:          true,
				LegacyID:          "codex-api-key:config-b",
				SourceKey:         "codex-api-key:config-b",
				SourceFingerprint: "fingerprint-config-b",
				CodexAPIKey: &CodexAPIKeyCredential{
					APIKey:            "sk-same",
					APIKeyFingerprint: "fp-b",
					BaseURL:           "https://api.example.com/v1",
					Prefix:            "team-a/",
					Websockets:        true,
				},
			},
			{
				AccountKey:        "acct_00000000-0000-4000-8000-000000000003",
				Kind:              KindAuthFile,
				Title:             "codex-pro",
				Provider:          "codex",
				CredentialSource:  SourceLegacyAuthFile,
				LegacyID:          "auth-file:codex-pro.json",
				SourcePath:        "/tmp/codex-pro.json",
				SourceKey:         "codex-pro.json",
				SourceFingerprint: "fingerprint-auth",
				AuthFile: &AuthFileCredential{
					SourceFileName: "codex-pro.json",
					AuthJSON:       `{"type":"codex","access_token":"token"}`,
					AuthType:       "codex",
					Email:          "user@example.com",
				},
			},
			{
				AccountKey:        "acct_00000000-0000-4000-8000-000000000004",
				Kind:              KindOpenAICompatible,
				Title:             "deepseek",
				Provider:          "deepseek",
				CredentialSource:  SourceLegacyConfigOpenAICompatible,
				LegacyID:          "openai-compatible:deepseek",
				SourceKey:         "deepseek",
				SourceFingerprint: "fingerprint-openai-compatible",
				OpenAICompatible: &OpenAICompatibleCredential{
					ProviderName:       "deepseek",
					RuntimeProviderKey: "openai-compatible:acct_00000000-0000-4000-8000-000000000004",
					BaseURL:            "https://api.deepseek.com",
					Prefix:             "ds/",
					APIKeyEntriesJSON:  `[{"api-key":"sk-ds"}]`,
					HeadersJSON:        `{"X-Provider":"DeepSeek"}`,
					ModelsJSON:         `[{"name":"deepseek-chat","alias":"deepseek-chat"}]`,
				},
			},
		},
	}

	commit, err := store.CommitImport(ctx, report)
	if err != nil {
		t.Fatalf("CommitImport: %v", err)
	}
	if commit.Imported != 4 || commit.Skipped != 0 {
		t.Fatalf("first commit imported/skipped = %d/%d, want 4/0", commit.Imported, commit.Skipped)
	}

	again, err := store.CommitImport(ctx, report)
	if err != nil {
		t.Fatalf("CommitImport again: %v", err)
	}
	if again.Imported != 0 || again.Skipped != 4 {
		t.Fatalf("second commit imported/skipped = %d/%d, want 0/4", again.Imported, again.Skipped)
	}

	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if got, want := len(accounts), 4; got != want {
		t.Fatalf("accounts len = %d, want %d", got, want)
	}
	if got, want := countAccountKind(accounts, KindCodexAPIKey), 2; got != want {
		t.Fatalf("codex api key accounts = %d, want %d", got, want)
	}
	for _, account := range accounts {
		if !IsAccountKey(account.AccountKey) {
			t.Fatalf("account key = %q, want acct_<uuid>", account.AccountKey)
		}
		if account.Revision != 1 {
			t.Fatalf("%s revision = %d, want 1", account.AccountKey, account.Revision)
		}
		if account.RuntimeApplyStatus != "pending" {
			t.Fatalf("%s runtime apply = %q, want pending", account.AccountKey, account.RuntimeApplyStatus)
		}
	}

	first := accountByKey(t, accounts, "acct_00000000-0000-4000-8000-000000000001")
	if first.CodexAPIKey == nil || first.CodexAPIKey.APIKey != "sk-same" || first.CodexAPIKey.HeadersJSON != `{"X-Team":"A"}` {
		t.Fatalf("first codex account did not round-trip: %+v", first.CodexAPIKey)
	}
	second := accountByKey(t, accounts, "acct_00000000-0000-4000-8000-000000000002")
	if !second.Disabled || second.Priority != 11 {
		t.Fatalf("second duplicate config account lost card fields: disabled=%v priority=%d", second.Disabled, second.Priority)
	}
	auth := accountByKey(t, accounts, "acct_00000000-0000-4000-8000-000000000003")
	if auth.AuthFile == nil || auth.AuthFile.AuthJSON == "" || auth.AuthFile.SourceFileName != "codex-pro.json" {
		t.Fatalf("auth file account did not round-trip: %+v", auth.AuthFile)
	}
	compat := accountByKey(t, accounts, "acct_00000000-0000-4000-8000-000000000004")
	if compat.OpenAICompatible == nil || compat.OpenAICompatible.RuntimeProviderKey != "openai-compatible:acct_00000000-0000-4000-8000-000000000004" {
		t.Fatalf("openai-compatible account did not round-trip: %+v", compat.OpenAICompatible)
	}
}

func TestUpdateAccountPreservesAccountKeyAndBumpsRevision(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	created, err := store.CreateAccount(ctx, AccountWrite{
		Kind:             KindCodexAPIKey,
		Title:            "Primary",
		Provider:         "codex",
		CredentialSource: SourceSidecarManagementAPI,
		Priority:         1,
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:            "sk-old",
			APIKeyFingerprint: "old-fp",
			BaseURL:           "https://api.example.com/v1",
			Prefix:            "team-a/",
			Websockets:        true,
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if !IsAccountKey(created.AccountKey) {
		t.Fatalf("created account key = %q, want acct_<uuid>", created.AccountKey)
	}

	updated, err := store.UpdateAccount(ctx, created.AccountKey, AccountWrite{
		Kind:             KindCodexAPIKey,
		Title:            "Primary Updated",
		Provider:         "codex",
		CredentialSource: SourceSidecarManagementAPI,
		Priority:         9,
		Disabled:         true,
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:            "sk-new",
			APIKeyFingerprint: "new-fp",
			BaseURL:           "https://api2.example.com/v1",
			Prefix:            "team-b/",
			Websockets:        true,
			HeadersJSON:       `{"X-Team":"B"}`,
		},
	})
	if err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	if updated.AccountKey != created.AccountKey {
		t.Fatalf("account_key changed from %q to %q", created.AccountKey, updated.AccountKey)
	}
	if updated.Revision != created.Revision+1 {
		t.Fatalf("revision = %d, want %d", updated.Revision, created.Revision+1)
	}
	if updated.RuntimeApplyStatus != "pending" {
		t.Fatalf("runtime apply status = %q, want pending", updated.RuntimeApplyStatus)
	}
	if updated.CodexAPIKey == nil || updated.CodexAPIKey.APIKey != "sk-new" || updated.CodexAPIKey.BaseURL != "https://api2.example.com/v1" {
		t.Fatalf("updated credential not stored: %+v", updated.CodexAPIKey)
	}
	if !updated.Disabled || updated.Priority != 9 {
		t.Fatalf("card fields not updated: disabled=%v priority=%d", updated.Disabled, updated.Priority)
	}
}

func TestSetAccountStatusOnlyUpdatesDisabledWithoutRuntimeApply(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	created, err := store.CreateAccount(ctx, AccountWrite{
		Kind:             KindCodexAPIKey,
		Title:            "Primary",
		Provider:         "codex",
		CredentialSource: SourceSidecarManagementAPI,
		Priority:         1,
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:            "sk-old",
			APIKeyFingerprint: "old-fp",
			BaseURL:           "https://api.example.com/v1",
			Websockets:        true,
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := store.MarkRuntimeApplyResult(ctx, created.AccountKey, created.Revision, "applied", ""); err != nil {
		t.Fatalf("MarkRuntimeApplyResult: %v", err)
	}

	disabled, err := store.SetAccountStatus(ctx, created.AccountKey, true)
	if err != nil {
		t.Fatalf("SetAccountStatus: %v", err)
	}
	if !disabled.Disabled {
		t.Fatal("disabled flag = false, want true")
	}
	if disabled.Revision != created.Revision {
		t.Fatalf("revision = %d, want unchanged %d", disabled.Revision, created.Revision)
	}
	if disabled.RuntimeApplyStatus != "applied" {
		t.Fatalf("runtime apply status = %q, want applied", disabled.RuntimeApplyStatus)
	}

	enabled, err := store.SetAccountStatus(ctx, created.AccountKey, false)
	if err != nil {
		t.Fatalf("SetAccountStatus enable: %v", err)
	}
	if enabled.Disabled {
		t.Fatal("disabled flag = true, want false")
	}
	if enabled.Revision != created.Revision {
		t.Fatalf("enable revision = %d, want unchanged %d", enabled.Revision, created.Revision)
	}
	if enabled.RuntimeApplyStatus != "applied" {
		t.Fatalf("enable runtime apply status = %q, want applied", enabled.RuntimeApplyStatus)
	}
}

func TestUpdateAuthFileCredentialUpdatesAuthJSONWithoutChangingRevision(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	created, err := store.CreateAccount(ctx, AccountWrite{
		Kind:             KindAuthFile,
		Title:            "Codex",
		Provider:         "codex",
		CredentialSource: SourceLegacyAuthFile,
		AuthFile: &AuthFileCredential{
			SourceFileName: "codex-user-free.json",
			AuthJSON:       `{"type":"codex","access_token":"old","refresh_token":"old-refresh","email":"user@example.com","plan_type":"free"}`,
			AuthType:       "codex",
			Email:          "user@example.com",
			PlanType:       "free",
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	updated, err := store.UpdateAuthFileCredential(ctx, created.AccountKey, AuthFileCredential{
		AuthJSON: `{"type":"codex","access_token":"new","refresh_token":"new-refresh","email":"user@example.com","plan_type":"plus"}`,
		PlanType: "plus",
	})
	if err != nil {
		t.Fatalf("UpdateAuthFileCredential: %v", err)
	}
	if updated.Revision != created.Revision {
		t.Fatalf("revision = %d, want %d", updated.Revision, created.Revision)
	}
	if updated.AuthFile == nil {
		t.Fatal("updated auth file is nil")
	}
	if got := updated.AuthFile.SourceFileName; got != "codex-user-free.json" {
		t.Fatalf("source file name = %q, want original", got)
	}
	if got := updated.AuthFile.PlanType; got != "plus" {
		t.Fatalf("plan type = %q, want plus", got)
	}
	if !strings.Contains(updated.AuthFile.AuthJSON, `"refresh_token":"new-refresh"`) {
		t.Fatalf("auth_json was not updated: %s", updated.AuthFile.AuthJSON)
	}
}

func TestListAccountsWorksWithSingleSQLiteConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	store.db.SetMaxOpenConns(1)

	_, err = store.CreateAccount(ctx, AccountWrite{
		Kind:             KindCodexAPIKey,
		Title:            "Primary",
		Provider:         "codex",
		CredentialSource: SourceSidecarManagementAPI,
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:            "sk-test",
			APIKeyFingerprint: "fp-test",
			BaseURL:           "https://api.example.com/v1",
			Prefix:            "team-a/",
			Websockets:        true,
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts with single SQLite connection: %v", err)
	}
	if len(accounts) != 1 || accounts[0].CodexAPIKey == nil || accounts[0].CodexAPIKey.APIKey != "sk-test" {
		t.Fatalf("ListAccounts returned incomplete credential payload: %+v", accounts)
	}
}

func countKind(candidates []ImportCandidate, kind AccountKind) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.Kind == kind {
			count++
		}
	}
	return count
}

func candidatesBySource(candidates []ImportCandidate, source CredentialSource) []ImportCandidate {
	var out []ImportCandidate
	for _, candidate := range candidates {
		if candidate.CredentialSource == source {
			out = append(out, candidate)
		}
	}
	return out
}

func countAccountKind(accounts []AccountRecord, kind AccountKind) int {
	count := 0
	for _, account := range accounts {
		if account.Kind == kind {
			count++
		}
	}
	return count
}

func accountByKey(t *testing.T, accounts []AccountRecord, accountKey string) AccountRecord {
	t.Helper()
	for _, account := range accounts {
		if account.AccountKey == accountKey {
			return account
		}
	}
	t.Fatalf("account %s not found in %+v", accountKey, accounts)
	return AccountRecord{}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
}

func mustWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(%s): %v", path, err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
