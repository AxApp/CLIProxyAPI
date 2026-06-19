package gettokenshooks

import (
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var installRoutingPoliciesOnce sync.Once

// InstallRoutingPolicies installs GetTokens-owned routing policies.
func InstallRoutingPolicies() {
	InstallRoutingPoliciesWithConfigPath("")
}

func InstallRoutingPoliciesWithConfigPath(configPath string) {
	SetChannelRoutingPolicyConfigPathFromConfig(configPath)
	SetProjectCandidatePoolPolicyConfigPathFromConfig(configPath)
	if err := SetQuotaRuntimeCalibrationPathFromConfig(configPath); err != nil {
		log.WithError(err).Warn("gettokenshooks: configure quota calibration ledger path failed")
	}
	if err := SetBudgetWindowDefinitionPathFromConfig(configPath); err != nil {
		log.WithError(err).Warn("gettokenshooks: configure budget window definition path failed")
	}
	if err := SetRouteResilienceActionLedgerPathFromConfig(configPath); err != nil {
		log.WithError(err).Warn("gettokenshooks: configure route resilience action ledger path failed")
	}
	installRoutingPoliciesOnce.Do(func() {
		gettokensrouting.RegisterPolicy(requestRouteHeaderPolicy())
		gettokensrouting.RegisterPolicy(channelRoutingPolicy())
		gettokensrouting.RegisterPolicy(projectCandidatePoolPolicy())
		gettokensrouting.RegisterPolicy(accountRouteGuardRoutingPolicy(nil))
	})
}

func authCandidatesFromRouteContext(routeCtx gettokensrouting.RouteContext) []*coreauth.Auth {
	out := make([]*coreauth.Auth, 0, len(routeCtx.Candidates))
	for _, candidate := range routeCtx.Candidates {
		if auth, ok := candidate.Value.(*coreauth.Auth); ok && auth != nil {
			out = append(out, auth.Clone())
			continue
		}
		if candidate.ID != "" {
			out = append(out, &coreauth.Auth{ID: candidate.ID})
		}
	}
	return out
}
