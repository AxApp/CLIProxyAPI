package accountstore

import (
	"context"
	"strings"
)

const MisclassifiedKnownOpenAICompatibleRemediation = "delete and recreate this account as openai-compatible"

type KnownOpenAICompatibleMisclassifiedAccount struct {
	AccountKey     string `json:"account_key"`
	Title          string `json:"title"`
	BaseURL        string `json:"base_url"`
	CompatProvider string `json:"compat_provider"`
	Remediation    string `json:"remediation"`
}

type KnownOpenAICompatibleAudit struct {
	TotalCodexAPIKey                    int                                         `json:"total_codex_api_key"`
	MisclassifiedKnownOpenAICompatible  int                                         `json:"misclassified_known_openai_compatible"`
	ByProvider                          map[string]int                              `json:"by_provider"`
	MisclassifiedKnownOpenAICompatCards []KnownOpenAICompatibleMisclassifiedAccount `json:"misclassified_known_openai_compatible_cards"`
}

func KnownOpenAICompatibleProviderFromBaseURL(baseURL string) string {
	lower := strings.ToLower(strings.TrimSpace(baseURL))
	switch {
	case strings.Contains(lower, "xiaomimimo.com"):
		return "xiaomimimo"
	case strings.Contains(lower, "api.deepseek.com"):
		return "deepseek"
	case strings.Contains(lower, "openrouter.ai"):
		return "openrouter"
	case strings.Contains(lower, "siliconflow"):
		return "siliconflow"
	case strings.Contains(lower, "bigmodel.cn"):
		return "zhipu"
	case strings.Contains(lower, "moonshot.cn"):
		return "moonshot"
	case strings.Contains(lower, "dashscope.aliyuncs.com"):
		return "dashscope"
	case strings.Contains(lower, "groq.com"):
		return "groq"
	case strings.Contains(lower, "together.xyz"):
		return "together"
	case strings.Contains(lower, "volces.com"):
		return "doubao"
	default:
		return ""
	}
}

func MisclassifiedKnownOpenAICompatibleCodexAPIKey(account AccountRecord) (KnownOpenAICompatibleMisclassifiedAccount, bool) {
	if account.Kind != KindCodexAPIKey || account.CodexAPIKey == nil {
		return KnownOpenAICompatibleMisclassifiedAccount{}, false
	}
	provider := KnownOpenAICompatibleProviderFromBaseURL(account.CodexAPIKey.BaseURL)
	if provider == "" {
		return KnownOpenAICompatibleMisclassifiedAccount{}, false
	}
	return KnownOpenAICompatibleMisclassifiedAccount{
		AccountKey:     account.AccountKey,
		Title:          account.Title,
		BaseURL:        strings.TrimSpace(account.CodexAPIKey.BaseURL),
		CompatProvider: provider,
		Remediation:    MisclassifiedKnownOpenAICompatibleRemediation,
	}, true
}

func (s *Store) AuditKnownOpenAICompatibleMisclassifications(ctx context.Context) (KnownOpenAICompatibleAudit, error) {
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return KnownOpenAICompatibleAudit{}, err
	}
	audit := KnownOpenAICompatibleAudit{
		ByProvider:                          map[string]int{},
		MisclassifiedKnownOpenAICompatCards: []KnownOpenAICompatibleMisclassifiedAccount{},
	}
	for _, account := range accounts {
		if account.Kind != KindCodexAPIKey {
			continue
		}
		audit.TotalCodexAPIKey++
		misclassified, ok := MisclassifiedKnownOpenAICompatibleCodexAPIKey(account)
		if !ok {
			continue
		}
		audit.MisclassifiedKnownOpenAICompatible++
		audit.ByProvider[misclassified.CompatProvider]++
		audit.MisclassifiedKnownOpenAICompatCards = append(audit.MisclassifiedKnownOpenAICompatCards, misclassified)
	}
	return audit, nil
}
