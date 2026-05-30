package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type countingTokenStore struct {
	saveCount atomic.Int32
}

func (s *countingTokenStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *countingTokenStore) Save(context.Context, *coreauth.Auth) (string, error) {
	s.saveCount.Add(1)
	return "fallback", nil
}

func (s *countingTokenStore) Delete(context.Context, string) error {
	return nil
}

func TestAccountStoreTokenStoreSaveUpdatesSQLiteAuthJSON(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	created, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindAuthFile,
		Title:            "Codex",
		Provider:         "codex",
		CredentialSource: accountstore.SourceLegacyAuthFile,
		AuthFile: &accountstore.AuthFileCredential{
			SourceFileName: "codex-user-plus.json",
			AuthJSON:       `{"type":"codex","access_token":"old","refresh_token":"old-refresh","email":"user@example.com","plan_type":"plus"}`,
			AuthType:       "codex",
			Email:          "user@example.com",
			PlanType:       "plus",
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fallback := &countingTokenStore{}
	tokenStore := newAccountStoreTokenStore(fallback, &config.Config{AccountStoreDB: dbPath})
	manager := coreauth.NewManager(tokenStore, nil, nil)
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), &coreauth.Auth{
		ID:         "codex-user-plus.json",
		AccountKey: created.AccountKey,
		Provider:   "codex",
		FileName:   "codex-user-plus.json",
		Attributes: map[string]string{"plan_type": "plus"},
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "old",
			"refresh_token": "old-refresh",
			"email":         "user@example.com",
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	updatedAuth, err := manager.Update(ctx, &coreauth.Auth{
		ID:         "codex-user-plus.json",
		AccountKey: created.AccountKey,
		Provider:   "codex",
		FileName:   "codex-user-plus.json",
		Metadata: map[string]any{
			"type":          "codex",
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"email":         "user@example.com",
		},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updatedAuth == nil {
		t.Fatal("Update returned nil auth")
	}
	if got := fallback.saveCount.Load(); got != 0 {
		t.Fatalf("fallback save count = %d, want 0", got)
	}

	store, err = accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	updated, err := store.GetAccount(ctx, created.AccountKey)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if updated.AuthFile == nil {
		t.Fatal("updated auth file is nil")
	}
	if updated.Revision != created.Revision {
		t.Fatalf("revision = %d, want %d", updated.Revision, created.Revision)
	}
	if updated.AuthFile.PlanType != "plus" {
		t.Fatalf("plan type = %q, want plus", updated.AuthFile.PlanType)
	}
	if !strings.Contains(updated.AuthFile.AuthJSON, `"refresh_token":"new-refresh"`) {
		t.Fatalf("auth_json was not updated: %s", updated.AuthFile.AuthJSON)
	}
	if !strings.Contains(updated.AuthFile.AuthJSON, `"plan_type":"plus"`) {
		t.Fatalf("auth_json did not preserve plan_type: %s", updated.AuthFile.AuthJSON)
	}
}

func TestAccountStoreTokenStoreSaveDoesNotFallbackForAccountStoreNonAuthFile(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	created, err := store.CreateAccount(ctx, accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "Codex API Key",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:  "sk-test",
			BaseURL: "https://api.openai.com",
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fallback := &countingTokenStore{}
	tokenStore := newAccountStoreTokenStore(fallback, &config.Config{AccountStoreDB: dbPath})
	savedPath, err := tokenStore.Save(ctx, &coreauth.Auth{
		ID:         "codex-api-key",
		AccountKey: created.AccountKey,
		Provider:   "codex",
		Metadata:   map[string]any{"api_key": "sk-updated"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if savedPath != "account-store:"+created.AccountKey {
		t.Fatalf("saved path = %q, want account-store marker", savedPath)
	}
	if got := fallback.saveCount.Load(); got != 0 {
		t.Fatalf("fallback save count = %d, want 0", got)
	}
}

func TestAccountStoreTokenStoreSaveFallsBackForNonAccountStoreAuth(t *testing.T) {
	fallback := &countingTokenStore{}
	tokenStore := newAccountStoreTokenStore(fallback, &config.Config{AccountStoreDB: filepath.Join(t.TempDir(), "accounts-v1.sqlite")})
	savedPath, err := tokenStore.Save(context.Background(), &coreauth.Auth{
		ID:       "codex-file.json",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if savedPath != "fallback" {
		t.Fatalf("saved path = %q, want fallback", savedPath)
	}
	if got := fallback.saveCount.Load(); got != 1 {
		t.Fatalf("fallback save count = %d, want 1", got)
	}
}
