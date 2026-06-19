package gettokenshooks

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestChannelRoutingPolicySelectsBalancedAccountWithoutRoutingStrategy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".config", "gettokens-data", "channel-routing", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir channel routing config: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(`{
  "channels": {
    "codex": {
	      "channel": "codex",
	      "routeMode": "balanced",
	      "orderedAccountIDs": ["auth-a", "auth-b"],
	      "channelGroupStates": {},
	      "accountGroups": []
	    }
	  }
	}`), 0o600); err != nil {
		t.Fatalf("write channel routing config: %v", err)
	}

	restoreSessions := channelRoutingActiveSessionsByAuthID
	channelRoutingActiveSessionsByAuthID = func() map[string]int {
		return map[string]int{"auth-a": 7, "auth-b": 0}
	}
	t.Cleanup(func() {
		channelRoutingActiveSessionsByAuthID = restoreSessions
	})

	decision := rewriteChannelRoutingCandidates(context.Background(), gettokensrouting.RouteContext{
		Provider: "codex",
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a", Value: &coreauth.Auth{ID: "auth-a", Provider: "codex", Status: coreauth.StatusActive}},
			{ID: "auth-b", Value: &coreauth.Auth{ID: "auth-b", Provider: "codex", Status: coreauth.StatusActive}},
		},
	})

	if len(decision.OrderIDs) == 0 || decision.OrderIDs[0] != "auth-b" {
		t.Fatalf("OrderIDs = %#v, want auth-b first from balanced channel routing", decision.OrderIDs)
	}
	if decision.Reason != "channel-routing:codex:balanced" {
		t.Fatalf("Reason = %q, want channel-routing:codex:balanced", decision.Reason)
	}
}

func TestQuotaThresholdRuleManagementRoutesCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setChannelRoutingPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureQuotaThresholdRuleRoutes(router.Group("/v0/management"), nil, nil)

	createBody := "{\n" +
		"  \"account_key\":\" acct_quota_threshold_001 \",\n" +
		"  \"window_key\":\" tokens_5h \",\n" +
		"  \"metric\":\"remaining-percent\",\n" +
		"  \"threshold_percent\":20,\n" +
		"  \"enabled\":true\n" +
		"}"
	created := performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/gettokens/quota-threshold-rules", createBody)
	var rulesResponse struct {
		Items []AccountQuotaThresholdRule
	}
	if err := json.Unmarshal(created.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode created quota threshold rules: %v", err)
	}
	if len(rulesResponse.Items) != 1 || rulesResponse.Items[0].ID == "" {
		t.Fatalf("created rules = %#v, want one rule with id", rulesResponse.Items)
	}
	rule := rulesResponse.Items[0]
	if rule.AccountKey != "acct_quota_threshold_001" || rule.WindowKey != "tokens_5h" || rule.Comparator != "<=" || rule.ThresholdPercent != 20 {
		t.Fatalf("created rule = %#v, want normalized threshold rule", rule)
	}

	listed := performProjectCandidatePoolRequest(t, router, http.MethodGet, "/v0/management/gettokens/quota-threshold-rules?account_key=acct_quota_threshold_001", "")
	if err := json.Unmarshal(listed.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode listed quota threshold rules: %v", err)
	}
	if len(rulesResponse.Items) != 1 || rulesResponse.Items[0].ID != rule.ID {
		t.Fatalf("listed rules = %#v, want created rule", rulesResponse.Items)
	}

	updateBody := "{\n" +
		"  \"account_key\":\"acct_quota_threshold_001\",\n" +
		"  \"window_key\":\"tokens_5h\",\n" +
		"  \"metric\":\"used-percent\",\n" +
		"  \"threshold_percent\":85,\n" +
		"  \"enabled\":true\n" +
		"}"
	updated := performProjectCandidatePoolRequest(t, router, http.MethodPut, "/v0/management/gettokens/quota-threshold-rules/"+rule.ID, updateBody)
	if err := json.Unmarshal(updated.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode updated quota threshold rules: %v", err)
	}
	if len(rulesResponse.Items) != 1 || rulesResponse.Items[0].Metric != "used-percent" || rulesResponse.Items[0].Comparator != ">=" || rulesResponse.Items[0].ThresholdPercent != 85 {
		t.Fatalf("updated rules = %#v, want used-percent >= 85", rulesResponse.Items)
	}

	loaded := loadQuotaThresholdRulesFromChannelRoutingConfig()
	if len(loaded) != 1 || loaded[0].ID != rule.ID || loaded[0].Metric != "used-percent" {
		t.Fatalf("loaded quota threshold rules = %#v, want persisted updated rule", loaded)
	}

	deleted := performProjectCandidatePoolRequest(t, router, http.MethodDelete, "/v0/management/gettokens/quota-threshold-rules/"+rule.ID, "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", deleted.Code)
	}
	listedAfterDelete := performProjectCandidatePoolRequest(t, router, http.MethodGet, "/v0/management/gettokens/quota-threshold-rules", "")
	if err := json.Unmarshal(listedAfterDelete.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode quota threshold rules after delete: %v", err)
	}
	if len(rulesResponse.Items) != 0 {
		t.Fatalf("rules after delete = %#v, want empty", rulesResponse.Items)
	}
}

func TestLoadQuotaThresholdRulesKeepsConditionOnlyRuntimeRule(t *testing.T) {
	configPath := writeRouteGuardChannelRoutingConfig(t, "{\n"+
		"\t\t\"channels\": {},\n"+
		"\t\t\"quotaThresholdRules\": [{\n"+
		"\t\t\t\"id\": \"condition-only-stop\",\n"+
		"\t\t\t\"accountKey\": \"acct_condition_only\",\n"+
		"\t\t\t\"condition\": {\"all\": [\n"+
		"\t\t\t\t{\"fact\":\"quota.window\",\"window_key\":\"tokens_1d\",\"metric\":\"remaining-percent\",\"comparator\":\"<=\",\"value\":10},\n"+
		"\t\t\t\t{\"fact\":\"quota.window\",\"window_key\":\"tokens_7d\",\"metric\":\"used-percent\",\"comparator\":\">=\",\"value\":70}\n"+
		"\t\t\t]},\n"+
		"\t\t\t\"enabled\": true\n"+
		"\t\t}]\n"+
		"\t}")
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	loaded := loadQuotaThresholdRulesFromChannelRoutingConfig()
	if len(loaded) != 1 || loaded[0].ID != "condition-only-stop" || loaded[0].Condition == nil {
		t.Fatalf("loaded rules = %#v, want condition-only rule preserved", loaded)
	}
}

func TestQuotaThresholdRuleManagementRoutesRejectInvalidRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setChannelRoutingPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureQuotaThresholdRuleRoutes(router.Group("/v0/management"), nil, nil)

	invalidBody := "{\n" +
		"  \"account_key\":\"acct_quota_threshold_001\",\n" +
		"  \"window_key\":\"tokens_5h\",\n" +
		"  \"metric\":\"remaining-percent\",\n" +
		"  \"threshold_percent\":120,\n" +
		"  \"enabled\":true\n" +
		"}"
	response := performProjectCandidatePoolRawRequest(router, http.MethodPost, "/v0/management/gettokens/quota-threshold-rules", invalidBody)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid threshold status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

func TestQuotaThresholdRuleManagementRoutesRejectConflictingEnabledRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setChannelRoutingPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureQuotaThresholdRuleRoutes(router.Group("/v0/management"), nil, nil)
	body := "{\n" +
		"  \"account_key\":\"acct_quota_threshold_conflict\",\n" +
		"  \"window_key\":\"tokens_5h\",\n" +
		"  \"metric\":\"remaining-percent\",\n" +
		"  \"threshold_percent\":20,\n" +
		"  \"enabled\":true\n" +
		"}"
	performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/gettokens/quota-threshold-rules", body)
	conflict := performProjectCandidatePoolRawRequest(router, http.MethodPost, "/v0/management/gettokens/quota-threshold-rules", body)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d body=%s, want 409", conflict.Code, conflict.Body.String())
	}
	if !strings.Contains(conflict.Body.String(), "conflicts") {
		t.Fatalf("conflict body = %s, want conflicts payload", conflict.Body.String())
	}
}

func TestQuotaThresholdRuleManagementRoutesRefreshRuntimeGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setChannelRoutingPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))
	guard := NewAccountRouteGuardStore()
	store := NewQuotaRuntimeStore(guard)
	previousDefaultStore := defaultQuotaRuntimeStore
	defaultQuotaRuntimeStore = store
	t.Cleanup(func() {
		defaultQuotaRuntimeStore = previousDefaultStore
	})

	router := gin.New()
	ConfigureQuotaThresholdRuleRoutes(router.Group("/v0/management"), nil, nil)
	now := quotaThresholdRuleTestNow()
	accountKey := "acct_00000000-0000-4000-8000-000000000125"
	remainingTokens := 18.0
	limitTokens := 100.0
	state, err := store.Upsert(QuotaRuntimeState{
		AccountKey: accountKey,
		Status:     QuotaRuntimeStatusSuccess,
		Windows: []QuotaRuntimeWindow{{
			ID:              "tokens_5h",
			RemainingTokens: &remainingTokens,
			LimitTokens:     &limitTokens,
			ResetAtUnix:     now.Add(2 * time.Hour).Unix(),
		}},
	}, now)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if state.Blocked {
		t.Fatalf("state = %#v, want no block before threshold rule exists", state)
	}

	createBody := "{\n" +
		"  \"account_key\":\"" + accountKey + "\",\n" +
		"  \"window_key\":\"tokens_5h\",\n" +
		"  \"metric\":\"remaining-percent\",\n" +
		"  \"threshold_percent\":20,\n" +
		"  \"enabled\":true\n" +
		"}"
	created := performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/gettokens/quota-threshold-rules", createBody)
	var rulesResponse struct {
		Items []AccountQuotaThresholdRule
	}
	if err := json.Unmarshal(created.Body.Bytes(), &rulesResponse); err != nil {
		t.Fatalf("decode created quota threshold rules: %v", err)
	}
	if len(rulesResponse.Items) != 1 {
		t.Fatalf("created rules = %#v, want one rule", rulesResponse.Items)
	}
	if got := guard.DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         "auth-threshold-refresh",
		Provider:   "codex",
		AccountKey: accountKey,
	}}); len(got) != 1 || got[0] != "auth-threshold-refresh" {
		t.Fatalf("guard deny ids = %#v, want quota-threshold deny immediately after create", got)
	}
	afterCreate, ok := store.StateForAccount(accountKey)
	if !ok || !afterCreate.Blocked || len(afterCreate.Sources) != 1 || afterCreate.Sources[0].Source != AccountRouteGuardSourceQuotaThreshold {
		t.Fatalf("state after create = %#v, want immediate quota-threshold block", afterCreate)
	}

	deleted := performProjectCandidatePoolRequest(t, router, http.MethodDelete, "/v0/management/gettokens/quota-threshold-rules/"+rulesResponse.Items[0].ID, "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", deleted.Code, deleted.Body.String())
	}
	if got := guard.DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         "auth-threshold-refresh",
		Provider:   "codex",
		AccountKey: accountKey,
	}}); len(got) != 0 {
		t.Fatalf("guard deny ids = %#v, want quota-threshold cleared immediately after delete", got)
	}
	afterDelete, ok := store.StateForAccount(accountKey)
	if !ok || afterDelete.Blocked {
		t.Fatalf("state after delete = %#v, want immediate quota-threshold recovery", afterDelete)
	}
}

func TestRouteGuardRuleSimulationRouteUsesPersistedQuotaThresholdRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setChannelRoutingPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureQuotaThresholdRuleRoutes(router.Group("/v0/management"), nil, nil)
	createBody := "{\n" +
		"  \"id\":\"rule_quota_001\",\n" +
		"  \"account_key\":\"acct_sim_api\",\n" +
		"  \"condition\":{\n" +
		"    \"all\":[\n" +
		"      {\"fact\":\"quota.window\",\"window_key\":\"tokens_5h\",\"metric\":\"remaining-percent\",\"comparator\":\"<=\",\"value\":20},\n" +
		"      {\"fact\":\"quota.window\",\"window_key\":\"tokens_5h\",\"metric\":\"used-percent\",\"comparator\":\">=\",\"value\":80}\n" +
		"    ]\n" +
		"  },\n" +
		"  \"enabled\":true\n" +
		"}"
	performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/gettokens/quota-threshold-rules", createBody)

	simulateBody := "{\n" +
		"  \"ruleIds\":[\"rule_quota_001\"],\n" +
		"  \"facts\":{\n" +
		"    \"now\":\"2026-06-19T10:00:00Z\",\n" +
		"    \"request\":{\"channel\":\"api\",\"model\":\"gpt-4.1\",\"project\":\"default\"},\n" +
		"    \"accounts\":[{\n" +
		"      \"accountId\":\"acct_sim_api\",\n" +
		"      \"quotaWindow\":{\n" +
		"        \"windowId\":\"tokens_5h\",\n" +
		"        \"startsAt\":\"2026-06-19T08:00:00Z\",\n" +
		"        \"endsAt\":\"2026-06-19T12:00:00Z\",\n" +
		"        \"observedUsed\":82,\n" +
		"        \"observedLimit\":100,\n" +
		"        \"observedRemaining\":18,\n" +
		"        \"status\":\"fresh\"\n" +
		"      },\n" +
		"      \"calibrationLedger\":[]\n" +
		"    }]\n" +
		"  }\n" +
		"}"
	response := performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/route-guard/rules/simulate", simulateBody)
	var result SimulationResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode simulation response: %v body=%s", err, response.Body.String())
	}
	if result.Summary.BlockedAccounts != 1 || len(result.Accounts) != 1 {
		t.Fatalf("simulation result = %#v, want one blocked account", result)
	}
	account := result.Accounts[0]
	if !account.Decision.Denied || account.Decision.DenySource != AccountRouteGuardSourceQuotaThreshold {
		t.Fatalf("decision = %#v, want quota-threshold deny", account.Decision)
	}
	if account.MatchedRule == nil || account.MatchedRule.RuleID != "rule_quota_001" {
		t.Fatalf("matchedRule = %#v, want persisted rule id", account.MatchedRule)
	}
	if !reasonTraceContains(account.ReasonTrace, "quota.metric.compared") || !reasonTraceContains(account.ReasonTrace, "routeguard.action.block") {
		t.Fatalf("reasonTrace = %#v, want comparison and block trace", account.ReasonTrace)
	}
}

func TestRouteGuardRuleSimulationRouteRejectsUnknownRuleID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setChannelRoutingPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureQuotaThresholdRuleRoutes(router.Group("/v0/management"), nil, nil)
	body := `{"ruleIds":["missing_rule"],"facts":{"now":"2026-06-19T10:00:00Z","accounts":[]}}`
	response := performProjectCandidatePoolRawRequest(router, http.MethodPost, "/v0/management/route-guard/rules/simulate", body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
	}
}

func TestRouteGuardRuleSimulationRouteAcceptsDraftQuotaThresholdRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setChannelRoutingPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureQuotaThresholdRuleRoutes(router.Group("/v0/management"), nil, nil)
	body := "{\"rule\":{\"account_key\":\"acct_draft_sim\",\"window_key\":\"tokens_5h\",\"metric\":\"remaining-percent\",\"threshold_percent\":20,\"enabled\":true},\"facts\":{\"now\":\"2026-06-19T10:00:00Z\",\"accounts\":[{\"accountId\":\"acct_draft_sim\",\"quotaWindow\":{\"windowId\":\"tokens_5h\",\"startsAt\":\"2026-06-19T08:00:00Z\",\"endsAt\":\"2026-06-19T12:00:00Z\",\"observedUsed\":82,\"observedLimit\":100,\"observedRemaining\":18,\"status\":\"fresh\"}}]}}"
	response := performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/route-guard/rules/simulate", body)
	var result SimulationResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode simulation response: %v body=%s", err, response.Body.String())
	}
	if result.Summary.BlockedAccounts != 1 || len(result.Accounts) != 1 || result.Accounts[0].MatchedRule == nil || result.Accounts[0].MatchedRule.RuleID != "simulation-draft-1" {
		t.Fatalf("result = %#v, want draft rule block", result)
	}
	if persisted := loadQuotaThresholdRulesFromChannelRoutingConfig(); len(persisted) != 0 {
		t.Fatalf("persisted rules = %#v, want draft simulation to avoid writes", persisted)
	}
}

func quotaThresholdRuleTestNow() time.Time {
	return time.Now().UTC().Truncate(time.Hour).Add(24 * time.Hour)
}

func setChannelRoutingPolicyConfigPathForTest(t *testing.T, configPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir channel routing profile dir: %v", err)
	}
	SetChannelRoutingPolicyConfigPathFromConfig(configPath)
	t.Cleanup(func() {
		SetChannelRoutingPolicyConfigPathFromConfig("")
	})
}
