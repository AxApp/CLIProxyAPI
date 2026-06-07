package gettokenshooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestProjectCandidatePoolPolicyExactMatchStrictAllowsAccounts(t *testing.T) {
	writeProjectCandidatePoolPolicyConfig(t, `{
	  "version": 1,
	  "rules": [
	    {
	      "id": "rule-gettokens-codex",
	      "channel": "codex",
	      "projectKey": "workspace:abc",
	      "projectName": "GetTokens",
	      "projectKeySource": "codex-turn-workspace",
	      "projectKeyConfidence": "strong",
	      "enabled": true,
	      "allowAccountIDs": ["auth-b", "auth-a", "auth-missing"]
	    }
	  ]
	}`)

	result := gettokensrouting.NewEngine(projectCandidatePoolPolicy()).Route(context.Background(), gettokensrouting.RouteContext{
		Provider:         "codex",
		ProjectKey:       "workspace:abc",
		ProjectMatchKeys: []string{"workspace:abc"},
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a", Value: &coreauth.Auth{ID: "auth-a", Provider: "codex", Status: coreauth.StatusActive}},
			{ID: "auth-b", Value: &coreauth.Auth{ID: "auth-b", Provider: "codex", Status: coreauth.StatusActive}},
		},
	})

	assertRouteCandidateIDs(t, result.Candidates, []string{"auth-b", "auth-a"})
	assertProjectCandidatePoolTrace(t, result.Trace, true, "project-candidate-pool:matched")
	if len(result.Trace) != 1 || strings.Join(result.Trace[0].OrderIDs, ",") != "auth-b,auth-a,auth-missing" {
		t.Fatalf("project candidate pool order ids = %#v, want allowAccountIDs order", result.Trace)
	}
}

func TestProjectCandidatePoolPolicyNoProjectKeyIsNotEvaluated(t *testing.T) {
	writeProjectCandidatePoolPolicyConfig(t, `{
	  "version": 1,
	  "rules": [
	    {"id": "rule-gettokens-codex", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-b"]}
	  ]
	}`)

	result := gettokensrouting.NewEngine(projectCandidatePoolPolicy()).Route(context.Background(), gettokensrouting.RouteContext{
		Provider: "codex",
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})

	assertRouteCandidateIDs(t, result.Candidates, []string{"auth-a", "auth-b"})
	assertProjectCandidatePoolTrace(t, result.Trace, false, "project-candidate-pool:not-evaluated:no-project-key")
}

func TestProjectCandidatePoolPolicyAmbiguousProjectIsNotEvaluated(t *testing.T) {
	writeProjectCandidatePoolPolicyConfig(t, `{
	  "version": 1,
	  "rules": [
	    {"id": "rule-gettokens-codex", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-b"]}
	  ]
	}`)

	result := gettokensrouting.NewEngine(projectCandidatePoolPolicy()).Route(context.Background(), gettokensrouting.RouteContext{
		Provider:             "codex",
		ProjectKeySource:     "codex-turn-workspace",
		ProjectKeyConfidence: "ambiguous",
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})

	assertRouteCandidateIDs(t, result.Candidates, []string{"auth-a", "auth-b"})
	assertProjectCandidatePoolTrace(t, result.Trace, false, "project-candidate-pool:not-evaluated:ambiguous-project")
}

func TestProjectCandidatePoolPolicyNotMatchedKeepsChannelPool(t *testing.T) {
	writeProjectCandidatePoolPolicyConfig(t, `{
	  "version": 1,
	  "rules": [
	    {"id": "rule-gettokens-codex", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-b"]}
	  ]
	}`)

	result := gettokensrouting.NewEngine(projectCandidatePoolPolicy()).Route(context.Background(), gettokensrouting.RouteContext{
		Provider:         "codex",
		ProjectKey:       "workspace:abcd",
		ProjectMatchKeys: []string{"workspace:abcd"},
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})

	assertRouteCandidateIDs(t, result.Candidates, []string{"auth-a", "auth-b"})
	assertProjectCandidatePoolTrace(t, result.Trace, false, "project-candidate-pool:not-matched")
}

func TestProjectCandidatePoolPolicyNoRouteableAccountFailsClosed(t *testing.T) {
	writeProjectCandidatePoolPolicyConfig(t, `{
	  "version": 1,
	  "rules": [
	    {"id": "rule-gettokens-codex", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-missing"]}
	  ]
	}`)

	result := gettokensrouting.NewEngine(projectCandidatePoolPolicy()).Route(context.Background(), gettokensrouting.RouteContext{
		Provider:         "codex",
		ProjectKey:       "workspace:abc",
		ProjectMatchKeys: []string{"workspace:abc"},
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})

	assertRouteCandidateIDs(t, result.Candidates, []string{})
	assertProjectCandidatePoolTrace(t, result.Trace, true, "project-candidate-pool:no-routeable-account")
}

func TestProjectCandidatePoolPolicyConflictFailsClosed(t *testing.T) {
	writeProjectCandidatePoolPolicyConfig(t, `{
	  "version": 1,
	  "rules": [
	    {"id": "rule-a", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-a"]},
	    {"id": "rule-b", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-b"]}
	  ]
	}`)

	result := gettokensrouting.NewEngine(projectCandidatePoolPolicy()).Route(context.Background(), gettokensrouting.RouteContext{
		Provider:         "codex",
		ProjectKey:       "workspace:abc",
		ProjectMatchKeys: []string{"workspace:abc"},
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})

	assertRouteCandidateIDs(t, result.Candidates, []string{})
	assertProjectCandidatePoolTrace(t, result.Trace, true, "project-candidate-pool:conflict")
}

func TestProjectCandidatePoolPolicyUsesLoadedSnapshotUntilManagementRefresh(t *testing.T) {
	configPath := writeProjectCandidatePoolPolicyConfig(t, `{
	  "version": 1,
	  "rules": [
	    {"id": "rule-a", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-a"]}
	  ]
	}`)

	first := routeProjectCandidatePoolForTest()
	assertRouteCandidateIDs(t, first.Candidates, []string{"auth-a"})
	assertProjectCandidatePoolTrace(t, first.Trace, true, "project-candidate-pool:matched")

	if err := os.WriteFile(configPath, []byte(`{
	  "version": 1,
	  "rules": [
	    {"id": "rule-a", "channel": "codex", "projectKey": "workspace:abc", "enabled": true, "allowAccountIDs": ["auth-b"]}
	  ]
	}`), 0o600); err != nil {
		t.Fatalf("rewrite project candidate pool config: %v", err)
	}

	second := routeProjectCandidatePoolForTest()
	assertRouteCandidateIDs(t, second.Candidates, []string{"auth-a"})
	assertProjectCandidatePoolTrace(t, second.Trace, true, "project-candidate-pool:matched")

	router := gin.New()
	ConfigureProjectCandidatePoolRoutes(router.Group("/v0/management"), nil, nil)
	updateBody := `{
	  "channel": "codex",
	  "projectKey": "workspace:abc",
	  "enabled": true,
	  "allowAccountIDs": ["auth-b"]
	}`
	performProjectCandidatePoolRequest(t, router, http.MethodPut, "/v0/management/gettokens/project-candidate-pool-rules/rule-a", updateBody)

	third := routeProjectCandidatePoolForTest()
	assertRouteCandidateIDs(t, third.Candidates, []string{"auth-b"})
	assertProjectCandidatePoolTrace(t, third.Trace, true, "project-candidate-pool:matched")
}

func TestProjectCandidatePoolManagementRoutesCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setProjectCandidatePoolPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureProjectCandidatePoolRoutes(router.Group("/v0/management"), nil, nil)

	createBody := `{
	  "channel": " Codex ",
	  "projectKey": "workspace:abc",
	  "projectName": "GetTokens",
	  "projectKeySource": "codex-turn-workspace",
	  "projectKeyConfidence": "strong",
	  "enabled": true,
	  "allowAccountIDs": [" auth-a ", "auth-a", "auth-b"]
	}`
	created := performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/gettokens/project-candidate-pool-rules", createBody)
	var createResponse struct {
		Items []ProjectCandidatePoolRule `json:"items"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createResponse); err != nil {
		t.Fatalf("decode created rules: %v", err)
	}
	if len(createResponse.Items) != 1 {
		t.Fatalf("created rules = %#v, want one rule", createResponse.Items)
	}
	rule := createResponse.Items[0]
	if rule.ID == "" || rule.Channel != "codex" || rule.ProjectKey != "workspace:abc" {
		t.Fatalf("created rule = %#v, want normalized id/channel/project key", rule)
	}
	if len(rule.AllowAccountIDs) != 2 || rule.AllowAccountIDs[0] != "auth-a" || rule.AllowAccountIDs[1] != "auth-b" {
		t.Fatalf("allow account ids = %#v, want trimmed deduped ids", rule.AllowAccountIDs)
	}
	if rule.CreatedAt == "" || rule.UpdatedAt == "" {
		t.Fatalf("rule timestamps missing: %#v", rule)
	}

	listed := performProjectCandidatePoolRequest(t, router, http.MethodGet, "/v0/management/gettokens/project-candidate-pool-rules?channel=codex", "")
	var listResponse struct {
		Items []ProjectCandidatePoolRule `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listResponse); err != nil {
		t.Fatalf("decode listed rules: %v", err)
	}
	if len(listResponse.Items) != 1 || listResponse.Items[0].ID != rule.ID {
		t.Fatalf("listed rules = %#v, want created rule", listResponse.Items)
	}

	updateBody := `{
	  "channel": "codex",
	  "projectKey": "workspace:abc",
	  "projectName": "GetTokens",
	  "enabled": false,
	  "allowAccountIDs": []
	}`
	updated := performProjectCandidatePoolRequest(t, router, http.MethodPut, "/v0/management/gettokens/project-candidate-pool-rules/"+rule.ID, updateBody)
	var updateResponse struct {
		Items []ProjectCandidatePoolRule `json:"items"`
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &updateResponse); err != nil {
		t.Fatalf("decode updated rules: %v", err)
	}
	if len(updateResponse.Items) != 1 || updateResponse.Items[0].Enabled || len(updateResponse.Items[0].AllowAccountIDs) != 0 {
		t.Fatalf("updated rules = %#v, want disabled draft with empty allow set", updateResponse.Items)
	}

	deleted := performProjectCandidatePoolRequest(t, router, http.MethodDelete, "/v0/management/gettokens/project-candidate-pool-rules/"+rule.ID, "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", deleted.Code)
	}
	listedAfterDelete := performProjectCandidatePoolRequest(t, router, http.MethodGet, "/v0/management/gettokens/project-candidate-pool-rules", "")
	if err := json.Unmarshal(listedAfterDelete.Body.Bytes(), &listResponse); err != nil {
		t.Fatalf("decode rules after delete: %v", err)
	}
	if len(listResponse.Items) != 0 {
		t.Fatalf("rules after delete = %#v, want empty", listResponse.Items)
	}
}

func TestProjectCandidatePoolManagementRoutesRejectInvalidEnabledRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setProjectCandidatePoolPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureProjectCandidatePoolRoutes(router.Group("/v0/management"), nil, nil)

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "enabled without allow set",
			body: `{"channel":"codex","projectKey":"workspace:abc","enabled":true,"allowAccountIDs":[]}`,
			want: "allowAccountIDs must not be empty",
		},
		{
			name: "enabled with project name only",
			body: `{"channel":"codex","projectName":"GetTokens","enabled":true,"allowAccountIDs":["auth-a"]}`,
			want: "projectKey is required",
		},
		{
			name: "enabled with bare project key",
			body: `{"channel":"codex","projectKey":"gettokens","enabled":true,"allowAccountIDs":["auth-a"]}`,
			want: "projectKey must be source-prefixed",
		},
		{
			name: "unsupported channel",
			body: `{"channel":"unknown","projectKey":"workspace:abc","enabled":true,"allowAccountIDs":["auth-a"]}`,
			want: "unsupported channel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := performProjectCandidatePoolRawRequest(router, http.MethodPost, "/v0/management/gettokens/project-candidate-pool-rules", tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), tc.want) {
				t.Fatalf("body = %s, want %q", response.Body.String(), tc.want)
			}
		})
	}
}

func TestProjectCandidatePoolManagementRoutesRejectDuplicateEnabledRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setProjectCandidatePoolPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	router := gin.New()
	ConfigureProjectCandidatePoolRoutes(router.Group("/v0/management"), nil, nil)

	first := `{"id":"rule-a","channel":"codex","projectKey":"workspace:abc","enabled":true,"allowAccountIDs":["auth-a"]}`
	performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/gettokens/project-candidate-pool-rules", first)

	duplicate := `{"id":"rule-b","channel":"codex","projectKey":"workspace:abc","enabled":true,"allowAccountIDs":["auth-b"]}`
	response := performProjectCandidatePoolRawRequest(router, http.MethodPost, "/v0/management/gettokens/project-candidate-pool-rules", duplicate)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want duplicate rejection", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "duplicate enabled project candidate pool rule") {
		t.Fatalf("body = %s, want duplicate error", response.Body.String())
	}
}

func TestProjectCandidatePoolManagementRoutesBumpSessionAffinityPoolEpoch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setProjectCandidatePoolPolicyConfigPathForTest(t, filepath.Join(t.TempDir(), "profile", "config.yaml"))

	selector := coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback: &coreauth.FillFirstSelector{},
	})
	manager := coreauth.NewManager(nil, selector, nil)
	handler := &handlers.BaseAPIHandler{AuthManager: manager}

	router := gin.New()
	ConfigureProjectCandidatePoolRoutes(router.Group("/v0/management"), handler, nil)

	createBody := `{
	  "id": "rule-a",
	  "channel": "codex",
	  "projectKey": "workspace:abc",
	  "enabled": true,
	  "allowAccountIDs": ["auth-a"]
	}`
	before := selector.CurrentPoolEpoch()
	performProjectCandidatePoolRequest(t, router, http.MethodPost, "/v0/management/gettokens/project-candidate-pool-rules", createBody)
	if got := selector.CurrentPoolEpoch(); got != before+1 {
		t.Fatalf("pool epoch after create = %d, want %d", got, before+1)
	}

	updateBody := `{
	  "channel": "codex",
	  "projectKey": "workspace:abc",
	  "enabled": true,
	  "allowAccountIDs": ["auth-b"]
	}`
	before = selector.CurrentPoolEpoch()
	performProjectCandidatePoolRequest(t, router, http.MethodPut, "/v0/management/gettokens/project-candidate-pool-rules/rule-a", updateBody)
	if got := selector.CurrentPoolEpoch(); got != before+1 {
		t.Fatalf("pool epoch after update = %d, want %d", got, before+1)
	}

	before = selector.CurrentPoolEpoch()
	performProjectCandidatePoolRequest(t, router, http.MethodDelete, "/v0/management/gettokens/project-candidate-pool-rules/rule-a", "")
	if got := selector.CurrentPoolEpoch(); got != before+1 {
		t.Fatalf("pool epoch after delete = %d, want %d", got, before+1)
	}
}

func writeProjectCandidatePoolPolicyConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".config", "gettokens-data", "project-candidate-pool-rules", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir project candidate pool config: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write project candidate pool config: %v", err)
	}
	SetProjectCandidatePoolPolicyConfigPathFromConfig("")
	return configPath
}

func setProjectCandidatePoolPolicyConfigPathForTest(t *testing.T, configPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir project candidate pool profile dir: %v", err)
	}
	SetProjectCandidatePoolPolicyConfigPathFromConfig(configPath)
	t.Cleanup(func() {
		SetProjectCandidatePoolPolicyConfigPathFromConfig("")
	})
}

func performProjectCandidatePoolRequest(t *testing.T, router http.Handler, method string, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := performProjectCandidatePoolRawRequest(router, method, target, body)
	if response.Code < http.StatusOK || response.Code >= http.StatusMultipleChoices {
		t.Fatalf("%s %s returned %d: %s", method, target, response.Code, response.Body.String())
	}
	return response
}

func performProjectCandidatePoolRawRequest(router http.Handler, method string, target string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func assertRouteCandidateIDs(t *testing.T, candidates []gettokensrouting.RouteCandidate, want []string) {
	t.Helper()
	got := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		got = append(got, candidate.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("candidate ids = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidate ids = %#v, want %#v", got, want)
		}
	}
}

func assertProjectCandidatePoolTrace(t *testing.T, trace []gettokensrouting.DecisionStep, activated bool, reason string) {
	t.Helper()
	if len(trace) != 1 {
		t.Fatalf("trace len = %d, want 1: %#v", len(trace), trace)
	}
	step := trace[0]
	if step.Stage != gettokensrouting.PolicyStagePoolScope || step.Policy != "project-candidate-pool" {
		t.Fatalf("trace step = %#v, want project candidate pool step", step)
	}
	if step.Activated != activated {
		t.Fatalf("trace activated = %v, want %v: %#v", step.Activated, activated, step)
	}
	if step.Reason != reason {
		t.Fatalf("trace reason = %q, want %q", step.Reason, reason)
	}
}

func routeProjectCandidatePoolForTest() gettokensrouting.RouteResult {
	return gettokensrouting.NewEngine(projectCandidatePoolPolicy()).Route(context.Background(), gettokensrouting.RouteContext{
		Provider:         "codex",
		ProjectKey:       "workspace:abc",
		ProjectMatchKeys: []string{"workspace:abc"},
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})
}
