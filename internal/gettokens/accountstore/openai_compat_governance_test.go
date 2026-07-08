package accountstore

import (
	"context"
	"strings"
	"testing"
)

func TestCreateAccountRejectsKnownOpenAICompatibleCodexAPIKey(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/accounts-v1.sqlite")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	_, err = store.CreateAccount(ctx, AccountWrite{
		Kind:     KindCodexAPIKey,
		Title:    "OpenRouter",
		Provider: "codex",
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:  "sk-openrouter",
			BaseURL: "https://openrouter.ai/api",
		},
	})
	if err == nil {
		t.Fatal("expected known openai-compatible provider saved as codex-api-key to be rejected")
	}
	message := err.Error()
	if !strings.Contains(message, "known openai-compatible provider") || !strings.Contains(message, "delete and recreate") {
		t.Fatalf("error = %q, want provider classification and remediation", message)
	}
}

func TestCreateAccountAllowsUnknownCodexAPIKey(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/accounts-v1.sqlite")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	account, err := store.CreateAccount(ctx, AccountWrite{
		Kind:     KindCodexAPIKey,
		Title:    "Internal CPA",
		Provider: "codex",
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:  "sk-internal",
			BaseURL: "https://cpa.example.test/v1",
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount unknown codex-api-key: %v", err)
	}
	if account.Kind != KindCodexAPIKey {
		t.Fatalf("kind = %q, want codex-api-key", account.Kind)
	}
}

func TestPreviewCreateAccountsFlagsKnownOpenAICompatibleCodexAPIKey(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/accounts-v1.sqlite")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	preview, err := store.PreviewCreateAccounts(ctx, []AccountWrite{{
		Kind:     KindCodexAPIKey,
		Title:    "OpenRouter",
		Provider: "codex",
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:  "sk-openrouter",
			BaseURL: "https://openrouter.ai/api",
		},
	}})
	if err != nil {
		t.Fatalf("PreviewCreateAccounts: %v", err)
	}
	if preview.WouldCreate != 0 || preview.Failed != 1 || len(preview.Errors) != 1 {
		t.Fatalf("preview = %+v, want one failed item and no creates", preview)
	}
	if !strings.Contains(preview.Errors[0].Error, "known openai-compatible provider") {
		t.Fatalf("preview error = %q, want known provider classification", preview.Errors[0].Error)
	}
}

func TestAuditKnownOpenAICompatibleMisclassifications(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/accounts-v1.sqlite")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	insertLegacyMisclassifiedCodexAPIKey(t, store, "acct_00000000-0000-4000-8000-000000000001", "OpenRouter", "https://openrouter.ai/api")
	if _, err := store.CreateAccount(ctx, AccountWrite{
		Kind:     KindCodexAPIKey,
		Title:    "Internal CPA",
		Provider: "codex",
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:  "sk-internal",
			BaseURL: "https://cpa.example.test/v1",
		},
	}); err != nil {
		t.Fatalf("CreateAccount internal codex: %v", err)
	}

	audit, err := store.AuditKnownOpenAICompatibleMisclassifications(ctx)
	if err != nil {
		t.Fatalf("AuditKnownOpenAICompatibleMisclassifications: %v", err)
	}
	if audit.TotalCodexAPIKey != 2 {
		t.Fatalf("total codex api key = %d, want 2", audit.TotalCodexAPIKey)
	}
	if audit.MisclassifiedKnownOpenAICompatible != 1 {
		t.Fatalf("misclassified = %d, want 1", audit.MisclassifiedKnownOpenAICompatible)
	}
	if audit.ByProvider["openrouter"] != 1 {
		t.Fatalf("openrouter count = %d, want 1", audit.ByProvider["openrouter"])
	}
	if len(audit.MisclassifiedKnownOpenAICompatCards) != 1 {
		t.Fatalf("cards len = %d, want 1", len(audit.MisclassifiedKnownOpenAICompatCards))
	}
	if audit.MisclassifiedKnownOpenAICompatCards[0].Remediation == "" {
		t.Fatal("expected remediation to be populated")
	}
}

func insertLegacyMisclassifiedCodexAPIKey(t *testing.T, store *Store, accountKey string, title string, baseURL string) {
	t.Helper()
	now := unixMs()
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	candidate := ImportCandidate{
		AccountKey:       accountKey,
		Kind:             KindCodexAPIKey,
		Title:            title,
		Provider:         "codex",
		CredentialSource: SourceSidecarManagementAPI,
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:             "sk-legacy-openrouter",
			BaseURL:            baseURL,
			FormatBaseURLsJSON: `{"openai_chat":"https://openrouter.ai/api"}`,
			ModelsJSON:         `[{"name":"tencent/hy3:free","alias":""}]`,
		},
	}
	if err := insertAccountRows(context.Background(), tx, candidate, 1); err != nil {
		t.Fatalf("seed legacy misclassified account: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(), `
UPDATE account_runtime_apply_state
SET status = 'applied', updated_at_unix_ms = ?, applied_at_unix_ms = ?
WHERE account_key = ?`, now, now, accountKey); err != nil {
		t.Fatalf("seed runtime apply state: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
}
