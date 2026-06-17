package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	quotaRefreshSource                = "codex-api-key-quota-curl"
	quotaDraftTestSource              = "codex-api-key-quota-test"
	quotaBillingDraftTestSource       = "codex-api-key-billing-test"
	quotaFiveHourWindowSeconds  int64 = 18000
	quotaWeeklyWindowSeconds    int64 = 604800
)

type quotaRefreshRequest struct {
	IncludeBilling bool `json:"include_billing"`
	Force          bool `json:"force"`
}

type quotaRefreshBatchRequest struct {
	AccountKeys    []string `json:"account_keys"`
	IncludeBilling bool     `json:"include_billing"`
	Force          bool     `json:"force"`
	Concurrency    int      `json:"concurrency,omitempty"`
}

type quotaRefreshBatchError struct {
	AccountKey string `json:"account_key"`
	Error      string `json:"error"`
}

type quotaRefreshBatchResponse struct {
	Items     []gettokenshooks.QuotaRuntimeState `json:"items"`
	Errors    []quotaRefreshBatchError           `json:"errors"`
	Succeeded int                                `json:"succeeded"`
	Failed    int                                `json:"failed"`
}

type quotaRefreshBatchWorkResult struct {
	index int
	state gettokenshooks.QuotaRuntimeState
	err   quotaRefreshBatchError
	ok    bool
}

const (
	quotaRefreshBatchJobStatusPending   = "pending"
	quotaRefreshBatchJobStatusRunning   = "running"
	quotaRefreshBatchJobStatusSucceeded = "succeeded"
	quotaRefreshBatchJobStatusFailed    = "failed"
	quotaRefreshBatchJobStatusCanceled  = "canceled"
)

type quotaRefreshBatchJob struct {
	JobID          string
	Status         string
	AccountKeys    []string
	IncludeBilling bool
	Force          bool
	Concurrency    int
	Context        context.Context
	Cancel         context.CancelFunc
	Total          int
	Pending        int
	Running        int
	Items          []gettokenshooks.QuotaRuntimeState
	Errors         []quotaRefreshBatchError
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
}

type quotaRefreshBatchJobResponse struct {
	JobID       string                             `json:"job_id"`
	Status      string                             `json:"status"`
	Total       int                                `json:"total"`
	Pending     int                                `json:"pending"`
	Running     int                                `json:"running"`
	Succeeded   int                                `json:"succeeded"`
	Failed      int                                `json:"failed"`
	Items       []gettokenshooks.QuotaRuntimeState `json:"items"`
	Errors      []quotaRefreshBatchError           `json:"errors"`
	CreatedAt   string                             `json:"created_at"`
	UpdatedAt   string                             `json:"updated_at"`
	CompletedAt string                             `json:"completed_at,omitempty"`
}

type quotaRefreshBatchJobStore struct {
	mu   sync.Mutex
	seq  int64
	jobs map[string]*quotaRefreshBatchJob
}

type quotaCurlTestRequest struct {
	APIKey         string            `json:"api_key"`
	BaseURL        string            `json:"base_url"`
	Prefix         string            `json:"prefix,omitempty"`
	QuotaCurl      string            `json:"quota_curl,omitempty"`
	BillingCurl    string            `json:"billing_curl,omitempty"`
	PlatformCookie string            `json:"platform_cookie,omitempty"`
	CurlVariables  map[string]string `json:"curl_variables,omitempty"`
	AccountKey     string            `json:"account_key,omitempty"`
}

type quotaCurlInput struct {
	Curl           string
	APIKey         string
	BaseURL        string
	Prefix         string
	PlatformCookie string
	CurlVariables  map[string]string
}

type quotaCurlRequest struct {
	Method         string
	URL            string
	Headers        map[string]string
	Body           string
	IgnoredOptions []string
}

type quotaParsedResponse struct {
	PlanType string
	Windows  []gettokenshooks.QuotaRuntimeWindow
	Billing  *gettokenshooks.QuotaRuntimeBilling
}

func newQuotaRefreshBatchJobStore() *quotaRefreshBatchJobStore {
	return &quotaRefreshBatchJobStore{jobs: map[string]*quotaRefreshBatchJob{}}
}

func (s *quotaRefreshBatchJobStore) create(req quotaRefreshBatchRequest, accountKeys []string, now time.Time) quotaRefreshBatchJobResponse {
	if s == nil {
		return quotaRefreshBatchJobResponse{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	jobCtx, cancel := context.WithCancel(context.Background())
	jobID := fmt.Sprintf("quota_refresh_%d_%d", now.UnixNano(), s.seq)
	job := &quotaRefreshBatchJob{
		JobID:          jobID,
		Status:         quotaRefreshBatchJobStatusPending,
		AccountKeys:    append([]string(nil), accountKeys...),
		IncludeBilling: req.IncludeBilling,
		Force:          req.Force,
		Concurrency:    req.Concurrency,
		Context:        jobCtx,
		Cancel:         cancel,
		Total:          len(accountKeys),
		Pending:        len(accountKeys),
		Items:          []gettokenshooks.QuotaRuntimeState{},
		Errors:         []quotaRefreshBatchError{},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.jobs[jobID] = job
	return quotaRefreshBatchJobSnapshot(job)
}

func (s *quotaRefreshBatchJobStore) markRunning(jobID string, now time.Time) (quotaRefreshBatchJob, bool) {
	if s == nil {
		return quotaRefreshBatchJob{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return quotaRefreshBatchJob{}, false
	}
	if isQuotaRefreshBatchJobTerminal(job.Status) {
		return cloneQuotaRefreshBatchJob(job), false
	}
	job.Status = quotaRefreshBatchJobStatusRunning
	job.Pending = 0
	job.Running = job.Total
	job.UpdatedAt = now
	return cloneQuotaRefreshBatchJob(job), true
}

func (s *quotaRefreshBatchJobStore) complete(jobID string, items []gettokenshooks.QuotaRuntimeState, errors []quotaRefreshBatchError, now time.Time) (quotaRefreshBatchJobResponse, bool) {
	if s == nil {
		return quotaRefreshBatchJobResponse{}, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return quotaRefreshBatchJobResponse{}, false
	}
	if job.Status == quotaRefreshBatchJobStatusCanceled {
		return quotaRefreshBatchJobSnapshot(job), true
	}
	job.Items = append([]gettokenshooks.QuotaRuntimeState(nil), items...)
	job.Errors = append([]quotaRefreshBatchError(nil), errors...)
	job.Pending = 0
	job.Running = 0
	if len(errors) > 0 {
		job.Status = quotaRefreshBatchJobStatusFailed
	} else {
		job.Status = quotaRefreshBatchJobStatusSucceeded
	}
	job.UpdatedAt = now
	job.CompletedAt = now
	if job.Cancel != nil {
		job.Cancel()
	}
	return quotaRefreshBatchJobSnapshot(job), true
}

func (s *quotaRefreshBatchJobStore) cancelForAccountKeys(accountKeys []string, reason string, now time.Time) int {
	if s == nil || len(accountKeys) == 0 {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	keySet := make(map[string]struct{}, len(accountKeys))
	for _, accountKey := range accountKeys {
		accountKey = strings.TrimSpace(accountKey)
		if accountKey != "" {
			keySet[accountKey] = struct{}{}
		}
	}
	if len(keySet) == 0 {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	canceled := 0
	for _, job := range s.jobs {
		if job == nil || isQuotaRefreshBatchJobTerminal(job.Status) || !quotaRefreshBatchJobIntersects(job, keySet) {
			continue
		}
		cancelQuotaRefreshBatchJobLocked(job, reason, now, keySet)
		canceled++
	}
	return canceled
}

func (s *quotaRefreshBatchJobStore) cancelAll(reason string, now time.Time) int {
	if s == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	canceled := 0
	for _, job := range s.jobs {
		if job == nil || isQuotaRefreshBatchJobTerminal(job.Status) {
			continue
		}
		cancelQuotaRefreshBatchJobLocked(job, reason, now, nil)
		canceled++
	}
	return canceled
}

func (s *quotaRefreshBatchJobStore) get(jobID string) (quotaRefreshBatchJobResponse, bool) {
	if s == nil {
		return quotaRefreshBatchJobResponse{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[strings.TrimSpace(jobID)]
	if !ok {
		return quotaRefreshBatchJobResponse{}, false
	}
	return quotaRefreshBatchJobSnapshot(job), true
}

func cloneQuotaRefreshBatchJob(job *quotaRefreshBatchJob) quotaRefreshBatchJob {
	if job == nil {
		return quotaRefreshBatchJob{}
	}
	cloned := *job
	cloned.AccountKeys = append([]string(nil), job.AccountKeys...)
	cloned.Items = append([]gettokenshooks.QuotaRuntimeState(nil), job.Items...)
	cloned.Errors = append([]quotaRefreshBatchError(nil), job.Errors...)
	return cloned
}

func isQuotaRefreshBatchJobTerminal(status string) bool {
	switch strings.TrimSpace(status) {
	case quotaRefreshBatchJobStatusSucceeded, quotaRefreshBatchJobStatusFailed, quotaRefreshBatchJobStatusCanceled:
		return true
	default:
		return false
	}
}

func quotaRefreshBatchJobIntersects(job *quotaRefreshBatchJob, keySet map[string]struct{}) bool {
	if job == nil || len(keySet) == 0 {
		return false
	}
	for _, accountKey := range job.AccountKeys {
		if _, ok := keySet[accountKey]; ok {
			return true
		}
	}
	return false
}

func cancelQuotaRefreshBatchJobLocked(job *quotaRefreshBatchJob, reason string, now time.Time, accountKeySet map[string]struct{}) {
	if job == nil {
		return
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "quota refresh job canceled"
	}
	if job.Cancel != nil {
		job.Cancel()
	}
	job.Status = quotaRefreshBatchJobStatusCanceled
	job.Pending = 0
	job.Running = 0
	job.UpdatedAt = now
	job.CompletedAt = now
	if len(job.Errors) == 0 {
		job.Errors = quotaRefreshBatchJobCancellationErrors(job, reason, accountKeySet)
	}
}

func quotaRefreshBatchJobCancellationErrors(job *quotaRefreshBatchJob, reason string, accountKeySet map[string]struct{}) []quotaRefreshBatchError {
	if job == nil {
		return []quotaRefreshBatchError{{Error: reason}}
	}
	errors := make([]quotaRefreshBatchError, 0)
	for _, accountKey := range job.AccountKeys {
		if len(accountKeySet) > 0 {
			if _, ok := accountKeySet[accountKey]; !ok {
				continue
			}
		}
		errors = append(errors, quotaRefreshBatchError{AccountKey: accountKey, Error: reason})
	}
	if len(errors) == 0 {
		errors = append(errors, quotaRefreshBatchError{Error: reason})
	}
	return errors
}

func quotaRefreshBatchJobSnapshot(job *quotaRefreshBatchJob) quotaRefreshBatchJobResponse {
	if job == nil {
		return quotaRefreshBatchJobResponse{}
	}
	completedAt := ""
	if !job.CompletedAt.IsZero() {
		completedAt = job.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	return quotaRefreshBatchJobResponse{
		JobID:       job.JobID,
		Status:      job.Status,
		Total:       job.Total,
		Pending:     job.Pending,
		Running:     job.Running,
		Succeeded:   len(job.Items),
		Failed:      len(job.Errors),
		Items:       append([]gettokenshooks.QuotaRuntimeState(nil), job.Items...),
		Errors:      append([]quotaRefreshBatchError(nil), job.Errors...),
		CreatedAt:   job.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   job.UpdatedAt.UTC().Format(time.RFC3339Nano),
		CompletedAt: completedAt,
	}
}

func (h *Handler) RefreshAccountQuota(c *gin.Context) {
	accountKey := strings.TrimSpace(c.Param("account_key"))
	if !accountstore.IsAccountKey(accountKey) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid account_key %q", accountKey)})
		return
	}

	var req quotaRefreshRequest
	if err := bindOptionalQuotaJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	store, err := h.openAccountStore(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	account, err := store.GetAccount(c.Request.Context(), accountKey)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}
	var state gettokenshooks.QuotaRuntimeState
	switch account.Kind {
	case accountstore.KindCodexAPIKey:
		state, err = h.refreshCodexAPIKeyQuota(c.Request.Context(), account, req.IncludeBilling)
	case accountstore.KindOpenAICompatible:
		state, err = h.refreshOpenAICompatibleQuota(c.Request.Context(), account, req.IncludeBilling)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "account kind does not support quota refresh"})
		return
	}
	if err != nil {
		if fallback, ok := degradedQuotaRuntimeState(accountKey, err); ok {
			c.JSON(http.StatusOK, fallback)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (h *Handler) RefreshAccountQuotaBatch(c *gin.Context) {
	var req quotaRefreshBatchRequest
	if err := bindOptionalQuotaJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	accountKeys, err := normalizeQuotaRefreshBatchAccountKeys(req.AccountKeys)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	targets, missing, err := h.resolveQuotaRefreshBatchTargets(c.Request.Context(), accountKeys)
	if err != nil {
		writeAccountStoreError(c, err)
		return
	}

	items, errors := h.refreshAccountQuotaBatch(c.Request.Context(), targets, req.IncludeBilling, req.Concurrency)
	errors = append(missing, errors...)
	c.JSON(http.StatusOK, quotaRefreshBatchResponse{
		Items:     items,
		Errors:    errors,
		Succeeded: len(items),
		Failed:    len(errors),
	})
}

func (h *Handler) StartAccountQuotaBatchRefreshJob(c *gin.Context) {
	var req quotaRefreshBatchRequest
	if err := bindOptionalQuotaJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	accountKeys, err := normalizeQuotaRefreshBatchAccountKeys(req.AccountKeys)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	store := h.quotaRefreshBatchJobStore()
	started := store.create(req, accountKeys, time.Now().UTC())
	go h.runAccountQuotaBatchRefreshJob(started.JobID)
	c.JSON(http.StatusAccepted, started)
}

func (h *Handler) GetAccountQuotaBatchRefreshJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("job_id"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "job_id is required"})
		return
	}
	if snapshot, ok := h.quotaRefreshBatchJobStore().get(jobID); ok {
		c.JSON(http.StatusOK, snapshot)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "quota refresh job not found"})
}

func (h *Handler) quotaRefreshBatchJobStore() *quotaRefreshBatchJobStore {
	if h == nil {
		return newQuotaRefreshBatchJobStore()
	}
	if h.quotaRefreshBatchJobs == nil {
		h.quotaRefreshBatchJobs = newQuotaRefreshBatchJobStore()
	}
	return h.quotaRefreshBatchJobs
}

func (h *Handler) cancelQuotaRefreshBatchJobsForAccountKeys(accountKeys []string, reason string, now time.Time) int {
	if h == nil {
		return 0
	}
	return h.quotaRefreshBatchJobStore().cancelForAccountKeys(accountKeys, reason, now)
}

func (h *Handler) cancelAllQuotaRefreshBatchJobs(reason string, now time.Time) int {
	if h == nil {
		return 0
	}
	return h.quotaRefreshBatchJobStore().cancelAll(reason, now)
}

func (h *Handler) CancelQuotaRefreshBatchJobs(reason string, now time.Time) int {
	return h.cancelAllQuotaRefreshBatchJobs(reason, now)
}

func (h *Handler) runAccountQuotaBatchRefreshJob(jobID string) {
	job, ok := h.quotaRefreshBatchJobStore().markRunning(jobID, time.Now().UTC())
	if !ok {
		return
	}
	jobCtx := job.Context
	if jobCtx == nil {
		jobCtx = context.Background()
	}
	targets, missing, err := h.resolveQuotaRefreshBatchTargets(jobCtx, job.AccountKeys)
	if err != nil {
		h.quotaRefreshBatchJobStore().complete(jobID, []gettokenshooks.QuotaRuntimeState{}, []quotaRefreshBatchError{{Error: err.Error()}}, time.Now().UTC())
		return
	}
	items, errors := h.refreshAccountQuotaBatch(jobCtx, targets, job.IncludeBilling, job.Concurrency)
	errors = append(missing, errors...)
	h.quotaRefreshBatchJobStore().complete(jobID, items, errors, time.Now().UTC())
}

func (h *Handler) TestQuotaCurl(c *gin.Context) {
	var req quotaCurlTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	state, err := h.testQuotaCurl(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func (h *Handler) TestBillingCurl(c *gin.Context) {
	var req quotaCurlTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	state, err := h.testBillingCurl(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, state)
}

func bindOptionalQuotaJSON(c *gin.Context, out any) error {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil
	}
	err := c.ShouldBindJSON(out)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func normalizeQuotaRefreshBatchAccountKeys(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, errors.New("account_keys is required")
	}
	keys := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		accountKey := strings.TrimSpace(value)
		if !accountstore.IsAccountKey(accountKey) {
			return nil, fmt.Errorf("invalid account_key %q", accountKey)
		}
		if _, ok := seen[accountKey]; ok {
			continue
		}
		seen[accountKey] = struct{}{}
		keys = append(keys, accountKey)
	}
	if len(keys) == 0 {
		return nil, errors.New("account_keys is required")
	}
	return keys, nil
}

func (h *Handler) resolveQuotaRefreshBatchTargets(ctx context.Context, accountKeys []string) ([]accountstore.AccountRecord, []quotaRefreshBatchError, error) {
	store, err := h.openAccountStore(ctx)
	if err != nil {
		return nil, nil, err
	}
	accounts, err := store.GetAccounts(ctx, accountKeys)
	if err != nil {
		return nil, nil, err
	}
	accountByKey := make(map[string]accountstore.AccountRecord, len(accounts))
	for _, account := range accounts {
		accountByKey[account.AccountKey] = account
	}
	targets := make([]accountstore.AccountRecord, 0, len(accountKeys))
	missing := make([]quotaRefreshBatchError, 0)
	for _, accountKey := range accountKeys {
		account, ok := accountByKey[accountKey]
		if !ok {
			missing = append(missing, quotaRefreshBatchError{AccountKey: accountKey, Error: "account not found"})
			continue
		}
		targets = append(targets, account)
	}
	return targets, missing, nil
}

func (h *Handler) refreshAccountQuotaBatch(ctx context.Context, accounts []accountstore.AccountRecord, includeBilling bool, concurrency int) ([]gettokenshooks.QuotaRuntimeState, []quotaRefreshBatchError) {
	if len(accounts) == 0 {
		return []gettokenshooks.QuotaRuntimeState{}, []quotaRefreshBatchError{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workerCount := normalizeQuotaRefreshBatchConcurrency(concurrency, len(accounts))
	jobs := make(chan int)
	results := make(chan quotaRefreshBatchWorkResult, len(accounts))
	var workers sync.WaitGroup
	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				account := accounts[index]
				if err := ctx.Err(); err != nil {
					sendQuotaRefreshBatchResult(ctx, results, quotaRefreshBatchWorkResult{index: index, err: quotaRefreshBatchError{AccountKey: account.AccountKey, Error: err.Error()}})
					continue
				}
				if err := h.ensureQuotaRefreshAccountStillActive(ctx, account.AccountKey); err != nil {
					sendQuotaRefreshBatchResult(ctx, results, quotaRefreshBatchWorkResult{index: index, err: quotaRefreshBatchError{AccountKey: account.AccountKey, Error: err.Error()}})
					continue
				}
				state, err := h.refreshAccountQuotaRecord(ctx, account, includeBilling)
				if err != nil {
					if fallback, ok := degradedQuotaRuntimeState(account.AccountKey, err); ok {
						sendQuotaRefreshBatchResult(ctx, results, quotaRefreshBatchWorkResult{index: index, state: fallback, ok: true})
						continue
					}
					sendQuotaRefreshBatchResult(ctx, results, quotaRefreshBatchWorkResult{index: index, err: quotaRefreshBatchError{AccountKey: account.AccountKey, Error: err.Error()}})
					continue
				}
				sendQuotaRefreshBatchResult(ctx, results, quotaRefreshBatchWorkResult{index: index, state: state, ok: true})
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range accounts {
			select {
			case <-ctx.Done():
				return
			case jobs <- index:
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	orderedStates := make([]gettokenshooks.QuotaRuntimeState, len(accounts))
	stateOK := make([]bool, len(accounts))
	orderedErrors := make([]quotaRefreshBatchError, len(accounts))
	errorOK := make([]bool, len(accounts))
	for result := range results {
		if result.ok {
			orderedStates[result.index] = result.state
			stateOK[result.index] = true
		} else {
			orderedErrors[result.index] = result.err
			errorOK[result.index] = true
		}
	}
	items := make([]gettokenshooks.QuotaRuntimeState, 0, len(accounts))
	for index, ok := range stateOK {
		if ok {
			items = append(items, orderedStates[index])
		}
	}
	errors := make([]quotaRefreshBatchError, 0)
	for index, ok := range errorOK {
		if ok {
			errors = append(errors, orderedErrors[index])
		}
	}
	if err := ctx.Err(); err != nil {
		for index, account := range accounts {
			if stateOK[index] || errorOK[index] {
				continue
			}
			errors = append(errors, quotaRefreshBatchError{AccountKey: account.AccountKey, Error: err.Error()})
		}
	}
	return items, errors
}

func sendQuotaRefreshBatchResult(ctx context.Context, results chan<- quotaRefreshBatchWorkResult, item quotaRefreshBatchWorkResult) {
	select {
	case <-ctx.Done():
	case results <- item:
	}
}

func (h *Handler) ensureQuotaRefreshAccountStillActive(ctx context.Context, accountKey string) error {
	store, err := h.openAccountStore(ctx)
	if err != nil {
		return err
	}
	if _, err := store.GetAccount(ctx, accountKey); err != nil {
		return err
	}
	return nil
}

func normalizeQuotaRefreshBatchConcurrency(value int, itemCount int) int {
	if itemCount <= 0 {
		return 1
	}
	if value <= 0 {
		value = 4
	}
	if value > 6 {
		value = 6
	}
	if value > itemCount {
		return itemCount
	}
	return value
}

func (h *Handler) refreshAccountQuotaRecord(ctx context.Context, account accountstore.AccountRecord, includeBilling bool) (gettokenshooks.QuotaRuntimeState, error) {
	switch account.Kind {
	case accountstore.KindCodexAPIKey:
		return h.refreshCodexAPIKeyQuota(ctx, account, includeBilling)
	case accountstore.KindOpenAICompatible:
		return h.refreshOpenAICompatibleQuota(ctx, account, includeBilling)
	default:
		return gettokenshooks.QuotaRuntimeState{}, errors.New("account kind does not support quota refresh")
	}
}

func (h *Handler) refreshCodexAPIKeyQuota(ctx context.Context, account accountstore.AccountRecord, includeBilling bool) (gettokenshooks.QuotaRuntimeState, error) {
	credential := account.CodexAPIKey
	if credential == nil {
		return gettokenshooks.QuotaRuntimeState{}, errors.New("codex api key credential is missing")
	}
	if strings.TrimSpace(credential.APIKey) == "" {
		return gettokenshooks.QuotaRuntimeState{}, errors.New("codex api key is empty")
	}
	if !credential.QuotaEnabled || strings.TrimSpace(credential.QuotaCurl) == "" {
		return gettokenshooks.QuotaRuntimeState{}, errors.New("codex api key quota curl is not configured")
	}

	quota, err := h.fetchQuotaFromCurl(ctx, quotaCurlInput{
		Curl:           credential.QuotaCurl,
		APIKey:         credential.APIKey,
		BaseURL:        credential.BaseURL,
		Prefix:         credential.Prefix,
		PlatformCookie: credential.PlatformCookie,
		CurlVariables:  decodeCurlVariablesJSON(credential.CurlVariablesJSON, credential.PlatformCookie),
	}, authForCodexAPIKeyAccount(account))
	if err != nil {
		return gettokenshooks.QuotaRuntimeState{}, err
	}

	if includeBilling && credential.BillingEnabled && strings.TrimSpace(credential.BillingCurl) != "" {
		if billing, errBilling := h.fetchBillingFromCurl(ctx, quotaCurlInput{
			Curl:           credential.BillingCurl,
			APIKey:         credential.APIKey,
			BaseURL:        credential.BaseURL,
			Prefix:         credential.Prefix,
			PlatformCookie: credential.PlatformCookie,
			CurlVariables:  decodeCurlVariablesJSON(credential.CurlVariablesJSON, credential.PlatformCookie),
		}, authForCodexAPIKeyAccount(account)); errBilling == nil {
			quota.Billing = billing
		} else {
			log.WithError(errBilling).WithField("account_key", account.AccountKey).Debug("codex api key billing refresh failed")
		}
	}

	return gettokenshooks.DefaultQuotaRuntimeStore().Upsert(quotaRuntimeStateFromParsed(account.AccountKey, quotaRefreshSource, gettokenshooks.QuotaRuntimeStatusSuccess, quota), time.Now().UTC())
}

func (h *Handler) refreshOpenAICompatibleQuota(ctx context.Context, account accountstore.AccountRecord, includeBilling bool) (gettokenshooks.QuotaRuntimeState, error) {
	credential := account.OpenAICompatible
	if credential == nil {
		return gettokenshooks.QuotaRuntimeState{}, errors.New("openai-compatible credential is missing")
	}
	apiKey, proxyURL := firstOpenAICompatibleAPIKeyEntry(credential.APIKeyEntriesJSON)
	if strings.TrimSpace(apiKey) == "" {
		return gettokenshooks.QuotaRuntimeState{}, errors.New("openai-compatible api key is empty")
	}
	if !credential.QuotaEnabled || strings.TrimSpace(credential.QuotaCurl) == "" {
		return gettokenshooks.QuotaRuntimeState{}, errors.New("openai-compatible quota curl is not configured")
	}

	auth := authForOpenAICompatibleAccount(account, apiKey, proxyURL)
	quota, err := h.fetchQuotaFromCurl(ctx, quotaCurlInput{
		Curl:           credential.QuotaCurl,
		APIKey:         apiKey,
		BaseURL:        credential.BaseURL,
		Prefix:         credential.Prefix,
		PlatformCookie: credential.PlatformCookie,
		CurlVariables:  decodeCurlVariablesJSON(credential.CurlVariablesJSON, credential.PlatformCookie),
	}, auth)
	if err != nil {
		return gettokenshooks.QuotaRuntimeState{}, err
	}

	if includeBilling && credential.BillingEnabled && strings.TrimSpace(credential.BillingCurl) != "" {
		if billing, errBilling := h.fetchBillingFromCurl(ctx, quotaCurlInput{
			Curl:           credential.BillingCurl,
			APIKey:         apiKey,
			BaseURL:        credential.BaseURL,
			Prefix:         credential.Prefix,
			PlatformCookie: credential.PlatformCookie,
			CurlVariables:  decodeCurlVariablesJSON(credential.CurlVariablesJSON, credential.PlatformCookie),
		}, auth); errBilling == nil {
			quota.Billing = billing
		} else {
			log.WithError(errBilling).WithField("account_key", account.AccountKey).Debug("openai-compatible billing refresh failed")
		}
	}

	return gettokenshooks.DefaultQuotaRuntimeStore().Upsert(quotaRuntimeStateFromParsed(account.AccountKey, "openai-compatible-quota-curl", gettokenshooks.QuotaRuntimeStatusSuccess, quota), time.Now().UTC())
}

func (h *Handler) testQuotaCurl(ctx context.Context, req quotaCurlTestRequest) (gettokenshooks.QuotaRuntimeState, error) {
	input, err := quotaCurlInputFromTestRequest(req, strings.TrimSpace(req.QuotaCurl))
	if err != nil {
		return gettokenshooks.QuotaRuntimeState{}, err
	}
	quota, err := h.fetchQuotaFromCurl(ctx, input, nil)
	if err != nil {
		return gettokenshooks.QuotaRuntimeState{}, err
	}
	return quotaRuntimeStateFromParsed(strings.TrimSpace(req.AccountKey), quotaDraftTestSource, gettokenshooks.QuotaRuntimeStatusSuccess, quota), nil
}

func (h *Handler) testBillingCurl(ctx context.Context, req quotaCurlTestRequest) (gettokenshooks.QuotaRuntimeState, error) {
	input, err := quotaCurlInputFromTestRequest(req, strings.TrimSpace(req.BillingCurl))
	if err != nil {
		return gettokenshooks.QuotaRuntimeState{}, err
	}
	billing, err := h.fetchBillingFromCurl(ctx, input, nil)
	if err != nil {
		return gettokenshooks.QuotaRuntimeState{}, err
	}
	return gettokenshooks.EnsureQuotaRuntimeFact(gettokenshooks.QuotaRuntimeState{
		AccountKey: strings.TrimSpace(req.AccountKey),
		Source:     quotaBillingDraftTestSource,
		Status:     gettokenshooks.QuotaRuntimeStatusSuccess,
		Windows:    []gettokenshooks.QuotaRuntimeWindow{},
		Billing:    billing,
		Sources:    []gettokenshooks.QuotaRuntimeSourceState{},
	}, time.Now().UTC()), nil
}

func quotaCurlInputFromTestRequest(req quotaCurlTestRequest, curl string) (quotaCurlInput, error) {
	input := quotaCurlInput{
		Curl:           strings.TrimSpace(curl),
		APIKey:         strings.TrimSpace(req.APIKey),
		BaseURL:        strings.TrimSpace(req.BaseURL),
		Prefix:         strings.TrimSpace(req.Prefix),
		PlatformCookie: normalizePlatformCookie(req.PlatformCookie),
		CurlVariables:  normalizeCurlVariables(req.CurlVariables),
	}
	if input.APIKey == "" {
		return quotaCurlInput{}, errors.New("api key is empty")
	}
	if input.BaseURL == "" {
		return quotaCurlInput{}, errors.New("base url is empty")
	}
	if input.Curl == "" {
		return quotaCurlInput{}, errors.New("quota curl is empty")
	}
	return input, nil
}

func (h *Handler) fetchQuotaFromCurl(ctx context.Context, input quotaCurlInput, auth *coreauth.Auth) (quotaParsedResponse, error) {
	request, err := buildQuotaCurlRequest(input)
	if err != nil {
		return quotaParsedResponse{}, err
	}
	body, status, err := h.executeQuotaCurlRequest(ctx, request, auth)
	if err != nil {
		return quotaParsedResponse{}, quotaCurlErrorWithIgnoredOptions(err, request.IgnoredOptions)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return quotaParsedResponse{}, quotaCurlErrorWithIgnoredOptions(fmt.Errorf("codex api key quota request failed with status %d", status), request.IgnoredOptions)
	}
	quota, err := buildQuotaResponseFromUsagePayload(body, "")
	if err != nil {
		return quotaParsedResponse{}, quotaCurlErrorWithIgnoredOptions(err, request.IgnoredOptions)
	}
	return quota, nil
}

func (h *Handler) fetchBillingFromCurl(ctx context.Context, input quotaCurlInput, auth *coreauth.Auth) (*gettokenshooks.QuotaRuntimeBilling, error) {
	request, err := buildQuotaCurlRequest(input)
	if err != nil {
		return nil, err
	}
	body, status, err := h.executeQuotaCurlRequest(ctx, request, auth)
	if err != nil {
		return nil, quotaCurlErrorWithIgnoredOptions(err, request.IgnoredOptions)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, quotaCurlErrorWithIgnoredOptions(fmt.Errorf("codex api key billing request failed with status %d", status), request.IgnoredOptions)
	}
	billing := tryParseQuotaBillingResponse(body)
	if billing == nil {
		return nil, quotaCurlErrorWithIgnoredOptions(errors.New("unable to parse billing response"), request.IgnoredOptions)
	}
	return billing, nil
}

func (h *Handler) executeQuotaCurlRequest(ctx context.Context, request *quotaCurlRequest, auth *coreauth.Auth) ([]byte, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request == nil {
		return nil, 0, errors.New("quota request is nil")
	}
	var body io.Reader
	if request.Body != "" {
		body = strings.NewReader(request.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, request.Method, request.URL, body)
	if err != nil {
		return nil, 0, err
	}
	var hostOverride string
	for key, value := range request.Headers {
		if strings.EqualFold(strings.TrimSpace(key), "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		httpReq.Header.Set(key, value)
	}
	if hostOverride != "" {
		httpReq.Host = hostOverride
	}
	client := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

func authForCodexAPIKeyAccount(account accountstore.AccountRecord) *coreauth.Auth {
	auth := &coreauth.Auth{
		ID:         account.AccountKey,
		AccountKey: account.AccountKey,
		Provider:   "codex",
		Attributes: map[string]string{},
	}
	if account.CodexAPIKey != nil {
		auth.ProxyURL = strings.TrimSpace(account.CodexAPIKey.ProxyURL)
		auth.Prefix = strings.TrimSpace(account.CodexAPIKey.Prefix)
		auth.Attributes["api_key"] = strings.TrimSpace(account.CodexAPIKey.APIKey)
		auth.Attributes["base_url"] = strings.TrimSpace(account.CodexAPIKey.BaseURL)
	}
	return auth
}

func firstOpenAICompatibleAPIKeyEntry(raw string) (string, string) {
	var entries []struct {
		APIKey   string `json:"api-key"`
		ProxyURL string `json:"proxy-url"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &entries); err != nil {
		return "", ""
	}
	for _, entry := range entries {
		apiKey := strings.TrimSpace(entry.APIKey)
		if apiKey != "" {
			return apiKey, strings.TrimSpace(entry.ProxyURL)
		}
	}
	return "", ""
}

func authForOpenAICompatibleAccount(account accountstore.AccountRecord, apiKey string, proxyURL string) *coreauth.Auth {
	provider := strings.TrimSpace(account.Provider)
	if provider == "" {
		provider = "openai-compatible"
	}
	auth := &coreauth.Auth{
		ID:         account.AccountKey,
		AccountKey: account.AccountKey,
		Provider:   provider,
		ProxyURL:   strings.TrimSpace(proxyURL),
		Attributes: map[string]string{},
	}
	if account.OpenAICompatible != nil {
		auth.Prefix = strings.TrimSpace(account.OpenAICompatible.Prefix)
		auth.Attributes["api_key"] = strings.TrimSpace(apiKey)
		auth.Attributes["base_url"] = strings.TrimSpace(account.OpenAICompatible.BaseURL)
	}
	return auth
}

func degradedQuotaRuntimeState(accountKey string, cause error) (gettokenshooks.QuotaRuntimeState, bool) {
	store := gettokenshooks.DefaultQuotaRuntimeStore()
	state, ok := store.StateForAccount(accountKey)
	if !ok || !quotaRuntimeStateHasDisplayData(state) {
		return gettokenshooks.QuotaRuntimeState{}, false
	}
	state.Status = gettokenshooks.QuotaRuntimeStatusDegraded
	state.Stale = true
	if cause != nil {
		state.DegradedReason = cause.Error()
	}
	state.Sources = []gettokenshooks.QuotaRuntimeSourceState{}
	state.Blocked = false
	state.BlockReason = ""
	state.Fact = nil
	next, err := store.Upsert(state, time.Now().UTC())
	if err != nil {
		return state, true
	}
	return next, true
}

func quotaRuntimeStateHasDisplayData(state gettokenshooks.QuotaRuntimeState) bool {
	return strings.TrimSpace(state.PlanType) != "" ||
		len(state.Windows) > 0 ||
		(state.Billing != nil && (state.Billing.IsAvailable || len(state.Billing.BalanceInfos) > 0))
}

func quotaRuntimeStateFromParsed(accountKey string, source string, status string, quota quotaParsedResponse) gettokenshooks.QuotaRuntimeState {
	return gettokenshooks.EnsureQuotaRuntimeFact(gettokenshooks.QuotaRuntimeState{
		AccountKey: strings.TrimSpace(accountKey),
		Source:     strings.TrimSpace(source),
		Status:     strings.TrimSpace(status),
		PlanType:   quota.PlanType,
		Windows:    append([]gettokenshooks.QuotaRuntimeWindow(nil), quota.Windows...),
		Billing:    cloneQuotaRuntimeBilling(quota.Billing),
		Sources:    []gettokenshooks.QuotaRuntimeSourceState{},
	}, time.Now().UTC())
}

func cloneQuotaRuntimeBilling(billing *gettokenshooks.QuotaRuntimeBilling) *gettokenshooks.QuotaRuntimeBilling {
	if billing == nil {
		return nil
	}
	cloned := *billing
	cloned.BalanceInfos = append([]gettokenshooks.QuotaRuntimeBalanceInfo(nil), billing.BalanceInfos...)
	return &cloned
}

func quotaCurlErrorWithIgnoredOptions(err error, ignoredOptions []string) error {
	if err == nil {
		return nil
	}
	if hint := quotaCurlIgnoredOptionsHint(ignoredOptions); hint != "" {
		return fmt.Errorf("%w. %s", err, hint)
	}
	return err
}

func buildQuotaCurlRequest(input quotaCurlInput) (*quotaCurlRequest, error) {
	raw := strings.TrimSpace(input.Curl)
	if raw == "" {
		return nil, errors.New("quota curl is empty")
	}
	if containsUnsupportedQuotaShellOperator(raw) {
		return nil, errors.New("quota curl does not support pipe, redirect, or multi-command shell syntax")
	}

	tokens, err := splitQuotaCurlCommand(raw)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, errors.New("quota curl is empty")
	}
	if tokens[0] == "curl" {
		tokens = tokens[1:]
	}

	request := &quotaCurlRequest{
		Method:  http.MethodGet,
		Headers: map[string]string{},
	}
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		switch token {
		case "-s", "-S", "-sS", "--silent", "--show-error", "-L", "--location", "--compressed", "-i", "--include":
			continue
		case "-X", "--request":
			value, next, err := nextQuotaCurlValue(tokens, index)
			if err != nil {
				return nil, err
			}
			request.Method = strings.ToUpper(applyQuotaCurlPlaceholders(value, input))
			index = next
		case "-H", "--header":
			value, next, err := nextQuotaCurlValue(tokens, index)
			if err != nil {
				return nil, err
			}
			key, headerValue, ok := strings.Cut(applyQuotaCurlPlaceholders(value, input), ":")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, fmt.Errorf("invalid quota curl header: %s", value)
			}
			request.Headers[strings.TrimSpace(key)] = strings.TrimSpace(headerValue)
			index = next
		case "-b", "--cookie":
			value, next, err := nextQuotaCurlValue(tokens, index)
			if err != nil {
				return nil, err
			}
			applyQuotaCurlCookieHeader(request, applyQuotaCurlPlaceholders(value, input))
			index = next
		case "-d", "--data", "--data-raw", "--data-binary", "--data-ascii":
			value, next, err := nextQuotaCurlValue(tokens, index)
			if err != nil {
				return nil, err
			}
			if request.Method == http.MethodGet {
				request.Method = http.MethodPost
			}
			request.Body = applyQuotaCurlPlaceholders(value, input)
			index = next
		case "--url":
			value, next, err := nextQuotaCurlValue(tokens, index)
			if err != nil {
				return nil, err
			}
			request.URL = applyQuotaCurlPlaceholders(value, input)
			index = next
		default:
			if strings.HasPrefix(token, "-") {
				next, ignored := ignoreUnsupportedQuotaCurlOption(tokens, index)
				request.IgnoredOptions = append(request.IgnoredOptions, ignored)
				index = next
				continue
			}
			if request.URL == "" || (!isQuotaHTTPURL(request.URL) && isQuotaHTTPURL(token)) {
				request.URL = applyQuotaCurlPlaceholders(token, input)
			}
		}
	}
	request.URL = strings.TrimSpace(request.URL)
	if request.URL == "" {
		return nil, errors.New("quota curl missing URL")
	}
	if !isQuotaHTTPURL(request.URL) {
		return nil, errors.New("quota curl URL must be http or https")
	}
	if request.Method == "" {
		request.Method = http.MethodGet
	}
	return request, nil
}

func quotaCurlIgnoredOptionsHint(options []string) string {
	if len(options) == 0 {
		return ""
	}
	return "ignored unsupported curl options: " + strings.Join(uniqueQuotaStrings(options), ", ")
}

func applyQuotaCurlCookieHeader(request *quotaCurlRequest, value string) {
	cookie := strings.TrimSpace(value)
	if cookie == "" {
		return
	}
	key := findQuotaCurlHeaderKey(request.Headers, "Cookie")
	if key == "" {
		key = "Cookie"
	}
	if existing := strings.TrimSpace(request.Headers[key]); existing != "" {
		request.Headers[key] = existing + "; " + cookie
		return
	}
	request.Headers[key] = cookie
}

func findQuotaCurlHeaderKey(headers map[string]string, target string) string {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), target) {
			return key
		}
	}
	return ""
}

func ignoreUnsupportedQuotaCurlOption(tokens []string, index int) (int, string) {
	token := tokens[index]
	if strings.Contains(token, "=") || quotaCurlOptionHasNoSeparateValue(token) {
		return index, token
	}
	next := index + 1
	if next < len(tokens) && !strings.HasPrefix(tokens[next], "-") && quotaCurlOptionLikelyHasValue(token) {
		return next, token + " " + tokens[next]
	}
	return index, token
}

func quotaCurlOptionHasNoSeparateValue(token string) bool {
	switch token {
	case "-4", "--ipv4",
		"-6", "--ipv6",
		"-f", "--fail", "--fail-with-body",
		"-g", "--globoff",
		"-I", "--head",
		"-k", "--insecure",
		"-L", "--location",
		"-v", "--verbose",
		"--http1.0", "--http1.1", "--http2", "--http2-prior-knowledge", "--http3",
		"--no-progress-meter", "--progress-bar":
		return true
	default:
		return false
	}
}

func quotaCurlOptionLikelyHasValue(token string) bool {
	switch token {
	case "-A", "--user-agent",
		"-c", "--cookie-jar",
		"-e", "--referer",
		"-m", "--max-time",
		"-o", "--output",
		"-u", "--user",
		"-x", "--proxy",
		"--cacert", "--capath",
		"--cert", "--cert-type",
		"--connect-timeout", "--connect-to",
		"--dns-interface", "--dns-ipv4-addr", "--dns-ipv6-addr", "--dns-servers",
		"--form", "--form-string",
		"--interface",
		"--key", "--key-type",
		"--limit-rate",
		"--local-port",
		"--max-filesize",
		"--proto", "--proto-default", "--proto-redir",
		"--rate", "--request-target",
		"--resolve",
		"--retry", "--retry-delay", "--retry-max-time":
		return true
	default:
		return false
	}
}

func isQuotaHTTPURL(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

func uniqueQuotaStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		result = append(result, trimmed)
	}
	return result
}

func nextQuotaCurlValue(tokens []string, index int) (string, int, error) {
	next := index + 1
	if next >= len(tokens) {
		return "", index, fmt.Errorf("quota curl option missing value: %s", tokens[index])
	}
	return tokens[next], next, nil
}

func applyQuotaCurlPlaceholders(value string, input quotaCurlInput) string {
	variables := map[string]string{
		"apiKey":  strings.TrimSpace(input.APIKey),
		"baseUrl": normalizeQuotaBaseURL(input.BaseURL),
		"prefix":  normalizeQuotaPrefix(input.Prefix),
	}
	if cookie := strings.TrimSpace(input.PlatformCookie); cookie != "" {
		variables["platformCookie"] = cookie
	}
	for key, variableValue := range input.CurlVariables {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		variables[trimmedKey] = strings.TrimSpace(variableValue)
	}
	if _, ok := variables["platformCookie"]; !ok {
		variables["platformCookie"] = ""
	}
	for key, variableValue := range variables {
		value = strings.ReplaceAll(value, "{{"+key+"}}", variableValue)
	}
	return value
}

func containsUnsupportedQuotaShellOperator(value string) bool {
	inSingle := false
	inDouble := false
	escaped := false
	previous := rune(0)
	for _, r := range value {
		if escaped {
			escaped = false
			previous = r
			continue
		}
		if r == '\\' {
			escaped = true
			previous = r
			continue
		}
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '|', '>', '<', ';', '&', '`':
			if !inSingle && !inDouble {
				return true
			}
		case '(':
			if !inSingle && !inDouble && previous == '$' {
				return true
			}
		}
		previous = r
	}
	return false
}

func splitQuotaCurlCommand(value string) ([]string, error) {
	tokens := []string{}
	var builder strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	flush := func() {
		if builder.Len() == 0 {
			return
		}
		tokens = append(tokens, builder.String())
		builder.Reset()
	}
	for _, r := range value {
		if escaped {
			if r == '\n' || r == '\r' {
				escaped = false
				flush()
				continue
			}
			builder.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
				continue
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
				continue
			}
		case ' ', '\n', '\t', '\r':
			if !inSingle && !inDouble {
				flush()
				continue
			}
		}
		builder.WriteRune(r)
	}
	if escaped {
		builder.WriteRune('\\')
	}
	if inSingle || inDouble {
		return nil, errors.New("quota curl quote is not closed")
	}
	flush()
	return tokens, nil
}

func normalizeQuotaBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return strings.TrimRight(strings.ToLower(trimmed), "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.RawQuery = ""
	parsed.Fragment = ""
	normalized := strings.TrimRight(parsed.String(), "/")
	if normalized == "" {
		return strings.TrimRight(strings.ToLower(trimmed), "/")
	}
	return normalized
}

func normalizeQuotaPrefix(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), "/")
}

func buildQuotaResponseFromUsagePayload(body []byte, fallbackPlanType string) (quotaParsedResponse, error) {
	if quota := tryBuildXiaomiMiMoQuota(body, fallbackPlanType); quota != nil {
		return *quota, nil
	}

	var payload quotaUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		if billing := tryParseQuotaBillingResponse(body); billing != nil {
			return quotaParsedResponse{
				PlanType: fallbackPlanType,
				Windows:  []gettokenshooks.QuotaRuntimeWindow{},
				Billing:  billing,
			}, nil
		}
		return quotaParsedResponse{}, fmt.Errorf("codex quota response parse failed: %w", err)
	}

	quota := quotaParsedResponse{
		PlanType: normalizeQuotaPlanType(quotaFirstNonEmpty(payload.PlanType, payload.PlanTypeCamel, fallbackPlanType)),
		Windows:  buildQuotaWindows(&payload),
	}
	if billing := tryParseQuotaBillingResponse(body); billing != nil {
		quota.Billing = billing
	}
	return quota, nil
}

type quotaUsagePayload struct {
	PlanType             string                 `json:"plan_type"`
	PlanTypeCamel        string                 `json:"planType"`
	RateLimit            *quotaRateLimitInfo    `json:"rate_limit"`
	RateLimitCamel       *quotaRateLimitInfo    `json:"rateLimit"`
	CodeReviewRateLimit  *quotaRateLimitInfo    `json:"code_review_rate_limit"`
	CodeReviewRateCamel  *quotaRateLimitInfo    `json:"codeReviewRateLimit"`
	AdditionalRateLimits []quotaAdditionalLimit `json:"additional_rate_limits"`
	AdditionalRateCamel  []quotaAdditionalLimit `json:"additionalRateLimits"`
}

type quotaAdditionalLimit struct {
	LimitName         string              `json:"limit_name"`
	LimitNameCamel    string              `json:"limitName"`
	MeteredFeature    string              `json:"metered_feature"`
	MeteredFeatureCam string              `json:"meteredFeature"`
	RateLimit         *quotaRateLimitInfo `json:"rate_limit"`
	RateLimitCamel    *quotaRateLimitInfo `json:"rateLimit"`
}

type quotaRateLimitInfo struct {
	Allowed            *bool             `json:"allowed"`
	LimitReached       *bool             `json:"limit_reached"`
	LimitReachedCamel  *bool             `json:"limitReached"`
	PrimaryWindow      *quotaUsageWindow `json:"primary_window"`
	PrimaryWindowCamel *quotaUsageWindow `json:"primaryWindow"`
	SecondWindow       *quotaUsageWindow `json:"secondary_window"`
	SecondWindowCamel  *quotaUsageWindow `json:"secondaryWindow"`
}

type quotaUsageWindow struct {
	UsedPercent          interface{} `json:"used_percent"`
	UsedPercentCamel     interface{} `json:"usedPercent"`
	UsedTokens           interface{} `json:"used_tokens"`
	UsedTokensCamel      interface{} `json:"usedTokens"`
	LimitTokens          interface{} `json:"limit_tokens"`
	LimitTokensCamel     interface{} `json:"limitTokens"`
	RemainingTokens      interface{} `json:"remaining_tokens"`
	RemainingTokensCamel interface{} `json:"remainingTokens"`
	LimitWindowSeconds   interface{} `json:"limit_window_seconds"`
	LimitWindowCamel     interface{} `json:"limitWindowSeconds"`
	ResetAfterSeconds    interface{} `json:"reset_after_seconds"`
	ResetAfterSecondsCam interface{} `json:"resetAfterSeconds"`
	ResetAt              interface{} `json:"reset_at"`
	ResetAtCamel         interface{} `json:"resetAt"`
}

func buildQuotaWindows(payload *quotaUsagePayload) []gettokenshooks.QuotaRuntimeWindow {
	if payload == nil {
		return []gettokenshooks.QuotaRuntimeWindow{}
	}
	windows := make([]gettokenshooks.QuotaRuntimeWindow, 0, 6)
	addPair := func(prefix string, labelFive string, labelWeekly string, info *quotaRateLimitInfo) {
		fiveHourWindow, weeklyWindow := classifyQuotaWindows(info)
		limitReached := quotaBoolPtrValue(quotaInfoLimitReached(info))
		allowed := quotaInfoAllowedValue(info)
		if window := newQuotaWindow(quotaWindowID(prefix, "five-hour"), labelFive, fiveHourWindow, limitReached, allowed); window != nil {
			windows = append(windows, *window)
		}
		if window := newQuotaWindow(quotaWindowID(prefix, "weekly"), labelWeekly, weeklyWindow, limitReached, allowed); window != nil {
			windows = append(windows, *window)
		}
	}
	addPair("", "5H", "7D", firstQuotaRateLimit(payload.RateLimit, payload.RateLimitCamel))
	addPair("code-review", "CR 5H", "CR 7D", firstQuotaRateLimit(payload.CodeReviewRateLimit, payload.CodeReviewRateCamel))
	for index, limit := range firstQuotaAdditionalRateLimits(payload.AdditionalRateLimits, payload.AdditionalRateCamel) {
		name := quotaFirstNonEmpty(limit.LimitName, limit.LimitNameCamel, limit.MeteredFeature, limit.MeteredFeatureCam)
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("LIMIT %d", index+1)
		}
		addPair(
			fmt.Sprintf("additional-%d", index+1),
			fmt.Sprintf("%s 5H", name),
			fmt.Sprintf("%s 7D", name),
			firstQuotaRateLimit(limit.RateLimit, limit.RateLimitCamel),
		)
	}
	return windows
}

func quotaWindowID(prefix string, suffix string) string {
	if strings.TrimSpace(prefix) == "" {
		return suffix
	}
	return prefix + "-" + suffix
}

func newQuotaWindow(id string, label string, window *quotaUsageWindow, limitReached bool, allowed *bool) *gettokenshooks.QuotaRuntimeWindow {
	if window == nil {
		return nil
	}
	resetAtUnix := quotaResetUnixSeconds(window)
	resetLabel := formatQuotaResetLabel(window)
	usedPercent := quotaNumberValue(quotaFirstNonNil(window.UsedPercent, window.UsedPercentCamel))
	remainingPercent := remainingQuotaPercentFromUsed(usedPercent, limitReached, allowed, resetLabel)
	usedTokens, limitTokens, remainingTokens := quotaTokenProgressFromWindow(window)
	return &gettokenshooks.QuotaRuntimeWindow{
		ID:               id,
		Label:            label,
		RemainingPercent: remainingPercent,
		UsedTokens:       usedTokens,
		LimitTokens:      limitTokens,
		RemainingTokens:  remainingTokens,
		ResetLabel:       resetLabel,
		ResetAtUnix:      resetAtUnix,
	}
}

func quotaTokenProgressFromWindow(window *quotaUsageWindow) (*float64, *float64, *float64) {
	if window == nil {
		return nil, nil, nil
	}
	return normalizeQuotaTokenProgress(
		quotaNumberValue(quotaFirstNonNil(window.UsedTokens, window.UsedTokensCamel)),
		quotaNumberValue(quotaFirstNonNil(window.LimitTokens, window.LimitTokensCamel)),
		quotaNumberValue(quotaFirstNonNil(window.RemainingTokens, window.RemainingTokensCamel)),
	)
}

func normalizeQuotaTokenProgress(used *float64, limit *float64, remaining *float64) (*float64, *float64, *float64) {
	if used != nil && *used < 0 {
		used = nil
	}
	if limit != nil && *limit <= 0 {
		limit = nil
	}
	if remaining != nil && *remaining < 0 {
		remaining = nil
	}
	if remaining == nil && used != nil && limit != nil {
		value := clampQuotaNumber(*limit-*used, 0, *limit)
		remaining = &value
	}
	if used == nil && remaining != nil && limit != nil {
		value := clampQuotaNumber(*limit-*remaining, 0, *limit)
		used = &value
	}
	return used, limit, remaining
}

func classifyQuotaWindows(info *quotaRateLimitInfo) (*quotaUsageWindow, *quotaUsageWindow) {
	if info == nil {
		return nil, nil
	}
	primary := firstQuotaWindow(info.PrimaryWindow, info.PrimaryWindowCamel)
	secondary := firstQuotaWindow(info.SecondWindow, info.SecondWindowCamel)
	raw := []*quotaUsageWindow{primary, secondary}
	var fiveHourWindow *quotaUsageWindow
	var weeklyWindow *quotaUsageWindow
	for _, window := range raw {
		seconds := quotaWindowSeconds(window)
		switch seconds {
		case quotaFiveHourWindowSeconds:
			if fiveHourWindow == nil {
				fiveHourWindow = window
			}
		case quotaWeeklyWindowSeconds:
			if weeklyWindow == nil {
				weeklyWindow = window
			}
		}
	}
	if fiveHourWindow == nil && primary != nil && primary != weeklyWindow {
		fiveHourWindow = primary
	}
	if weeklyWindow == nil && secondary != nil && secondary != fiveHourWindow {
		weeklyWindow = secondary
	}
	return fiveHourWindow, weeklyWindow
}

func quotaInfoLimitReached(info *quotaRateLimitInfo) *bool {
	if info == nil {
		return nil
	}
	if info.LimitReached != nil {
		return info.LimitReached
	}
	return info.LimitReachedCamel
}

func quotaInfoAllowedValue(info *quotaRateLimitInfo) *bool {
	if info == nil {
		return nil
	}
	return info.Allowed
}

func firstQuotaRateLimit(primary *quotaRateLimitInfo, secondary *quotaRateLimitInfo) *quotaRateLimitInfo {
	if primary != nil {
		return primary
	}
	return secondary
}

func firstQuotaWindow(primary *quotaUsageWindow, secondary *quotaUsageWindow) *quotaUsageWindow {
	if primary != nil {
		return primary
	}
	return secondary
}

func firstQuotaAdditionalRateLimits(primary []quotaAdditionalLimit, secondary []quotaAdditionalLimit) []quotaAdditionalLimit {
	if len(primary) > 0 {
		return primary
	}
	return secondary
}

func formatQuotaResetLabel(window *quotaUsageWindow) string {
	resetAtUnix := quotaResetUnixSeconds(window)
	if resetAtUnix > 0 {
		return formatQuotaUnixSeconds(resetAtUnix)
	}
	return "-"
}

func quotaResetUnixSeconds(window *quotaUsageWindow) int64 {
	if window == nil {
		return 0
	}
	if resetAt := quotaNumberValue(quotaFirstNonNil(window.ResetAt, window.ResetAtCamel)); resetAt != nil && *resetAt > 0 {
		return int64(*resetAt)
	}
	if resetAfter := quotaNumberValue(quotaFirstNonNil(window.ResetAfterSeconds, window.ResetAfterSecondsCam)); resetAfter != nil && *resetAfter > 0 {
		return time.Now().Unix() + int64(*resetAfter)
	}
	return 0
}

func quotaWindowSeconds(window *quotaUsageWindow) int64 {
	if window == nil {
		return 0
	}
	value := quotaNumberValue(quotaFirstNonNil(window.LimitWindowSeconds, window.LimitWindowCamel))
	if value == nil {
		return 0
	}
	return int64(*value)
}

func remainingQuotaPercentFromUsed(usedPercent *float64, limitReached bool, allowed *bool, resetLabel string) *int {
	if usedPercent != nil {
		remaining := int(roundQuotaNumber(clampQuotaNumber(100-*usedPercent, 0, 100)))
		return &remaining
	}
	if limitReached || (allowed != nil && !*allowed) {
		if resetLabel == "-" {
			return nil
		}
		remaining := 0
		return &remaining
	}
	return nil
}

func tryBuildXiaomiMiMoQuota(body []byte, fallbackPlanType string) *quotaParsedResponse {
	var payload struct {
		Data struct {
			MonthUsage xiaomiMiMoUsageGroup `json:"monthUsage"`
			Usage      xiaomiMiMoUsageGroup `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	windows := make([]gettokenshooks.QuotaRuntimeWindow, 0, 2)
	if window := xiaomiMiMoQuotaWindow("mimo-plan-total-token", "PLAN", payload.Data.Usage, "plan_total_token"); window != nil {
		windows = append(windows, *window)
	}
	if window := xiaomiMiMoQuotaWindow("mimo-month-total-token", "MONTH", payload.Data.MonthUsage, "month_total_token"); window != nil {
		windows = append(windows, *window)
	}
	if len(windows) == 0 {
		return nil
	}
	return &quotaParsedResponse{
		PlanType: normalizeQuotaPlanType(quotaFirstNonEmpty(fallbackPlanType, "xiaomimimo")),
		Windows:  windows,
	}
}

type xiaomiMiMoUsageGroup struct {
	Percent interface{}           `json:"percent"`
	Items   []xiaomiMiMoUsageItem `json:"items"`
}

type xiaomiMiMoUsageItem struct {
	Name    string      `json:"name"`
	Used    interface{} `json:"used"`
	Limit   interface{} `json:"limit"`
	Percent interface{} `json:"percent"`
}

func xiaomiMiMoQuotaWindow(id string, label string, group xiaomiMiMoUsageGroup, preferredItemName string) *gettokenshooks.QuotaRuntimeWindow {
	item := xiaomiMiMoQuotaItem(group, preferredItemName)
	if item == nil {
		return nil
	}
	usedPercent := xiaomiMiMoUsedPercent(*item, group)
	if usedPercent == nil {
		return nil
	}
	remaining := int(roundQuotaNumber(clampQuotaNumber(100-*usedPercent, 0, 100)))
	usedTokens, limitTokens, remainingTokens := normalizeQuotaTokenProgress(
		quotaNumberValue(item.Used),
		quotaNumberValue(item.Limit),
		nil,
	)
	return &gettokenshooks.QuotaRuntimeWindow{
		ID:               id,
		Label:            label,
		RemainingPercent: &remaining,
		UsedTokens:       usedTokens,
		LimitTokens:      limitTokens,
		RemainingTokens:  remainingTokens,
		ResetLabel:       "-",
	}
}

func xiaomiMiMoQuotaItem(group xiaomiMiMoUsageGroup, preferredName string) *xiaomiMiMoUsageItem {
	for index := range group.Items {
		if strings.EqualFold(strings.TrimSpace(group.Items[index].Name), preferredName) {
			return &group.Items[index]
		}
	}
	for index := range group.Items {
		if xiaomiMiMoItemHasUsage(group.Items[index]) {
			return &group.Items[index]
		}
	}
	return nil
}

func xiaomiMiMoItemHasUsage(item xiaomiMiMoUsageItem) bool {
	for _, value := range []interface{}{item.Used, item.Limit, item.Percent} {
		if parsed := quotaNumberValue(value); parsed != nil && *parsed > 0 {
			return true
		}
	}
	return false
}

func xiaomiMiMoUsedPercent(item xiaomiMiMoUsageItem, group xiaomiMiMoUsageGroup) *float64 {
	if percent := quotaRatioOrPercentValue(item.Percent); percent != nil {
		return percent
	}
	if percent := quotaRatioOrPercentValue(group.Percent); percent != nil {
		return percent
	}
	used := quotaNumberValue(item.Used)
	limit := quotaNumberValue(item.Limit)
	if used == nil || limit == nil || *limit <= 0 {
		if percent := quotaRatioOrPercentValue(item.Percent); percent != nil {
			return percent
		}
		if percent := quotaRatioOrPercentValue(group.Percent); percent != nil {
			return percent
		}
		return nil
	}
	calculated := (*used / *limit) * 100
	return &calculated
}

func quotaRatioOrPercentValue(value interface{}) *float64 {
	percent := quotaNumberValue(value)
	if percent == nil {
		return nil
	}
	if *percent >= 0 && *percent <= 1 {
		normalized := *percent * 100
		return &normalized
	}
	return percent
}

func tryParseQuotaBillingResponse(body []byte) *gettokenshooks.QuotaRuntimeBilling {
	if billing := tryBuildNestedBalanceBilling(body); billing != nil {
		return billing
	}
	if billing := tryBuildDeepSeekBilling(body); billing != nil {
		return billing
	}
	var openRouter struct {
		Data struct {
			TotalCredits float64 `json:"total_credits"`
			TotalUsage   float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &openRouter); err == nil && openRouter.Data.TotalCredits > 0 {
		remaining := openRouter.Data.TotalCredits - openRouter.Data.TotalUsage
		return &gettokenshooks.QuotaRuntimeBilling{
			IsAvailable: true,
			BalanceInfos: []gettokenshooks.QuotaRuntimeBalanceInfo{{
				Currency:        "USD",
				TotalBalance:    fmt.Sprintf("%.2f", openRouter.Data.TotalCredits),
				GrantedBalance:  fmt.Sprintf("%.2f", remaining),
				ToppedUpBalance: fmt.Sprintf("%.2f", openRouter.Data.TotalCredits),
			}},
		}
	}
	var openAI struct {
		TotalGranted   float64 `json:"total_granted"`
		TotalUsed      float64 `json:"total_used"`
		TotalAvailable float64 `json:"total_available"`
		HardLimitUSD   float64 `json:"hard_limit_usd"`
		SoftLimitUSD   float64 `json:"soft_limit_usd"`
	}
	if err := json.Unmarshal(body, &openAI); err == nil && (openAI.TotalGranted > 0 || openAI.HardLimitUSD > 0) {
		granted := openAI.HardLimitUSD
		if granted == 0 {
			granted = openAI.SoftLimitUSD
		}
		if granted == 0 {
			granted = openAI.TotalGranted
		}
		return &gettokenshooks.QuotaRuntimeBilling{
			IsAvailable: true,
			BalanceInfos: []gettokenshooks.QuotaRuntimeBalanceInfo{{
				Currency:        "USD",
				TotalBalance:    fmt.Sprintf("%.2f", granted),
				GrantedBalance:  fmt.Sprintf("%.2f", openAI.TotalAvailable),
				ToppedUpBalance: fmt.Sprintf("%.2f", granted),
			}},
		}
	}
	var generic struct {
		TotalBalance     interface{} `json:"total_balance"`
		Balance          interface{} `json:"balance"`
		RemainingCredits interface{} `json:"remaining_credits"`
		Currency         string      `json:"currency"`
	}
	if err := json.Unmarshal(body, &generic); err == nil {
		balance := firstQuotaFloatFromInterface(generic.TotalBalance, generic.Balance, generic.RemainingCredits)
		if balance > 0 {
			currency := strings.TrimSpace(generic.Currency)
			if currency == "" {
				currency = "USD"
			}
			return &gettokenshooks.QuotaRuntimeBilling{
				IsAvailable: true,
				BalanceInfos: []gettokenshooks.QuotaRuntimeBalanceInfo{{
					Currency:        currency,
					TotalBalance:    fmt.Sprintf("%.2f", balance),
					GrantedBalance:  fmt.Sprintf("%.2f", balance),
					ToppedUpBalance: fmt.Sprintf("%.2f", balance),
				}},
			}
		}
	}
	return nil
}

func tryBuildNestedBalanceBilling(body []byte) *gettokenshooks.QuotaRuntimeBilling {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	data, ok := payload["data"].(map[string]interface{})
	if !ok {
		return nil
	}
	currency := quotaFirstNonEmpty(
		quotaMapStringValue(data, "currency"),
		quotaMapStringValue(data, "currency_code"),
		quotaMapStringValue(data, "currencyCode"),
	)
	total := quotaFirstNonEmpty(
		quotaBalanceStringValue(data, "total_balance"),
		quotaBalanceStringValue(data, "totalBalance"),
		quotaBalanceStringValue(data, "balance"),
		quotaBalanceStringValue(data, "remaining_credits"),
		quotaBalanceStringValue(data, "remainingCredits"),
	)
	granted := quotaFirstNonEmpty(
		quotaBalanceStringValue(data, "granted_balance"),
		quotaBalanceStringValue(data, "grantedBalance"),
		quotaBalanceStringValue(data, "gift_balance"),
		quotaBalanceStringValue(data, "giftBalance"),
	)
	toppedUp := quotaFirstNonEmpty(
		quotaBalanceStringValue(data, "topped_up_balance"),
		quotaBalanceStringValue(data, "toppedUpBalance"),
		quotaBalanceStringValue(data, "cash_balance"),
		quotaBalanceStringValue(data, "cashBalance"),
	)
	if currency == "" && total == "" && granted == "" && toppedUp == "" {
		return nil
	}
	if currency == "" {
		currency = "CNY"
	}
	return &gettokenshooks.QuotaRuntimeBilling{
		IsAvailable: true,
		BalanceInfos: []gettokenshooks.QuotaRuntimeBalanceInfo{{
			Currency:        currency,
			TotalBalance:    total,
			GrantedBalance:  granted,
			ToppedUpBalance: toppedUp,
		}},
	}
}

func quotaMapStringValue(root map[string]interface{}, key string) string {
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

func quotaBalanceStringValue(root map[string]interface{}, key string) string {
	if value := quotaMapStringValue(root, key); value != "" {
		return value
	}
	if value, ok := root[key]; ok {
		if parsed := quotaNumberValue(value); parsed != nil {
			return fmt.Sprintf("%.2f", *parsed)
		}
	}
	return ""
}

func tryBuildDeepSeekBilling(body []byte) *gettokenshooks.QuotaRuntimeBilling {
	var payload struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.BalanceInfos) == 0 {
		return nil
	}
	infos := make([]gettokenshooks.QuotaRuntimeBalanceInfo, 0, len(payload.BalanceInfos))
	for _, info := range payload.BalanceInfos {
		infos = append(infos, gettokenshooks.QuotaRuntimeBalanceInfo{
			Currency:        info.Currency,
			TotalBalance:    info.TotalBalance,
			GrantedBalance:  info.GrantedBalance,
			ToppedUpBalance: info.ToppedUpBalance,
		})
	}
	return &gettokenshooks.QuotaRuntimeBilling{IsAvailable: payload.IsAvailable, BalanceInfos: infos}
}

func firstQuotaFloatFromInterface(values ...interface{}) float64 {
	for _, value := range values {
		if parsed := quotaNumberValue(value); parsed != nil && *parsed > 0 {
			return *parsed
		}
	}
	return 0
}

func quotaNumberValue(value interface{}) *float64 {
	switch typed := value.(type) {
	case float64:
		return &typed
	case float32:
		converted := float64(typed)
		return &converted
	case int:
		converted := float64(typed)
		return &converted
	case int32:
		converted := float64(typed)
		return &converted
	case int64:
		converted := float64(typed)
		return &converted
	case json.Number:
		converted, err := typed.Float64()
		if err == nil {
			return &converted
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		converted, err := strconv.ParseFloat(trimmed, 64)
		if err == nil {
			return &converted
		}
	}
	return nil
}

func quotaFirstNonNil(values ...interface{}) interface{} {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func quotaFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func normalizeQuotaPlanType(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		return ""
	}
	replacer := strings.NewReplacer(" ", "", "-", "", "_", "")
	switch replacer.Replace(trimmed) {
	case "pro":
		return "pro"
	case "plus":
		return "plus"
	case "team":
		return "team"
	case "free":
		return "free"
	}
	return trimmed
}

func formatQuotaUnixSeconds(value int64) string {
	if value <= 0 {
		return "-"
	}
	return time.Unix(value, 0).Format("01/02 15:04")
}

func clampQuotaNumber(value float64, min float64, max float64) float64 {
	return math.Max(min, math.Min(max, value))
}

func roundQuotaNumber(value float64) float64 {
	return math.Round(value)
}

func quotaBoolPtrValue(value *bool) bool {
	return value != nil && *value
}

func decodeCurlVariablesJSON(value string, platformCookie string) map[string]string {
	variables := map[string]string{}
	if strings.TrimSpace(value) != "" {
		var decoded map[string]string
		if err := json.Unmarshal([]byte(value), &decoded); err == nil {
			for key, variableValue := range normalizeCurlVariables(decoded) {
				variables[key] = variableValue
			}
		}
	}
	if cookie := normalizePlatformCookie(platformCookie); cookie != "" {
		if _, ok := variables["platformCookie"]; !ok {
			variables["platformCookie"] = cookie
		}
	}
	if len(variables) == 0 {
		return nil
	}
	return variables
}

func normalizeCurlVariables(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		out[trimmedKey] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizePlatformCookie(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "Cookie:"))
}
