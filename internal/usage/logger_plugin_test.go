package usage

import (
	"context"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecordKeepsAggregatesWithoutDetails(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	model := snapshot.APIs["test-key"].Models["gpt-5.4"]
	if model.TotalRequests != 1 {
		t.Fatalf("total requests = %d, want 1", model.TotalRequests)
	}
	if model.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want 30", model.TotalTokens)
	}
	if len(model.Details) != 0 {
		t.Fatalf("details len = %d, want 0; details should be read from disk with pagination", len(model.Details))
	}
}

func TestRequestStatisticsMergeSnapshotRestoresAggregatesWithoutDetails(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	snapshotInput := StatisticsSnapshot{
		TotalRequests: 1,
		SuccessCount:  1,
		TotalTokens:   30,
		APIs: map[string]APISnapshot{
			"test-key": {
				TotalRequests: 1,
				TotalTokens:   30,
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						TotalRequests: 1,
						TotalTokens:   30,
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 0,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
		RequestsByDay: map[string]int64{"2026-03-20": 1},
		TokensByDay:   map[string]int64{"2026-03-20": 30},
	}

	result := stats.MergeSnapshot(snapshotInput)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("merge = %+v, want added=1 skipped=0", result)
	}

	snapshot := stats.Snapshot()
	model := snapshot.APIs["test-key"].Models["gpt-5.4"]
	if model.TotalRequests != 1 || model.TotalTokens != 30 {
		t.Fatalf("model aggregate = %+v, want requests=1 tokens=30", model)
	}
	if len(model.Details) != 0 {
		t.Fatalf("details len = %d, want 0", len(model.Details))
	}
}
