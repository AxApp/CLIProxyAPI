package gettokenshooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestGetTokensRoutePolicyReadsTrustedLocalHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:51000"
	req.Header.Set(routeOrderHeader, "auth-b, auth-a")
	req.Header.Set(routeDenyHeader, "auth-c")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	decision := gettokensRoutePolicy{}.RewriteCandidates(context.WithValue(context.Background(), "gin", c), coreauth.RoutePolicyRequest{})
	if got := stringsJoin(decision.OrderIDs); got != "auth-b,auth-a" {
		t.Fatalf("OrderIDs = %q", got)
	}
	if got := stringsJoin(decision.DenyIDs); got != "auth-c" {
		t.Fatalf("DenyIDs = %q", got)
	}
}

func TestGetTokensRoutePolicyIgnoresRemoteHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "203.0.113.10:51000"
	req.Header.Set(routeOrderHeader, "auth-b")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	decision := gettokensRoutePolicy{}.RewriteCandidates(context.WithValue(context.Background(), "gin", c), coreauth.RoutePolicyRequest{})
	if len(decision.OrderIDs) != 0 {
		t.Fatalf("OrderIDs = %v, want empty for remote request", decision.OrderIDs)
	}
}

func TestGetTokensRoutePolicyReadsMetadataWithoutHeaderTrust(t *testing.T) {
	fallback := false
	decision := gettokensRoutePolicy{}.RewriteCandidates(context.Background(), coreauth.RoutePolicyRequest{
		Options: cliproxyexecutor.Options{
			Metadata: RouteMetadata([]string{"auth-a"}, nil, []string{"auth-a"}, &fallback),
		},
	})
	if got := stringsJoin(decision.AllowIDs); got != "auth-a" {
		t.Fatalf("AllowIDs = %q", got)
	}
	if decision.AllowFallback == nil || *decision.AllowFallback {
		t.Fatalf("AllowFallback = %v, want false", decision.AllowFallback)
	}
}

func stringsJoin(values []string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += ","
		}
		out += value
	}
	return out
}
