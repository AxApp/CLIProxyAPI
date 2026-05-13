package gettokenshooks

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

func TestResolveUsageSnapshotPathPrefersConfigDir(t *testing.T) {
	t.Setenv("GETTOKENS_USAGE_SQLITE_PATH", "")
	got, err := resolveUsageSnapshotPath(UsagePersistenceOptions{
		ConfigFilePath: "/tmp/gettokens/config.yaml",
		WritableBase:   "/tmp/fallback",
	})
	if err != nil {
		t.Fatalf("resolve path: %v", err)
	}
	want := "/tmp/gettokens/usage-observed-v1.sqlite"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestUsageSnapshotStoreSaveLoadRoundTrip(t *testing.T) {
	store, err := newUsageSnapshotStore(filepath.Join(t.TempDir(), "usage-observed-v1.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	input := usage.StatisticsSnapshot{
		TotalRequests: 1,
		SuccessCount:  1,
		TotalTokens:   30,
		APIs: map[string]usage.APISnapshot{
			"codex": {
				TotalRequests: 1,
				TotalTokens:   30,
				Models: map[string]usage.ModelSnapshot{
					"gpt-5.4": {
						TotalRequests: 1,
						TotalTokens:   30,
						Details: []usage.RequestDetail{{
							Timestamp: time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC),
							LatencyMs: 1200,
							Source:    "codex",
							AuthIndex: "0",
							Tokens: usage.TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
		RequestsByDay: map[string]int64{"2026-04-28": 1},
		TokensByDay:   map[string]int64{"2026-04-28": 30},
	}

	if err := store.save(input); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	output, err := store.load()
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if output == nil {
		t.Fatal("load snapshot = nil, want data")
	}
	if output.TotalRequests != input.TotalRequests {
		t.Fatalf("total requests = %d, want %d", output.TotalRequests, input.TotalRequests)
	}
	if output.APIs["codex"].Models["gpt-5.4"].Details[0].LatencyMs != 1200 {
		t.Fatalf("latency = %d, want 1200", output.APIs["codex"].Models["gpt-5.4"].Details[0].LatencyMs)
	}
}

func TestResolveFlushIntervalPrefersEnv(t *testing.T) {
	t.Setenv("GETTOKENS_USAGE_FLUSH_INTERVAL", "250ms")
	if got := resolveFlushInterval(); got != 250*time.Millisecond {
		t.Fatalf("flush interval = %s, want 250ms", got)
	}
}
