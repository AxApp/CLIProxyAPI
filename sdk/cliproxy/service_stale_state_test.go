package cliproxy

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestServiceApplyCoreAuthAddOrUpdate_DeleteReAddDoesNotInheritStaleRuntimeState(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	authID := "service-stale-state-auth"
	modelID := "stale-model"
	lastRefreshedAt := time.Date(2026, time.March, 1, 8, 0, 0, 0, time.UTC)
	nextRefreshAfter := lastRefreshedAt.Add(30 * time.Minute)

	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:               authID,
		Provider:         "claude",
		Status:           coreauth.StatusActive,
		LastRefreshedAt:  lastRefreshedAt,
		NextRefreshAfter: nextRefreshAfter,
		ModelStates: map[string]*coreauth.ModelState{
			modelID: {
				Quota: coreauth.QuotaState{BackoffLevel: 7},
			},
		},
	})

	service.applyCoreAuthRemoval(context.Background(), authID)

	disabled, ok := service.coreManager.GetByID(authID)
	if !ok || disabled == nil {
		t.Fatalf("expected disabled auth after removal")
	}
	if !disabled.Disabled || disabled.Status != coreauth.StatusDisabled {
		t.Fatalf("expected disabled auth after removal, got disabled=%v status=%v", disabled.Disabled, disabled.Status)
	}
	if disabled.LastRefreshedAt.IsZero() {
		t.Fatalf("expected disabled auth to still carry prior LastRefreshedAt for regression setup")
	}
	if disabled.NextRefreshAfter.IsZero() {
		t.Fatalf("expected disabled auth to still carry prior NextRefreshAfter for regression setup")
	}

	// Reconcile prunes unsupported model state during registration, so seed the
	// disabled snapshot explicitly before exercising delete -> re-add behavior.
	disabled.ModelStates = map[string]*coreauth.ModelState{
		modelID: {
			Quota: coreauth.QuotaState{BackoffLevel: 7},
		},
	}
	if _, err := service.coreManager.Update(context.Background(), disabled); err != nil {
		t.Fatalf("seed disabled auth stale ModelStates: %v", err)
	}

	disabled, ok = service.coreManager.GetByID(authID)
	if !ok || disabled == nil {
		t.Fatalf("expected disabled auth after stale state seeding")
	}
	if len(disabled.ModelStates) == 0 {
		t.Fatalf("expected disabled auth to carry seeded ModelStates for regression setup")
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
	})

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected re-added auth to be present")
	}
	if updated.Disabled {
		t.Fatalf("expected re-added auth to be active")
	}
	if !updated.LastRefreshedAt.IsZero() {
		t.Fatalf("expected LastRefreshedAt to reset on delete -> re-add, got %v", updated.LastRefreshedAt)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected NextRefreshAfter to reset on delete -> re-add, got %v", updated.NextRefreshAfter)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected ModelStates to reset on delete -> re-add, got %d entries", len(updated.ModelStates))
	}
	if models := registry.GetGlobalRegistry().GetModelsForClient(authID); len(models) == 0 {
		t.Fatalf("expected re-added auth to re-register models in global registry")
	}
}

func TestServiceApplyCoreAuthAddOrUpdate_CredentialRefreshResetsStaleRuntimeState(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	authID := "service-credential-refresh-reset"
	modelID := "stale-model"
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	staleRetry := time.Now().Add(30 * time.Minute)
	if _, err := service.coreManager.Register(context.Background(), &coreauth.Auth{
		ID:               authID,
		Provider:         "codex",
		Status:           coreauth.StatusError,
		StatusMessage:    "token refresh failed",
		Unavailable:      true,
		LastError:        &coreauth.Error{Code: "unauthorized", HTTPStatus: 401, Message: "token refresh failed"},
		LastRefreshedAt:  time.Now().Add(-24 * time.Hour),
		NextRefreshAfter: staleRetry,
		Metadata: map[string]any{
			"refresh_token": "old-refresh-token",
			"access_token":  "old-access-token",
		},
		ModelStates: map[string]*coreauth.ModelState{
			modelID: {
				Status:         coreauth.StatusError,
				StatusMessage:  "stale quota",
				Unavailable:    true,
				NextRetryAfter: staleRetry,
				LastError:      &coreauth.Error{HTTPStatus: 429, Message: "quota"},
			},
		},
	}); err != nil {
		t.Fatalf("seed stale auth: %v", err)
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"refresh_token": "new-refresh-token",
			"access_token":  "new-access-token",
			"last_refresh":  time.Now().Format(time.RFC3339),
		},
	})

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected refreshed auth to be present")
	}
	if updated.Status != coreauth.StatusActive || updated.Unavailable || updated.StatusMessage != "" || updated.LastError != nil {
		t.Fatalf("refreshed auth state = status=%q unavailable=%v message=%q last_error=%v, want clean active", updated.Status, updated.Unavailable, updated.StatusMessage, updated.LastError)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected NextRefreshAfter to reset after credential refresh, got %v", updated.NextRefreshAfter)
	}
	if !updated.LastRefreshedAt.IsZero() {
		t.Fatalf("expected LastRefreshedAt to reset after credential refresh, got %v", updated.LastRefreshedAt)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected ModelStates to reset after credential refresh, got %d entries", len(updated.ModelStates))
	}
}

func TestForceHomeRuntimeConfigEnablesUsageStatistics(t *testing.T) {
	cfg := &config.Config{
		UsageStatisticsEnabled: false,
	}

	forceHomeRuntimeConfig(cfg)

	if !cfg.UsageStatisticsEnabled {
		t.Fatal("expected home runtime config to force usage statistics enabled")
	}
}

func TestServiceApplyCoreAuthAddOrUpdate_DisablingCodexAuthGuardsRouteAndClosesWebsocket(t *testing.T) {
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	t.Cleanup(func() {
		gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	authID := "codex-disable-route-guard-auth"
	var closed []string
	previousClose := closeCodexWebsocketSessionsForAuthID
	closeCodexWebsocketSessionsForAuthID = func(id string, reason string) {
		closed = append(closed, id+":"+reason)
	}
	t.Cleanup(func() {
		closeCodexWebsocketSessionsForAuthID = previousClose
		GlobalModelRegistry().UnregisterClient(authID)
	})

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	})
	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusDisabled,
		Disabled: true,
	})

	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{
		{ID: authID, Provider: "codex"},
		{ID: "other-codex-auth", Provider: "codex"},
	}); len(got) != 1 || got[0] != authID {
		t.Fatalf("manual route guard deny ids = %#v, want disabled auth", got)
	}
	if len(closed) != 1 || closed[0] != authID+":auth_disabled" {
		t.Fatalf("closed websocket sessions = %#v, want disabled auth close", closed)
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
	})
	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("manual route guard deny ids after re-enable = %#v, want empty", got)
	}
}

func TestServiceApplyAccountStoreStatusChangeGuardsRouteAndClosesWebsocket(t *testing.T) {
	gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	t.Cleanup(func() {
		gettokenshooks.ClearAccountRouteGuardSource(gettokenshooks.AccountRouteGuardSourceManualDisabled)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	authID := "account-store-status-codex-auth"
	accountKey := "acct_00000000-0000-4000-8000-000000000010"
	var closed []string
	previousClose := closeCodexWebsocketSessionsForAuthID
	closeCodexWebsocketSessionsForAuthID = func(id string, reason string) {
		closed = append(closed, id+":"+reason)
	}
	t.Cleanup(func() {
		closeCodexWebsocketSessionsForAuthID = previousClose
		GlobalModelRegistry().UnregisterClient(authID)
	})

	if _, err := service.coreManager.Register(context.Background(), &coreauth.Auth{
		ID:         authID,
		AccountKey: accountKey,
		Provider:   "codex",
		Status:     coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := service.applyAccountStoreStatusChange(context.Background(), accountstore.AccountRecord{
		AccountKey: accountKey,
		Disabled:   true,
	}); err != nil {
		t.Fatalf("disable status change: %v", err)
	}
	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{
		{ID: authID, AccountKey: accountKey, Provider: "codex"},
		{ID: "other-codex-auth", AccountKey: "acct_00000000-0000-4000-8000-000000000011", Provider: "codex"},
	}); len(got) != 1 || got[0] != authID {
		t.Fatalf("manual route guard deny ids = %#v, want disabled account auth", got)
	}
	if len(closed) != 1 || closed[0] != authID+":auth_disabled" {
		t.Fatalf("closed websocket sessions = %#v, want disabled auth close", closed)
	}
	if auth, ok := service.coreManager.GetByID(authID); !ok || auth == nil || !auth.Disabled || auth.Status != coreauth.StatusDisabled {
		t.Fatalf("runtime auth after disable = %+v, want disabled route state", auth)
	}

	if err := service.applyAccountStoreStatusChange(context.Background(), accountstore.AccountRecord{
		AccountKey: accountKey,
		Disabled:   false,
	}); err != nil {
		t.Fatalf("enable status change: %v", err)
	}
	if got := gettokenshooks.DefaultAccountRouteGuardStore().DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, AccountKey: accountKey, Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("manual route guard deny ids after enable = %#v, want empty", got)
	}
	if auth, ok := service.coreManager.GetByID(authID); !ok || auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		t.Fatalf("runtime auth after enable = %+v, want active route state", auth)
	}
}

func TestApplyHomeOverlayForcesUsageStatisticsEnabled(t *testing.T) {
	baseCfg := &config.Config{}
	baseCfg.Home.Enabled = true
	service := &Service{cfg: baseCfg}

	service.applyHomeOverlay(&config.Config{
		UsageStatisticsEnabled: false,
	})

	if service.cfg == nil || !service.cfg.UsageStatisticsEnabled {
		t.Fatal("expected home overlay to force usage statistics enabled")
	}
	if !service.cfg.Home.Enabled {
		t.Fatal("expected home overlay to preserve local home settings")
	}
}
