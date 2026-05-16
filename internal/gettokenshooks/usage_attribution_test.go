package gettokenshooks

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestResolveUsageAttributionPathPrefersConfigDir(t *testing.T) {
	t.Setenv("GETTOKENS_USAGE_ATTRIBUTION_SQLITE_PATH", "")
	got, err := resolveUsageAttributionPath(UsageAttributionOptions{
		ConfigFilePath: "/tmp/gettokens/config.yaml",
		WritableBase:   "/tmp/fallback",
	})
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}
	want := "/tmp/gettokens/usage-attribution-v1.sqlite"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestUsageAttributionEventPrefersAuthIDOverSource(t *testing.T) {
	record := coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.4",
		Alias:       "client-gpt",
		Source:      "sk-upstream-secret",
		APIKey:      "relay-key-should-not-be-used",
		AuthID:      "codex:apikey:abc123",
		AuthIndex:   "auth-index-should-not-win",
		AuthType:    "api-key",
		RequestedAt: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		Latency:     1200 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
		},
	}

	event := buildUsageAttributionEvent(context.Background(), record)
	if event.AttributionKey != "auth-id:codex:apikey:abc123" {
		t.Fatalf("attribution key = %q, want auth-id", event.AttributionKey)
	}
	if event.APIKeyHash != "" {
		t.Fatalf("api key hash = %q, want empty because record APIKey is relay key", event.APIKeyHash)
	}
	if event.SourceHash == "" {
		t.Fatal("source hash is empty, want diagnostic source hash")
	}
	if event.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want 30", event.TotalTokens)
	}
}

func TestUsageAttributionEventKeepsAuthIndexForOAuthAuthFile(t *testing.T) {
	record := coreusage.Record{
		Provider:  "codex",
		Model:     "gpt-5.4",
		AuthID:    "some-runtime-auth-id",
		AuthIndex: "auth-file-001",
		AuthType:  "oauth",
	}

	event := buildUsageAttributionEvent(context.Background(), record)
	if event.AttributionKey != "auth-index:auth-file-001" {
		t.Fatalf("attribution key = %q, want auth-index", event.AttributionKey)
	}
}

func TestUsageAttributionEventPrefersAuthIDForAPIUnderscoreKeyType(t *testing.T) {
	record := coreusage.Record{
		Provider:  "mi",
		Model:     "gpt-5.4",
		AuthID:    "runtime-mi-auth-id",
		AuthIndex: "auth-index-should-not-win",
		AuthType:  "api_key",
	}

	event := buildUsageAttributionEvent(context.Background(), record)
	if event.AttributionKey != "auth-id:runtime-mi-auth-id" {
		t.Fatalf("attribution key = %q, want auth-id", event.AttributionKey)
	}
}

func TestUsageAttributionStoreSummaryAggregatesBuckets(t *testing.T) {
	store, err := newUsageAttributionStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	first := usageAttributionEvent{
		ID:                "event-1",
		CompletedAtUnixMs: now.UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:abc123",
		AttributionKind:   "auth_id",
		AccountKey:        "codex-api-key:stable-001",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		LatencyMs:         100,
		InputTokens:       10,
		OutputTokens:      20,
		TotalTokens:       30,
		EvidenceKind:      "auth_id",
	}
	second := first
	second.ID = "event-2"
	second.CompletedAtUnixMs = now.Add(10 * time.Minute).UnixMilli()
	second.Failed = true
	second.LatencyMs = 300
	second.InputTokens = 1
	second.OutputTokens = 2
	second.TotalTokens = 3

	if err := store.insert(first); err != nil {
		t.Fatalf("insert first: %v", err)
	}
	if err := store.insert(second); err != nil {
		t.Fatalf("insert second: %v", err)
	}

	summary, err := store.summary(24*time.Hour, time.Hour, false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(summary.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(summary.Items))
	}
	item := summary.Items[0]
	if item.AccountKey != "codex-api-key:stable-001" {
		t.Fatalf("account key = %q, want local-id account key", item.AccountKey)
	}
	if item.RequestCount != 2 || item.FailedCount != 1 {
		t.Fatalf("counts = %d/%d, want 2/1", item.RequestCount, item.FailedCount)
	}
	if item.LatencyAverageMs != 200 {
		t.Fatalf("avg latency = %d, want 200", item.LatencyAverageMs)
	}
	if item.TotalTokens != 33 {
		t.Fatalf("total tokens = %d, want 33", item.TotalTokens)
	}
	if len(item.Buckets) != 1 || item.Buckets[0].RequestCount != 2 {
		t.Fatalf("unexpected buckets: %#v", item.Buckets)
	}
}

func TestUsageAttributionStoreSummarySupportsAllWindow(t *testing.T) {
	store, err := newUsageAttributionStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	oldEvent := usageAttributionEvent{
		ID:                "event-old",
		CompletedAtUnixMs: now.Add(-90 * 24 * time.Hour).UnixMilli(),
		AttributionKey:    "auth-id:codex:apikey:old",
		AttributionKind:   "auth_id",
		AccountKey:        "codex-api-key:old-001",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		LatencyMs:         120,
		InputTokens:       40,
		OutputTokens:      10,
		TotalTokens:       50,
		EvidenceKind:      "auth_id",
	}
	newEvent := oldEvent
	newEvent.ID = "event-new"
	newEvent.AccountKey = "codex-api-key:new-001"
	newEvent.AttributionKey = "auth-id:codex:apikey:new"
	newEvent.CompletedAtUnixMs = now.UnixMilli()

	if err := store.insert(oldEvent); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if err := store.insert(newEvent); err != nil {
		t.Fatalf("insert new: %v", err)
	}

	summary, err := store.summary(-1, time.Hour, false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Window != "all" {
		t.Fatalf("window = %q, want all", summary.Window)
	}
	if len(summary.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(summary.Items))
	}
}
