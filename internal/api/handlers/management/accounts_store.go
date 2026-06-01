package management

import (
	"context"
	"encoding/json"
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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
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

type deleteLegacySourcesRequest struct {
	BackupDir string `json:"backup_dir,omitempty"`
}

func (h *Handler) GetAccount(c *gin.Context) {
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	account, err := store.GetAccount(c.Request.Context(), c.Param("account_key"))
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
	if err := store.DeleteAccount(c.Request.Context(), c.Param("account_key")); err != nil {
		writeAccountStoreError(c, err)
		return
	}
	_ = h.triggerAccountStoreApply(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"ok": true})
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
	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	accounts, err := store.ListAccounts(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if hasPendingAccountStoreRuntime(accounts) {
		_ = h.applyPendingAccountStoreRuntime(c.Request.Context(), store)
		accounts, err = store.ListAccounts(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts})
}

func decodeAccountWriteRequest(c *gin.Context) (accountstore.AccountWrite, error) {
	var req accountWriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return accountstore.AccountWrite{}, err
	}
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
	c.JSON(status, gin.H{"error": message})
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

func (h *Handler) applyPendingAccountStoreRuntime(ctx context.Context, store *accountstore.Store) error {
	if h == nil || h.accountStoreApply == nil || store == nil {
		return nil
	}
	if err := h.accountStoreApply(ctx); err != nil {
		_ = store.MarkPendingRuntimeApplyResults(ctx, "failed", err.Error())
		return err
	}
	return store.MarkPendingRuntimeApplyResults(ctx, "applied", "")
}

func hasPendingAccountStoreRuntime(accounts []accountstore.AccountRecord) bool {
	for _, account := range accounts {
		if strings.TrimSpace(account.RuntimeApplyStatus) == "pending" {
			return true
		}
	}
	return false
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
