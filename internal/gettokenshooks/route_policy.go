package gettokenshooks

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	routeAllowHeader    = "X-GetTokens-Route-Allow"
	routeDenyHeader     = "X-GetTokens-Route-Deny"
	routeOrderHeader    = "X-GetTokens-Route-Order"
	routeFallbackHeader = "X-GetTokens-Route-Fallback"

	routeAllowMetadataKey    = "gettokens.route.allow"
	routeDenyMetadataKey     = "gettokens.route.deny"
	routeOrderMetadataKey    = "gettokens.route.order"
	routeFallbackMetadataKey = "gettokens.route.fallback"
)

// InstallRoutePolicyHook installs GetTokens-specific request route controls.
func InstallRoutePolicyHook() {
	coreauth.RegisterRoutePolicy(channelRoutingRoutePolicy{})
	coreauth.RegisterRoutePolicy(gettokensRoutePolicy{})
	coreauth.RegisterRoutePolicy(accountRouteGuardPolicy{})
}

type gettokensRoutePolicy struct{}

func (gettokensRoutePolicy) RewriteCandidates(ctx context.Context, req coreauth.RoutePolicyRequest) coreauth.RoutePolicyDecision {
	if decision, ok := routeDecisionFromMetadata(req.Options.Metadata); ok {
		return decision
	}
	if !trustedRouteHeaderRequest(ctx) {
		return coreauth.RoutePolicyDecision{}
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return routeDecisionFromHeader(ginCtx.Request.Header)
	}
	return coreauth.RoutePolicyDecision{}
}

func routeDecisionFromMetadata(meta map[string]any) (coreauth.RoutePolicyDecision, bool) {
	if len(meta) == 0 {
		return coreauth.RoutePolicyDecision{}, false
	}
	decision := coreauth.RoutePolicyDecision{
		AllowIDs: splitRouteIDs(metadataString(meta, routeAllowMetadataKey)),
		DenyIDs:  splitRouteIDs(metadataString(meta, routeDenyMetadataKey)),
		OrderIDs: splitRouteIDs(metadataString(meta, routeOrderMetadataKey)),
	}
	if raw, exists := meta[routeFallbackMetadataKey]; exists {
		if parsed, ok := metadataBool(raw); ok {
			decision.AllowFallback = &parsed
		}
	}
	return decision, routeDecisionActive(decision)
}

func routeDecisionFromHeader(header http.Header) coreauth.RoutePolicyDecision {
	if header == nil {
		return coreauth.RoutePolicyDecision{}
	}
	decision := coreauth.RoutePolicyDecision{
		AllowIDs: splitRouteIDs(header.Get(routeAllowHeader)),
		DenyIDs:  splitRouteIDs(header.Get(routeDenyHeader)),
		OrderIDs: splitRouteIDs(header.Get(routeOrderHeader)),
	}
	if raw := strings.TrimSpace(header.Get(routeFallbackHeader)); raw != "" {
		if parsed, ok := parseRouteBool(raw); ok {
			decision.AllowFallback = &parsed
		}
	}
	return decision
}

func trustedRouteHeaderRequest(ctx context.Context) bool {
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return false
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(ginCtx.Request.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(ginCtx.Request.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func routeDecisionActive(decision coreauth.RoutePolicyDecision) bool {
	return len(decision.AllowIDs) > 0 ||
		len(decision.DenyIDs) > 0 ||
		len(decision.OrderIDs) > 0 ||
		decision.AllowFallback != nil
}

func splitRouteIDs(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t' || r == ' '
	})
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
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
	return out
}

func metadataString(meta map[string]any, key string) string {
	raw := meta[key]
	switch value := raw.(type) {
	case string:
		return value
	case []string:
		return strings.Join(value, ",")
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}

func metadataBool(raw any) (bool, bool) {
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		return parseRouteBool(value)
	default:
		return false, false
	}
}

func parseRouteBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

// RouteMetadata returns metadata that can be passed through executor options by
// trusted internal callers.
func RouteMetadata(allowIDs, denyIDs, orderIDs []string, allowFallback *bool) map[string]any {
	meta := map[string]any{}
	if len(allowIDs) > 0 {
		meta[routeAllowMetadataKey] = strings.Join(allowIDs, ",")
	}
	if len(denyIDs) > 0 {
		meta[routeDenyMetadataKey] = strings.Join(denyIDs, ",")
	}
	if len(orderIDs) > 0 {
		meta[routeOrderMetadataKey] = strings.Join(orderIDs, ",")
	}
	if allowFallback != nil {
		meta[routeFallbackMetadataKey] = *allowFallback
	}
	return meta
}
