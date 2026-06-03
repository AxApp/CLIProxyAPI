package accountstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type AccountKind string

const (
	KindAuthFile         AccountKind = "auth-file"
	KindCodexAPIKey      AccountKind = "codex-api-key"
	KindOpenAICompatible AccountKind = "openai-compatible"
)

type CredentialSource string

const (
	SourceLegacyAuthFile               CredentialSource = "legacy-auth-file"
	SourceLegacyGetTokensCodexAPIKey   CredentialSource = "legacy-gettokens-codex-api-key"
	SourceLegacyConfigCodexAPIKey      CredentialSource = "legacy-config-codex-api-key"
	SourceLegacyConfigOpenAICompatible CredentialSource = "legacy-config-openai-compatible"
	SourceSidecarManagementAPI         CredentialSource = "sidecar-management-api"
	SourceSidecarOAuth                 CredentialSource = "sidecar-oauth"
)

type LegacySources struct {
	AuthDir              string
	CodexAPIKeyStoreDirs []string
	Config               *config.Config
}

type MigrationReport struct {
	GeneratedAtUnixMs int64             `json:"generated_at_unix_ms"`
	Candidates        []ImportCandidate `json:"candidates"`
	Warnings          []string          `json:"warnings,omitempty"`
}

type ImportCandidate struct {
	AccountKey        string           `json:"account_key"`
	Kind              AccountKind      `json:"kind"`
	Title             string           `json:"title"`
	Provider          string           `json:"provider"`
	CredentialSource  CredentialSource `json:"credential_source"`
	Priority          int              `json:"priority"`
	Disabled          bool             `json:"disabled"`
	LegacyID          string           `json:"legacy_id"`
	SourcePath        string           `json:"source_path,omitempty"`
	SourceKey         string           `json:"source_key,omitempty"`
	SourceFingerprint string           `json:"source_fingerprint,omitempty"`

	AuthFile         *AuthFileCredential         `json:"auth_file,omitempty"`
	CodexAPIKey      *CodexAPIKeyCredential      `json:"codex_api_key,omitempty"`
	OpenAICompatible *OpenAICompatibleCredential `json:"openai_compatible,omitempty"`
}

type AuthFileCredential struct {
	SourceFileName string `json:"source_file_name"`
	AuthJSON       string `json:"auth_json"`
	AuthType       string `json:"auth_type,omitempty"`
	Email          string `json:"email,omitempty"`
	PlanType       string `json:"plan_type,omitempty"`
	ModifiedUnixMs int64  `json:"modified_unix_ms,omitempty"`
	SizeBytes      int64  `json:"size_bytes,omitempty"`
}

type CodexAPIKeyCredential struct {
	APIKey             string `json:"api_key"`
	APIKeyFingerprint  string `json:"api_key_fingerprint"`
	BaseURL            string `json:"base_url"`
	Prefix             string `json:"prefix,omitempty"`
	ProxyURL           string `json:"proxy_url,omitempty"`
	Websockets         bool   `json:"websockets"`
	QuotaCurl          string `json:"quota_curl,omitempty"`
	QuotaEnabled       bool   `json:"quota_enabled,omitempty"`
	BillingCurl        string `json:"billing_curl,omitempty"`
	BillingEnabled     bool   `json:"billing_enabled,omitempty"`
	FormatBaseURLsJSON string `json:"format_base_urls_json,omitempty"`
	HeadersJSON        string `json:"headers_json,omitempty"`
	ModelsJSON         string `json:"models_json,omitempty"`
	ExcludedModelsJSON string `json:"excluded_models_json,omitempty"`
}

type OpenAICompatibleCredential struct {
	ProviderName       string `json:"provider_name"`
	RuntimeProviderKey string `json:"runtime_provider_key"`
	BaseURL            string `json:"base_url"`
	Prefix             string `json:"prefix,omitempty"`
	APIKeyEntriesJSON  string `json:"api_key_entries_json"`
	HeadersJSON        string `json:"headers_json"`
	FormatBaseURLsJSON string `json:"format_base_urls_json,omitempty"`
	ModelsJSON         string `json:"models_json"`
}

type codexAPIKeyJSON struct {
	LocalID        string            `json:"local-id"`
	APIKey         string            `json:"api-key"`
	Label          string            `json:"label"`
	Priority       int               `json:"priority"`
	Disabled       bool              `json:"disabled"`
	Prefix         string            `json:"prefix"`
	BaseURL        string            `json:"base-url"`
	FormatBaseURLs map[string]string `json:"format-base-urls"`
	Websockets     bool              `json:"websockets"`
	ProxyURL       string            `json:"proxy-url"`
	Models         []codexModelJSON  `json:"models"`
	Headers        map[string]string `json:"headers"`
	ExcludedModels []string          `json:"excluded-models"`
	QuotaCurl      string            `json:"quota-curl"`
	QuotaEnabled   bool              `json:"quota-enabled"`
	BillingCurl    string            `json:"billing-curl"`
	BillingEnabled bool              `json:"billing-enabled"`
}

type codexModelJSON struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

func DryRunLegacyImport(ctx context.Context, sources LegacySources) (*MigrationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := &MigrationReport{GeneratedAtUnixMs: time.Now().UnixMilli()}
	if err := appendAuthFileCandidates(ctx, report, sources.AuthDir); err != nil {
		return nil, err
	}
	if err := appendCodexAPIKeyStoreCandidates(ctx, report, sources.CodexAPIKeyStoreDirs); err != nil {
		return nil, err
	}
	if err := appendConfigCandidates(ctx, report, sources.Config); err != nil {
		return nil, err
	}
	sort.SliceStable(report.Candidates, func(i, j int) bool {
		left := report.Candidates[i]
		right := report.Candidates[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.CredentialSource != right.CredentialSource {
			return left.CredentialSource < right.CredentialSource
		}
		return left.LegacyID < right.LegacyID
	})
	return report, nil
}

func appendAuthFileCandidates(ctx context.Context, report *MigrationReport, authDir string) error {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return nil
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read auth dir: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(entry.Name()), "codex-") {
			continue
		}
		path := filepath.Join(authDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read auth file %s: %w", path, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("skip invalid auth json %s: %v", path, err))
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat auth file %s: %w", path, err)
		}
		accountKey, err := newAccountKey()
		if err != nil {
			return fmt.Errorf("generate account key: %w", err)
		}
		report.Candidates = append(report.Candidates, ImportCandidate{
			AccountKey:        accountKey,
			Kind:              KindAuthFile,
			Title:             strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
			Provider:          "codex",
			CredentialSource:  SourceLegacyAuthFile,
			LegacyID:          "auth-file:" + entry.Name(),
			SourcePath:        path,
			SourceKey:         entry.Name(),
			SourceFingerprint: fingerprintBytes(data),
			AuthFile: &AuthFileCredential{
				SourceFileName: entry.Name(),
				AuthJSON:       string(data),
				AuthType:       stringFromMap(raw, "type"),
				Email:          stringFromMap(raw, "email"),
				PlanType:       inferAuthFilePlanType(entry.Name(), raw),
				ModifiedUnixMs: info.ModTime().UnixMilli(),
				SizeBytes:      info.Size(),
			},
		})
	}
	return nil
}

func appendCodexAPIKeyStoreCandidates(ctx context.Context, report *MigrationReport, dirs []string) error {
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read codex api key store %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read codex api key file %s: %w", path, err)
			}
			var item codexAPIKeyJSON
			if err := json.Unmarshal(data, &item); err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("skip invalid codex api key json %s: %v", path, err))
				continue
			}
			item.normalize()
			if item.APIKey == "" || item.BaseURL == "" {
				continue
			}
			candidate, err := buildCodexAPIKeyCandidate(
				SourceLegacyGetTokensCodexAPIKey,
				item.legacyID(entry.Name()),
				path,
				item.sourceKey(entry.Name()),
				fingerprintBytes(data),
				item,
			)
			if err != nil {
				return err
			}
			report.Candidates = append(report.Candidates, candidate)
		}
	}
	return nil
}

func appendConfigCandidates(ctx context.Context, report *MigrationReport, cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	for index, entry := range cfg.CodexKey {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := codexAPIKeyJSON{
			LocalID:        strings.TrimSpace(entry.LocalID),
			APIKey:         strings.TrimSpace(entry.APIKey),
			Priority:       entry.Priority,
			Disabled:       entry.Disabled,
			Prefix:         normalizePrefix(entry.Prefix),
			BaseURL:        strings.TrimSpace(entry.BaseURL),
			Websockets:     entry.Websockets,
			ProxyURL:       strings.TrimSpace(entry.ProxyURL),
			Headers:        normalizeHeaders(entry.Headers),
			ExcludedModels: normalizeStringSlice(entry.ExcludedModels),
		}
		for _, model := range entry.Models {
			item.Models = append(item.Models, codexModelJSON{Name: strings.TrimSpace(model.Name), Alias: strings.TrimSpace(model.Alias)})
		}
		item.normalize()
		if item.APIKey == "" || item.BaseURL == "" {
			continue
		}
		sourceKey := item.LocalID
		if sourceKey == "" {
			sourceKey = fmt.Sprintf("config-index:%d", index)
		}
		candidate, err := buildCodexAPIKeyCandidate(
			SourceLegacyConfigCodexAPIKey,
			"codex-api-key:"+sourceKey,
			"",
			sourceKey,
			fingerprintString(strings.Join([]string{item.APIKey, item.BaseURL, item.Prefix, fmt.Sprintf("%d", index)}, "\x00")),
			item,
		)
		if err != nil {
			return err
		}
		report.Candidates = append(report.Candidates, candidate)
	}
	for index, entry := range cfg.OpenAICompatibility {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSpace(entry.Name)
		baseURL := strings.TrimSpace(entry.BaseURL)
		if name == "" || baseURL == "" {
			continue
		}
		accountKey, err := newAccountKey()
		if err != nil {
			return fmt.Errorf("generate account key: %w", err)
		}
		apiKeyEntriesJSON, err := jsonString(entry.APIKeyEntries, "[]")
		if err != nil {
			return fmt.Errorf("marshal openai-compatible api key entries: %w", err)
		}
		headersJSON, err := jsonString(normalizeHeaders(entry.Headers), "{}")
		if err != nil {
			return fmt.Errorf("marshal openai-compatible headers: %w", err)
		}
		modelsJSON, err := jsonString(entry.Models, "[]")
		if err != nil {
			return fmt.Errorf("marshal openai-compatible models: %w", err)
		}
		report.Candidates = append(report.Candidates, ImportCandidate{
			AccountKey:        accountKey,
			Kind:              KindOpenAICompatible,
			Title:             name,
			Provider:          name,
			CredentialSource:  SourceLegacyConfigOpenAICompatible,
			Priority:          entry.Priority,
			Disabled:          entry.Disabled,
			LegacyID:          "openai-compatible:" + name,
			SourceKey:         name,
			SourceFingerprint: fingerprintString(strings.Join([]string{name, baseURL, normalizePrefix(entry.Prefix), apiKeyEntriesJSON, headersJSON, modelsJSON, fmt.Sprintf("%d", index)}, "\x00")),
			OpenAICompatible: &OpenAICompatibleCredential{
				ProviderName:       name,
				RuntimeProviderKey: "openai-compatible:" + accountKey,
				BaseURL:            baseURL,
				Prefix:             normalizePrefix(entry.Prefix),
				APIKeyEntriesJSON:  apiKeyEntriesJSON,
				HeadersJSON:        headersJSON,
				ModelsJSON:         modelsJSON,
			},
		})
	}
	return nil
}

func buildCodexAPIKeyCandidate(source CredentialSource, legacyID string, sourcePath string, sourceKey string, sourceFingerprint string, item codexAPIKeyJSON) (ImportCandidate, error) {
	accountKey, err := newAccountKey()
	if err != nil {
		return ImportCandidate{}, fmt.Errorf("generate account key: %w", err)
	}
	title := item.Label
	if title == "" {
		title = item.LocalID
	}
	if title == "" {
		title = "Codex API Key"
	}
	formatBaseURLsJSON, err := jsonString(item.FormatBaseURLs, "{}")
	if err != nil {
		return ImportCandidate{}, fmt.Errorf("marshal codex format base urls: %w", err)
	}
	headersJSON, err := jsonString(item.Headers, "{}")
	if err != nil {
		return ImportCandidate{}, fmt.Errorf("marshal codex headers: %w", err)
	}
	modelsJSON, err := jsonString(item.Models, "[]")
	if err != nil {
		return ImportCandidate{}, fmt.Errorf("marshal codex models: %w", err)
	}
	excludedModelsJSON, err := jsonString(item.ExcludedModels, "[]")
	if err != nil {
		return ImportCandidate{}, fmt.Errorf("marshal codex excluded models: %w", err)
	}
	return ImportCandidate{
		AccountKey:        accountKey,
		Kind:              KindCodexAPIKey,
		Title:             title,
		Provider:          "codex",
		CredentialSource:  source,
		Priority:          item.Priority,
		Disabled:          item.Disabled,
		LegacyID:          legacyID,
		SourcePath:        sourcePath,
		SourceKey:         sourceKey,
		SourceFingerprint: sourceFingerprint,
		CodexAPIKey: &CodexAPIKeyCredential{
			APIKey:             item.APIKey,
			APIKeyFingerprint:  apiKeyFingerprint(item.APIKey),
			BaseURL:            item.BaseURL,
			Prefix:             item.Prefix,
			ProxyURL:           item.ProxyURL,
			Websockets:         item.Websockets,
			QuotaCurl:          item.QuotaCurl,
			QuotaEnabled:       item.QuotaEnabled,
			BillingCurl:        item.BillingCurl,
			BillingEnabled:     item.BillingEnabled,
			FormatBaseURLsJSON: formatBaseURLsJSON,
			HeadersJSON:        headersJSON,
			ModelsJSON:         modelsJSON,
			ExcludedModelsJSON: excludedModelsJSON,
		},
	}, nil
}

func (item *codexAPIKeyJSON) normalize() {
	if item == nil {
		return
	}
	item.LocalID = strings.TrimSpace(item.LocalID)
	item.APIKey = strings.TrimSpace(item.APIKey)
	item.Label = strings.TrimSpace(item.Label)
	item.BaseURL = strings.TrimSpace(item.BaseURL)
	item.Prefix = normalizePrefix(item.Prefix)
	item.ProxyURL = strings.TrimSpace(item.ProxyURL)
	item.Headers = normalizeHeaders(item.Headers)
	item.ExcludedModels = normalizeStringSlice(item.ExcludedModels)
	item.QuotaCurl = strings.TrimSpace(item.QuotaCurl)
	item.QuotaEnabled = item.QuotaEnabled && item.QuotaCurl != ""
	item.BillingCurl = strings.TrimSpace(item.BillingCurl)
	item.BillingEnabled = item.BillingEnabled && item.BillingCurl != ""
	for i := range item.Models {
		item.Models[i].Name = strings.TrimSpace(item.Models[i].Name)
		item.Models[i].Alias = strings.TrimSpace(item.Models[i].Alias)
	}
}

func (item codexAPIKeyJSON) legacyID(fileName string) string {
	if item.LocalID != "" {
		return item.LocalID
	}
	return "codex-api-key-json:" + fileName
}

func (item codexAPIKeyJSON) sourceKey(fileName string) string {
	if item.LocalID != "" {
		return item.LocalID
	}
	return fileName
}

func stringFromMap(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	if value, ok := raw[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func inferAuthFilePlanType(fileName string, raw map[string]any) string {
	if raw == nil {
		return inferPlanTypeFromFileName(fileName)
	}
	for _, key := range []string{"plan_type", "planType", "plan"} {
		if value := stringFromMap(raw, key); value != "" {
			return normalizePlanType(value)
		}
	}
	if metadata, ok := raw["metadata"].(map[string]any); ok {
		for _, key := range []string{"plan_type", "planType", "plan"} {
			if value := stringFromMap(metadata, key); value != "" {
				return normalizePlanType(value)
			}
		}
	}
	for _, key := range []string{"id_token", "idToken"} {
		if token := stringFromMap(raw, key); token != "" {
			if planType := planTypeFromIDToken(token); planType != "" {
				return planType
			}
		}
	}
	if metadata, ok := raw["metadata"].(map[string]any); ok {
		for _, key := range []string{"id_token", "idToken"} {
			if token := stringFromMap(metadata, key); token != "" {
				if planType := planTypeFromIDToken(token); planType != "" {
					return planType
				}
			}
		}
	}
	return inferPlanTypeFromFileName(fileName)
}

func planTypeFromIDToken(token string) string {
	claims, err := codex.ParseJWTToken(strings.TrimSpace(token))
	if err != nil || claims == nil {
		return ""
	}
	return normalizePlanType(claims.CodexAuthInfo.ChatgptPlanType)
}

func inferPlanTypeFromFileName(fileName string) string {
	name := strings.TrimSpace(strings.TrimSuffix(fileName, filepath.Ext(fileName)))
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "-plus"):
		return "plus"
	case strings.HasSuffix(lower, "-pro"):
		return "pro"
	case strings.HasSuffix(lower, "-free"):
		return "free"
	default:
		return ""
	}
}

func normalizePlanType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "":
		return ""
	case "free", "chatgptfree", "freeplan":
		return "free"
	case "plus", "chatgptplus", "plusplan":
		return "plus"
	case "pro", "chatgptpro", "proplan", "professional", "prolite":
		return "pro"
	default:
		if strings.Contains(normalized, "plus") {
			return "plus"
		}
		if strings.Contains(normalized, "pro") {
			return "pro"
		}
		if strings.Contains(normalized, "free") {
			return "free"
		}
		return normalized
	}
}

func jsonString(value any, empty string) (string, error) {
	if value == nil {
		return empty, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if string(data) == "null" {
		return empty, nil
	}
	return string(data), nil
}

func fingerprintBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fingerprintString(value string) string {
	return fingerprintBytes([]byte(value))
}

func apiKeyFingerprint(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])[:16]
}

func normalizePrefix(value string) string {
	trimmed := strings.Trim(strings.TrimSpace(value), "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + "/"
}

func normalizeHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}
