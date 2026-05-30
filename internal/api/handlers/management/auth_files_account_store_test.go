package management

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSaveTokenRecordToAccountStoreStoresNormalizedAuthPayload(t *testing.T) {
	handler := &Handler{
		accountStorePath: filepath.Join(t.TempDir(), "accounts.sqlite"),
	}

	record := &coreauth.Auth{
		ID:       "codex-user-plus.json",
		Provider: "codex",
		FileName: "codex-user-plus.json",
		Storage: &codexauth.CodexTokenStorage{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			AccountID:    "acct-123",
			Email:        "user@example.com",
		},
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct-123",
		},
		Attributes: map[string]string{
			"plan_type": "plus",
		},
	}

	savedPath, err := handler.saveTokenRecordToAccountStore(context.Background(), record)
	if err != nil {
		t.Fatalf("saveTokenRecordToAccountStore() error = %v", err)
	}
	if savedPath == "" {
		t.Fatal("saveTokenRecordToAccountStore() returned empty saved path")
	}

	store, err := accountstore.Open(handler.accountStorePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if account.CredentialSource != accountstore.SourceSidecarOAuth {
		t.Fatalf("credential source = %q, want %q", account.CredentialSource, accountstore.SourceSidecarOAuth)
	}
	if account.AuthFile == nil {
		t.Fatal("auth file credential is nil")
	}
	if account.AuthFile.PlanType != "plus" {
		t.Fatalf("plan type = %q, want plus", account.AuthFile.PlanType)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(account.AuthFile.AuthJSON), &payload); err != nil {
		t.Fatalf("unmarshal auth_json: %v", err)
	}
	if got := stringFromAny(payload["type"]); got != "codex" {
		t.Fatalf("payload type = %q, want codex", got)
	}
	if got := stringFromAny(payload["access_token"]); got != "access-token" {
		t.Fatalf("payload access_token = %q, want access-token", got)
	}
	if got := stringFromAny(payload["refresh_token"]); got != "refresh-token" {
		t.Fatalf("payload refresh_token = %q, want refresh-token", got)
	}
	if got := stringFromAny(payload["plan_type"]); got != "plus" {
		t.Fatalf("payload plan_type = %q, want plus", got)
	}
	if _, ok := payload["provider"]; ok {
		t.Fatalf("payload should not contain runtime auth wrapper fields: %#v", payload)
	}
}

func TestSaveTokenRecordToAccountStoreUpdatesExistingAuthFileByAccountID(t *testing.T) {
	handler := &Handler{
		accountStorePath: filepath.Join(t.TempDir(), "accounts.sqlite"),
	}

	first := &coreauth.Auth{
		ID:       "codex-user-free.json",
		Provider: "codex",
		FileName: "codex-user-free.json",
		Storage: &codexauth.CodexTokenStorage{
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			AccountID:    "acct-stable",
			Email:        "user@example.com",
		},
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct-stable",
		},
		Attributes: map[string]string{
			"plan_type": "free",
		},
	}
	if _, err := handler.saveTokenRecordToAccountStore(context.Background(), first); err != nil {
		t.Fatalf("first saveTokenRecordToAccountStore() error = %v", err)
	}

	second := &coreauth.Auth{
		ID:       "codex-user-plus.json",
		Provider: "codex",
		FileName: "codex-user-plus.json",
		Storage: &codexauth.CodexTokenStorage{
			AccessToken:  "access-2",
			RefreshToken: "refresh-2",
			AccountID:    "acct-stable",
			Email:        "user@example.com",
		},
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct-stable",
		},
		Attributes: map[string]string{
			"plan_type": "plus",
		},
	}
	secondSavedPath, err := handler.saveTokenRecordToAccountStore(context.Background(), second)
	if err != nil {
		t.Fatalf("second saveTokenRecordToAccountStore() error = %v", err)
	}
	if secondSavedPath == "" {
		t.Fatal("second saveTokenRecordToAccountStore() returned empty saved path")
	}

	store, err := accountstore.Open(handler.accountStorePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if account.AuthFile == nil {
		t.Fatal("auth file credential is nil")
	}
	if got := account.AuthFile.SourceFileName; got != "codex-user-plus.json" {
		t.Fatalf("source file name = %q, want codex-user-plus.json", got)
	}
	if got := account.AuthFile.PlanType; got != "plus" {
		t.Fatalf("plan type = %q, want plus", got)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(account.AuthFile.AuthJSON), &payload); err != nil {
		t.Fatalf("unmarshal auth_json: %v", err)
	}
	if got := stringFromAny(payload["access_token"]); got != "access-2" {
		t.Fatalf("payload access_token = %q, want access-2", got)
	}
	if got := stringFromAny(payload["refresh_token"]); got != "refresh-2" {
		t.Fatalf("payload refresh_token = %q, want refresh-2", got)
	}
}
