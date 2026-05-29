package accountstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type AccountRecord struct {
	AccountKey         string           `json:"account_key"`
	Kind               AccountKind      `json:"kind"`
	Title              string           `json:"title"`
	Provider           string           `json:"provider"`
	CredentialSource   CredentialSource `json:"credential_source"`
	Priority           int              `json:"priority"`
	Disabled           bool             `json:"disabled"`
	Revision           int              `json:"revision"`
	MetadataJSON       string           `json:"metadata_json"`
	CreatedAtUnixMs    int64            `json:"created_at_unix_ms"`
	UpdatedAtUnixMs    int64            `json:"updated_at_unix_ms"`
	DeletedAtUnixMs    int64            `json:"deleted_at_unix_ms,omitempty"`
	RuntimeApplyStatus string           `json:"runtime_apply_status,omitempty"`
	RuntimeApplyError  string           `json:"runtime_apply_error,omitempty"`

	AuthFile         *AuthFileCredential         `json:"auth_file,omitempty"`
	CodexAPIKey      *CodexAPIKeyCredential      `json:"codex_api_key,omitempty"`
	OpenAICompatible *OpenAICompatibleCredential `json:"openai_compatible,omitempty"`
}

type AccountWrite struct {
	Kind             AccountKind      `json:"kind"`
	Title            string           `json:"title"`
	Provider         string           `json:"provider"`
	CredentialSource CredentialSource `json:"credential_source"`
	Priority         int              `json:"priority"`
	Disabled         bool             `json:"disabled"`

	AuthFile         *AuthFileCredential         `json:"auth_file,omitempty"`
	CodexAPIKey      *CodexAPIKeyCredential      `json:"codex_api_key,omitempty"`
	OpenAICompatible *OpenAICompatibleCredential `json:"openai_compatible,omitempty"`
}

type CommitReport struct {
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

type MigrationSourceRecord struct {
	ID                string `json:"id"`
	AccountKey        string `json:"account_key"`
	SourceKind        string `json:"source_kind"`
	SourcePath        string `json:"source_path"`
	SourceKey         string `json:"source_key"`
	SourceFingerprint string `json:"source_fingerprint"`
	ImportedAtUnixMs  int64  `json:"imported_at_unix_ms"`
	DeletedAtUnixMs   int64  `json:"deleted_at_unix_ms"`
	BackupPath        string `json:"backup_path"`
}

func (s *Store) CreateAccount(ctx context.Context, write AccountWrite) (AccountRecord, error) {
	if s == nil || s.db == nil {
		return AccountRecord{}, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return AccountRecord{}, err
	}
	accountKey, err := newAccountKey()
	if err != nil {
		return AccountRecord{}, fmt.Errorf("generate account key: %w", err)
	}
	candidate := candidateFromWrite(accountKey, write)
	if err := validateCandidate(candidate); err != nil {
		return AccountRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("begin create account transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err := insertAccountRows(ctx, tx, candidate, 1); err != nil {
		return AccountRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccountRecord{}, fmt.Errorf("commit create account transaction: %w", err)
	}
	tx = nil
	return s.GetAccount(ctx, accountKey)
}

func (s *Store) UpdateAccount(ctx context.Context, accountKey string, write AccountWrite) (AccountRecord, error) {
	if s == nil || s.db == nil {
		return AccountRecord{}, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return AccountRecord{}, fmt.Errorf("invalid account key %q", accountKey)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("begin update account transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	var currentKind AccountKind
	var currentRevision int
	err = tx.QueryRowContext(ctx, "SELECT kind, revision FROM account_cards WHERE account_key = ? AND deleted_at_unix_ms IS NULL", accountKey).Scan(&currentKind, &currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountRecord{}, fmt.Errorf("account %s not found", accountKey)
	}
	if err != nil {
		return AccountRecord{}, fmt.Errorf("query account %s: %w", accountKey, err)
	}
	if write.Kind == "" {
		write.Kind = currentKind
	}
	if write.Kind != currentKind {
		return AccountRecord{}, fmt.Errorf("cannot change account kind from %s to %s", currentKind, write.Kind)
	}
	candidate := candidateFromWrite(accountKey, write)
	if err := validateCandidate(candidate); err != nil {
		return AccountRecord{}, err
	}
	nextRevision := currentRevision + 1
	now := unixMs()
	_, err = tx.ExecContext(ctx, `
UPDATE account_cards
SET title = ?, provider = ?, credential_source = ?, priority = ?, disabled = ?, revision = ?, updated_at_unix_ms = ?
WHERE account_key = ? AND deleted_at_unix_ms IS NULL`,
		candidate.Title,
		candidate.Provider,
		string(candidate.CredentialSource),
		candidate.Priority,
		boolInt(candidate.Disabled),
		nextRevision,
		now,
		accountKey,
	)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("update account card %s: %w", accountKey, err)
	}
	if err := deleteCredentialRows(ctx, tx, accountKey); err != nil {
		return AccountRecord{}, err
	}
	if err := insertCredentialRows(ctx, tx, candidate, now); err != nil {
		return AccountRecord{}, err
	}
	if err := upsertRuntimeApplyState(ctx, tx, accountKey, nextRevision, now); err != nil {
		return AccountRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccountRecord{}, fmt.Errorf("commit update account transaction: %w", err)
	}
	tx = nil
	return s.GetAccount(ctx, accountKey)
}

func (s *Store) GetAccount(ctx context.Context, accountKey string) (AccountRecord, error) {
	if !IsAccountKey(accountKey) {
		return AccountRecord{}, fmt.Errorf("invalid account key %q", accountKey)
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return AccountRecord{}, err
	}
	for _, account := range accounts {
		if account.AccountKey == accountKey {
			return account, nil
		}
	}
	return AccountRecord{}, fmt.Errorf("account %s not found", accountKey)
}

func (s *Store) SetAccountStatus(ctx context.Context, accountKey string, disabled bool) (AccountRecord, error) {
	return s.updateAccountCardOnly(ctx, accountKey, func(current AccountRecord) (string, int, bool) {
		return current.Title, current.Priority, disabled
	})
}

func (s *Store) SetAccountPriority(ctx context.Context, accountKey string, priority int) (AccountRecord, error) {
	return s.updateAccountCardOnly(ctx, accountKey, func(current AccountRecord) (string, int, bool) {
		return current.Title, priority, current.Disabled
	})
}

func (s *Store) DeleteAccount(ctx context.Context, accountKey string) error {
	if s == nil || s.db == nil {
		return errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return fmt.Errorf("invalid account key %q", accountKey)
	}
	now := unixMs()
	result, err := s.db.ExecContext(ctx, "UPDATE account_cards SET deleted_at_unix_ms = ?, updated_at_unix_ms = ? WHERE account_key = ? AND deleted_at_unix_ms IS NULL", now, now, accountKey)
	if err != nil {
		return fmt.Errorf("delete account %s: %w", accountKey, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete account %s rows affected: %w", accountKey, err)
	}
	if rows == 0 {
		return fmt.Errorf("account %s not found", accountKey)
	}
	return nil
}

func (s *Store) MarkRuntimeApplyResult(ctx context.Context, accountKey string, revision int, status string, lastError string) error {
	if s == nil || s.db == nil {
		return errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return fmt.Errorf("invalid account key %q", accountKey)
	}
	status = strings.TrimSpace(status)
	switch status {
	case "applied", "failed":
	default:
		return fmt.Errorf("invalid runtime apply status %q", status)
	}
	now := unixMs()
	appliedAt := int64(0)
	if status == "applied" {
		appliedAt = now
		lastError = ""
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE account_runtime_apply_state
SET status = ?, last_error = ?, applied_at_unix_ms = ?, updated_at_unix_ms = ?
WHERE account_key = ? AND revision = ?`,
		status,
		strings.TrimSpace(lastError),
		appliedAt,
		now,
		accountKey,
		revision,
	)
	if err != nil {
		return fmt.Errorf("mark runtime apply result %s: %w", accountKey, err)
	}
	return nil
}

func (s *Store) ListUndeletedMigrationSources(ctx context.Context) ([]MigrationSourceRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, account_key, source_kind, source_path, source_key, source_fingerprint, imported_at_unix_ms, deleted_at_unix_ms, backup_path
FROM account_migration_sources
WHERE deleted_at_unix_ms = 0
ORDER BY imported_at_unix_ms ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query migration sources: %w", err)
	}
	defer rows.Close()
	var out []MigrationSourceRecord
	for rows.Next() {
		var source MigrationSourceRecord
		if err := rows.Scan(
			&source.ID,
			&source.AccountKey,
			&source.SourceKind,
			&source.SourcePath,
			&source.SourceKey,
			&source.SourceFingerprint,
			&source.ImportedAtUnixMs,
			&source.DeletedAtUnixMs,
			&source.BackupPath,
		); err != nil {
			return nil, fmt.Errorf("scan migration source: %w", err)
		}
		out = append(out, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration sources: %w", err)
	}
	return out, nil
}

func (s *Store) MarkMigrationSourceDeleted(ctx context.Context, id string, backupPath string) error {
	if s == nil || s.db == nil {
		return errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("migration source id is empty")
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE account_migration_sources
SET deleted_at_unix_ms = ?, backup_path = ?
WHERE id = ?`, unixMs(), strings.TrimSpace(backupPath), id)
	if err != nil {
		return fmt.Errorf("mark migration source deleted %s: %w", id, err)
	}
	return nil
}

func (s *Store) updateAccountCardOnly(ctx context.Context, accountKey string, mutate func(AccountRecord) (string, int, bool)) (AccountRecord, error) {
	if s == nil || s.db == nil {
		return AccountRecord{}, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	current, err := s.GetAccount(ctx, accountKey)
	if err != nil {
		return AccountRecord{}, err
	}
	title, priority, disabled := mutate(current)
	nextRevision := current.Revision + 1
	now := unixMs()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("begin update account card transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.ExecContext(ctx, `
UPDATE account_cards
SET title = ?, priority = ?, disabled = ?, revision = ?, updated_at_unix_ms = ?
WHERE account_key = ? AND deleted_at_unix_ms IS NULL`,
		title,
		priority,
		boolInt(disabled),
		nextRevision,
		now,
		accountKey,
	)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("update account card %s: %w", accountKey, err)
	}
	if err := upsertRuntimeApplyState(ctx, tx, accountKey, nextRevision, now); err != nil {
		return AccountRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccountRecord{}, fmt.Errorf("commit update account card transaction: %w", err)
	}
	tx = nil
	return s.GetAccount(ctx, accountKey)
}

func (s *Store) CommitImport(ctx context.Context, report *MigrationReport) (*CommitReport, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if report == nil {
		return nil, errors.New("migration report is nil")
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin account import transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	out := &CommitReport{}
	for _, candidate := range report.Candidates {
		if err := validateCandidate(candidate); err != nil {
			return nil, err
		}
		sourceID := migrationSourceID(candidate)
		existing, err := migrationSourceAccountKey(ctx, tx, sourceID)
		if err != nil {
			return nil, err
		}
		if existing != "" {
			out.Skipped++
			continue
		}
		if err := insertCandidate(ctx, tx, candidate, sourceID); err != nil {
			return nil, err
		}
		out.Imported++
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit account import transaction: %w", err)
	}
	tx = nil
	return out, nil
}

func (s *Store) ListAccounts(ctx context.Context) ([]AccountRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
  c.account_key,
  c.kind,
  c.title,
  c.provider,
  c.credential_source,
  c.priority,
  c.disabled,
  c.revision,
  c.metadata_json,
  c.created_at_unix_ms,
  c.updated_at_unix_ms,
  COALESCE(c.deleted_at_unix_ms, 0),
  COALESCE(a.status, ''),
  COALESCE(a.last_error, '')
FROM account_cards c
LEFT JOIN account_runtime_apply_state a ON a.account_key = c.account_key
WHERE c.deleted_at_unix_ms IS NULL
ORDER BY c.priority DESC, c.created_at_unix_ms ASC, c.account_key ASC`)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	var accounts []AccountRecord
	for rows.Next() {
		var account AccountRecord
		var disabled int
		if err := rows.Scan(
			&account.AccountKey,
			&account.Kind,
			&account.Title,
			&account.Provider,
			&account.CredentialSource,
			&account.Priority,
			&disabled,
			&account.Revision,
			&account.MetadataJSON,
			&account.CreatedAtUnixMs,
			&account.UpdatedAtUnixMs,
			&account.DeletedAtUnixMs,
			&account.RuntimeApplyStatus,
			&account.RuntimeApplyError,
		); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		account.Disabled = disabled != 0
		if err := s.attachCredential(ctx, &account); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	return accounts, nil
}

func (s *Store) attachCredential(ctx context.Context, account *AccountRecord) error {
	switch account.Kind {
	case KindAuthFile:
		var credential AuthFileCredential
		err := s.db.QueryRowContext(ctx, `
SELECT source_file_name, auth_json, auth_type, email, plan_type, modified_unix_ms, size_bytes
FROM auth_file_accounts WHERE account_key = ?`, account.AccountKey).Scan(
			&credential.SourceFileName,
			&credential.AuthJSON,
			&credential.AuthType,
			&credential.Email,
			&credential.PlanType,
			&credential.ModifiedUnixMs,
			&credential.SizeBytes,
		)
		if err != nil {
			return fmt.Errorf("query auth-file credential for %s: %w", account.AccountKey, err)
		}
		account.AuthFile = &credential
	case KindCodexAPIKey:
		var credential CodexAPIKeyCredential
		var websockets, quotaEnabled, billingEnabled int
		err := s.db.QueryRowContext(ctx, `
SELECT api_key, api_key_fingerprint, base_url, prefix, proxy_url, websockets, quota_curl, quota_enabled, billing_curl, billing_enabled, format_base_urls_json, headers_json, models_json, excluded_models_json
FROM codex_api_key_accounts WHERE account_key = ?`, account.AccountKey).Scan(
			&credential.APIKey,
			&credential.APIKeyFingerprint,
			&credential.BaseURL,
			&credential.Prefix,
			&credential.ProxyURL,
			&websockets,
			&credential.QuotaCurl,
			&quotaEnabled,
			&credential.BillingCurl,
			&billingEnabled,
			&credential.FormatBaseURLsJSON,
			&credential.HeadersJSON,
			&credential.ModelsJSON,
			&credential.ExcludedModelsJSON,
		)
		if err != nil {
			return fmt.Errorf("query codex-api-key credential for %s: %w", account.AccountKey, err)
		}
		credential.Websockets = websockets != 0
		credential.QuotaEnabled = quotaEnabled != 0
		credential.BillingEnabled = billingEnabled != 0
		account.CodexAPIKey = &credential
	case KindOpenAICompatible:
		var credential OpenAICompatibleCredential
		err := s.db.QueryRowContext(ctx, `
SELECT provider_name, runtime_provider_key, base_url, prefix, api_key_entries_json, headers_json, models_json
FROM openai_compatible_accounts WHERE account_key = ?`, account.AccountKey).Scan(
			&credential.ProviderName,
			&credential.RuntimeProviderKey,
			&credential.BaseURL,
			&credential.Prefix,
			&credential.APIKeyEntriesJSON,
			&credential.HeadersJSON,
			&credential.ModelsJSON,
		)
		if err != nil {
			return fmt.Errorf("query openai-compatible credential for %s: %w", account.AccountKey, err)
		}
		account.OpenAICompatible = &credential
	default:
		return fmt.Errorf("unsupported account kind %q", account.Kind)
	}
	return nil
}

func validateCandidate(candidate ImportCandidate) error {
	if !IsAccountKey(candidate.AccountKey) {
		return fmt.Errorf("invalid account key %q", candidate.AccountKey)
	}
	switch candidate.Kind {
	case KindAuthFile:
		if candidate.AuthFile == nil || candidate.AuthFile.AuthJSON == "" {
			return fmt.Errorf("auth-file candidate %s missing credential", candidate.AccountKey)
		}
	case KindCodexAPIKey:
		if candidate.CodexAPIKey == nil || candidate.CodexAPIKey.APIKey == "" || candidate.CodexAPIKey.BaseURL == "" {
			return fmt.Errorf("codex-api-key candidate %s missing credential", candidate.AccountKey)
		}
	case KindOpenAICompatible:
		if candidate.OpenAICompatible == nil || candidate.OpenAICompatible.BaseURL == "" {
			return fmt.Errorf("openai-compatible candidate %s missing credential", candidate.AccountKey)
		}
	default:
		return fmt.Errorf("unsupported account kind %q", candidate.Kind)
	}
	return nil
}

func migrationSourceAccountKey(ctx context.Context, tx *sql.Tx, sourceID string) (string, error) {
	var accountKey string
	err := tx.QueryRowContext(ctx, "SELECT account_key FROM account_migration_sources WHERE id = ?", sourceID).Scan(&accountKey)
	if err == nil {
		return accountKey, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return "", fmt.Errorf("query migration source %s: %w", sourceID, err)
}

func insertCandidate(ctx context.Context, tx *sql.Tx, candidate ImportCandidate, sourceID string) error {
	now := unixMs()
	if err := insertAccountRows(ctx, tx, candidate, 1); err != nil {
		return err
	}

	if candidate.LegacyID != "" {
		if err := insertRuntimeIdentity(ctx, tx, candidate.AccountKey, "legacy-account-key:"+candidate.LegacyID, "legacy-account-key", now); err != nil {
			return err
		}
	}
	if candidate.SourceKey != "" {
		if err := insertRuntimeIdentity(ctx, tx, candidate.AccountKey, "source-key:"+candidate.SourceKey, "source-key", now); err != nil {
			return err
		}
	}
	// insertAccountRows has already marked runtime apply pending for revision 1.
	_, err := tx.ExecContext(ctx, `
INSERT INTO account_migration_sources(id, account_key, source_kind, source_path, source_key, source_fingerprint, imported_at_unix_ms)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sourceID,
		candidate.AccountKey,
		migrationSourceKind(candidate),
		candidate.SourcePath,
		candidate.SourceKey,
		candidate.SourceFingerprint,
		now,
	)
	if err != nil {
		return fmt.Errorf("insert migration source %s: %w", sourceID, err)
	}
	return nil
}

func insertAccountRows(ctx context.Context, tx *sql.Tx, candidate ImportCandidate, revision int) error {
	now := unixMs()
	title := candidate.Title
	if title == "" {
		title = string(candidate.Kind)
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO account_cards(account_key, kind, title, provider, credential_source, priority, disabled, revision, metadata_json, created_at_unix_ms, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?)`,
		candidate.AccountKey,
		string(candidate.Kind),
		title,
		candidate.Provider,
		string(candidate.CredentialSource),
		candidate.Priority,
		boolInt(candidate.Disabled),
		revision,
		now,
		now,
	)
	if err != nil {
		return fmt.Errorf("insert account card %s: %w", candidate.AccountKey, err)
	}
	if err := insertCredentialRows(ctx, tx, candidate, now); err != nil {
		return err
	}
	return upsertRuntimeApplyState(ctx, tx, candidate.AccountKey, revision, now)
}

func insertCredentialRows(ctx context.Context, tx *sql.Tx, candidate ImportCandidate, now int64) error {
	var err error
	switch candidate.Kind {
	case KindAuthFile:
		auth := candidate.AuthFile
		_, err = tx.ExecContext(ctx, `
INSERT INTO auth_file_accounts(account_key, source_file_name, auth_json, auth_fingerprint, auth_type, email, plan_type, modified_unix_ms, size_bytes, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			candidate.AccountKey,
			auth.SourceFileName,
			auth.AuthJSON,
			candidate.SourceFingerprint,
			auth.AuthType,
			auth.Email,
			auth.PlanType,
			auth.ModifiedUnixMs,
			auth.SizeBytes,
			now,
		)
	case KindCodexAPIKey:
		key := candidate.CodexAPIKey
		_, err = tx.ExecContext(ctx, `
INSERT INTO codex_api_key_accounts(account_key, api_key, api_key_fingerprint, base_url, prefix, proxy_url, websockets, quota_curl, quota_enabled, billing_curl, billing_enabled, format_base_urls_json, headers_json, models_json, excluded_models_json, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			candidate.AccountKey,
			key.APIKey,
			defaultString(key.APIKeyFingerprint, apiKeyFingerprint(key.APIKey)),
			key.BaseURL,
			key.Prefix,
			key.ProxyURL,
			boolInt(key.Websockets),
			key.QuotaCurl,
			boolInt(key.QuotaEnabled),
			key.BillingCurl,
			boolInt(key.BillingEnabled),
			defaultJSON(key.FormatBaseURLsJSON, "{}"),
			defaultJSON(key.HeadersJSON, "{}"),
			defaultJSON(key.ModelsJSON, "[]"),
			defaultJSON(key.ExcludedModelsJSON, "[]"),
			now,
		)
	case KindOpenAICompatible:
		compat := candidate.OpenAICompatible
		runtimeProviderKey := compat.RuntimeProviderKey
		if runtimeProviderKey == "" {
			runtimeProviderKey = "openai-compatible:" + candidate.AccountKey
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO openai_compatible_accounts(account_key, provider_name, runtime_provider_key, base_url, prefix, api_key_entries_json, headers_json, models_json, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			candidate.AccountKey,
			compat.ProviderName,
			runtimeProviderKey,
			compat.BaseURL,
			compat.Prefix,
			defaultJSON(compat.APIKeyEntriesJSON, "[]"),
			defaultJSON(compat.HeadersJSON, "{}"),
			defaultJSON(compat.ModelsJSON, "[]"),
			now,
		)
	}
	if err != nil {
		return fmt.Errorf("insert %s credential %s: %w", candidate.Kind, candidate.AccountKey, err)
	}
	return nil
}

func deleteCredentialRows(ctx context.Context, tx *sql.Tx, accountKey string) error {
	for _, statement := range []string{
		"DELETE FROM auth_file_accounts WHERE account_key = ?",
		"DELETE FROM codex_api_key_accounts WHERE account_key = ?",
		"DELETE FROM openai_compatible_accounts WHERE account_key = ?",
	} {
		if _, err := tx.ExecContext(ctx, statement, accountKey); err != nil {
			return fmt.Errorf("delete credential rows for %s: %w", accountKey, err)
		}
	}
	return nil
}

func upsertRuntimeApplyState(ctx context.Context, tx *sql.Tx, accountKey string, revision int, now int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO account_runtime_apply_state(account_key, revision, status, last_error, applied_at_unix_ms, updated_at_unix_ms)
VALUES (?, ?, 'pending', '', 0, ?)
ON CONFLICT(account_key) DO UPDATE SET
  revision = excluded.revision,
  status = 'pending',
  last_error = '',
  updated_at_unix_ms = excluded.updated_at_unix_ms`,
		accountKey,
		revision,
		now,
	)
	if err != nil {
		return fmt.Errorf("upsert runtime apply state %s: %w", accountKey, err)
	}
	return nil
}

func insertRuntimeIdentity(ctx context.Context, tx *sql.Tx, accountKey string, identityKey string, identityKind string, now int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO account_runtime_identities(identity_key, account_key, identity_kind, created_at_unix_ms, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?)`, identityKey, accountKey, identityKind, now, now)
	if err != nil {
		return fmt.Errorf("insert runtime identity %s: %w", identityKey, err)
	}
	return nil
}

func migrationSourceID(candidate ImportCandidate) string {
	material := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s",
		candidate.Kind,
		candidate.CredentialSource,
		candidate.SourcePath,
		candidate.SourceKey,
		candidate.SourceFingerprint,
	)
	return "migration-source:" + fingerprintString(material)
}

func migrationSourceKind(candidate ImportCandidate) string {
	switch candidate.CredentialSource {
	case SourceLegacyAuthFile:
		return "auth-file"
	case SourceLegacyGetTokensCodexAPIKey:
		return "codex-api-key-json"
	case SourceLegacyConfigCodexAPIKey:
		return "config-codex-api-key"
	case SourceLegacyConfigOpenAICompatible:
		return "config-openai-compatible"
	default:
		return string(candidate.Kind)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func defaultJSON(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func defaultString(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func candidateFromWrite(accountKey string, write AccountWrite) ImportCandidate {
	source := write.CredentialSource
	if source == "" {
		source = SourceSidecarManagementAPI
	}
	provider := write.Provider
	switch write.Kind {
	case KindCodexAPIKey, KindAuthFile:
		if provider == "" {
			provider = "codex"
		}
	case KindOpenAICompatible:
		if provider == "" && write.OpenAICompatible != nil {
			provider = write.OpenAICompatible.ProviderName
		}
	}
	return ImportCandidate{
		AccountKey:        accountKey,
		Kind:              write.Kind,
		Title:             write.Title,
		Provider:          provider,
		CredentialSource:  source,
		Priority:          write.Priority,
		Disabled:          write.Disabled,
		AuthFile:          write.AuthFile,
		CodexAPIKey:       write.CodexAPIKey,
		OpenAICompatible:  write.OpenAICompatible,
		SourceFingerprint: "",
	}
}

func unixMs() int64 {
	return time.Now().UnixMilli()
}
