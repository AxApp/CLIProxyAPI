package gettokenshooks

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
)

const (
	requestRouteHeaderAllow    = "X-GetTokens-Route-Allow"
	requestRouteHeaderDeny     = "X-GetTokens-Route-Deny"
	requestRouteHeaderOrder    = "X-GetTokens-Route-Order"
	requestRouteHeaderFallback = "X-GetTokens-Route-Fallback"
)

func requestRouteHeaderPolicy() gettokensrouting.Policy {
	return gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageHardFilter,
		Name:  "request-route-header",
		Rewrite: func(_ context.Context, routeCtx gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			headers := routeCtx.Options.Headers
			allow := parseRequestRouteHeaderIDs(headers, requestRouteHeaderAllow)
			deny := parseRequestRouteHeaderIDs(headers, requestRouteHeaderDeny)
			order := parseRequestRouteHeaderIDs(headers, requestRouteHeaderOrder)
			fallback, hasFallback := parseRequestRouteHeaderFallback(headers)
			if len(allow) == 0 && len(deny) == 0 && len(order) == 0 && !hasFallback {
				return gettokensrouting.PolicyDecision{}
			}
			decision := gettokensrouting.PolicyDecision{
				AllowIDs: allow,
				DenyIDs:  deny,
				OrderIDs: order,
				Reason:   "request route headers",
			}
			if hasFallback {
				decision.AllowFallback = &fallback
			}
			return decision
		},
	}
}

func parseRequestRouteHeaderIDs(headers http.Header, name string) []string {
	values := requestRouteHeaderValues(headers, name)
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			id := strings.TrimSpace(part)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func parseRequestRouteHeaderFallback(headers http.Header) (bool, bool) {
	values := requestRouteHeaderValues(headers, requestRouteHeaderFallback)
	if len(values) == 0 {
		return false, false
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return false, false
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off":
		return false, true
	case "1", "true", "yes", "on":
		return true, true
	default:
		return false, false
	}
}

func requestRouteHeaderValues(headers http.Header, name string) []string {
	if len(headers) == 0 {
		return nil
	}
	values := headers.Values(name)
	if len(values) > 0 {
		return values
	}
	for key, items := range headers {
		if strings.EqualFold(key, name) {
			return items
		}
	}
	return nil
}
