package gettokenshooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestDoctorDiagnosticsReturnsNotReadyWithoutFacts(t *testing.T) {
	response := BuildDoctorDiagnosticsSnapshot(DoctorDiagnosticsOptions{
		QuotaStore: NewQuotaRuntimeStore(NewAccountRouteGuardStore()),
		GuardStore: NewAccountRouteGuardStore(),
		Now:        time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC),
	})

	if response.Authority != "sidecar" || response.Source != "sidecar-diagnostics" {
		t.Fatalf("response source = %#v, want sidecar diagnostics", response)
	}
	if response.Summary.Status != DoctorDiagnosticStatusNotReady {
		t.Fatalf("summary = %#v, want not_ready without facts", response.Summary)
	}
	if len(response.Checks) != 2 {
		t.Fatalf("checks = %#v, want route guard and quota fact checks", response.Checks)
	}
	for _, check := range response.Checks {
		if check.Status != DoctorDiagnosticStatusNotReady {
			t.Fatalf("check = %#v, want not_ready without evidence", check)
		}
		if len(check.Evidence) != 0 {
			t.Fatalf("check evidence = %#v, want empty", check.Evidence)
		}
	}
}

func TestDoctorDiagnosticsIncludesRouteDroppedReasonEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	guard := NewAccountRouteGuardStore()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "codex-auth-route",
		AccountKey: "acct_00000000-0000-4000-8000-000000000401",
		Provider:   "codex",
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	now := time.Now().UTC()
	guard.MarkBlocked(AccountRouteGuardBlock{
		Source:       AccountRouteGuardSourceUpstreamRateLimit,
		FailureScope: RouteResilienceScopeAccount,
		AuthID:       auth.ID,
		AccountKey:   auth.AccountKey,
		Reason:       "upstream 429 active cooldown",
		ExpiresAt:    now.Add(time.Hour),
		UpdatedAt:    now,
	})

	response := BuildDoctorDiagnosticsSnapshot(DoctorDiagnosticsOptions{
		QuotaStore: NewQuotaRuntimeStore(guard),
		GuardStore: guard,
		Handler:    &handlers.BaseAPIHandler{AuthManager: manager},
		Now:        now,
	})

	check := findDoctorDiagnosticCheck(response, DoctorDiagnosticCheckRouteGuard)
	if check == nil {
		t.Fatalf("checks = %#v, want route guard check", response.Checks)
	}
	if check.Status != DoctorDiagnosticStatusWarning {
		t.Fatalf("route check = %#v, want warning with active dropped reason", check)
	}
	if len(check.Evidence) != 1 {
		t.Fatalf("route evidence = %#v, want one dropped reason", check.Evidence)
	}
	evidence := check.Evidence[0]
	if evidence.Kind != DoctorDiagnosticEvidenceRouteDroppedReason ||
		evidence.AccountKey != auth.AccountKey ||
		evidence.AuthID != auth.ID ||
		evidence.Source != AccountRouteGuardSourceUpstreamRateLimit ||
		evidence.Reason != "upstream 429 active cooldown" {
		t.Fatalf("route evidence = %#v, want seeded dropped reason", evidence)
	}
	if evidence.DroppedReason == nil || evidence.DroppedReason.Source != AccountRouteGuardSourceUpstreamRateLimit {
		t.Fatalf("dropped reason = %#v, want embedded channel routing dropped reason", evidence.DroppedReason)
	}
}

func TestDoctorDiagnosticsIncludesQuotaFactEvidence(t *testing.T) {
	guard := NewAccountRouteGuardStore()
	store := NewQuotaRuntimeStore(guard)
	now := time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC)
	accountKey := "acct_00000000-0000-4000-8000-000000000402"
	_, err := store.Upsert(QuotaRuntimeState{
		AccountKey: accountKey,
		Status:     QuotaRuntimeStatusError,
		Source:     "quota-curl",
		Windows:    []QuotaRuntimeWindow{},
		Fact: &QuotaRuntimeFact{
			State:        QuotaFactStateDenied,
			Source:       "quota-curl",
			Freshness:    QuotaFactFreshnessFresh,
			Confidence:   QuotaFactConfidenceHigh,
			Risk:         QuotaFactRiskDenied,
			Explanation:  "Provider denied quota check: Authorization: Bearer sk-secret-12345",
			ObservedAt:   now.Format(time.RFC3339),
			EvidenceRefs: []string{"quota-status:" + accountKey, "quota-source:quota-curl"},
		},
	}, now)
	if err != nil {
		t.Fatalf("Upsert quota: %v", err)
	}

	response := BuildDoctorDiagnosticsSnapshot(DoctorDiagnosticsOptions{
		QuotaStore: store,
		GuardStore: guard,
		Now:        now,
	})

	check := findDoctorDiagnosticCheck(response, DoctorDiagnosticCheckQuotaFacts)
	if check == nil {
		t.Fatalf("checks = %#v, want quota fact check", response.Checks)
	}
	if check.Status != DoctorDiagnosticStatusWarning {
		t.Fatalf("quota check = %#v, want warning for denied fact", check)
	}
	if len(check.Evidence) != 1 {
		t.Fatalf("quota evidence = %#v, want one fact", check.Evidence)
	}
	evidence := check.Evidence[0]
	if evidence.Kind != DoctorDiagnosticEvidenceQuotaFact ||
		evidence.AccountKey != accountKey ||
		evidence.State != QuotaFactStateDenied ||
		evidence.Source != "quota-curl" ||
		evidence.Freshness != QuotaFactFreshnessFresh ||
		evidence.Risk != QuotaFactRiskDenied {
		t.Fatalf("quota evidence = %#v, want seeded quota fact fields", evidence)
	}
	if !strings.Contains(evidence.Explanation, "Provider denied quota check") ||
		strings.Contains(evidence.Explanation, "sk-secret") ||
		strings.Contains(evidence.Explanation, "Bearer sk-") {
		t.Fatalf("quota evidence explanation = %q, want redacted provider denial", evidence.Explanation)
	}
	if len(evidence.EvidenceRefs) != 2 || !doctorDiagnosticsTestHasRef(evidence.EvidenceRefs, "quota-status:"+accountKey) || !doctorDiagnosticsTestHasRef(evidence.EvidenceRefs, "quota-source:quota-curl") {
		t.Fatalf("evidence refs = %#v, want preserved refs", evidence.EvidenceRefs)
	}
	if evidence.QuotaFact == nil || evidence.QuotaFact.State != QuotaFactStateDenied || evidence.QuotaFact.Explanation != evidence.Explanation {
		t.Fatalf("quota fact = %#v, want embedded fact", evidence.QuotaFact)
	}
	evidence.QuotaFact.EvidenceRefs[0] = "mutated"
	if state, ok := store.StateForAccount(accountKey); !ok || state.Fact == nil || state.Fact.EvidenceRefs[0] == "mutated" {
		t.Fatalf("quota fact copy mutated store state: %#v", state.Fact)
	}
}

func TestDoctorDiagnosticsDoesNotInferQuotaFactWhenMissing(t *testing.T) {
	accountKey := "acct_00000000-0000-4000-8000-000000000403"
	remaining := 0
	store := &QuotaRuntimeStore{
		states: map[string]QuotaRuntimeState{
			accountKey: {
				AccountKey:  accountKey,
				Source:      "quota-curl",
				Status:      QuotaRuntimeStatusSuccess,
				BlockReason: "quota empty",
				Windows: []QuotaRuntimeWindow{{
					ID:               "five-hour",
					Label:            "5H",
					RemainingPercent: &remaining,
					ResetAtUnix:      time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC).Unix(),
				}},
			},
		},
	}

	response := BuildDoctorDiagnosticsSnapshot(DoctorDiagnosticsOptions{
		QuotaStore: store,
		GuardStore: NewAccountRouteGuardStore(),
		Now:        time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC),
	})

	check := findDoctorDiagnosticCheck(response, DoctorDiagnosticCheckQuotaFacts)
	if check == nil {
		t.Fatalf("checks = %#v, want quota fact check", response.Checks)
	}
	if check.Status != DoctorDiagnosticStatusNotReady || len(check.Evidence) != 0 {
		t.Fatalf("quota check = %#v, want no authority without explicit fact", check)
	}
}

func TestDoctorDiagnosticsRegisteredOnGetTokensManagementRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureGetTokensManagementRoutes(group, nil, nil)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/doctor-diagnostics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s, want registered doctor diagnostics route", recorder.Code, recorder.Body.String())
	}
	var response DoctorDiagnosticsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal: %v body=%s", err, recorder.Body.String())
	}
	if response.Authority != "sidecar" || response.Source != "sidecar-diagnostics" {
		t.Fatalf("response = %#v, want sidecar diagnostics", response)
	}
	if findDoctorDiagnosticCheck(response, DoctorDiagnosticCheckRouteGuard) == nil ||
		findDoctorDiagnosticCheck(response, DoctorDiagnosticCheckQuotaFacts) == nil {
		t.Fatalf("checks = %#v, want route and quota checks", response.Checks)
	}
}

func findDoctorDiagnosticCheck(response DoctorDiagnosticsResponse, id string) *DoctorDiagnosticCheck {
	for index := range response.Checks {
		if response.Checks[index].ID == id {
			return &response.Checks[index]
		}
	}
	return nil
}

func doctorDiagnosticsTestHasRef(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}
