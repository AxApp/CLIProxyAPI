package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type accountStoreTokenStore struct {
	fallback           coreauth.Store
	cfg                *config.Config
	accountStoreMu     sync.Mutex
	accountStore       *accountstore.Store
	accountStoreDBPath string
}

func newAccountStoreTokenStore(fallback coreauth.Store, cfg *config.Config) coreauth.Store {
	if fallback == nil || cfg == nil || strings.TrimSpace(cfg.AccountStoreDB) == "" {
		return fallback
	}
	return &accountStoreTokenStore{fallback: fallback, cfg: cfg}
}

func (s *accountStoreTokenStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	if s == nil || s.fallback == nil {
		return nil, nil
	}
	return s.fallback.List(ctx)
}

func (s *accountStoreTokenStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if saved, handled, err := s.saveAccountStoreAuth(ctx, auth); handled {
		return saved, err
	}
	if s == nil || s.fallback == nil {
		return "", nil
	}
	return s.fallback.Save(ctx, auth)
}

func (s *accountStoreTokenStore) Delete(ctx context.Context, id string) error {
	if s == nil || s.fallback == nil {
		return nil
	}
	return s.fallback.Delete(ctx, id)
}

func (s *accountStoreTokenStore) saveAccountStoreAuth(ctx context.Context, auth *coreauth.Auth) (string, bool, error) {
	if s == nil || s.cfg == nil || auth == nil || !accountstore.IsAccountKey(strings.TrimSpace(auth.AccountKey)) {
		return "", false, nil
	}
	dbPath := expandAccountStoreDBPath(s.cfg.AccountStoreDB)
	if strings.TrimSpace(dbPath) == "" {
		return "", false, nil
	}
	s.accountStoreMu.Lock()
	defer s.accountStoreMu.Unlock()
	store, err := s.openAccountStoreLocked(ctx, dbPath)
	if err != nil {
		return "", true, err
	}
	account, err := store.GetAccount(ctx, auth.AccountKey)
	if err != nil {
		return "", true, err
	}
	if account.Kind != accountstore.KindAuthFile {
		return "account-store:" + auth.AccountKey, true, nil
	}
	credential, err := authFileCredentialFromRuntimeAuth(auth, account.AuthFile)
	if err != nil {
		return "", true, err
	}
	if _, err := store.UpdateAuthFileCredential(ctx, auth.AccountKey, credential); err != nil {
		return "", true, err
	}
	return "account-store:" + auth.AccountKey, true, nil
}

func (s *accountStoreTokenStore) openAccountStoreLocked(ctx context.Context, dbPath string) (*accountstore.Store, error) {
	if s.accountStore != nil && s.accountStoreDBPath == dbPath {
		return s.accountStore, nil
	}
	if s.accountStore != nil {
		_ = s.accountStore.Close()
		s.accountStore = nil
		s.accountStoreDBPath = ""
	}
	store, err := accountstore.Open(dbPath)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureSchema(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	s.accountStore = store
	s.accountStoreDBPath = dbPath
	return store, nil
}

func authFileCredentialFromRuntimeAuth(auth *coreauth.Auth, existing *accountstore.AuthFileCredential) (accountstore.AuthFileCredential, error) {
	if auth == nil {
		return accountstore.AuthFileCredential{}, fmt.Errorf("auth is nil")
	}
	payload := map[string]any{}
	if strings.TrimSpace(existingAuthFileValue(existing, "auth_json")) != "" {
		_ = json.Unmarshal([]byte(existing.AuthJSON), &payload)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	if auth.Metadata != nil {
		for key, value := range auth.Metadata {
			payload[key] = value
		}
	}
	if len(payload) == 0 {
		return accountstore.AuthFileCredential{}, fmt.Errorf("auth %s has no metadata payload", auth.ID)
	}
	if strings.TrimSpace(stringFromMapValue(payload, "type")) == "" {
		payload["type"] = strings.TrimSpace(auth.Provider)
	}
	if strings.TrimSpace(stringFromMapValue(payload, "type")) == "" {
		payload["type"] = "codex"
	}
	email := firstNonEmptyRuntimeValue(
		stringFromMapValue(payload, "email"),
		existingAuthFileValue(existing, "email"),
	)
	accountID := stringFromMapValue(payload, "account_id")
	planType := normalizeRuntimePlanType(
		stringFromMapValue(payload, "plan_type"),
		stringFromMapValue(payload, "chatgpt_plan_type"),
		existingAuthFileValue(existing, "plan_type"),
	)
	if planType == "" && auth.Attributes != nil {
		planType = normalizeRuntimePlanType(auth.Attributes["plan_type"])
	}
	if idToken := strings.TrimSpace(stringFromMapValue(payload, "id_token")); idToken != "" {
		if claims, err := codexauth.ParseJWTToken(idToken); err == nil && claims != nil {
			if planType == "" {
				planType = normalizeRuntimePlanType(claims.CodexAuthInfo.ChatgptPlanType)
			}
			if accountID == "" {
				accountID = strings.TrimSpace(claims.GetAccountID())
			}
			if email == "" {
				email = strings.TrimSpace(claims.Email)
			}
		}
	}
	if email != "" {
		payload["email"] = email
	}
	if accountID != "" {
		payload["account_id"] = accountID
	}
	if planType != "" {
		payload["plan_type"] = planType
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return accountstore.AuthFileCredential{}, fmt.Errorf("marshal auth metadata: %w", err)
	}
	sourceFileName := strings.TrimSpace(auth.FileName)
	if sourceFileName == "" {
		sourceFileName = strings.TrimSpace(auth.ID)
	}
	if existing != nil && strings.TrimSpace(existing.SourceFileName) != "" {
		sourceFileName = existing.SourceFileName
	}
	authType := strings.TrimSpace(stringFromMapValue(payload, "type"))
	if authType == "" {
		authType = "codex"
	}
	return accountstore.AuthFileCredential{
		SourceFileName: sourceFileName,
		AuthJSON:       string(raw),
		AuthType:       authType,
		Email:          email,
		PlanType:       planType,
		SizeBytes:      int64(len(raw)),
	}, nil
}

func existingAuthFileValue(existing *accountstore.AuthFileCredential, key string) string {
	if existing == nil {
		return ""
	}
	switch key {
	case "email":
		return existing.Email
	case "plan_type":
		return existing.PlanType
	case "auth_json":
		return existing.AuthJSON
	default:
		return ""
	}
}

func firstNonEmptyRuntimeValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func stringFromMapValue(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	switch value := payload[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		return ""
	}
}

func normalizeRuntimePlanType(values ...string) string {
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "free":
			return "free"
		case "plus":
			return "plus"
		case "pro":
			return "pro"
		case "team":
			return "team"
		case "enterprise":
			return "enterprise"
		}
	}
	return ""
}

func expandAccountStoreDBPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			rest := strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\")
			if rest == "" {
				return filepath.Clean(home)
			}
			return filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/")))
		}
	}
	return filepath.Clean(path)
}
