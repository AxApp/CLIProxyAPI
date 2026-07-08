package gettokenshooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountRouteGuardManualDisabledDeniesCandidate(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{ID: "codex-auth-1", Provider: "codex"}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     auth.ID,
		AccountKey: "auth-file:codex-auth.json",
		Reason:     "disabled by user",
	})

	decision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "codex-auth-1", Value: &coreauth.Auth{ID: "codex-auth-1", Provider: "codex"}},
			{ID: "codex-auth-2", Value: &coreauth.Auth{ID: "codex-auth-2", Provider: "codex"}},
		},
	})

	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != "codex-auth-1" {
		t.Fatalf("DenyIDs = %#v, want disabled auth only", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, "gettokens account route guard") || !strings.Contains(decision.Reason, AccountRouteGuardSourceManualDisabled) {
		t.Fatalf("Reason = %q", decision.Reason)
	}
	if !store.IsAuthBlocked(auth) {
		t.Fatalf("IsAuthBlocked() = false, want true")
	}
}

func TestAccountRouteGuardSourcesDoNotUnblockEachOther(t *testing.T) {
	store := NewAccountRouteGuardStore()
	authID := "codex-auth-1"
	store.MarkBlocked(AccountRouteGuardBlock{
		Source: AccountRouteGuardSourceManualDisabled,
		AuthID: authID,
		Reason: "disabled by user",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:    AccountRouteGuardSourceRateLimit,
		AuthID:    authID,
		Reason:    "24h tokens full",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	store.ClearAuth(AccountRouteGuardSourceRateLimit, authID)
	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID}}); len(got) != 1 || got[0] != authID {
		t.Fatalf("DenyIDs after clearing rate-limit = %#v, want manual block to remain", got)
	}

	store.ClearAuth(AccountRouteGuardSourceManualDisabled, authID)
	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID}}); len(got) != 0 {
		t.Fatalf("DenyIDs after clearing manual-disabled = %#v, want empty", got)
	}
}

func TestAccountRouteGuardActiveBlocksForAuthReturnsSourceDetails(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{
		ID:         "codex-auth-1",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Provider:   "codex",
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AccountKey: auth.AccountKey,
		Reason:     "1h requests 已满",
		ExpiresAt:  expiresAt,
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:    AccountRouteGuardSourceManualDisabled,
		AuthID:    auth.ID,
		Reason:    "disabled by user",
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
	})

	blocks := store.ActiveBlocksForAuth(auth)
	if len(blocks) != 1 {
		t.Fatalf("active blocks = %#v, want only unexpired rate-limit block", blocks)
	}
	if blocks[0].Source != AccountRouteGuardSourceRateLimit || blocks[0].Reason != "1h requests 已满" {
		t.Fatalf("active block = %#v, want rate-limit source details", blocks[0])
	}
	if !blocks[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", blocks[0].ExpiresAt, expiresAt)
	}
}

func TestRouteResilienceStateDefaultsRouteGuardSourcesToAccountScope(t *testing.T) {
	sources := []string{
		AccountRouteGuardSourceManualDisabled,
		AccountRouteGuardSourceRateLimit,
		AccountRouteGuardSourceQuotaEmpty,
		AccountRouteGuardSourceAuthError,
		AccountRouteGuardSourceUpstreamRateLimit,
		AccountRouteGuardSourceUpstreamTransientErr,
	}
	for _, source := range sources {
		t.Run(source, func(t *testing.T) {
			state, ok := routeResilienceStateFromBlock(AccountRouteGuardBlock{
				Source: source,
				AuthID: "auth-a",
				Reason: "blocked",
			})
			if !ok {
				t.Fatalf("routeResilienceStateFromBlock ok = false")
			}
			if state.Scope != RouteResilienceScopeAccount {
				t.Fatalf("Scope = %q, want account", state.Scope)
			}
			if !state.RouteBlocking {
				t.Fatalf("RouteBlocking = false, want true")
			}
		})
	}
}

func TestRouteResilienceStatePreservesExplicitModelScope(t *testing.T) {
	state, ok := routeResilienceStateFromBlock(AccountRouteGuardBlock{
		Source:       AccountRouteGuardSourceUpstreamTransientErr,
		FailureScope: RouteResilienceScopeModel,
		AuthID:       "auth-a",
		AccountKey:   "acct_00000000-0000-4000-8000-000000000001",
		Model:        "gpt-5",
		Reason:       "model capacity",
	})
	if !ok {
		t.Fatalf("routeResilienceStateFromBlock ok = false")
	}
	if state.Scope != RouteResilienceScopeModel {
		t.Fatalf("Scope = %q, want model", state.Scope)
	}
	if state.Model != "gpt-5" {
		t.Fatalf("Model = %q, want gpt-5", state.Model)
	}
}

func TestRouteResilienceModelScopedGuardDoesNotDenyOtherModels(t *testing.T) {
	store := NewAccountRouteGuardStore()
	auth := &coreauth.Auth{
		ID:         "auth-model-specific",
		AccountKey: "acct_00000000-0000-4000-8000-000000000003",
		Provider:   "codex",
	}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:       AccountRouteGuardSourceUpstreamTransientErr,
		FailureScope: RouteResilienceScopeModel,
		AuthID:       auth.ID,
		AccountKey:   auth.AccountKey,
		Model:        "gpt-5",
		Reason:       "model capacity",
	})

	otherModelDecision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Model: "gpt-5-mini",
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: auth.ID, Value: auth},
		},
	})
	if len(otherModelDecision.DenyIDs) != 0 {
		t.Fatalf("other model DenyIDs = %#v, want no deny for model-scoped block", otherModelDecision.DenyIDs)
	}

	blockedModelDecision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Model: "gpt-5",
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: auth.ID, Value: auth},
		},
	})
	if len(blockedModelDecision.DenyIDs) != 1 || blockedModelDecision.DenyIDs[0] != auth.ID {
		t.Fatalf("blocked model DenyIDs = %#v, want auth denied", blockedModelDecision.DenyIDs)
	}
}

func TestAccountRouteGuardPolicyReasonIncludesActiveSources(t *testing.T) {
	store := NewAccountRouteGuardStore()
	authA := &coreauth.Auth{ID: "auth-a", AccountKey: "acct_00000000-0000-4000-8000-000000000001", Provider: "codex"}
	authB := &coreauth.Auth{ID: "auth-b", AccountKey: "acct_00000000-0000-4000-8000-000000000002", Provider: "codex"}
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     authA.ID,
		AccountKey: authA.AccountKey,
		Reason:     "disabled by user",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceRateLimit,
		AccountKey: authB.AccountKey,
		Reason:     "1h requests 已满",
	})

	decision := accountRouteGuardPolicy{store: store}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: authA.ID, Value: authA},
			{ID: authB.ID, Value: authB},
		},
	})

	if len(decision.DenyIDs) != 2 || decision.DenyIDs[0] != authA.ID || decision.DenyIDs[1] != authB.ID {
		t.Fatalf("DenyIDs = %#v, want both guarded auths", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, AccountRouteGuardSourceManualDisabled) || !strings.Contains(decision.Reason, AccountRouteGuardSourceRateLimit) {
		t.Fatalf("Reason = %q, want active guard sources", decision.Reason)
	}
}

func TestRouteResilienceStatesFromPersistedRuntimeState(t *testing.T) {
	configPath := writeRouteGuardChannelRoutingConfig(t, `{
  "channels": {
    "codex": {
      "channel": "codex",
      "routeMode": "sequential",
      "orderedAccountIDs": [],
      "channelGroupStates": {}
    }
  },
  "runtimeStates": {
    "acct_00000000-0000-4000-8000-000000000001": {
      "accountID": "acct_00000000-0000-4000-8000-000000000001",
      "updatedAt": "2026-06-09T10:00:00Z",
      "sources": {
        "auth-error": {
          "source": "auth-error",
          "reason": "token expired",
          "model": "gpt-5",
          "updatedAt": "2026-06-09T10:00:00Z"
        }
      }
    }
  }
}`)
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	blocksByAuthID := activePersistedChannelRuntimeBlocksForCandidates([]*coreauth.Auth{{
		ID:         "auth-a",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Provider:   "codex",
	}})
	states := routeResilienceStatesFromBlocks(blocksByAuthID["auth-a"])
	if len(states) != 1 {
		t.Fatalf("states = %#v, want one persisted route resilience state", states)
	}
	if states[0].Scope != RouteResilienceScopeAccount || states[0].Source != AccountRouteGuardSourceAuthError {
		t.Fatalf("state = %#v, want account-scoped auth-error", states[0])
	}
	if states[0].Model != "gpt-5" || states[0].Reason != "token expired" {
		t.Fatalf("state = %#v, want model and reason preserved", states[0])
	}
}

func TestHydratedAuthErrorBlocksSiblingAccountWithSharedOpenAIAccountID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	first := createRouteGuardAuthFileAccount(t, dbPath, "first.json", `{"type":"codex","access_token":"token-a","account_id":"openai-shared-account","email":"first@example.test"}`)
	second := createRouteGuardAuthFileAccount(t, dbPath, "second.json", `{"type":"codex","access_token":"token-b","account_id":"openai-shared-account","email":"second@example.test"}`)
	setRouteGuardAccountStorePathForTest(t, dbPath)

	configPath := writeRouteGuardChannelRoutingConfig(t, fmt.Sprintf(`{
  "channels": {},
  "runtimeStates": {
    %q: {
      "accountID": %q,
      "updatedAt": "2026-07-08T00:00:00Z",
      "sources": {
        "auth-error": {
          "source": "auth-error",
          "scope": "account",
          "reason": "token_invalidated",
          "updatedAt": "2026-07-08T00:00:00Z"
        }
      }
    }
  }
}`, first.AccountKey, first.AccountKey))
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	guard := NewAccountRouteGuardStore()
	if err := hydrateAccountRouteGuardStoreFromPersistedRuntimeStates(guard, time.Date(2026, 7, 8, 0, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("hydrate route guard: %v", err)
	}

	state := NewQuotaRuntimeStore(guard).StatesForAccounts([]string{second.AccountKey})[0]
	if !state.Blocked || len(state.Sources) != 1 || state.Sources[0].Source != AccountRouteGuardSourceAuthError {
		t.Fatalf("sibling quota state = %#v, want shared auth-error block", state)
	}

	decision := accountRouteGuardPolicy{store: guard}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Provider: "codex",
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-sibling", Value: &coreauth.Auth{ID: "auth-sibling", AccountKey: second.AccountKey, Provider: "codex"}},
		},
	})
	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != "auth-sibling" {
		t.Fatalf("DenyIDs = %#v, want shared account sibling denied", decision.DenyIDs)
	}
}

func TestAccountRouteGuardResultHookBlocksAndClearsTransientFailure(t *testing.T) {
	store := NewAccountRouteGuardStore()
	hook := AccountRouteGuardResultHook{Store: store}
	retryAfter := 2 * time.Minute

	hook.OnResult(context.Background(), coreauth.Result{
		AuthID:     "codex-auth-1",
		Provider:   "codex",
		Model:      "gpt-5",
		Success:    false,
		RetryAfter: &retryAfter,
		Error:      &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota window exhausted"},
	})

	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex-auth-1", Provider: "codex"}}); len(got) != 1 || got[0] != "codex-auth-1" {
		t.Fatalf("DenyIDs after 429 = %#v, want transient block", got)
	}

	hook.OnResult(context.Background(), coreauth.Result{
		AuthID:   "codex-auth-1",
		Provider: "codex",
		Model:    "gpt-5",
		Success:  true,
	})

	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: "codex-auth-1", Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("DenyIDs after success = %#v, want transient block cleared", got)
	}
}

func TestAccountRouteGuardResultHookBlocksTerminalOAuthRefreshFailureOnAuthUpdated(t *testing.T) {
	store := NewAccountRouteGuardStore()
	hook := AccountRouteGuardResultHook{Store: store}
	auth := &coreauth.Auth{
		ID:          "codex-auth-refresh-failed",
		AccountKey:  "acct_00000000-0000-4000-8000-000000000001",
		Provider:    "codex",
		Status:      coreauth.StatusError,
		Unavailable: true,
		LastError: &coreauth.Error{
			HTTPStatus: http.StatusUnauthorized,
			Code:       "unauthorized",
			Message:    `token refresh failed with status 400: {"error":{"code":"invalid_refresh_token","message":"Could not validate your refresh token. Please try signing in again."}}`,
			Retryable:  false,
		},
	}

	hook.OnAuthUpdated(context.Background(), auth)

	states := NewQuotaRuntimeStore(store).StatesForAccounts([]string{auth.AccountKey})
	if len(states) != 1 || !states[0].Blocked {
		t.Fatalf("quota states = %#v, want auth-error block from refresh failure", states)
	}
	if len(states[0].Sources) != 1 || states[0].Sources[0].Source != AccountRouteGuardSourceAuthError {
		t.Fatalf("sources = %#v, want auth-error", states[0].Sources)
	}
	if !strings.Contains(states[0].Sources[0].Reason, "invalid_refresh_token") {
		t.Fatalf("reason = %q, want invalid_refresh_token evidence", states[0].Sources[0].Reason)
	}
}

func TestAccountRouteGuardResultHookDoesNotClearManualDisabled(t *testing.T) {
	store := NewAccountRouteGuardStore()
	hook := AccountRouteGuardResultHook{Store: store}
	authID := "codex-auth-1"
	store.MarkBlocked(AccountRouteGuardBlock{
		Source: AccountRouteGuardSourceManualDisabled,
		AuthID: authID,
		Reason: "disabled by user",
	})
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:    AccountRouteGuardSourceUpstreamTransientErr,
		AuthID:    authID,
		Reason:    "temporary upstream failure",
		ExpiresAt: time.Now().Add(time.Minute),
	})

	hook.OnResult(context.Background(), coreauth.Result{
		AuthID:   authID,
		Provider: "codex",
		Success:  true,
	})

	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, Provider: "codex"}}); len(got) != 1 || got[0] != authID {
		t.Fatalf("DenyIDs after success = %#v, want manual-disabled block to remain", got)
	}
	store.ClearAuth(AccountRouteGuardSourceManualDisabled, authID)
	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{ID: authID, Provider: "codex"}}); len(got) != 0 {
		t.Fatalf("DenyIDs after clearing manual-disabled = %#v, want empty", got)
	}
}

func TestAccountRouteGuardPolicyDeniesCandidatesFromPersistedRuntimeStates(t *testing.T) {
	configPath := writeRouteGuardChannelRoutingConfig(t, `{
  "channels": {
    "codex": {
      "channel": "codex",
      "routeMode": "sequential",
      "orderedAccountIDs": [],
      "channelGroupStates": {}
    }
  },
  "runtimeStates": {
    "acct_00000000-0000-4000-8000-000000000001": {
      "accountID": "acct_00000000-0000-4000-8000-000000000001",
      "updatedAt": "2026-06-09T10:00:00Z",
      "sources": {
        "auth-error": {
          "source": "auth-error",
          "reason": "token expired",
          "updatedAt": "2026-06-09T10:00:00Z"
        }
      }
    }
  }
}`)
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	decision := accountRouteGuardPolicy{store: NewAccountRouteGuardStore()}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{
				ID: "auth-a",
				Value: &coreauth.Auth{
					ID:         "auth-a",
					AccountKey: "acct_00000000-0000-4000-8000-000000000001",
					Provider:   "codex",
				},
			},
			{
				ID: "auth-b",
				Value: &coreauth.Auth{
					ID:         "auth-b",
					AccountKey: "acct_00000000-0000-4000-8000-000000000002",
					Provider:   "codex",
				},
			},
		},
	})

	if len(decision.DenyIDs) != 1 || decision.DenyIDs[0] != "auth-a" {
		t.Fatalf("DenyIDs = %#v, want persisted blocked auth only", decision.DenyIDs)
	}
	if !strings.Contains(decision.Reason, "auth-error") {
		t.Fatalf("Reason = %q, want persisted auth-error source", decision.Reason)
	}
}

func TestSetChannelRoutingPolicyConfigPathHydratesPersistedRuntimeStatesForQuotaStatus(t *testing.T) {
	const accountKey = "acct_00000000-0000-4000-8000-000000000001"
	configPath := writeRouteGuardChannelRoutingConfig(t, `{
  "channels": {
    "codex": {
      "channel": "codex",
      "routeMode": "sequential",
      "orderedAccountIDs": [],
      "channelGroupStates": {}
    }
  },
  "runtimeStates": {
    "acct_00000000-0000-4000-8000-000000000001": {
      "accountID": "acct_00000000-0000-4000-8000-000000000001",
      "updatedAt": "2026-06-09T10:00:00Z",
      "sources": {
        "auth-error": {
          "source": "auth-error",
          "scope": "account",
          "reason": "token_invalidated",
          "updatedAt": "2026-06-09T10:00:00Z"
        }
      }
    }
  }
}`)

	channelRoutingPolicyConfigPathState.Lock()
	previousPath := channelRoutingPolicyConfigPathState.path
	channelRoutingPolicyConfigPathState.Unlock()
	previousDefaultGuard := defaultAccountRouteGuardStore
	defaultAccountRouteGuardStore = NewAccountRouteGuardStore()
	t.Cleanup(func() {
		defaultAccountRouteGuardStore = previousDefaultGuard
		channelRoutingPolicyConfigPathState.Lock()
		channelRoutingPolicyConfigPathState.path = previousPath
		channelRoutingPolicyConfigPathState.Unlock()
	})

	SetChannelRoutingPolicyConfigPathFromConfig(configPath)

	store := NewQuotaRuntimeStore(DefaultAccountRouteGuardStore())
	states := store.StatesForAccounts([]string{accountKey})
	if len(states) != 1 {
		t.Fatalf("StatesForAccounts len = %d, want 1", len(states))
	}
	if !states[0].Blocked {
		t.Fatalf("blocked = false, want true for persisted auth-error: %#v", states[0])
	}
	if len(states[0].Sources) != 1 || states[0].Sources[0].Source != AccountRouteGuardSourceAuthError {
		t.Fatalf("sources = %#v, want persisted auth-error", states[0].Sources)
	}
	if states[0].Sources[0].Reason != "token_invalidated" {
		t.Fatalf("source reason = %q, want token_invalidated", states[0].Sources[0].Reason)
	}
}

func TestAccountRouteGuardPolicyIgnoresLegacyManualDisabledPersistedRuntimeState(t *testing.T) {
	configPath := writeRouteGuardChannelRoutingConfig(t, `{
  "channels": {
    "codex": {
      "channel": "codex",
      "routeMode": "sequential",
      "orderedAccountIDs": [],
      "channelGroupStates": {}
    }
  },
  "runtimeStates": {
    "acct_00000000-0000-4000-8000-000000000001": {
      "accountID": "acct_00000000-0000-4000-8000-000000000001",
      "updatedAt": "2026-06-09T10:00:00Z",
      "sources": {
        "manual-disabled": {
          "source": "manual-disabled",
          "reason": "account disabled",
          "updatedAt": "2026-06-09T10:00:00Z"
        }
      }
    }
  }
}`)
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	decision := accountRouteGuardPolicy{store: NewAccountRouteGuardStore()}.RewriteCandidates(context.Background(), gettokensrouting.RouteContext{
		Candidates: []gettokensrouting.RouteCandidate{
			{
				ID: "auth-a",
				Value: &coreauth.Auth{
					ID:         "auth-a",
					AccountKey: "acct_00000000-0000-4000-8000-000000000001",
					Provider:   "codex",
				},
			},
		},
	})

	if len(decision.DenyIDs) != 0 {
		t.Fatalf("DenyIDs = %#v, want legacy persisted manual-disabled ignored", decision.DenyIDs)
	}
}

func TestAccountRouteGuardStorePersistsRuntimeStateToChannelRoutingConfig(t *testing.T) {
	configPath := writeRouteGuardChannelRoutingConfig(t, `{
  "channels": {
    "codex": {
      "channel": "codex",
      "routeMode": "sequential",
      "manualRequestableAccountIDs": ["acct_manual"],
      "orderedAccountIDs": [],
      "accountGroups": [
        {
          "id": "group-default",
          "name": "Default",
          "enabled": true,
          "accountIDs": ["acct_00000000-0000-4000-8000-000000000001"]
        }
      ],
      "shadowEnabled": true,
      "shadowRouteMode": "balanced",
      "channelGroupStates": {}
    }
  },
  "events": [
    {
      "id": "route-000001",
      "recordedAt": "2026-06-09T10:00:00Z",
      "channel": "codex",
      "routeMode": "sequential",
      "snapshotVersion": "snapshot-a",
      "policyVersion": "channel-routing-v1",
      "redacted": true
    }
  ],
  "nextEventID": 1,
  "runtimeStates": {
    "acct_00000000-0000-4000-8000-000000000001": {
      "accountID": "acct_00000000-0000-4000-8000-000000000001",
      "updatedAt": "2026-06-09T10:00:00Z",
      "sources": {
        "rate-limit": {
          "source": "rate-limit",
          "reason": "cooldown",
          "updatedAt": "2026-06-09T10:00:00Z"
        }
      }
    }
  }
}`)
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	store := NewAccountRouteGuardStore()
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceAuthError,
		AuthID:     "auth-a",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Model:      "gpt-5",
		Reason:     "token expired",
	})

	raw := readRouteGuardChannelRoutingConfig(t, configPath)
	state, ok := raw.RuntimeStates["acct_00000000-0000-4000-8000-000000000001"]
	if !ok {
		t.Fatalf("runtimeStates missing persisted account key: %#v", raw.RuntimeStates)
	}
	if source := state.Sources[AccountRouteGuardSourceAuthError]; source.Source != AccountRouteGuardSourceAuthError {
		t.Fatalf("persisted source = %#v, want auth-error", source)
	}
	if source := state.Sources[AccountRouteGuardSourceAuthError]; source.Reason != "token expired" {
		t.Fatalf("persisted reason = %q, want token expired", source.Reason)
	}
	if source := state.Sources[AccountRouteGuardSourceAuthError]; source.Scope != string(RouteResilienceScopeAccount) {
		t.Fatalf("persisted scope = %q, want account", source.Scope)
	}
	if source := state.Sources[AccountRouteGuardSourceAuthError]; source.Model != "gpt-5" {
		t.Fatalf("persisted model = %q, want gpt-5", source.Model)
	}
	if source := state.Sources[AccountRouteGuardSourceRateLimit]; source.Reason != "cooldown" {
		t.Fatalf("existing persisted rate-limit source = %#v, want preserved cooldown", source)
	}
	if raw.NextEventID != 1 {
		t.Fatalf("nextEventID = %d, want preserved 1", raw.NextEventID)
	}
	if len(raw.Events) == 0 {
		t.Fatalf("events = %s, want preserved route event", string(raw.Events))
	}
	assertRouteGuardRawJSONPath(t, raw.Channels["codex"], []string{"manualRequestableAccountIDs"}, []string{"acct_manual"})
	assertRouteGuardRawJSONPath(t, raw.Channels["codex"], []string{"accountGroups", "0", "name"}, "Default")
	assertRouteGuardRawJSONPath(t, raw.Channels["codex"], []string{"shadowEnabled"}, true)
	assertRouteGuardRawJSONPath(t, raw.Channels["codex"], []string{"shadowRouteMode"}, "balanced")

	store.ClearAuth(AccountRouteGuardSourceAuthError, "auth-a")

	raw = readRouteGuardChannelRoutingConfig(t, configPath)
	state, ok = raw.RuntimeStates["acct_00000000-0000-4000-8000-000000000001"]
	if !ok {
		t.Fatalf("runtimeStates after source clear = %#v, want account preserved for rate-limit", raw.RuntimeStates)
	}
	if _, ok := state.Sources[AccountRouteGuardSourceAuthError]; ok {
		t.Fatalf("runtimeStates after source clear = %#v, want auth-error removed", state.Sources)
	}
	if source := state.Sources[AccountRouteGuardSourceRateLimit]; source.Reason != "cooldown" {
		t.Fatalf("runtimeStates after source clear = %#v, want rate-limit preserved", state.Sources)
	}
}

func TestAccountRouteGuardStoreDoesNotPersistManualDisabledRuntimeState(t *testing.T) {
	configPath := writeRouteGuardChannelRoutingConfig(t, `{
  "channels": {
    "codex": {
      "channel": "codex",
      "routeMode": "sequential",
      "orderedAccountIDs": [],
      "channelGroupStates": {}
    }
  },
  "runtimeStates": {
    "acct_00000000-0000-4000-8000-000000000001": {
      "accountID": "acct_00000000-0000-4000-8000-000000000001",
      "updatedAt": "2026-06-09T10:00:00Z",
      "sources": {
        "rate-limit": {
          "source": "rate-limit",
          "reason": "cooldown",
          "updatedAt": "2026-06-09T10:00:00Z"
        }
      }
    }
  }
}`)
	setRouteGuardChannelRoutingConfigPathForTest(t, configPath)

	store := NewAccountRouteGuardStore()
	store.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     "auth-a",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Reason:     "disabled by user",
	})

	raw := readRouteGuardChannelRoutingConfig(t, configPath)
	state := raw.RuntimeStates["acct_00000000-0000-4000-8000-000000000001"]
	if _, ok := state.Sources[AccountRouteGuardSourceManualDisabled]; ok {
		t.Fatalf("manual-disabled source should not be persisted: %#v", state.Sources)
	}
	if source := state.Sources[AccountRouteGuardSourceRateLimit]; source.Reason != "cooldown" {
		t.Fatalf("rate-limit source = %#v, want preserved cooldown", source)
	}
	if got := store.DenyIDsForCandidates([]*coreauth.Auth{{
		ID:         "auth-a",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Provider:   "codex",
	}}); len(got) != 1 || got[0] != "auth-a" {
		t.Fatalf("in-memory DenyIDs = %#v, want immediate manual-disabled block", got)
	}
}

type routeGuardChannelRoutingConfig struct {
	Channels      map[string]json.RawMessage                 `json:"channels"`
	Events        json.RawMessage                            `json:"events"`
	NextEventID   int                                        `json:"nextEventID"`
	RuntimeStates map[string]routeGuardPersistedRuntimeState `json:"runtimeStates"`
}

type routeGuardPersistedRuntimeState struct {
	Sources map[string]routeGuardPersistedRuntimeSource `json:"sources"`
}

type routeGuardPersistedRuntimeSource struct {
	Source string `json:"source"`
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
	Model  string `json:"model"`
}

func writeRouteGuardChannelRoutingConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".config", "gettokens-dev", "config.yaml")
	channelConfigPath := filepath.Join(filepath.Dir(configPath), "channel-routing", "config.json")
	if err := os.MkdirAll(filepath.Dir(channelConfigPath), 0o700); err != nil {
		t.Fatalf("mkdir channel routing config: %v", err)
	}
	if err := os.WriteFile(channelConfigPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write channel routing config: %v", err)
	}
	return configPath
}

func setRouteGuardChannelRoutingConfigPathForTest(t *testing.T, configPath string) {
	t.Helper()
	channelRoutingPolicyConfigPathState.Lock()
	previous := channelRoutingPolicyConfigPathState.path
	channelRoutingPolicyConfigPathState.path = filepath.Join(filepath.Dir(configPath), "channel-routing", "config.json")
	channelRoutingPolicyConfigPathState.Unlock()
	t.Cleanup(func() {
		channelRoutingPolicyConfigPathState.Lock()
		channelRoutingPolicyConfigPathState.path = previous
		channelRoutingPolicyConfigPathState.Unlock()
	})
}

func setRouteGuardAccountStorePathForTest(t *testing.T, dbPath string) {
	t.Helper()
	accountRouteGuardAccountStorePathState.Lock()
	previousPath := accountRouteGuardAccountStorePathState.path
	previousCache := accountRouteGuardAccountStorePathState.cache
	accountRouteGuardAccountStorePathState.path = strings.TrimSpace(dbPath)
	accountRouteGuardAccountStorePathState.cache = nil
	accountRouteGuardAccountStorePathState.Unlock()
	t.Cleanup(func() {
		accountRouteGuardAccountStorePathState.Lock()
		accountRouteGuardAccountStorePathState.path = previousPath
		accountRouteGuardAccountStorePathState.cache = previousCache
		accountRouteGuardAccountStorePathState.Unlock()
	})
}

func createRouteGuardAuthFileAccount(t *testing.T, dbPath string, sourceFileName string, authJSON string) accountstore.AccountRecord {
	t.Helper()
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure account store schema: %v", err)
	}
	account, err := store.CreateAccount(context.Background(), accountstore.AccountWrite{
		Kind:             accountstore.KindAuthFile,
		Title:            sourceFileName,
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		AuthFile: &accountstore.AuthFileCredential{
			SourceFileName: sourceFileName,
			AuthJSON:       authJSON,
			AuthType:       "codex",
			SizeBytes:      int64(len(authJSON)),
		},
	})
	if err != nil {
		t.Fatalf("create auth-file account: %v", err)
	}
	return account
}

func readRouteGuardChannelRoutingConfig(t *testing.T, configPath string) routeGuardChannelRoutingConfig {
	t.Helper()
	path := filepath.Join(filepath.Dir(configPath), "channel-routing", "config.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read channel routing config: %v", err)
	}
	var cfg routeGuardChannelRoutingConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("unmarshal channel routing config: %v", err)
	}
	if cfg.RuntimeStates == nil {
		cfg.RuntimeStates = map[string]routeGuardPersistedRuntimeState{}
	}
	return cfg
}

func assertRouteGuardRawJSONPath(t *testing.T, raw json.RawMessage, path []string, want any) {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("unmarshal raw channel config: %v", err)
	}
	for _, part := range path {
		switch node := value.(type) {
		case map[string]any:
			value = node[part]
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				t.Fatalf("invalid raw JSON array path %v at %q", path, part)
			}
			value = node[index]
		default:
			t.Fatalf("raw JSON path %v reached non-container %#v", path, value)
		}
	}
	gotJSON, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw JSON path %v value: %v", path, err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal raw JSON path %v want: %v", path, err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("raw JSON path %v = %#v, want %#v", path, value, want)
	}
}
