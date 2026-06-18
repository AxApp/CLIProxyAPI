package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
)

const (
	openAIQuotaResetSource            = "openai-quota-reset"
	openAIQuotaResetUsageDefaultURL   = "https://chatgpt.com/backend-api/wham/usage"
	openAIQuotaResetConsumeDefaultURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	openAIQuotaResetOriginator        = "Codex Desktop"
	openAIQuotaResetLanguageTag       = "zh-CN"
	openAIQuotaResetCodeUnavailable   = "reset_credit_unavailable"
)

var (
	openAIQuotaResetUsageURL   = openAIQuotaResetUsageDefaultURL
	openAIQuotaResetConsumeURL = openAIQuotaResetConsumeDefaultURL
)

type openAIQuotaResetCredits struct {
	AvailableCount int `json:"available_count"`
}

type openAIQuotaResetUsagePayload struct {
	UserID                string                     `json:"user_id,omitempty"`
	AccountID             string                     `json:"account_id,omitempty"`
	Email                 string                     `json:"email,omitempty"`
	PlanType              string                     `json:"plan_type,omitempty"`
	PlanTypeCamel         string                     `json:"planType,omitempty"`
	RateLimitResetCredits *openAIQuotaResetCredits   `json:"rate_limit_reset_credits,omitempty"`
	ResetCreditsCamel     *openAIQuotaResetCredits   `json:"rateLimitResetCredits,omitempty"`
	Raw                   map[string]json.RawMessage `json:"-"`
}

type openAIQuotaResetCredit struct {
	ID              string `json:"id,omitempty"`
	ResetType       string `json:"reset_type,omitempty"`
	Status          string `json:"status,omitempty"`
	GrantedAt       string `json:"granted_at,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	RedeemStartedAt string `json:"redeem_started_at,omitempty"`
	RedeemedAt      string `json:"redeemed_at,omitempty"`
}

type openAIQuotaResetConsumeUpstreamResponse struct {
	Code         string                  `json:"code"`
	Credit       *openAIQuotaResetCredit `json:"credit,omitempty"`
	WindowsReset int                     `json:"windows_reset"`
}

type openAIQuotaResetQueryResponse struct {
	AccountKey     string                            `json:"account_key"`
	Status         string                            `json:"status"`
	AvailableCount int                               `json:"available_count"`
	PlanType       string                            `json:"plan_type,omitempty"`
	FetchedAt      int64                             `json:"fetched_at"`
	QuotaState     *gettokenshooks.QuotaRuntimeState `json:"quota_state,omitempty"`
	Usage          *openAIQuotaResetUsagePayload     `json:"usage,omitempty"`
}

type openAIQuotaResetConsumeResponse struct {
	AccountKey             string                            `json:"account_key"`
	Status                 string                            `json:"status"`
	Code                   string                            `json:"code,omitempty"`
	Credit                 *openAIQuotaResetCredit           `json:"credit,omitempty"`
	WindowsReset           int                               `json:"windows_reset"`
	AvailableCount         int                               `json:"available_count"`
	PlanType               string                            `json:"plan_type,omitempty"`
	FetchedAt              int64                             `json:"fetched_at"`
	QuotaState             *gettokenshooks.QuotaRuntimeState `json:"quota_state,omitempty"`
	PostResetRefreshStatus string                            `json:"post_reset_refresh_status,omitempty"`
	PostResetRefreshError  string                            `json:"post_reset_refresh_error,omitempty"`
}

type openAIQuotaResetAuthInfo struct {
	AccessToken      string
	ChatGPTAccountID string
	PlanType         string
}

func overrideOpenAIQuotaResetUpstreamForTest(usageURL string, consumeURL string) func() {
	prevUsage := openAIQuotaResetUsageURL
	prevConsume := openAIQuotaResetConsumeURL
	openAIQuotaResetUsageURL = strings.TrimSpace(usageURL)
	openAIQuotaResetConsumeURL = strings.TrimSpace(consumeURL)
	return func() {
		openAIQuotaResetUsageURL = prevUsage
		openAIQuotaResetConsumeURL = prevConsume
	}
}

func (h *Handler) GetOpenAIQuotaResetCredit(c *gin.Context) {
	account, authInfo, err := h.openAIQuotaResetAccount(c.Request.Context(), c.Param("account_key"))
	if err != nil {
		writeOpenAIQuotaResetError(c, err)
		return
	}
	usage, quotaState, err := h.fetchOpenAIQuotaResetUsage(c.Request.Context(), account, authInfo)
	if err != nil {
		writeOpenAIQuotaResetError(c, err)
		return
	}
	c.JSON(http.StatusOK, openAIQuotaResetQueryResponse{
		AccountKey:     account.AccountKey,
		Status:         "success",
		AvailableCount: openAIQuotaResetAvailableCount(usage),
		PlanType:       normalizeQuotaPlanType(quotaFirstNonEmpty(usage.PlanType, usage.PlanTypeCamel, authInfo.PlanType)),
		FetchedAt:      time.Now().Unix(),
		QuotaState:     quotaState,
		Usage:          usage,
	})
}

func (h *Handler) ConsumeOpenAIQuotaResetCredit(c *gin.Context) {
	account, authInfo, err := h.openAIQuotaResetAccount(c.Request.Context(), c.Param("account_key"))
	if err != nil {
		writeOpenAIQuotaResetError(c, err)
		return
	}
	preflight, _, err := h.fetchOpenAIQuotaResetUsage(c.Request.Context(), account, authInfo)
	if err != nil {
		writeOpenAIQuotaResetError(c, err)
		return
	}
	if openAIQuotaResetAvailableCount(preflight) <= 0 {
		c.JSON(http.StatusConflict, gin.H{
			"code":        openAIQuotaResetCodeUnavailable,
			"status":      "failed",
			"retryable":   false,
			"error":       "no OpenAI quota reset credits are available",
			"account_key": account.AccountKey,
		})
		return
	}

	result, err := h.consumeOpenAIQuotaResetCredit(c.Request.Context(), authInfo)
	if err != nil {
		writeOpenAIQuotaResetError(c, err)
		return
	}

	refreshStatus := "success"
	refreshed, quotaState, errRefresh := h.fetchOpenAIQuotaResetUsage(c.Request.Context(), account, authInfo)
	if errRefresh != nil {
		refreshStatus = "degraded"
	}
	response := openAIQuotaResetConsumeResponse{
		AccountKey:             account.AccountKey,
		Status:                 "success",
		Code:                   strings.TrimSpace(result.Code),
		Credit:                 result.Credit,
		WindowsReset:           result.WindowsReset,
		AvailableCount:         openAIQuotaResetAvailableCount(refreshed),
		PlanType:               normalizeQuotaPlanType(quotaFirstNonEmpty(refreshed.PlanType, refreshed.PlanTypeCamel, preflight.PlanType, preflight.PlanTypeCamel, authInfo.PlanType)),
		FetchedAt:              time.Now().Unix(),
		QuotaState:             quotaState,
		PostResetRefreshStatus: refreshStatus,
	}
	if errRefresh != nil {
		response.PostResetRefreshError = errRefresh.Error()
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) openAIQuotaResetAccount(ctx context.Context, accountKey string) (accountstore.AccountRecord, openAIQuotaResetAuthInfo, error) {
	accountKey = strings.TrimSpace(accountKey)
	if !accountstore.IsAccountKey(accountKey) {
		return accountstore.AccountRecord{}, openAIQuotaResetAuthInfo{}, newOpenAIQuotaResetHTTPError(http.StatusBadRequest, "invalid_account_key", fmt.Sprintf("invalid account key %q", accountKey), false)
	}
	store, err := h.openAccountStore(ctx)
	if err != nil {
		return accountstore.AccountRecord{}, openAIQuotaResetAuthInfo{}, err
	}
	account, err := store.GetAccount(ctx, accountKey)
	if err != nil {
		return accountstore.AccountRecord{}, openAIQuotaResetAuthInfo{}, err
	}
	if account.Kind != accountstore.KindAuthFile || account.AuthFile == nil {
		return accountstore.AccountRecord{}, openAIQuotaResetAuthInfo{}, newOpenAIQuotaResetHTTPError(http.StatusBadRequest, "unsupported_account_kind", "OpenAI quota reset is only available for OAuth auth-file accounts", false)
	}
	authInfo, err := parseOpenAIQuotaResetAuthFile([]byte(account.AuthFile.AuthJSON))
	if err != nil {
		return accountstore.AccountRecord{}, openAIQuotaResetAuthInfo{}, err
	}
	if authInfo.PlanType == "" {
		authInfo.PlanType = normalizeQuotaPlanType(account.AuthFile.PlanType)
	}
	return account, authInfo, nil
}

func (h *Handler) fetchOpenAIQuotaResetUsage(ctx context.Context, account accountstore.AccountRecord, authInfo openAIQuotaResetAuthInfo) (*openAIQuotaResetUsagePayload, *gettokenshooks.QuotaRuntimeState, error) {
	body, status, err := h.doOpenAIQuotaResetRequest(ctx, http.MethodGet, openAIQuotaResetUsageURL, authInfo, nil)
	if err != nil {
		return nil, nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, nil, newOpenAIQuotaResetUpstreamError(status, body, "OPENAI_QUOTA_UPSTREAM_ERROR")
	}
	var usage openAIQuotaResetUsagePayload
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, nil, fmt.Errorf("openai quota usage response parse failed: %w", err)
	}
	usage.Raw = map[string]json.RawMessage{}
	_ = json.Unmarshal(body, &usage.Raw)

	var quotaState *gettokenshooks.QuotaRuntimeState
	if quota, err := buildQuotaResponseFromUsagePayload(body, normalizeQuotaPlanType(quotaFirstNonEmpty(usage.PlanType, usage.PlanTypeCamel, authInfo.PlanType))); err == nil {
		state, errState := gettokenshooks.DefaultQuotaRuntimeStore().Upsert(quotaRuntimeStateFromParsed(account.AccountKey, openAIQuotaResetSource, gettokenshooks.QuotaRuntimeStatusSuccess, quota), time.Now().UTC())
		if errState == nil {
			quotaState = &state
		}
	}
	return &usage, quotaState, nil
}

func (h *Handler) consumeOpenAIQuotaResetCredit(ctx context.Context, authInfo openAIQuotaResetAuthInfo) (*openAIQuotaResetConsumeUpstreamResponse, error) {
	redeemID, err := generateOpenAIQuotaResetRedeemRequestID()
	if err != nil {
		return nil, err
	}
	requestBody, err := json.Marshal(map[string]string{"redeem_request_id": redeemID})
	if err != nil {
		return nil, err
	}
	body, status, err := h.doOpenAIQuotaResetRequest(ctx, http.MethodPost, openAIQuotaResetConsumeURL, authInfo, requestBody)
	if err != nil {
		return nil, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, newOpenAIQuotaResetUpstreamError(status, body, "OPENAI_QUOTA_RESET_UPSTREAM_ERROR")
	}
	var result openAIQuotaResetConsumeUpstreamResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("openai quota reset response parse failed: %w", err)
	}
	return &result, nil
}

func (h *Handler) doOpenAIQuotaResetRequest(ctx context.Context, method string, targetURL string, authInfo openAIQuotaResetAuthInfo, body []byte) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSpace(targetURL), reader)
	if err != nil {
		return nil, 0, err
	}
	for key, value := range openAIQuotaResetHeaders(authInfo) {
		req.Header.Set(key, value)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	client := &http.Client{Timeout: defaultAPICallTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, newOpenAIQuotaResetHTTPError(http.StatusBadGateway, "OPENAI_QUOTA_REQUEST_FAILED", err.Error(), true)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

func openAIQuotaResetHeaders(authInfo openAIQuotaResetAuthInfo) map[string]string {
	return map[string]string{
		"authorization":      "Bearer " + strings.TrimSpace(authInfo.AccessToken),
		"chatgpt-account-id": strings.TrimSpace(authInfo.ChatGPTAccountID),
		"oai-language":       openAIQuotaResetLanguageTag,
		"originator":         openAIQuotaResetOriginator,
		"accept":             "application/json",
		"sec-fetch-site":     "none",
		"sec-fetch-mode":     "no-cors",
		"sec-fetch-dest":     "empty",
		"priority":           "u=4, i",
	}
}

func openAIQuotaResetAvailableCount(usage *openAIQuotaResetUsagePayload) int {
	if usage == nil {
		return 0
	}
	if usage.RateLimitResetCredits != nil {
		return usage.RateLimitResetCredits.AvailableCount
	}
	if usage.ResetCreditsCamel != nil {
		return usage.ResetCreditsCamel.AvailableCount
	}
	return 0
}

func parseOpenAIQuotaResetAuthFile(body []byte) (openAIQuotaResetAuthInfo, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return openAIQuotaResetAuthInfo{}, fmt.Errorf("codex auth file parse failed: %w", err)
	}
	metadata := openAIQuotaResetNestedMap(payload, "metadata")
	attributes := openAIQuotaResetNestedMap(payload, "attributes")
	rootToken := openAIQuotaResetNestedMap(payload, "token")
	tokens := openAIQuotaResetNestedMap(payload, "tokens")
	metadataToken := openAIQuotaResetNestedMap(metadata, "token")
	attributesToken := openAIQuotaResetNestedMap(attributes, "token")
	accessToken := quotaFirstNonEmpty(
		openAIQuotaResetStringValue(tokens, "access_token"),
		openAIQuotaResetStringValue(tokens, "accessToken"),
		openAIQuotaResetStringValue(payload, "access_token"),
		openAIQuotaResetStringValue(payload, "accessToken"),
		openAIQuotaResetStringValue(metadata, "access_token"),
		openAIQuotaResetStringValue(metadata, "accessToken"),
		openAIQuotaResetStringValue(attributes, "access_token"),
		openAIQuotaResetStringValue(attributes, "accessToken"),
		openAIQuotaResetStringValue(rootToken, "access_token"),
		openAIQuotaResetStringValue(rootToken, "accessToken"),
		openAIQuotaResetStringValue(metadataToken, "access_token"),
		openAIQuotaResetStringValue(metadataToken, "accessToken"),
		openAIQuotaResetStringValue(attributesToken, "access_token"),
		openAIQuotaResetStringValue(attributesToken, "accessToken"),
	)
	idTokenRaw := quotaFirstNonEmpty(
		openAIQuotaResetStringValue(payload, "id_token"),
		openAIQuotaResetStringValue(metadata, "id_token"),
		openAIQuotaResetStringValue(attributes, "id_token"),
		openAIQuotaResetStringValue(tokens, "id_token"),
	)
	idClaims := openAIQuotaResetJWTClaims(idTokenRaw)
	openAIAuthClaims := openAIQuotaResetNestedMap(idClaims, "https://api.openai.com/auth")
	chatGPTAccountID := quotaFirstNonEmpty(
		openAIQuotaResetStringValue(tokens, "account_id"),
		openAIQuotaResetStringValue(tokens, "accountId"),
		openAIQuotaResetStringValue(tokens, "chatgpt_account_id"),
		openAIQuotaResetStringValue(tokens, "chatgptAccountId"),
		openAIQuotaResetStringValue(tokens, "organization_id"),
		openAIQuotaResetStringValue(payload, "account_id"),
		openAIQuotaResetStringValue(payload, "accountId"),
		openAIQuotaResetStringValue(payload, "chatgpt_account_id"),
		openAIQuotaResetStringValue(payload, "chatgptAccountId"),
		openAIQuotaResetStringValue(payload, "organization_id"),
		openAIQuotaResetStringValue(metadata, "account_id"),
		openAIQuotaResetStringValue(metadata, "accountId"),
		openAIQuotaResetStringValue(metadata, "chatgpt_account_id"),
		openAIQuotaResetStringValue(metadata, "chatgptAccountId"),
		openAIQuotaResetStringValue(metadata, "organization_id"),
		openAIQuotaResetStringValue(idClaims, "chatgpt_account_id"),
		openAIQuotaResetStringValue(idClaims, "chatgptAccountId"),
		openAIQuotaResetStringValue(openAIAuthClaims, "chatgpt_account_id"),
		openAIQuotaResetStringValue(openAIAuthClaims, "chatgptAccountId"),
	)
	planType := normalizeQuotaPlanType(quotaFirstNonEmpty(
		openAIQuotaResetStringValue(payload, "plan_type"),
		openAIQuotaResetStringValue(payload, "planType"),
		openAIQuotaResetStringValue(metadata, "plan_type"),
		openAIQuotaResetStringValue(metadata, "planType"),
		openAIQuotaResetStringValue(attributes, "plan_type"),
		openAIQuotaResetStringValue(attributes, "planType"),
		openAIQuotaResetStringValue(idClaims, "plan_type"),
		openAIQuotaResetStringValue(idClaims, "planType"),
		openAIQuotaResetStringValue(openAIAuthClaims, "chatgpt_plan_type"),
		openAIQuotaResetStringValue(openAIAuthClaims, "chatgptPlanType"),
	))
	if strings.TrimSpace(accessToken) == "" {
		return openAIQuotaResetAuthInfo{}, newOpenAIQuotaResetHTTPError(http.StatusUnauthorized, "OPENAI_QUOTA_TOKEN_UNAVAILABLE", "codex credential is missing access_token; please re-authorize this account", false)
	}
	if strings.TrimSpace(chatGPTAccountID) == "" {
		return openAIQuotaResetAuthInfo{}, newOpenAIQuotaResetHTTPError(http.StatusBadRequest, "OPENAI_QUOTA_MISSING_ACCOUNT_ID", "codex credential is missing chatgpt_account_id; please re-authorize this account", false)
	}
	return openAIQuotaResetAuthInfo{
		AccessToken:      strings.TrimSpace(accessToken),
		ChatGPTAccountID: strings.TrimSpace(chatGPTAccountID),
		PlanType:         planType,
	}, nil
}

func openAIQuotaResetNestedMap(root map[string]interface{}, key string) map[string]interface{} {
	if root == nil {
		return map[string]interface{}{}
	}
	value, ok := root[key]
	if !ok {
		return map[string]interface{}{}
	}
	typed, ok := value.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	return typed
}

func openAIQuotaResetStringValue(root map[string]interface{}, key string) string {
	if root == nil {
		return ""
	}
	value, ok := root[key]
	if !ok {
		return ""
	}
	typed, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(typed)
}

func openAIQuotaResetJWTClaims(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]interface{}{}
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return map[string]interface{}{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]interface{}{}
	}
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return map[string]interface{}{}
	}
	return claims
}

func generateOpenAIQuotaResetRedeemRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexStr := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:]), nil
}

type openAIQuotaResetHTTPError struct {
	StatusCode int
	Code       string
	Message    string
	Retryable  bool
}

func (e openAIQuotaResetHTTPError) Error() string {
	return e.Message
}

func newOpenAIQuotaResetHTTPError(status int, code string, message string, retryable bool) error {
	return openAIQuotaResetHTTPError{
		StatusCode: status,
		Code:       strings.TrimSpace(code),
		Message:    strings.TrimSpace(message),
		Retryable:  retryable,
	}
}

func newOpenAIQuotaResetUpstreamError(status int, body []byte, code string) error {
	message := fmt.Sprintf("upstream returned %d", status)
	if len(body) > 0 {
		message = message + ": " + truncateOpenAIQuotaResetBody(string(body), 240)
	}
	return openAIQuotaResetHTTPError{
		StatusCode: mapOpenAIQuotaResetUpstreamStatus(status),
		Code:       code,
		Message:    message,
		Retryable:  status == http.StatusTooManyRequests || status >= 500,
	}
}

func mapOpenAIQuotaResetUpstreamStatus(status int) int {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return status
	case status == http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case status >= 400:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

func truncateOpenAIQuotaResetBody(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func writeOpenAIQuotaResetError(c *gin.Context, err error) {
	if err == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unknown openai quota reset error", "code": "unknown_error", "retryable": false})
		return
	}
	var typed openAIQuotaResetHTTPError
	if errors.As(err, &typed) {
		status := typed.StatusCode
		if status == 0 {
			status = http.StatusInternalServerError
		}
		c.JSON(status, gin.H{
			"error":     typed.Message,
			"code":      typed.Code,
			"retryable": typed.Retryable,
			"status":    "failed",
		})
		return
	}
	writeAccountStoreError(c, err)
}
