package gettokenshooks

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRequestRouteHeaderPolicyStrictlyAllowsCandidates(t *testing.T) {
	policy := requestRouteHeaderPolicy()
	fallback := false
	got := policy.Rewrite(context.Background(), gettokensrouting.RouteContext{
		Options: testRouteHeaderOptions(http.Header{
			requestRouteHeaderAllow:    {"auth-b, auth-a"},
			requestRouteHeaderFallback: {"false"},
		}),
	})

	if len(got.AllowIDs) != 2 || got.AllowIDs[0] != "auth-b" || got.AllowIDs[1] != "auth-a" {
		t.Fatalf("AllowIDs = %#v, want auth-b/auth-a", got.AllowIDs)
	}
	if got.AllowFallback == nil || *got.AllowFallback != fallback {
		t.Fatalf("AllowFallback = %#v, want false", got.AllowFallback)
	}
	if got.Reason != "request route headers" {
		t.Fatalf("Reason = %q", got.Reason)
	}

	result := gettokensrouting.NewEngine(policy).Route(context.Background(), gettokensrouting.RouteContext{
		Options: testRouteHeaderOptions(http.Header{
			requestRouteHeaderAllow:    {"auth-b, auth-a"},
			requestRouteHeaderFallback: {"false"},
		}),
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
			{ID: "auth-c"},
		},
	})
	if len(result.Candidates) != 2 || result.Candidates[0].ID != "auth-a" || result.Candidates[1].ID != "auth-b" {
		t.Fatalf("candidates = %#v, want only auth-a/auth-b in original order", result.Candidates)
	}
}

func TestRequestRouteHeaderPolicyOrdersAllowedCandidates(t *testing.T) {
	policy := requestRouteHeaderPolicy()
	result := gettokensrouting.NewEngine(policy).Route(context.Background(), gettokensrouting.RouteContext{
		Options: testRouteHeaderOptions(http.Header{
			requestRouteHeaderAllow:    {"auth-a,auth-b"},
			requestRouteHeaderOrder:    {"auth-b,auth-a"},
			requestRouteHeaderFallback: {"false"},
		}),
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
			{ID: "auth-c"},
		},
	})
	if len(result.Candidates) != 2 || result.Candidates[0].ID != "auth-b" || result.Candidates[1].ID != "auth-a" {
		t.Fatalf("candidates = %#v, want ordered auth-b/auth-a", result.Candidates)
	}
}

func TestRequestRouteHeaderPolicyDenyCandidates(t *testing.T) {
	policy := requestRouteHeaderPolicy()
	result := gettokensrouting.NewEngine(policy).Route(context.Background(), gettokensrouting.RouteContext{
		Options: testRouteHeaderOptions(http.Header{
			requestRouteHeaderDeny: {"auth-b"},
		}),
		Candidates: []gettokensrouting.RouteCandidate{
			{ID: "auth-a"},
			{ID: "auth-b"},
		},
	})
	if len(result.Candidates) != 1 || result.Candidates[0].ID != "auth-a" {
		t.Fatalf("candidates = %#v, want only auth-a", result.Candidates)
	}
}

func testRouteHeaderOptions(headers http.Header) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Headers: headers}
}
