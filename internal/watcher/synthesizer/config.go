package synthesizer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// ConfigSynthesizer generates Auth entries from configuration API keys.
// It handles Gemini, Claude, Codex, OpenAI-compat, and Vertex-compat providers.
type ConfigSynthesizer struct{}

// NewConfigSynthesizer creates a new ConfigSynthesizer instance.
func NewConfigSynthesizer() *ConfigSynthesizer {
	return &ConfigSynthesizer{}
}

// Synthesize generates Auth entries from config API keys.
func (s *ConfigSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 32)
	if ctx == nil || ctx.Config == nil {
		return out, nil
	}
	accountStoreAuths, accountStoreActive := s.synthesizeAccountStore(ctx)

	// Gemini API Keys
	out = append(out, s.synthesizeGeminiKeys(ctx)...)
	// Claude API Keys
	out = append(out, s.synthesizeClaudeKeys(ctx)...)
	if accountStoreActive {
		out = append(out, accountStoreAuths...)
	} else {
		// Codex API Keys
		out = append(out, s.synthesizeCodexKeys(ctx)...)
		// OpenAI-compat
		out = append(out, s.synthesizeOpenAICompat(ctx)...)
	}
	// Vertex-compat
	out = append(out, s.synthesizeVertexCompat(ctx)...)

	return out, nil
}

func (s *ConfigSynthesizer) synthesizeAccountStore(ctx *SynthesisContext) ([]*coreauth.Auth, bool) {
	path := strings.TrimSpace(ctx.Config.AccountStoreDB)
	if path == "" {
		return nil, false
	}
	path = expandAccountStorePath(path)
	if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
		return nil, false
	}
	store, err := accountstore.Open(path)
	if err != nil {
		return nil, false
	}
	defer store.Close()
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		return nil, false
	}
	out := make([]*coreauth.Auth, 0, len(accounts))
	for _, account := range accounts {
		switch account.Kind {
		case accountstore.KindAuthFile:
			out = append(out, synthesizeAccountStoreAuthFile(ctx, account)...)
		case accountstore.KindCodexAPIKey:
			if auth := s.synthesizeAccountStoreCodexKey(ctx, account); auth != nil {
				out = append(out, auth)
			}
		case accountstore.KindOpenAICompatible:
			out = append(out, s.synthesizeAccountStoreOpenAICompat(ctx, account)...)
		}
	}
	return out, len(accounts) > 0
}

func accountStoreHasKind(cfg *config.Config, kind accountstore.AccountKind) bool {
	if cfg == nil {
		return false
	}
	path := strings.TrimSpace(cfg.AccountStoreDB)
	if path == "" {
		return false
	}
	path = expandAccountStorePath(path)
	if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
		return false
	}
	store, err := accountstore.Open(path)
	if err != nil {
		return false
	}
	defer store.Close()
	accounts, err := store.ListAccounts(context.Background())
	if err != nil {
		return false
	}
	for _, account := range accounts {
		if account.Kind == kind {
			return true
		}
	}
	return false
}

// synthesizeGeminiKeys creates Auth entries for Gemini API keys.
func (s *ConfigSynthesizer) synthesizeGeminiKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.GeminiKey))
	for i := range cfg.GeminiKey {
		entry := cfg.GeminiKey[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		id, token := idGen.Next("gemini:apikey", key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:gemini[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if entry.DisableCooling {
			metadata["disable_cooling"] = true
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeGeminiModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "gemini",
			Label:      "gemini-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeClaudeKeys creates Auth entries for Claude API keys.
func (s *ConfigSynthesizer) synthesizeClaudeKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		ck := cfg.ClaudeKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		base := strings.TrimSpace(ck.BaseURL)
		id, token := idGen.Next("claude:apikey", key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:claude[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if ck.DisableCooling {
			metadata["disable_cooling"] = true
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeClaudeModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "claude",
			Label:      "claude-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeCodexKeys creates Auth entries for Codex API keys.
func (s *ConfigSynthesizer) synthesizeCodexKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		ck := cfg.CodexKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		id, token := idGen.Next("codex:apikey", key, ck.BaseURL)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:codex[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if ck.DisableCooling {
			metadata["disable_cooling"] = true
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if ck.BaseURL != "" {
			attrs["base_url"] = ck.BaseURL
		}
		if ck.Websockets {
			attrs["websockets"] = "true"
		}
		if hash := diff.ComputeCodexModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		a := &coreauth.Auth{
			ID:         id,
			AccountKey: strings.TrimSpace(ck.LocalID),
			Provider:   "codex",
			Label:      "codex-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			Disabled:   ck.Disabled,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if ck.Disabled {
			a.Status = coreauth.StatusDisabled
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

func (s *ConfigSynthesizer) synthesizeAccountStoreCodexKey(ctx *SynthesisContext, account accountstore.AccountRecord) *coreauth.Auth {
	if account.CodexAPIKey == nil {
		return nil
	}
	key := strings.TrimSpace(account.CodexAPIKey.APIKey)
	if key == "" {
		return nil
	}
	base := strings.TrimSpace(account.CodexAPIKey.BaseURL)
	id, token := ctx.IDGenerator.Next("codex:apikey", key, base, account.AccountKey)
	attrs := map[string]string{
		"source":      fmt.Sprintf("account-store:codex[%s]", token),
		"api_key":     key,
		"account_key": account.AccountKey,
	}
	if account.Priority != 0 {
		attrs["priority"] = strconv.Itoa(account.Priority)
	}
	if base != "" {
		attrs["base_url"] = base
	}
	if account.CodexAPIKey.Websockets {
		attrs["websockets"] = "true"
	}
	if models := decodeCodexModels(account.CodexAPIKey.ModelsJSON); len(models) > 0 {
		if hash := diff.ComputeCodexModelsHash(models); hash != "" {
			attrs["models_hash"] = hash
		}
	}
	addJSONHeadersToAttrs(account.CodexAPIKey.HeadersJSON, attrs)
	auth := &coreauth.Auth{
		ID:         id,
		AccountKey: account.AccountKey,
		Provider:   "codex",
		Label:      defaultLabel(account.Title, "codex-apikey"),
		Prefix:     strings.TrimSpace(account.CodexAPIKey.Prefix),
		Status:     coreauth.StatusActive,
		Disabled:   account.Disabled,
		ProxyURL:   strings.TrimSpace(account.CodexAPIKey.ProxyURL),
		Attributes: attrs,
		CreatedAt:  ctx.Now,
		UpdatedAt:  ctx.Now,
	}
	if account.Disabled {
		auth.Status = coreauth.StatusDisabled
	}
	ApplyAuthExcludedModelsMeta(auth, ctx.Config, decodeStringSlice(account.CodexAPIKey.ExcludedModelsJSON), "apikey")
	return auth
}

func (s *ConfigSynthesizer) synthesizeAccountStoreOpenAICompat(ctx *SynthesisContext, account accountstore.AccountRecord) []*coreauth.Auth {
	if account.OpenAICompatible == nil {
		return nil
	}
	compat := account.OpenAICompatible
	providerName := strings.ToLower(strings.TrimSpace(compat.ProviderName))
	if providerName == "" {
		providerName = strings.ToLower(strings.TrimSpace(account.Provider))
	}
	if providerName == "" {
		providerName = "openai-compatibility"
	}
	base := strings.TrimSpace(compat.BaseURL)
	prefix := strings.TrimSpace(compat.Prefix)
	headers := decodeStringMap(compat.HeadersJSON)
	models := decodeOpenAICompatModels(compat.ModelsJSON)
	entries := decodeOpenAICompatAPIKeys(compat.APIKeyEntriesJSON)
	if len(entries) == 0 {
		entries = []config.OpenAICompatibilityAPIKey{{}}
	}
	out := make([]*coreauth.Auth, 0, len(entries))
	for index := range entries {
		entry := entries[index]
		key := strings.TrimSpace(entry.APIKey)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
		id, token := ctx.IDGenerator.Next(idKind, key, base, proxyURL, account.AccountKey, strconv.Itoa(index))
		attrs := map[string]string{
			"source":       fmt.Sprintf("account-store:%s[%s]", providerName, token),
			"base_url":     base,
			"compat_name":  defaultLabel(compat.ProviderName, account.Provider),
			"provider_key": providerName,
			"account_key":  account.AccountKey,
		}
		if account.Priority != 0 {
			attrs["priority"] = strconv.Itoa(account.Priority)
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if hash := diff.ComputeOpenAICompatModelsHash(models); hash != "" {
			attrs["models_hash"] = hash
		}
		for header, value := range headers {
			attrs["header:"+header] = value
		}
		auth := &coreauth.Auth{
			ID:         id,
			AccountKey: account.AccountKey,
			Provider:   providerName,
			Label:      defaultLabel(account.Title, compat.ProviderName),
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			Disabled:   account.Disabled,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			CreatedAt:  ctx.Now,
			UpdatedAt:  ctx.Now,
		}
		if account.Disabled {
			auth.Status = coreauth.StatusDisabled
		}
		out = append(out, auth)
	}
	return out
}

func synthesizeAccountStoreAuthFile(ctx *SynthesisContext, account accountstore.AccountRecord) []*coreauth.Auth {
	if account.AuthFile == nil || strings.TrimSpace(account.AuthFile.AuthJSON) == "" {
		return nil
	}
	fullPath := account.AuthFile.SourceFileName
	if fullPath == "" {
		fullPath = account.AccountKey + ".json"
	}
	auths := synthesizeFileAuths(ctx, fullPath, []byte(account.AuthFile.AuthJSON), false)
	for _, auth := range auths {
		auth.AccountKey = account.AccountKey
		auth.Disabled = account.Disabled
		if account.Disabled {
			auth.Status = coreauth.StatusDisabled
		}
		if account.Priority != 0 {
			if auth.Attributes == nil {
				auth.Attributes = map[string]string{}
			}
			auth.Attributes["priority"] = strconv.Itoa(account.Priority)
		}
	}
	return auths
}

// synthesizeOpenAICompat creates Auth entries for OpenAI-compatible providers.
func (s *ConfigSynthesizer) synthesizeOpenAICompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0)
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		prefix := strings.TrimSpace(compat.Prefix)
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		base := strings.TrimSpace(compat.BaseURL)
		disableCooling := compat.DisableCooling

		// Handle new APIKeyEntries format (preferred)
		createdEntries := 0
		for j := range compat.APIKeyEntries {
			entry := &compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, key, base, proxyURL)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
			}
			metadata := map[string]any{}
			if disableCooling {
				metadata["disable_cooling"] = true
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if key != "" {
				attrs["api_key"] = key
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:         id,
				AccountKey: openAICompatAccountKey(compat.Name),
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				ProxyURL:   proxyURL,
				Attributes: attrs,
				Metadata:   metadata,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			out = append(out, a)
			createdEntries++
		}
		// Fallback: create entry without API key if no APIKeyEntries
		if createdEntries == 0 {
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, base)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": providerName,
			}
			metadata := map[string]any{}
			if disableCooling {
				metadata["disable_cooling"] = true
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			a := &coreauth.Auth{
				ID:         id,
				AccountKey: openAICompatAccountKey(compat.Name),
				Provider:   providerName,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				Attributes: attrs,
				Metadata:   metadata,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			out = append(out, a)
		}
	}
	return out
}

func openAICompatAccountKey(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		trimmed = "openai-compatibility"
	}
	return "openai-compatible:" + trimmed
}

// synthesizeVertexCompat creates Auth entries for Vertex-compatible providers.
func (s *ConfigSynthesizer) synthesizeVertexCompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		compat := &cfg.VertexCompatAPIKey[i]
		providerName := "vertex"
		base := strings.TrimSpace(compat.BaseURL)

		key := strings.TrimSpace(compat.APIKey)
		prefix := strings.TrimSpace(compat.Prefix)
		proxyURL := strings.TrimSpace(compat.ProxyURL)
		idKind := "vertex:apikey"
		id, token := idGen.Next(idKind, key, base, proxyURL)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:vertex-apikey[%s]", token),
			"base_url":     base,
			"provider_key": providerName,
		}
		if compat.Priority != 0 {
			attrs["priority"] = strconv.Itoa(compat.Priority)
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if hash := diff.ComputeVertexCompatModelsHash(compat.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(compat.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   providerName,
			Label:      "vertex-apikey",
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, compat.ExcludedModels, "apikey")
		out = append(out, a)
	}
	return out
}

func decodeCodexModels(raw string) []config.CodexModel {
	var models []config.CodexModel
	_ = json.Unmarshal([]byte(strings.TrimSpace(raw)), &models)
	return models
}

func decodeOpenAICompatModels(raw string) []config.OpenAICompatibilityModel {
	var models []config.OpenAICompatibilityModel
	_ = json.Unmarshal([]byte(strings.TrimSpace(raw)), &models)
	return models
}

func decodeOpenAICompatAPIKeys(raw string) []config.OpenAICompatibilityAPIKey {
	var entries []config.OpenAICompatibilityAPIKey
	_ = json.Unmarshal([]byte(strings.TrimSpace(raw)), &entries)
	return entries
}

func decodeStringMap(raw string) map[string]string {
	var values map[string]string
	_ = json.Unmarshal([]byte(strings.TrimSpace(raw)), &values)
	return values
}

func decodeStringSlice(raw string) []string {
	var values []string
	_ = json.Unmarshal([]byte(strings.TrimSpace(raw)), &values)
	return values
}

func addJSONHeadersToAttrs(raw string, attrs map[string]string) {
	for key, value := range decodeStringMap(raw) {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		attrs["header:"+key] = strings.TrimSpace(value)
	}
}

func defaultLabel(value string, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(fallback)
}

func expandAccountStorePath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			rest := strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\")
			if rest == "" {
				return home
			}
			return home + string(os.PathSeparator) + strings.ReplaceAll(rest, "\\", string(os.PathSeparator))
		}
	}
	return path
}
