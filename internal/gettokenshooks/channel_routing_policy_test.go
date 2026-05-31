package gettokenshooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
