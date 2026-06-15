package gettokenshooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestConfigureChannelRoutingDecisionRoutesListsRecentSnapshots(t *testing.T) {
	coreauth.ResetRouteDecisionSnapshotsForTests()
	coreauth.SeedRouteDecisionSnapshotForTests(coreauth.RouteDecisionSnapshot{
		Provider:           "codex",
		Model:              "decision-endpoint-model",
		Source:             "scheduler-routing-policy",
		CandidateCount:     1,
		SelectedAuthID:     "auth-b",
		SelectedAccountKey: "acct_b",
		SelectedProvider:   "codex",
		Candidates: []coreauth.RouteDecisionCandidateSnapshot{{
			AuthID:     "auth-b",
			AccountKey: "acct_b",
			Provider:   "codex",
		}},
		Trace: []gettokensrouting.DecisionStep{{
			Stage:     gettokensrouting.PolicyStageRequest,
			Policy:    "decision-endpoint-order",
			Reason:    "prefer auth-b for endpoint test",
			Before:    2,
			After:     1,
			OrderIDs:  []string{"auth-b", "auth-a"},
			Activated: true,
		}},
		RecordedAt: time.Now().UTC(),
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v0/management")
	ConfigureChannelRoutingDecisionRoutes(group, nil, nil)

	request := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/channel-routing/decisions?channel=codex&limit=5", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}

	var payload ChannelRoutingDecisionListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}
	if payload.Items[0].SelectedAccountID != "acct_b" || payload.Items[0].Trace[0].Reason != "prefer auth-b for endpoint test" {
		t.Fatalf("payload item = %#v", payload.Items[0])
	}
}
