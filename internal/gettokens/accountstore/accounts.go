package accountstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type accountStoreQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type AccountRecord struct {
	AccountKey                   string           `json:"account_key"`
	Kind                         AccountKind      `json:"kind"`
	Title                        string           `json:"title"`
	Provider                     string           `json:"provider"`
	CredentialSource             CredentialSource `json:"credential_source"`
	Priority                     int              `json:"priority"`
	Disabled                     bool             `json:"disabled"`
	Revision                     int              `json:"revision"`
	MetadataJSON                 string           `json:"metadata_json"`
	CreatedAtUnixMs              int64            `json:"created_at_unix_ms"`
	UpdatedAtUnixMs              int64            `json:"updated_at_unix_ms"`
	DeletedAtUnixMs              int64            `json:"deleted_at_unix_ms,omitempty"`
	RuntimeApplyStatus           string           `json:"runtime_apply_status,omitempty"`
	RuntimeApplyError            string           `json:"runtime_apply_error,omitempty"`
	RuntimeRouteabilityStatus    string           `json:"runtime_routeability_status,omitempty"`
	RuntimeRouteabilityReason    string           `json:"runtime_routeability_reason,omitempty"`
	RuntimeRegisteredModelsCount int              `json:"runtime_registered_models_count,omitempty"`
	LastRuntimeReconcileAtUnixMs int64            `json:"last_runtime_reconcile_at_unix_ms,omitempty"`
	RuntimeFailureClass          string           `json:"runtime_failure_class,omitempty"`
	RuntimeRepairOutcome         string           `json:"runtime_repair_outcome,omitempty"`
	RuntimeRepairAction          string           `json:"runtime_repair_action,omitempty"`
	RuntimeRepairTriggerStatus   string           `json:"runtime_repair_trigger_status,omitempty"`
	RuntimeRepairTriggerClass    string           `json:"runtime_repair_trigger_class,omitempty"`
	RuntimeRepairTriggerReason   string           `json:"runtime_repair_trigger_reason,omitempty"`
	LastRuntimeRepairAtUnixMs    int64            `json:"last_runtime_repair_at_unix_ms,omitempty"`

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

type AccountBatchMutationError struct {
	AccountKey string `json:"account_key"`
	Error      string `json:"error"`
}

type AccountBatchCreateError struct {
	Index int    `json:"index"`
	Title string `json:"title,omitempty"`
	Error string `json:"error"`
}

type AccountBatchCreateSkipped struct {
	Index              int    `json:"index"`
	Title              string `json:"title,omitempty"`
	Reason             string `json:"reason"`
	ExistingAccountKey string `json:"existing_account_key,omitempty"`
	DedupeKeyKind      string `json:"dedupe_key_kind,omitempty"`
}

type AccountBatchCreatePreviewItem struct {
	Index              int    `json:"index"`
	Title              string `json:"title,omitempty"`
	Action             string `json:"action"`
	Reason             string `json:"reason,omitempty"`
	ExistingAccountKey string `json:"existing_account_key,omitempty"`
	DedupeKeyKind      string `json:"dedupe_key_kind,omitempty"`
}

type AccountBatchCreatePreviewResult struct {
	Items        []AccountBatchCreatePreviewItem `json:"items"`
	Skipped      []AccountBatchCreateSkipped     `json:"skipped"`
	Errors       []AccountBatchCreateError       `json:"errors"`
	WouldCreate  int                             `json:"would_create"`
	SkippedCount int                             `json:"skipped_count"`
	Failed       int                             `json:"failed"`
}

type authFileDedupeKey struct {
	Key  string
	Kind string
}

type accountBatchCreatePlan struct {
	candidates  []ImportCandidate
	accountKeys []string
	items       []AccountBatchCreatePreviewItem
	skipped     []AccountBatchCreateSkipped
	failures    []AccountBatchCreateError
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
	if identity, ok := authFileDedupeIdentity(candidate); ok {
		existing, err := runtimeIdentityAccountKey(ctx, tx, identity.Key)
		if err != nil {
			return AccountRecord{}, err
		}
		if existing != "" {
			return AccountRecord{}, fmt.Errorf("auth-file credential already exists as %s", existing)
		}
	}
	if err := insertAccountRows(ctx, tx, candidate, 1); err != nil {
		return AccountRecord{}, err
	}
	if identity, ok := authFileDedupeIdentity(candidate); ok {
		if err := insertRuntimeIdentityStrict(ctx, tx, accountKey, identity.Key, "auth-dedupe-key", unixMs()); err != nil {
			return AccountRecord{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AccountRecord{}, fmt.Errorf("commit create account transaction: %w", err)
	}
	tx = nil
	return s.GetAccount(ctx, accountKey)
}

func (s *Store) CreateAccounts(ctx context.Context, writes []AccountWrite) ([]AccountRecord, []AccountBatchCreateSkipped, []AccountBatchCreateError, error) {
	if s == nil || s.db == nil {
		return nil, nil, nil, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(writes) == 0 {
		return []AccountRecord{}, []AccountBatchCreateSkipped{}, []AccountBatchCreateError{}, nil
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return nil, nil, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin create accounts transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	plan, err := planAccountBatchCreates(ctx, tx, writes)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, candidate := range plan.candidates {
		if err := insertAccountRows(ctx, tx, candidate, 1); err != nil {
			return nil, nil, nil, err
		}
		if identity, ok := authFileDedupeIdentity(candidate); ok {
			if err := insertRuntimeIdentityStrict(ctx, tx, candidate.AccountKey, identity.Key, "auth-dedupe-key", unixMs()); err != nil {
				return nil, nil, nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("commit create accounts transaction: %w", err)
	}
	tx = nil
	accounts, err := s.GetAccounts(ctx, plan.accountKeys)
	if err != nil {
		return nil, nil, nil, err
	}
	return accounts, plan.skipped, plan.failures, nil
}

func (s *Store) PreviewCreateAccounts(ctx context.Context, writes []AccountWrite) (AccountBatchCreatePreviewResult, error) {
	if s == nil || s.db == nil {
		return AccountBatchCreatePreviewResult{}, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(writes) == 0 {
		return AccountBatchCreatePreviewResult{
			Items:   []AccountBatchCreatePreviewItem{},
			Skipped: []AccountBatchCreateSkipped{},
			Errors:  []AccountBatchCreateError{},
		}, nil
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return AccountBatchCreatePreviewResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AccountBatchCreatePreviewResult{}, fmt.Errorf("begin preview create accounts transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	plan, err := planAccountBatchCreates(ctx, tx, writes)
	if err != nil {
		return AccountBatchCreatePreviewResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccountBatchCreatePreviewResult{}, fmt.Errorf("commit preview create accounts transaction: %w", err)
	}
	tx = nil
	return AccountBatchCreatePreviewResult{
		Items:        plan.items,
		Skipped:      plan.skipped,
		Errors:       plan.failures,
		WouldCreate:  len(plan.candidates),
		SkippedCount: len(plan.skipped),
		Failed:       len(plan.failures),
	}, nil
}

func planAccountBatchCreates(ctx context.Context, tx *sql.Tx, writes []AccountWrite) (accountBatchCreatePlan, error) {
	plan := accountBatchCreatePlan{
		candidates:  make([]ImportCandidate, 0, len(writes)),
		accountKeys: make([]string, 0, len(writes)),
		items:       make([]AccountBatchCreatePreviewItem, 0, len(writes)),
		skipped:     make([]AccountBatchCreateSkipped, 0),
		failures:    make([]AccountBatchCreateError, 0),
	}
	seenDedupeKeys := make(map[string]string)
	for index, write := range writes {
		title := strings.TrimSpace(write.Title)
		accountKey, err := newAccountKey()
		if err != nil {
			failure := AccountBatchCreateError{Index: index, Title: title, Error: fmt.Sprintf("generate account key: %v", err)}
			plan.failures = append(plan.failures, failure)
			plan.items = append(plan.items, AccountBatchCreatePreviewItem{Index: index, Title: title, Action: "error", Reason: failure.Error})
			continue
		}
		candidate := candidateFromWrite(accountKey, write)
		if err := validateCandidate(candidate); err != nil {
			failure := AccountBatchCreateError{Index: index, Title: title, Error: err.Error()}
			plan.failures = append(plan.failures, failure)
			plan.items = append(plan.items, AccountBatchCreatePreviewItem{Index: index, Title: title, Action: "error", Reason: failure.Error})
			continue
		}
		if identity, ok := authFileDedupeIdentity(candidate); ok {
			if existing := seenDedupeKeys[identity.Key]; existing != "" {
				skip := AccountBatchCreateSkipped{
					Index:              index,
					Title:              title,
					Reason:             "duplicate_in_batch",
					ExistingAccountKey: existing,
					DedupeKeyKind:      identity.Kind,
				}
				plan.skipped = append(plan.skipped, skip)
				plan.items = append(plan.items, AccountBatchCreatePreviewItem{
					Index:              index,
					Title:              title,
					Action:             "skip",
					Reason:             skip.Reason,
					ExistingAccountKey: skip.ExistingAccountKey,
					DedupeKeyKind:      skip.DedupeKeyKind,
				})
				continue
			}
			existing, err := runtimeIdentityAccountKey(ctx, tx, identity.Key)
			if err != nil {
				return plan, err
			}
			if existing != "" {
				skip := AccountBatchCreateSkipped{
					Index:              index,
					Title:              title,
					Reason:             "existing_account",
					ExistingAccountKey: existing,
					DedupeKeyKind:      identity.Kind,
				}
				plan.skipped = append(plan.skipped, skip)
				plan.items = append(plan.items, AccountBatchCreatePreviewItem{
					Index:              index,
					Title:              title,
					Action:             "skip",
					Reason:             skip.Reason,
					ExistingAccountKey: skip.ExistingAccountKey,
					DedupeKeyKind:      skip.DedupeKeyKind,
				})
				continue
			}
			seenDedupeKeys[identity.Key] = accountKey
		}
		plan.candidates = append(plan.candidates, candidate)
		plan.accountKeys = append(plan.accountKeys, accountKey)
		plan.items = append(plan.items, AccountBatchCreatePreviewItem{Index: index, Title: title, Action: "create"})
	}
	return plan, nil
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
	if s == nil || s.db == nil {
		return AccountRecord{}, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return AccountRecord{}, fmt.Errorf("invalid account key %q", accountKey)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AccountRecord{}, fmt.Errorf("begin get account transaction %s: %w", accountKey, err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var account AccountRecord
	var disabled int
	err = tx.QueryRowContext(ctx, `
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
  COALESCE(a.last_error, ''),
  COALESCE(a.routeability_status, ''),
  COALESCE(a.routeability_reason, ''),
  COALESCE(a.registered_models_count, 0),
  COALESCE(a.last_reconcile_at_unix_ms, 0),
  COALESCE(a.failure_class, ''),
  COALESCE(a.repair_outcome, ''),
  COALESCE(a.repair_action, ''),
  COALESCE(a.repair_trigger_status, ''),
  COALESCE(a.repair_trigger_class, ''),
  COALESCE(a.repair_trigger_reason, ''),
  COALESCE(a.last_repair_at_unix_ms, 0)
FROM account_cards c
LEFT JOIN account_runtime_apply_state a ON a.account_key = c.account_key
WHERE c.account_key = ? AND c.deleted_at_unix_ms IS NULL`, accountKey).Scan(
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
		&account.RuntimeRouteabilityStatus,
		&account.RuntimeRouteabilityReason,
		&account.RuntimeRegisteredModelsCount,
		&account.LastRuntimeReconcileAtUnixMs,
		&account.RuntimeFailureClass,
		&account.RuntimeRepairOutcome,
		&account.RuntimeRepairAction,
		&account.RuntimeRepairTriggerStatus,
		&account.RuntimeRepairTriggerClass,
		&account.RuntimeRepairTriggerReason,
		&account.LastRuntimeRepairAtUnixMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountRecord{}, fmt.Errorf("account %s not found", accountKey)
	}
	if err != nil {
		return AccountRecord{}, fmt.Errorf("query account %s: %w", accountKey, err)
	}
	account.Disabled = disabled != 0
	if err := attachCredential(ctx, tx, &account); err != nil {
		return AccountRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccountRecord{}, fmt.Errorf("commit get account transaction %s: %w", accountKey, err)
	}
	return account, nil
}

func (s *Store) AccountExists(ctx context.Context, accountKey string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return false, fmt.Errorf("invalid account key %q", accountKey)
	}
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM account_cards WHERE account_key = ? AND deleted_at_unix_ms IS NULL LIMIT 1", accountKey).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query account existence %s: %w", accountKey, err)
	}
	return exists == 1, nil
}

func (s *Store) GetAccounts(ctx context.Context, accountKeys []string) ([]AccountRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	keys := make([]string, 0, len(accountKeys))
	seen := make(map[string]struct{}, len(accountKeys))
	for _, value := range accountKeys {
		accountKey := strings.TrimSpace(value)
		if !IsAccountKey(accountKey) {
			return nil, fmt.Errorf("invalid account key %q", accountKey)
		}
		if _, ok := seen[accountKey]; ok {
			continue
		}
		seen[accountKey] = struct{}{}
		keys = append(keys, accountKey)
	}
	if len(keys) == 0 {
		return []AccountRecord{}, nil
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin get accounts transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	accountByKey := make(map[string]AccountRecord, len(keys))
	const chunkSize = 400
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk))
		for _, key := range chunk {
			args = append(args, key)
		}
		rows, err := tx.QueryContext(ctx, `
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
  COALESCE(a.last_error, ''),
  COALESCE(a.routeability_status, ''),
  COALESCE(a.routeability_reason, ''),
  COALESCE(a.registered_models_count, 0),
  COALESCE(a.last_reconcile_at_unix_ms, 0),
  COALESCE(a.failure_class, ''),
  COALESCE(a.repair_outcome, ''),
  COALESCE(a.repair_action, ''),
  COALESCE(a.repair_trigger_status, ''),
  COALESCE(a.repair_trigger_class, ''),
  COALESCE(a.repair_trigger_reason, ''),
  COALESCE(a.last_repair_at_unix_ms, 0)
FROM account_cards c
LEFT JOIN account_runtime_apply_state a ON a.account_key = c.account_key
WHERE c.deleted_at_unix_ms IS NULL
  AND c.account_key IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("query accounts by keys: %w", err)
		}
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
				&account.RuntimeRouteabilityStatus,
				&account.RuntimeRouteabilityReason,
				&account.RuntimeRegisteredModelsCount,
				&account.LastRuntimeReconcileAtUnixMs,
				&account.RuntimeFailureClass,
				&account.RuntimeRepairOutcome,
				&account.RuntimeRepairAction,
				&account.RuntimeRepairTriggerStatus,
				&account.RuntimeRepairTriggerClass,
				&account.RuntimeRepairTriggerReason,
				&account.LastRuntimeRepairAtUnixMs,
			); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan account: %w", err)
			}
			account.Disabled = disabled != 0
			accountByKey[account.AccountKey] = account
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate accounts by keys: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close account rows by keys: %w", err)
		}
	}

	accounts := make([]AccountRecord, 0, len(accountByKey))
	for _, key := range keys {
		account, ok := accountByKey[key]
		if !ok {
			continue
		}
		if err := attachCredential(ctx, tx, &account); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit get accounts transaction: %w", err)
	}
	return accounts, nil
}

func (s *Store) SetAccountStatus(ctx context.Context, accountKey string, disabled bool) (AccountRecord, error) {
	if s == nil || s.db == nil {
		return AccountRecord{}, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return AccountRecord{}, fmt.Errorf("invalid account key %q", accountKey)
	}
	now := unixMs()
	result, err := s.db.ExecContext(ctx, `
UPDATE account_cards
SET disabled = ?, updated_at_unix_ms = ?
WHERE account_key = ? AND deleted_at_unix_ms IS NULL`,
		boolInt(disabled),
		now,
		accountKey,
	)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("update account status %s: %w", accountKey, err)
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return AccountRecord{}, fmt.Errorf("account %s not found", accountKey)
	}
	return s.GetAccount(ctx, accountKey)
}

func (s *Store) SetAccountPriority(ctx context.Context, accountKey string, priority int) (AccountRecord, error) {
	return s.updateAccountCardOnly(ctx, accountKey, func(current AccountRecord) (string, int, bool) {
		return current.Title, priority, current.Disabled
	})
}

func (s *Store) UpdateAuthFileCredential(ctx context.Context, accountKey string, credential AuthFileCredential) (AccountRecord, error) {
	if s == nil || s.db == nil {
		return AccountRecord{}, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return AccountRecord{}, fmt.Errorf("invalid account key %q", accountKey)
	}
	if strings.TrimSpace(credential.AuthJSON) == "" {
		return AccountRecord{}, fmt.Errorf("auth-file account %s missing credential", accountKey)
	}
	current, err := s.GetAccount(ctx, accountKey)
	if err != nil {
		return AccountRecord{}, err
	}
	if current.Kind != KindAuthFile {
		return AccountRecord{}, fmt.Errorf("account %s is %s, not auth-file", accountKey, current.Kind)
	}
	if current.AuthFile != nil {
		if strings.TrimSpace(credential.SourceFileName) == "" {
			credential.SourceFileName = current.AuthFile.SourceFileName
		}
		if strings.TrimSpace(credential.AuthType) == "" {
			credential.AuthType = current.AuthFile.AuthType
		}
		if strings.TrimSpace(credential.Email) == "" {
			credential.Email = current.AuthFile.Email
		}
		if strings.TrimSpace(credential.PlanType) == "" {
			credential.PlanType = current.AuthFile.PlanType
		}
		if credential.ModifiedUnixMs == 0 {
			credential.ModifiedUnixMs = current.AuthFile.ModifiedUnixMs
		}
	}
	if strings.TrimSpace(credential.SourceFileName) == "" {
		credential.SourceFileName = accountKey + ".json"
	}
	if strings.TrimSpace(credential.AuthType) == "" {
		credential.AuthType = "codex"
	}
	if credential.SizeBytes == 0 {
		credential.SizeBytes = int64(len([]byte(credential.AuthJSON)))
	}
	now := unixMs()
	if credential.ModifiedUnixMs == 0 {
		credential.ModifiedUnixMs = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("begin update auth-file credential transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.ExecContext(ctx, `
UPDATE auth_file_accounts
SET source_file_name = ?, auth_json = ?, auth_fingerprint = ?, auth_type = ?, email = ?, plan_type = ?, modified_unix_ms = ?, size_bytes = ?, updated_at_unix_ms = ?
WHERE account_key = ?`,
		strings.TrimSpace(credential.SourceFileName),
		credential.AuthJSON,
		fingerprintString(credential.AuthJSON),
		strings.TrimSpace(credential.AuthType),
		strings.TrimSpace(credential.Email),
		strings.TrimSpace(credential.PlanType),
		credential.ModifiedUnixMs,
		credential.SizeBytes,
		now,
		accountKey,
	)
	if err != nil {
		return AccountRecord{}, fmt.Errorf("update auth-file credential %s: %w", accountKey, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return AccountRecord{}, fmt.Errorf("update auth-file credential %s rows affected: %w", accountKey, err)
	}
	if rows == 0 {
		return AccountRecord{}, fmt.Errorf("auth-file credential %s not found", accountKey)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE account_cards SET updated_at_unix_ms = ? WHERE account_key = ? AND deleted_at_unix_ms IS NULL", now, accountKey); err != nil {
		return AccountRecord{}, fmt.Errorf("touch account card %s: %w", accountKey, err)
	}
	if err := tx.Commit(); err != nil {
		return AccountRecord{}, fmt.Errorf("commit update auth-file credential transaction: %w", err)
	}
	tx = nil
	return s.GetAccount(ctx, accountKey)
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

func (s *Store) DeleteAccounts(ctx context.Context, accountKeys []string) ([]string, []AccountBatchMutationError, error) {
	if s == nil || s.db == nil {
		return nil, nil, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if len(accountKeys) == 0 {
		return []string{}, []AccountBatchMutationError{}, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin delete accounts transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	now := unixMs()
	deleted := make([]string, 0, len(accountKeys))
	failures := make([]AccountBatchMutationError, 0)
	for _, accountKey := range accountKeys {
		accountKey = strings.TrimSpace(accountKey)
		if !IsAccountKey(accountKey) {
			failures = append(failures, AccountBatchMutationError{AccountKey: accountKey, Error: fmt.Sprintf("invalid account key %q", accountKey)})
			continue
		}
		result, err := tx.ExecContext(ctx, "UPDATE account_cards SET deleted_at_unix_ms = ?, updated_at_unix_ms = ? WHERE account_key = ? AND deleted_at_unix_ms IS NULL", now, now, accountKey)
		if err != nil {
			failures = append(failures, AccountBatchMutationError{AccountKey: accountKey, Error: err.Error()})
			continue
		}
		rows, err := result.RowsAffected()
		if err != nil {
			failures = append(failures, AccountBatchMutationError{AccountKey: accountKey, Error: err.Error()})
			continue
		}
		if rows == 0 {
			failures = append(failures, AccountBatchMutationError{AccountKey: accountKey, Error: fmt.Sprintf("account %s not found", accountKey)})
			continue
		}
		deleted = append(deleted, accountKey)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit delete accounts transaction: %w", err)
	}
	tx = nil
	return deleted, failures, nil
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
SET status = ?, last_error = ?, applied_at_unix_ms = ?, updated_at_unix_ms = ?,
    routeability_status = CASE WHEN ? = 'failed' THEN 'degraded' ELSE routeability_status END,
    routeability_reason = CASE WHEN ? = 'failed' THEN ? ELSE routeability_reason END,
    failure_class = CASE WHEN ? = 'failed' THEN 'runtime_apply_failed' WHEN ? = 'applied' THEN '' ELSE failure_class END,
    registered_models_count = CASE WHEN ? = 'failed' THEN 0 ELSE registered_models_count END,
    last_reconcile_at_unix_ms = CASE WHEN ? = 'failed' THEN ? ELSE last_reconcile_at_unix_ms END
WHERE account_key = ? AND revision = ?`,
		status,
		strings.TrimSpace(lastError),
		appliedAt,
		now,
		status,
		status,
		strings.TrimSpace(lastError),
		status,
		status,
		status,
		status,
		now,
		accountKey,
		revision,
	)
	if err != nil {
		return fmt.Errorf("mark runtime apply result %s: %w", accountKey, err)
	}
	return nil
}

func (s *Store) MarkPendingRuntimeApplyResults(ctx context.Context, status string, lastError string) error {
	if s == nil || s.db == nil {
		return errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
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
SET status = ?, last_error = ?, applied_at_unix_ms = ?, updated_at_unix_ms = ?,
    routeability_status = CASE WHEN ? = 'failed' THEN 'degraded' WHEN ? = 'applied' THEN 'pending' ELSE routeability_status END,
    routeability_reason = CASE WHEN ? = 'failed' THEN ? ELSE '' END,
    failure_class = CASE WHEN ? = 'failed' THEN 'runtime_apply_failed' ELSE '' END,
    registered_models_count = 0,
    last_reconcile_at_unix_ms = CASE WHEN ? = 'failed' THEN ? ELSE 0 END
WHERE status = 'pending'
  AND EXISTS (
    SELECT 1
    FROM account_cards c
    WHERE c.account_key = account_runtime_apply_state.account_key
      AND c.revision = account_runtime_apply_state.revision
      AND c.deleted_at_unix_ms IS NULL
  )`,
		status,
		strings.TrimSpace(lastError),
		appliedAt,
		now,
		status,
		status,
		status,
		strings.TrimSpace(lastError),
		status,
		status,
		now,
	)
	if err != nil {
		return fmt.Errorf("mark pending runtime apply results: %w", err)
	}
	return nil
}

func (s *Store) MarkRuntimeRouteability(ctx context.Context, accountKey string, revision int, status string, reason string, failureClass string, registeredModelsCount int) error {
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
	case "pending", "applied_not_registered", "registered_routeable", "degraded":
	default:
		return fmt.Errorf("invalid runtime routeability status %q", status)
	}
	if registeredModelsCount < 0 {
		registeredModelsCount = 0
	}
	now := unixMs()
	_, err := s.db.ExecContext(ctx, `
UPDATE account_runtime_apply_state
SET routeability_status = ?, routeability_reason = ?, failure_class = ?, registered_models_count = ?, last_reconcile_at_unix_ms = ?, updated_at_unix_ms = ?
WHERE account_key = ? AND revision = ?`,
		status,
		strings.TrimSpace(reason),
		strings.TrimSpace(failureClass),
		registeredModelsCount,
		now,
		now,
		accountKey,
		revision,
	)
	if err != nil {
		return fmt.Errorf("mark runtime routeability %s: %w", accountKey, err)
	}
	return nil
}

func (s *Store) MarkRuntimeRepairResult(ctx context.Context, accountKey string, revision int, outcome string, action string, triggerStatus string, triggerClass string, triggerReason string) error {
	if s == nil || s.db == nil {
		return errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !IsAccountKey(accountKey) {
		return fmt.Errorf("invalid account key %q", accountKey)
	}
	outcome = strings.TrimSpace(outcome)
	switch outcome {
	case "recovered", "failed":
	default:
		return fmt.Errorf("invalid runtime repair outcome %q", outcome)
	}
	action = strings.TrimSpace(action)
	switch action {
	case "watcher_refresh", "resynthesize_refresh":
	default:
		return fmt.Errorf("invalid runtime repair action %q", action)
	}
	now := unixMs()
	_, err := s.db.ExecContext(ctx, `
UPDATE account_runtime_apply_state
SET repair_outcome = ?, repair_action = ?, repair_trigger_status = ?, repair_trigger_class = ?, repair_trigger_reason = ?, last_repair_at_unix_ms = ?, updated_at_unix_ms = ?
WHERE account_key = ? AND revision = ?`,
		outcome,
		action,
		strings.TrimSpace(triggerStatus),
		strings.TrimSpace(triggerClass),
		strings.TrimSpace(triggerReason),
		now,
		now,
		accountKey,
		revision,
	)
	if err != nil {
		return fmt.Errorf("mark runtime repair result %s: %w", accountKey, err)
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
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin list accounts transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	rows, err := tx.QueryContext(ctx, `
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
  COALESCE(a.last_error, ''),
  COALESCE(a.routeability_status, ''),
  COALESCE(a.routeability_reason, ''),
  COALESCE(a.registered_models_count, 0),
  COALESCE(a.last_reconcile_at_unix_ms, 0),
  COALESCE(a.failure_class, ''),
  COALESCE(a.repair_outcome, ''),
  COALESCE(a.repair_action, ''),
  COALESCE(a.repair_trigger_status, ''),
  COALESCE(a.repair_trigger_class, ''),
  COALESCE(a.repair_trigger_reason, ''),
  COALESCE(a.last_repair_at_unix_ms, 0)
FROM account_cards c
LEFT JOIN account_runtime_apply_state a ON a.account_key = c.account_key
WHERE c.deleted_at_unix_ms IS NULL
ORDER BY c.priority DESC, c.created_at_unix_ms ASC, c.account_key ASC`)
	if err != nil {
		return nil, fmt.Errorf("query accounts: %w", err)
	}

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
			&account.RuntimeRouteabilityStatus,
			&account.RuntimeRouteabilityReason,
			&account.RuntimeRegisteredModelsCount,
			&account.LastRuntimeReconcileAtUnixMs,
			&account.RuntimeFailureClass,
			&account.RuntimeRepairOutcome,
			&account.RuntimeRepairAction,
			&account.RuntimeRepairTriggerStatus,
			&account.RuntimeRepairTriggerClass,
			&account.RuntimeRepairTriggerReason,
			&account.LastRuntimeRepairAtUnixMs,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan account: %w", err)
		}
		account.Disabled = disabled != 0
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate accounts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close account rows: %w", err)
	}
	for i := range accounts {
		if err := attachCredential(ctx, tx, &accounts[i]); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit list accounts transaction: %w", err)
	}
	return accounts, nil
}

func (s *Store) ListAccountCards(ctx context.Context) ([]AccountRecord, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin list account cards transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	rows, err := tx.QueryContext(ctx, `
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
  COALESCE(a.last_error, ''),
  COALESCE(a.routeability_status, ''),
  COALESCE(a.routeability_reason, ''),
  COALESCE(a.registered_models_count, 0),
  COALESCE(a.last_reconcile_at_unix_ms, 0),
  COALESCE(a.failure_class, ''),
  COALESCE(a.repair_outcome, ''),
  COALESCE(a.repair_action, ''),
  COALESCE(a.repair_trigger_status, ''),
  COALESCE(a.repair_trigger_class, ''),
  COALESCE(a.repair_trigger_reason, ''),
  COALESCE(a.last_repair_at_unix_ms, 0)
FROM account_cards c
LEFT JOIN account_runtime_apply_state a ON a.account_key = c.account_key
WHERE c.deleted_at_unix_ms IS NULL
ORDER BY c.priority DESC, c.created_at_unix_ms ASC, c.account_key ASC`)
	if err != nil {
		return nil, fmt.Errorf("query account cards: %w", err)
	}

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
			&account.RuntimeRouteabilityStatus,
			&account.RuntimeRouteabilityReason,
			&account.RuntimeRegisteredModelsCount,
			&account.LastRuntimeReconcileAtUnixMs,
			&account.RuntimeFailureClass,
			&account.RuntimeRepairOutcome,
			&account.RuntimeRepairAction,
			&account.RuntimeRepairTriggerStatus,
			&account.RuntimeRepairTriggerClass,
			&account.RuntimeRepairTriggerReason,
			&account.LastRuntimeRepairAtUnixMs,
		); err != nil {
			_ = rows.Close()
			if len(accounts) > 0 {
				return accounts, fmt.Errorf("scan account card: %w", err)
			}
			return nil, fmt.Errorf("scan account card: %w", err)
		}
		account.Disabled = disabled != 0
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		if len(accounts) > 0 {
			return accounts, fmt.Errorf("iterate account cards: %w", err)
		}
		return nil, fmt.Errorf("iterate account cards: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close account card rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit list account cards transaction: %w", err)
	}
	return accounts, nil
}

func attachCredential(ctx context.Context, queryer accountStoreQueryer, account *AccountRecord) error {
	switch account.Kind {
	case KindAuthFile:
		var credential AuthFileCredential
		err := queryer.QueryRowContext(ctx, `
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
		err := queryer.QueryRowContext(ctx, `
SELECT api_key, api_key_fingerprint, base_url, prefix, proxy_url, websockets, quota_curl, quota_enabled, billing_curl, billing_enabled, platform_cookie, curl_variables_json, format_base_urls_json, headers_json, models_json, excluded_models_json
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
			&credential.PlatformCookie,
			&credential.CurlVariablesJSON,
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
		var quotaEnabled, billingEnabled int
		err := queryer.QueryRowContext(ctx, `
SELECT provider_name, runtime_provider_key, base_url, prefix, api_key_entries_json, quota_curl, quota_enabled, billing_curl, billing_enabled, platform_cookie, curl_variables_json, headers_json, format_base_urls_json, models_json, model_fetch_api_key, model_fetch_base_url
FROM openai_compatible_accounts WHERE account_key = ?`, account.AccountKey).Scan(
			&credential.ProviderName,
			&credential.RuntimeProviderKey,
			&credential.BaseURL,
			&credential.Prefix,
			&credential.APIKeyEntriesJSON,
			&credential.QuotaCurl,
			&quotaEnabled,
			&credential.BillingCurl,
			&billingEnabled,
			&credential.PlatformCookie,
			&credential.CurlVariablesJSON,
			&credential.HeadersJSON,
			&credential.FormatBaseURLsJSON,
			&credential.ModelsJSON,
			&credential.ModelFetchAPIKey,
			&credential.ModelFetchBaseURL,
		)
		if err != nil {
			return fmt.Errorf("query openai-compatible credential for %s: %w", account.AccountKey, err)
		}
		credential.QuotaEnabled = quotaEnabled != 0
		credential.BillingEnabled = billingEnabled != 0
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
INSERT INTO codex_api_key_accounts(account_key, api_key, api_key_fingerprint, base_url, prefix, proxy_url, websockets, quota_curl, quota_enabled, billing_curl, billing_enabled, platform_cookie, curl_variables_json, format_base_urls_json, headers_json, models_json, excluded_models_json, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
			normalizePlatformCookie(key.PlatformCookie),
			defaultJSON(key.CurlVariablesJSON, "{}"),
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
INSERT INTO openai_compatible_accounts(account_key, provider_name, runtime_provider_key, base_url, prefix, api_key_entries_json, quota_curl, quota_enabled, billing_curl, billing_enabled, platform_cookie, curl_variables_json, headers_json, format_base_urls_json, models_json, model_fetch_api_key, model_fetch_base_url, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			candidate.AccountKey,
			compat.ProviderName,
			runtimeProviderKey,
			compat.BaseURL,
			compat.Prefix,
			defaultJSON(compat.APIKeyEntriesJSON, "[]"),
			strings.TrimSpace(compat.QuotaCurl),
			boolInt(compat.QuotaEnabled && strings.TrimSpace(compat.QuotaCurl) != ""),
			strings.TrimSpace(compat.BillingCurl),
			boolInt(compat.BillingEnabled && strings.TrimSpace(compat.BillingCurl) != ""),
			strings.TrimSpace(compat.PlatformCookie),
			defaultJSON(compat.CurlVariablesJSON, "{}"),
			defaultJSON(compat.HeadersJSON, "{}"),
			defaultJSON(compat.FormatBaseURLsJSON, "{}"),
			defaultJSON(compat.ModelsJSON, "[]"),
			strings.TrimSpace(compat.ModelFetchAPIKey),
			strings.TrimSpace(compat.ModelFetchBaseURL),
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
INSERT INTO account_runtime_apply_state(
  account_key, revision, status, last_error,
  routeability_status, routeability_reason, registered_models_count, last_reconcile_at_unix_ms,
  failure_class, repair_outcome, repair_action, repair_trigger_status, repair_trigger_class, repair_trigger_reason, last_repair_at_unix_ms,
  applied_at_unix_ms, updated_at_unix_ms
)
VALUES (?, ?, 'pending', '', 'pending', '', 0, 0, '', '', '', '', '', '', 0, 0, ?)
ON CONFLICT(account_key) DO UPDATE SET
  revision = excluded.revision,
  status = 'pending',
  last_error = '',
  routeability_status = 'pending',
  routeability_reason = '',
  registered_models_count = 0,
  last_reconcile_at_unix_ms = 0,
  failure_class = '',
  repair_outcome = '',
  repair_action = '',
  repair_trigger_status = '',
  repair_trigger_class = '',
  repair_trigger_reason = '',
  last_repair_at_unix_ms = 0,
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

func insertRuntimeIdentityStrict(ctx context.Context, tx *sql.Tx, accountKey string, identityKey string, identityKind string, now int64) error {
	if strings.TrimSpace(identityKey) == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM account_runtime_identities
WHERE identity_key = ?
  AND account_key IN (SELECT account_key FROM account_cards WHERE deleted_at_unix_ms IS NOT NULL)`, identityKey); err != nil {
		return fmt.Errorf("delete stale runtime identity %s: %w", identityKey, err)
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO account_runtime_identities(identity_key, account_key, identity_kind, created_at_unix_ms, updated_at_unix_ms)
VALUES (?, ?, ?, ?, ?)`, identityKey, accountKey, identityKind, now, now)
	if err != nil {
		return fmt.Errorf("insert runtime identity %s: %w", identityKey, err)
	}
	return nil
}

func runtimeIdentityAccountKey(ctx context.Context, tx *sql.Tx, identityKey string) (string, error) {
	if strings.TrimSpace(identityKey) == "" {
		return "", nil
	}
	var accountKey string
	err := tx.QueryRowContext(ctx, `
SELECT ri.account_key
FROM account_runtime_identities ri
JOIN account_cards ac ON ac.account_key = ri.account_key
WHERE ri.identity_key = ? AND ac.deleted_at_unix_ms IS NULL
LIMIT 1`, identityKey).Scan(&accountKey)
	if err == nil {
		return accountKey, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return "", fmt.Errorf("query runtime identity %s: %w", identityKey, err)
}

func authFileDedupeIdentity(candidate ImportCandidate) (authFileDedupeKey, bool) {
	if candidate.Kind != KindAuthFile || candidate.AuthFile == nil {
		return authFileDedupeKey{}, false
	}
	authJSON := strings.TrimSpace(candidate.AuthFile.AuthJSON)
	if authJSON == "" {
		return authFileDedupeKey{}, false
	}
	var payload any
	if err := json.Unmarshal([]byte(authJSON), &payload); err != nil {
		return authFileDedupeKey{
			Key:  authDedupeIdentityKey("codex", "normalized-json", authJSON),
			Kind: "normalized-json",
		}, true
	}
	authType := strings.ToLower(strings.TrimSpace(candidate.AuthFile.AuthType))
	if authType == "" {
		authType = strings.ToLower(firstStringField(payload, "type"))
	}
	if authType == "" {
		authType = "codex"
	}
	for _, field := range []struct {
		kind string
		keys []string
	}{
		{kind: "refresh-token", keys: []string{"refresh_token", "refreshToken"}},
		{kind: "id-token", keys: []string{"id_token", "idToken"}},
		{kind: "access-token", keys: []string{"access_token", "accessToken"}},
	} {
		for _, key := range field.keys {
			if value := firstStringField(payload, key); value != "" {
				return authFileDedupeKey{
					Key:  authDedupeIdentityKey(authType, field.kind, value),
					Kind: field.kind,
				}, true
			}
		}
	}
	email := strings.ToLower(firstStringField(payload, "email"))
	accountID := firstStringField(payload, "account_id")
	if accountID == "" {
		accountID = firstStringField(payload, "chatgpt_account_id")
	}
	if email != "" && accountID != "" {
		return authFileDedupeKey{
			Key:  authDedupeIdentityKey(authType, "principal", email+"\x00"+accountID),
			Kind: "principal",
		}, true
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		canonical = []byte(authJSON)
	}
	return authFileDedupeKey{
		Key:  authDedupeIdentityKey(authType, "normalized-json", string(canonical)),
		Kind: "normalized-json",
	}, true
}

func authDedupeIdentityKey(authType string, kind string, material string) string {
	authType = strings.ToLower(strings.TrimSpace(authType))
	if authType == "" {
		authType = "codex"
	}
	return "auth-file:" + authType + ":" + kind + ":" + fingerprintString(material)
}

func firstStringField(value any, key string) string {
	switch typed := value.(type) {
	case map[string]any:
		if raw, ok := typed[key]; ok {
			if str, ok := raw.(string); ok {
				return strings.TrimSpace(str)
			}
		}
		for _, child := range typed {
			if found := firstStringField(child, key); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := firstStringField(child, key); found != "" {
				return found
			}
		}
	}
	return ""
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
