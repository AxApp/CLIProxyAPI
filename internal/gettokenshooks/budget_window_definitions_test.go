package gettokenshooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestBudgetWindowDefinitionManagementRoutesCRUDAndSoftDisable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setBudgetWindowDefinitionPathForTest(t, filepath.Join(t.TempDir(), "profile", "budget-window-definitions", "config.json"))
	router := gin.New()
	configureBudgetWindowDefinitionRoutes(router.Group("/v0/management"), newBudgetWindowFactsTestStore(t))

	createBody := `{
		"id": "tokens_daily",
		"kind": "daily",
		"metric": "tokens",
		"limit": 100,
		"timezone": "Asia/Shanghai",
		"enabled": true
	}`
	created := performBudgetWindowDefinitionRequest(t, router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", createBody)
	var response struct {
		Items []BudgetWindowDefinition `json:"items"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "tokens_daily" || !response.Items[0].Enabled {
		t.Fatalf("created items = %#v, want enabled tokens_daily", response.Items)
	}

	updateBody := `{
		"id": "tokens_daily",
		"kind": "multi-day",
		"semantics": "calendar",
		"days": 7,
		"metric": "tokens",
		"limit": 500,
		"timezone": "Asia/Shanghai",
		"enabled": true
	}`
	updated := performBudgetWindowDefinitionRequest(t, router, http.MethodPut, "/v0/management/gettokens/budget-window-definitions/tokens_daily", updateBody)
	if err := json.Unmarshal(updated.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].Kind != BudgetWindowKindMultiDay || response.Items[0].Days != 7 || response.Items[0].Limit != 500 {
		t.Fatalf("updated items = %#v, want multi-day calendar", response.Items)
	}

	immutable := performBudgetWindowDefinitionRawRequest(router, http.MethodPut, "/v0/management/gettokens/budget-window-definitions/tokens_daily", strings.Replace(updateBody, `"tokens_daily"`, `"other_window"`, 1))
	if immutable.Code != http.StatusBadRequest {
		t.Fatalf("immutable id status = %d body=%s, want 400", immutable.Code, immutable.Body.String())
	}

	deleted := performBudgetWindowDefinitionRequest(t, router, http.MethodDelete, "/v0/management/gettokens/budget-window-definitions/tokens_daily", "")
	if err := json.Unmarshal(deleted.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].Enabled {
		t.Fatalf("deleted items = %#v, want soft-disabled definition retained", response.Items)
	}
}

func TestBudgetWindowDefinitionManagementRoutesRejectInvalidDefinition(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setBudgetWindowDefinitionPathForTest(t, filepath.Join(t.TempDir(), "profile", "budget-window-definitions", "config.json"))
	router := gin.New()
	configureBudgetWindowDefinitionRoutes(router.Group("/v0/management"), newBudgetWindowFactsTestStore(t))

	invalidTimezone := performBudgetWindowDefinitionRawRequest(router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", `{
		"id": "bad_tz",
		"kind": "daily",
		"metric": "tokens",
		"limit": 100,
		"timezone": "Mars/Olympus",
		"enabled": true
	}`)
	if invalidTimezone.Code != http.StatusBadRequest {
		t.Fatalf("invalid timezone status = %d body=%s, want 400", invalidTimezone.Code, invalidTimezone.Body.String())
	}

	invalidBounded := performBudgetWindowDefinitionRawRequest(router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", `{
		"id": "bad_bounded",
		"kind": "bounded",
		"metric": "tokens",
		"limit": 100,
		"startsAt": "2026-06-19T10:00:00Z",
		"endsAt": "2026-06-19T09:00:00Z",
		"enabled": true
	}`)
	if invalidBounded.Code != http.StatusBadRequest {
		t.Fatalf("invalid bounded status = %d body=%s, want 400", invalidBounded.Code, invalidBounded.Body.String())
	}

	invalidLimit := performBudgetWindowDefinitionRawRequest(router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", `{
		"id": "bad_limit",
		"kind": "daily",
		"metric": "tokens",
		"limit": 0,
		"timezone": "Asia/Shanghai",
		"enabled": true
	}`)
	if invalidLimit.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d body=%s, want 400", invalidLimit.Code, invalidLimit.Body.String())
	}

	cronKind := performBudgetWindowDefinitionRawRequest(router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", `{
		"id": "workday_cron",
		"kind": "cron",
		"metric": "tokens",
		"limit": 100,
		"timezone": "Asia/Shanghai",
		"enabled": true
	}`)
	if cronKind.Code != http.StatusBadRequest {
		t.Fatalf("cron kind status = %d body=%s, want 400", cronKind.Code, cronKind.Body.String())
	}

	rollingSemantics := performBudgetWindowDefinitionRawRequest(router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", `{
		"id": "rolling_7d",
		"kind": "multi-day",
		"semantics": "rolling",
		"days": 7,
		"metric": "tokens",
		"limit": 100,
		"timezone": "Asia/Shanghai",
		"enabled": true
	}`)
	if rollingSemantics.Code != http.StatusBadRequest {
		t.Fatalf("rolling semantics status = %d body=%s, want 400", rollingSemantics.Code, rollingSemantics.Body.String())
	}
}

func TestBudgetWindowDefinitionPreviewUsesStoredDefinitionsAndUsageAggregator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setBudgetWindowDefinitionPathForTest(t, filepath.Join(t.TempDir(), "profile", "budget-window-definitions", "config.json"))
	store := newBudgetWindowFactsTestStore(t)
	router := gin.New()
	configureBudgetWindowDefinitionRoutes(router.Group("/v0/management"), store)
	accountKey := "acct_preview_budget_window"
	now := time.Date(2026, 6, 19, 16, 30, 0, 0, time.UTC)
	windowStart := time.Date(2026, 6, 19, 16, 0, 0, 0, time.UTC)
	insertBudgetWindowUsageEvent(t, store, "preview-usage", accountKey, windowStart.Add(10*time.Minute), 70, false)
	insertBudgetWindowUsageEvent(t, store, "preview-failed", accountKey, windowStart.Add(20*time.Minute), 100, true)

	performBudgetWindowDefinitionRequest(t, router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", `{
		"id": "tokens_daily",
		"kind": "daily",
		"metric": "tokens",
		"limit": 100,
		"timezone": "Asia/Shanghai",
		"enabled": true
	}`)
	performBudgetWindowDefinitionRequest(t, router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions", `{
		"id": "disabled_daily",
		"kind": "daily",
		"metric": "tokens",
		"limit": 100,
		"timezone": "Asia/Shanghai",
		"enabled": false
	}`)

	previewBody := `{
		"account_key": "` + accountKey + `",
		"now": "` + now.Format(time.RFC3339) + `",
		"calibrations": [{
			"id": "external-delta-10",
			"account_key": "` + accountKey + `",
			"window_key": "tokens_daily",
			"metric": "tokens",
			"mode": "delta",
			"value": 10,
			"created_at": "` + windowStart.Add(30*time.Minute).Format(time.RFC3339) + `"
		}]
	}`
	preview := performBudgetWindowDefinitionRequest(t, router, http.MethodPost, "/v0/management/gettokens/budget-window-definitions/preview", previewBody)
	var response struct {
		Items []QuotaWindowFacts `json:"items"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if len(response.Items) != 1 {
		t.Fatalf("preview items = %#v, want one enabled definition fact", response.Items)
	}
	fact := response.Items[0]
	if fact.WindowID != "tokens_daily" || fact.RawUsed != 70 || fact.CalibrationDelta != 10 || fact.ObservedUsed != 80 || fact.ObservedRemaining != 20 {
		t.Fatalf("preview fact = %#v, want committed usage plus calibration", fact)
	}
}

func setBudgetWindowDefinitionPathForTest(t *testing.T, path string) {
	t.Helper()
	budgetWindowDefinitionPathState.Lock()
	previous := budgetWindowDefinitionPathState.path
	budgetWindowDefinitionPathState.path = path
	budgetWindowDefinitionPathState.Unlock()
	t.Cleanup(func() {
		budgetWindowDefinitionPathState.Lock()
		budgetWindowDefinitionPathState.path = previous
		budgetWindowDefinitionPathState.Unlock()
	})
}

func performBudgetWindowDefinitionRequest(t *testing.T, router *gin.Engine, method string, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := performBudgetWindowDefinitionRawRequest(router, method, path, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s %s status = %d body=%s, want 200", method, path, recorder.Code, recorder.Body.String())
	}
	return recorder
}

func performBudgetWindowDefinitionRawRequest(router *gin.Engine, method string, path string, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	router.ServeHTTP(recorder, request)
	return recorder
}
