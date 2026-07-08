package gettokenshooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"gopkg.in/yaml.v3"
)

var accountRouteGuardAccountStorePathState struct {
	sync.RWMutex
	path  string
	cache *accountRouteGuardAccountIdentityCache
}

type accountRouteGuardAccountIdentityCache struct {
	path      string
	modTime   time.Time
	size      int64
	loadedAt  time.Time
	byAccount map[string][]string
}

const accountRouteGuardAccountIdentityCacheTTL = time.Second

func SetAccountRouteGuardAccountStorePath(path string) {
	accountRouteGuardAccountStorePathState.Lock()
	accountRouteGuardAccountStorePathState.path = strings.TrimSpace(path)
	accountRouteGuardAccountStorePathState.cache = nil
	accountRouteGuardAccountStorePathState.Unlock()
}

func setAccountRouteGuardAccountStorePathFromConfig(configPath string) {
	path := accountRouteGuardAccountStorePathFromConfig(configPath)
	SetAccountRouteGuardAccountStorePath(path)
}

func accountRouteGuardAccountStorePathFromConfig(configPath string) string {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".config", "gettokens", "accounts-v1.sqlite")
	}
	if strings.EqualFold(filepath.Base(configPath), "config.yaml") || strings.EqualFold(filepath.Base(configPath), "config.yml") {
		if raw, err := os.ReadFile(configPath); err == nil {
			var cfg struct {
				AccountStoreDB string `yaml:"account-store-db"`
			}
			if yaml.Unmarshal(raw, &cfg) == nil {
				if path := expandAccountRouteGuardPath(cfg.AccountStoreDB); path != "" {
					return path
				}
			}
		}
		return filepath.Join(filepath.Dir(configPath), "accounts-v1.sqlite")
	}
	return filepath.Join(configPath, "accounts-v1.sqlite")
}

func expandAccountRouteGuardPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return ""
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
		return ""
	}
	return path
}

func accountRouteGuardAccountStoreKeysForAccountKey(accountKey string) []string {
	accountKey = strings.TrimSpace(accountKey)
	if !accountstore.IsAccountKey(accountKey) {
		return nil
	}
	byAccount := accountRouteGuardAccountIdentityMap()
	if len(byAccount) == 0 {
		return nil
	}
	return append([]string(nil), byAccount[accountKey]...)
}

func accountRouteGuardAccountIdentityMap() map[string][]string {
	accountRouteGuardAccountStorePathState.RLock()
	path := strings.TrimSpace(accountRouteGuardAccountStorePathState.path)
	cache := accountRouteGuardAccountStorePathState.cache
	accountRouteGuardAccountStorePathState.RUnlock()
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil
	}
	if cache != nil &&
		cache.path == path &&
		cache.modTime.Equal(info.ModTime()) &&
		cache.size == info.Size() &&
		time.Since(cache.loadedAt) < accountRouteGuardAccountIdentityCacheTTL {
		return cache.byAccount
	}
	byAccount := loadAccountRouteGuardAccountIdentityMap(path)
	accountRouteGuardAccountStorePathState.Lock()
	accountRouteGuardAccountStorePathState.cache = &accountRouteGuardAccountIdentityCache{
		path:      path,
		modTime:   info.ModTime(),
		size:      info.Size(),
		loadedAt:  time.Now(),
		byAccount: byAccount,
	}
	accountRouteGuardAccountStorePathState.Unlock()
	return byAccount
}

func loadAccountRouteGuardAccountIdentityMap(path string) map[string][]string {
	store, err := accountstore.Open(path)
	if err != nil {
		return nil
	}
	defer store.Close()
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		return nil
	}
	out := make(map[string][]string, len(accounts))
	for _, account := range accounts {
		if !accountstore.IsAccountKey(account.AccountKey) || account.AuthFile == nil {
			continue
		}
		keys := accountRouteGuardProviderAccountIdentityKeys(account.AuthFile.AuthJSON)
		if len(keys) > 0 {
			out[account.AccountKey] = keys
		}
	}
	return out
}

func accountRouteGuardProviderAccountIdentityKeys(authJSON string) []string {
	accountID := accountRouteGuardAuthJSONAccountID(authJSON)
	if accountID == "" {
		return nil
	}
	return accountRouteGuardOpenAIAccountIdentityKeys(accountID)
}

func accountRouteGuardOpenAIAccountIdentityKeys(accountID string) []string {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	return []string{
		"openai-account-id:" + accountID,
		"provider-account-id:openai:" + accountID,
	}
}

func accountRouteGuardAuthJSONAccountID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	if value := accountRouteGuardStringField(payload, "account_id"); value != "" {
		return value
	}
	if value := accountRouteGuardStringField(payload, "chatgpt_account_id"); value != "" {
		return value
	}
	if metadata, ok := payload["metadata"].(map[string]any); ok {
		if value := accountRouteGuardStringField(metadata, "account_id"); value != "" {
			return value
		}
		if value := accountRouteGuardStringField(metadata, "chatgpt_account_id"); value != "" {
			return value
		}
	}
	return ""
}

func accountRouteGuardStringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	switch value := payload[key].(type) {
	case string:
		return strings.TrimSpace(value)
	default:
		return ""
	}
}
