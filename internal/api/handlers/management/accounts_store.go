package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type accountWriteRequest struct {
	Kind             accountstore.AccountKind                 `json:"kind"`
	Title            string                                   `json:"title"`
	Provider         string                                   `json:"provider"`
	Priority         int                                      `json:"priority"`
	Disabled         bool                                     `json:"disabled"`
	Credential       json.RawMessage                          `json:"credential"`
	AuthFile         *accountstore.AuthFileCredential         `json:"auth_file,omitempty"`
	CodexAPIKey      *accountstore.CodexAPIKeyCredential      `json:"codex_api_key,omitempty"`
	OpenAICompatible *accountstore.OpenAICompatibleCredential `json:"openai_compatible,omitempty"`
}

type accountMigrationRequest struct {
	AuthDir              string                        `json:"auth_dir,omitempty"`
	CodexAPIKeyStoreDirs []string                      `json:"codex_api_key_store_dirs,omitempty"`
	Report               *accountstore.MigrationReport `json:"report,omitempty"`
}

type accountBatchDeleteRequest struct {
	AccountKeys []string `json:"account_keys"`
}

type accountBatchCreateRequest struct {
	Accounts []accountWriteRequest `json:"accounts"`
}

type accountBatchCreateResponse struct {
	Accounts     []accountstore.AccountRecord             `json:"accounts"`
	Skipped      []accountstore.AccountBatchCreateSkipped `json:"skipped"`
	Errors       []accountstore.AccountBatchCreateError   `json:"errors"`
	Succeeded    int                                      `json:"succeeded"`
	SkippedCount int                                      `json:"skipped_count"`
	Failed       int                                      `json:"failed"`
}

type accountBatchCreatePreviewResponse struct {
	Items        []accountstore.AccountBatchCreatePreviewItem `json:"items"`
	Skipped      []accountstore.AccountBatchCreateSkipped     `json:"skipped"`
	Errors       []accountstore.AccountBatchCreateError       `json:"errors"`
	WouldCreate  int                                          `json:"would_create"`
	SkippedCount int                                          `json:"skipped_count"`
	Failed       int                                          `json:"failed"`
}

type accountBatchDeleteResponse struct {
	DeletedAccountKeys []string                                 `json:"deleted_account_keys"`
	Errors             []accountstore.AccountBatchMutationError `json:"errors"`
	Succeeded          int                                      `json:"succeeded"`
	Failed             int                                      `json:"failed"`
}

type deleteLegacySourcesRequest struct {
	BackupDir string `json:"backup_dir,omitempty"`
}

type accountStoreDiagnosticsResponse struct {
	PathBasename               string                                  `json:"path_basename"`
	Configured                 bool                                    `json:"configured"`
	Open                       bool                                    `json:"open"`
	ReadRecovery               accountStoreReadRecoveryDiagnostics     `json:"read_recovery"`
	KnownOpenAICompatibleAudit accountstore.KnownOpenAICompatibleAudit `json:"known_openai_compatible_audit"`
}

type accountStoreReadRecoveryDiagnostics struct {
	Count             int    `json:"count"`
	LastEndpoint      string `json:"last_endpoint"`
	LastRecovered     bool   `json:"last_recovered"`
	LastError         string `json:"last_error"`
	LastRecoveredUnix int64  `json:"last_recovered_at_unix_ms"`
}

func (h *Handler) GetAccount(c *gin.Context) {
	store, account, err := h.getAccountWithReadRecovery(c.Request.Context(), c.Param("account_key"))
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	account = h.applyAccountStoreRuntime(c.Request.Context(), store, account)
	c.JSON(http.StatusOK, account)
}

func (h *Handler) CreateAccount(c *gin.Context) {
	write, err := decodeAccountWriteRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	account, err := store.CreateAccount(c.Request.Context(), write)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	account = h.applyAccountStoreRuntime(c.Request.Context(), store, account)
	c.JSON(http.StatusOK, account)
}

func (h *Handler) CreateAccountsBatch(c *gin.Context) {
	var body accountBatchCreateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if len(body.Accounts) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "accounts is required"})
		return
	}
	writes := make([]accountstore.AccountWrite, 0, len(body.Accounts))
	for index, req := range body.Accounts {
		write, err := decodeAccountWriteRequestBody(req)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("account %d: %v", index, err)})
			return
		}
		writes = append(writes, write)
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	accounts, skipped, failures, err := store.CreateAccounts(c.Request.Context(), writes)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	if len(accounts) > 0 {
		_ = h.applyPendingAccountStoreRuntime(c.Request.Context(), store)
		accountKeys := make([]string, 0, len(accounts))
		for _, account := range accounts {
			accountKeys = append(accountKeys, account.AccountKey)
		}
		if refreshed, refreshErr := store.GetAccounts(c.Request.Context(), accountKeys); refreshErr == nil {
			accounts = refreshed
		}
	}
	c.JSON(http.StatusOK, accountBatchCreateResponse{
		Accounts:     accounts,
		Skipped:      skipped,
		Errors:       failures,
		Succeeded:    len(accounts),
		SkippedCount: len(skipped),
		Failed:       len(failures),
	})
}

func (h *Handler) PreviewAccountsBatch(c *gin.Context) {
	var body accountBatchCreateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if len(body.Accounts) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "accounts is required"})
		return
	}
	writes := make([]accountstore.AccountWrite, 0, len(body.Accounts))
	for index, req := range body.Accounts {
		write, err := decodeAccountWriteRequestBody(req)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("account %d: %v", index, err)})
			return
		}
		writes = append(writes, write)
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	result, err := store.PreviewCreateAccounts(c.Request.Context(), writes)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, accountBatchCreatePreviewResponse{
		Items:        result.Items,
		Skipped:      result.Skipped,
		Errors:       result.Errors,
		WouldCreate:  result.WouldCreate,
		SkippedCount: result.SkippedCount,
		Failed:       result.Failed,
	})
}

func (h *Handler) PatchAccount(c *gin.Context) {
	write, err := decodeAccountWriteRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	account, err := store.UpdateAccount(c.Request.Context(), c.Param("account_key"), write)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	account = h.applyAccountStoreRuntime(c.Request.Context(), store, account)
	c.JSON(http.StatusOK, account)
}

func (h *Handler) DeleteAccount(c *gin.Context) {
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	accountKey := c.Param("account_key")
	if err := store.DeleteAccount(c.Request.Context(), accountKey); err != nil {
		writeAccountStoreError(c, err)
		return
	}
	_ = h.cancelQuotaRefreshBatchJobsForAccountKeys([]string{accountKey}, "quota refresh job canceled because account was deleted", time.Now().UTC())
	_ = h.triggerAccountStoreDelete(c.Request.Context(), []string{accountKey})
	_ = h.triggerAccountStoreApply(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) DeleteAccountsBatch(c *gin.Context) {
	var body accountBatchDeleteRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	accountKeys := normalizeAccountBatchKeys(body.AccountKeys)
	if len(accountKeys) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_keys is required"})
		return
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	deleted, failures, err := store.DeleteAccounts(c.Request.Context(), accountKeys)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	if len(deleted) > 0 {
		_ = h.cancelQuotaRefreshBatchJobsForAccountKeys(deleted, "quota refresh job canceled because account was deleted", time.Now().UTC())
		_ = h.triggerAccountStoreDelete(c.Request.Context(), deleted)
		_ = h.triggerAccountStoreApply(c.Request.Context())
	}
	c.JSON(http.StatusOK, accountBatchDeleteResponse{
		DeletedAccountKeys: deleted,
		Errors:             failures,
		Succeeded:          len(deleted),
		Failed:             len(failures),
	})
}

func (h *Handler) PatchAccountStatus(c *gin.Context) {
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	account, err := store.SetAccountStatus(c.Request.Context(), c.Param("account_key"), body.Disabled)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	_ = h.triggerAccountStoreStatusChange(c.Request.Context(), account)
	c.JSON(http.StatusOK, account)
}

func (h *Handler) PatchAccountPriority(c *gin.Context) {
	var body struct {
		Priority int `json:"priority"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	account, err := store.SetAccountPriority(c.Request.Context(), c.Param("account_key"), body.Priority)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	account = h.applyAccountStoreRuntime(c.Request.Context(), store, account)
	c.JSON(http.StatusOK, account)
}

func (h *Handler) ListAccounts(c *gin.Context) {
	store, accounts, err := h.listAccountsWithReadRecovery(c.Request.Context())
	if err != nil {
		_, cardAccounts, cardErr := h.listAccountCardsWithReadRecovery(c.Request.Context())
		if len(cardAccounts) > 0 {
			c.JSON(http.StatusOK, gin.H{
				"accounts": cardAccounts,
				"degraded": true,
				"warning":  err.Error(),
			})
			return
		}
		if cardErr != nil {
			err = fmt.Errorf("%w; degraded account card snapshot failed: %v", err, cardErr)
		}
		writeAccountStoreError(c, err)
		return
	}
	if hasPendingAccountStoreRuntime(accounts) {
		_ = h.applyPendingAccountStoreRuntime(c.Request.Context(), store)
		_, accounts, err = h.listAccountsWithReadRecovery(c.Request.Context())
		if err != nil {
			writeAccountStoreError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

func (h *Handler) getAccountWithReadRecovery(ctx context.Context, accountKey string) (*accountstore.Store, accountstore.AccountRecord, error) {
	store, err := h.openAccountStore(ctx)
	if err != nil {
		return nil, accountstore.AccountRecord{}, err
	}
	account, err := store.GetAccount(ctx, accountKey)
	if err == nil || !isRecoverableAccountStoreReadError(err) {
		return store, account, err
	}
	h.resetAccountStore(store)
	store, retryErr := h.openAccountStore(ctx)
	if retryErr != nil {
		h.recordAccountStoreReadRecovery("accounts/:account_key", err, false)
		return nil, accountstore.AccountRecord{}, retryErr
	}
	account, retryErr = store.GetAccount(ctx, accountKey)
	h.recordAccountStoreReadRecovery("accounts/:account_key", err, retryErr == nil)
	return store, account, retryErr
}

func (h *Handler) listAccountsWithReadRecovery(ctx context.Context) (*accountstore.Store, []accountstore.AccountRecord, error) {
	store, err := h.openAccountStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	accounts, err := store.ListAccounts(ctx)
	if err == nil || !isRecoverableAccountStoreReadError(err) {
		return store, accounts, err
	}
	h.resetAccountStore(store)
	store, retryErr := h.openAccountStore(ctx)
	if retryErr != nil {
		h.recordAccountStoreReadRecovery("accounts", err, false)
		return nil, nil, retryErr
	}
	accounts, retryErr = store.ListAccounts(ctx)
	h.recordAccountStoreReadRecovery("accounts", err, retryErr == nil)
	return store, accounts, retryErr
}

func (h *Handler) listAccountCardsWithReadRecovery(ctx context.Context) (*accountstore.Store, []accountstore.AccountRecord, error) {
	store, err := h.openAccountStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	accounts, err := store.ListAccountCards(ctx)
	if err == nil || !isRecoverableAccountStoreReadError(err) {
		return store, accounts, err
	}
	h.resetAccountStore(store)
	store, retryErr := h.openAccountStore(ctx)
	if retryErr != nil {
		h.recordAccountStoreReadRecovery("accounts:cards", err, false)
		return nil, accounts, retryErr
	}
	retryAccounts, retryErr := store.ListAccountCards(ctx)
	h.recordAccountStoreReadRecovery("accounts:cards", err, retryErr == nil)
	if len(retryAccounts) > 0 {
		return store, retryAccounts, retryErr
	}
	return store, accounts, retryErr
}

func (h *Handler) resetAccountStore(store *accountstore.Store) {
	if h == nil || store == nil {
		return
	}
	shouldClose := false
	h.accountStoreMu.Lock()
	if h.accountStore == store {
		h.accountStore = nil
		h.accountStoreDBPath = ""
		shouldClose = true
	}
	h.accountStoreMu.Unlock()
	if shouldClose {
		_ = store.Close()
	}
}

func isRecoverableAccountStoreReadError(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr interface{ Code() int }
	if errors.As(err, &sqliteErr) {
		code := sqliteErr.Code()
		if code == 10 || code&0xff == 10 {
			return true
		}
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		message := current.Error()
		if strings.Contains(message, "SQLITE_IOERR") ||
			strings.Contains(message, "disk I/O error") ||
			strings.Contains(message, "(522)") ||
			strings.Contains(message, "sql: database is closed") {
			return true
		}
	}
	return false
}

func (h *Handler) GetAccountStoreDiagnostics(c *gin.Context) {
	diagnostics, err := h.accountStoreDiagnostics(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, diagnostics)
}

func (h *Handler) accountStoreDiagnostics(ctx context.Context) (accountStoreDiagnosticsResponse, error) {
	if h == nil {
		return accountStoreDiagnosticsResponse{}, fmt.Errorf("management handler is nil")
	}
	path, err := h.resolveAccountStorePath()
	if err != nil {
		return accountStoreDiagnosticsResponse{}, err
	}
	audit := accountstore.KnownOpenAICompatibleAudit{
		ByProvider:                          map[string]int{},
		MisclassifiedKnownOpenAICompatCards: []accountstore.KnownOpenAICompatibleMisclassifiedAccount{},
	}
	if strings.TrimSpace(path) != "" {
		store, openErr := accountstore.Open(path)
		if openErr == nil {
			if ensureErr := store.EnsureSchema(ctx); ensureErr == nil {
				if result, auditErr := store.AuditKnownOpenAICompatibleMisclassifications(ctx); auditErr == nil {
					audit = result
				}
			}
			_ = store.Close()
		}
	}

	h.accountStoreMu.Lock()
	defer h.accountStoreMu.Unlock()
	return accountStoreDiagnosticsResponse{
		PathBasename: filepath.Base(path),
		Configured:   strings.TrimSpace(path) != "",
		Open:         h.accountStore != nil,
		ReadRecovery: accountStoreReadRecoveryDiagnostics{
			Count:             h.accountStoreReadRecoveryCount,
			LastEndpoint:      h.accountStoreLastReadEndpoint,
			LastRecovered:     h.accountStoreLastReadRecovered,
			LastError:         h.accountStoreLastReadError,
			LastRecoveredUnix: h.accountStoreLastReadRecoveredAt,
		},
		KnownOpenAICompatibleAudit: audit,
	}, nil
}

func (h *Handler) recordAccountStoreReadRecovery(endpoint string, err error, recovered bool) {
	if h == nil || err == nil {
		return
	}
	h.accountStoreMu.Lock()
	defer h.accountStoreMu.Unlock()
	h.accountStoreReadRecoveryCount++
	h.accountStoreLastReadEndpoint = strings.TrimSpace(endpoint)
	h.accountStoreLastReadError = err.Error()
	h.accountStoreLastReadRecovered = recovered
	if recovered {
		h.accountStoreLastReadRecoveredAt = time.Now().UnixMilli()
	}
}

func decodeAccountWriteRequest(c *gin.Context) (accountstore.AccountWrite, error) {
	var req accountWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return accountstore.AccountWrite{}, err
	}
	return decodeAccountWriteRequestBody(req)
}

func decodeAccountWriteRequestBody(req accountWriteRequest) (accountstore.AccountWrite, error) {
	write := accountstore.AccountWrite{
		Kind:             req.Kind,
		Title:            strings.TrimSpace(req.Title),
		Provider:         strings.TrimSpace(req.Provider),
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		Priority:         req.Priority,
		Disabled:         req.Disabled,
		AuthFile:         req.AuthFile,
		CodexAPIKey:      req.CodexAPIKey,
		OpenAICompatible: req.OpenAICompatible,
	}
	if len(req.Credential) > 0 && string(req.Credential) != "null" {
		switch req.Kind {
		case accountstore.KindAuthFile:
			var credential accountstore.AuthFileCredential
			if err := json.Unmarshal(req.Credential, &credential); err != nil {
				return accountstore.AccountWrite{}, fmt.Errorf("parse auth-file credential: %w", err)
			}
			write.AuthFile = &credential
		case accountstore.KindCodexAPIKey:
			var credential accountstore.CodexAPIKeyCredential
			if err := json.Unmarshal(req.Credential, &credential); err != nil {
				return accountstore.AccountWrite{}, fmt.Errorf("parse codex-api-key credential: %w", err)
			}
			write.CodexAPIKey = &credential
		case accountstore.KindOpenAICompatible:
			var credential accountstore.OpenAICompatibleCredential
			if err := json.Unmarshal(req.Credential, &credential); err != nil {
				return accountstore.AccountWrite{}, fmt.Errorf("parse openai-compatible credential: %w", err)
			}
			write.OpenAICompatible = &credential
		default:
			return accountstore.AccountWrite{}, fmt.Errorf("unsupported account kind %q", req.Kind)
		}
	}
	return write, nil
}

func writeAccountStoreError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := err.Error()
	if strings.Contains(message, "not found") {
		status = http.StatusNotFound
	} else if strings.Contains(message, "invalid") || strings.Contains(message, "missing") || strings.Contains(message, "unsupported") || strings.Contains(message, "cannot change") {
		status = http.StatusBadRequest
	}
	body := gin.H{"error": message}
	if isRecoverableAccountStoreReadError(err) {
		body["code"] = "account_store_io_error"
		body["recoverable"] = true
	}
	c.JSON(status, body)
}

func (h *Handler) DryRunAccountMigration(c *gin.Context) {
	req, err := decodeAccountMigrationRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	report, err := h.buildAccountMigrationReport(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}

func (h *Handler) CommitAccountMigration(c *gin.Context) {
	req, err := decodeAccountMigrationRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	report := req.Report
	if report == nil {
		report, err = h.buildAccountMigrationReport(c.Request.Context(), req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	commit, err := store.CommitImport(c.Request.Context(), report)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.applyPendingAccountStoreRuntime(c.Request.Context(), store)
	c.JSON(http.StatusOK, commit)
}

func (h *Handler) DeleteLegacyAccountSources(c *gin.Context) {
	var req deleteLegacySourcesRequest
	if c.Request != nil && c.Request.Body != nil {
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
	}
	backupDir, err := h.resolveLegacyBackupDir(req.BackupDir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	sources, err := store.ListUndeletedMigrationSources(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	type itemResult struct {
		ID         string `json:"id"`
		SourceKind string `json:"source_kind"`
		SourcePath string `json:"source_path,omitempty"`
		BackupPath string `json:"backup_path,omitempty"`
		Deleted    bool   `json:"deleted"`
	}
	results := make([]itemResult, 0, len(sources))
	for _, source := range sources {
		backupPath := ""
		deleted := false
		if strings.TrimSpace(source.SourcePath) != "" {
			if _, err := os.Stat(source.SourcePath); err == nil {
				backupPath = filepath.Join(backupDir, sanitizeBackupName(filepath.Base(source.SourcePath)))
				if err := copyFile(source.SourcePath, backupPath); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
				if err := os.Remove(source.SourcePath); err != nil && !os.IsNotExist(err) {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
				deleted = true
			} else if os.IsNotExist(err) {
				deleted = true
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		} else {
			h.removeLegacyConfigSource(source)
			deleted = true
		}
		if err := store.MarkMigrationSourceDeleted(c.Request.Context(), source.ID, backupPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		results = append(results, itemResult{
			ID:         source.ID,
			SourceKind: source.SourceKind,
			SourcePath: source.SourcePath,
			BackupPath: backupPath,
			Deleted:    deleted,
		})
	}
	if h.configFilePath != "" && h.cfg != nil {
		if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	_ = h.applyPendingAccountStoreRuntime(c.Request.Context(), store)
	c.JSON(http.StatusOK, gin.H{"deleted": len(results), "items": results, "backup_dir": backupDir})
}

func (h *Handler) applyAccountStoreRuntime(ctx context.Context, store *accountstore.Store, account accountstore.AccountRecord) accountstore.AccountRecord {
	if h == nil || h.accountStoreApply == nil || store == nil || account.AccountKey == "" || account.Revision == 0 {
		return account
	}
	if err := h.accountStoreApply(ctx); err != nil {
		_ = store.MarkRuntimeApplyResult(ctx, account.AccountKey, account.Revision, "failed", err.Error())
		if refreshed, refreshErr := store.GetAccount(ctx, account.AccountKey); refreshErr == nil {
			return refreshed
		}
		account.RuntimeApplyStatus = "failed"
		account.RuntimeApplyError = err.Error()
		return account
	}
	_ = store.MarkRuntimeApplyResult(ctx, account.AccountKey, account.Revision, "applied", "")
	_ = h.reconcileAccountStoreRouteability(ctx, store, account)
	if refreshed, err := store.GetAccount(ctx, account.AccountKey); err == nil {
		return refreshed
	}
	return account
}

func (h *Handler) triggerAccountStoreStatusChange(ctx context.Context, account accountstore.AccountRecord) error {
	if h == nil || h.accountStoreStatus == nil || account.AccountKey == "" {
		return nil
	}
	return h.accountStoreStatus(ctx, account)
}

func (h *Handler) triggerAccountStoreApply(ctx context.Context) error {
	if h == nil || h.accountStoreApply == nil {
		return nil
	}
	return h.accountStoreApply(ctx)
}

func (h *Handler) triggerAccountStoreDelete(ctx context.Context, accountKeys []string) error {
	if h == nil || h.accountStoreDelete == nil || len(accountKeys) == 0 {
		return nil
	}
	return h.accountStoreDelete(ctx, append([]string(nil), accountKeys...))
}

func (h *Handler) applyPendingAccountStoreRuntime(ctx context.Context, store *accountstore.Store) error {
	if h == nil || h.accountStoreApply == nil || store == nil {
		return nil
	}
	if err := h.accountStoreApply(ctx); err != nil {
		_ = store.MarkPendingRuntimeApplyResults(ctx, "failed", err.Error())
		return err
	}
	if err := store.MarkPendingRuntimeApplyResults(ctx, "applied", ""); err != nil {
		return err
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if strings.TrimSpace(account.RuntimeApplyStatus) != "applied" {
			continue
		}
		if err := h.reconcileAccountStoreRouteability(ctx, store, account); err != nil {
			return err
		}
	}
	return nil
}

func hasPendingAccountStoreRuntime(accounts []accountstore.AccountRecord) bool {
	for _, account := range accounts {
		if strings.TrimSpace(account.RuntimeApplyStatus) == "pending" {
			return true
		}
	}
	return false
}

func (h *Handler) reconcileAccountStoreRouteability(ctx context.Context, store *accountstore.Store, account accountstore.AccountRecord) error {
	if h == nil || store == nil || account.AccountKey == "" || account.Revision == 0 {
		return nil
	}
	if refreshed, err := store.GetAccount(ctx, account.AccountKey); err == nil {
		account = refreshed
	}
	status, reason, failureClass, registeredModelsCount := h.evaluateAccountStoreRouteabilityWithWait(ctx, account)
	return store.MarkRuntimeRouteability(ctx, account.AccountKey, account.Revision, status, reason, failureClass, registeredModelsCount)
}

func (h *Handler) evaluateAccountStoreRouteabilityWithWait(ctx context.Context, account accountstore.AccountRecord) (string, string, string, int) {
	if strings.TrimSpace(account.RuntimeApplyStatus) != "applied" {
		if strings.TrimSpace(account.RuntimeApplyStatus) == "failed" {
			return "degraded", strings.TrimSpace(account.RuntimeApplyError), "runtime_apply_failed", 0
		}
		return "pending", "", "", 0
	}
	if account.Disabled {
		return "pending", "", "", 0
	}
	deadline := time.Now().Add(1200 * time.Millisecond)
	for {
		status, reason, failureClass, registeredModelsCount, settled := h.evaluateAccountStoreRouteability(account)
		if settled || time.Now().After(deadline) {
			return status, reason, failureClass, registeredModelsCount
		}
		select {
		case <-ctx.Done():
			return status, reason, failureClass, registeredModelsCount
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (h *Handler) evaluateAccountStoreRouteability(account accountstore.AccountRecord) (string, string, string, int, bool) {
	if misclassified, ok := accountstore.MisclassifiedKnownOpenAICompatibleCodexAPIKey(account); ok {
		return "degraded", fmt.Sprintf("known openai-compatible provider %q is stored as codex-api-key; %s", misclassified.CompatProvider, misclassified.Remediation), "misclassified_openai_compatible_provider", 0, true
	}
	auth := h.findAccountStoreRuntimeAuth(account.AccountKey)
	if auth == nil {
		return "applied_not_registered", "runtime auth missing from registry", "runtime_auth_missing", 0, false
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return "applied_not_registered", "runtime auth disabled", "runtime_auth_disabled", 0, true
	}
	models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
	registeredModelsCount := len(models)
	if registeredModelsCount == 0 {
		return "applied_not_registered", "runtime auth registered without models", "runtime_models_missing", 0, false
	}
	if auth.Unavailable {
		return "degraded", firstNonEmptyValue(strings.TrimSpace(auth.StatusMessage), coreAuthRouteabilityMessage(auth.LastError), "runtime auth unavailable"), "runtime_auth_unavailable", registeredModelsCount, true
	}
	if auth.Status == coreauth.StatusError {
		return "degraded", firstNonEmptyValue(strings.TrimSpace(auth.StatusMessage), coreAuthRouteabilityMessage(auth.LastError), "runtime auth error"), "runtime_auth_error", registeredModelsCount, true
	}
	return "registered_routeable", "", "", registeredModelsCount, true
}

func (h *Handler) findAccountStoreRuntimeAuth(accountKey string) *coreauth.Auth {
	if h == nil || strings.TrimSpace(accountKey) == "" {
		return nil
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil
	}
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		if auth.AccountKey == accountKey {
			return auth
		}
	}
	return nil
}

func coreAuthRouteabilityMessage(err *coreauth.Error) string {
	if err == nil {
		return ""
	}
	if message := strings.TrimSpace(err.Message); message != "" {
		return message
	}
	if code := strings.TrimSpace(err.Code); code != "" {
		return code
	}
	return ""
}

func (h *Handler) buildAccountMigrationReport(ctx context.Context, req accountMigrationRequest) (*accountstore.MigrationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := h.cfg
	authDir := strings.TrimSpace(req.AuthDir)
	if authDir == "" && cfg != nil {
		authDir = strings.TrimSpace(cfg.AuthDir)
	}
	if authDir != "" {
		if resolved, err := util.ResolveAuthDir(authDir); err == nil {
			authDir = resolved
		}
	}
	codexDirs := req.CodexAPIKeyStoreDirs
	if len(codexDirs) == 0 {
		codexDirs = defaultCodexAPIKeyStoreDirs()
	}
	return accountstore.DryRunLegacyImport(ctx, accountstore.LegacySources{
		AuthDir:              authDir,
		CodexAPIKeyStoreDirs: codexDirs,
		Config:               cfg,
	})
}

func (h *Handler) openAccountStore(ctx context.Context) (*accountstore.Store, error) {
	if h == nil {
		return nil, fmt.Errorf("management handler is nil")
	}
	h.accountStoreMu.Lock()
	defer h.accountStoreMu.Unlock()
	dbPath, err := h.resolveAccountStorePath()
	if err != nil {
		return nil, err
	}
	if h.accountStore != nil && h.accountStoreDBPath == dbPath {
		return h.accountStore, nil
	}
	if h.accountStore != nil {
		_ = h.accountStore.Close()
		h.accountStore = nil
		h.accountStoreDBPath = ""
	}
	store, err := accountstore.Open(dbPath)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureSchema(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	h.accountStore = store
	h.accountStoreDBPath = dbPath
	return store, nil
}

func (h *Handler) resolveAccountStorePath() (string, error) {
	if h != nil {
		if path := strings.TrimSpace(h.accountStorePath); path != "" {
			return expandPath(path)
		}
		if h.cfg != nil {
			if path := strings.TrimSpace(h.cfg.AccountStoreDB); path != "" {
				return expandPath(path)
			}
		}
		if path := strings.TrimSpace(h.configFilePath); path != "" {
			return filepath.Join(filepath.Dir(path), "accounts-v1.sqlite"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve account store home: %w", err)
	}
	return filepath.Join(home, ".config", "gettokens", "accounts-v1.sqlite"), nil
}

func (h *Handler) resolveLegacyBackupDir(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return expandPath(path)
	}
	base := ""
	if h != nil && h.configFilePath != "" {
		base = filepath.Dir(h.configFilePath)
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config", "gettokens")
	}
	return filepath.Join(base, "migration-backups", "accounts-v1-"+time.Now().UTC().Format("20060102T150405Z")), nil
}

func decodeAccountMigrationRequest(c *gin.Context) (accountMigrationRequest, error) {
	var req accountMigrationRequest
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return req, nil
	}
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return req, fmt.Errorf("read request body: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return req, nil
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return req, fmt.Errorf("parse request body: %w", err)
	}
	return req, nil
}

func defaultCodexAPIKeyStoreDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".config", "gettokens-data", "codex-api-keys"),
		filepath.Join(home, ".config", "gettokens", "codex-api-keys"),
	}
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		rest := strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\")
		if rest == "" {
			return filepath.Clean(home), nil
		}
		return filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/"))), nil
	}
	return filepath.Clean(path), nil
}

func normalizeAccountBatchKeys(values []string) []string {
	keys := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		key := strings.TrimSpace(value)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func (h *Handler) removeLegacyConfigSource(source accountstore.MigrationSourceRecord) {
	if h == nil || h.cfg == nil {
		return
	}
	switch source.SourceKind {
	case "config-codex-api-key":
		out := h.cfg.CodexKey[:0]
		for _, item := range h.cfg.CodexKey {
			if strings.TrimSpace(item.LocalID) == strings.TrimSpace(source.SourceKey) {
				continue
			}
			out = append(out, item)
		}
		h.cfg.CodexKey = out
	case "config-openai-compatible":
		out := h.cfg.OpenAICompatibility[:0]
		for _, item := range h.cfg.OpenAICompatibility {
			if strings.TrimSpace(item.Name) == strings.TrimSpace(source.SourceKey) {
				continue
			}
			out = append(out, item)
		}
		h.cfg.OpenAICompatibility = out
	}
}

func sanitizeBackupName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	name = strings.Trim(replacer.Replace(strings.TrimSpace(name)), ". ")
	if name == "" {
		return "legacy-source"
	}
	return name
}

func copyFile(src string, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open legacy source %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create legacy backup %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy legacy backup %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close legacy backup %s: %w", dst, err)
	}
	return nil
}
