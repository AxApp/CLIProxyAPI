package gettokenshooks

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestRateLimitSchemaDropsMatchKey(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}

	for _, table := range []string{"rate_limit_rules", "rate_limit_events"} {
		rows, err := store.db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatalf("pragma %s: %v", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name string
			var typ string
			var notNull int
			var defaultValue any
			var pk int
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				t.Fatalf("scan %s column: %v", table, err)
			}
			if name == "match_key" {
				t.Fatalf("%s still contains deprecated match_key column", table)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s columns: %v", table, err)
		}
	}
}

func TestRateLimitStoreDropsLegacyMatchKeyTables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = db.Exec(`
CREATE TABLE rate_limit_rules (
  id TEXT PRIMARY KEY,
  account_key TEXT NOT NULL,
  match_key TEXT NOT NULL,
  strategy TEXT NOT NULL,
  window TEXT NOT NULL,
  limit_value INTEGER NOT NULL,
  action TEXT NOT NULL,
  enabled INTEGER NOT NULL,
  label TEXT NOT NULL,
  created_at_unix_ms INTEGER NOT NULL,
  updated_at_unix_ms INTEGER NOT NULL
);
CREATE TABLE rate_limit_events (
  id TEXT PRIMARY KEY,
  account_key TEXT NOT NULL,
  match_key TEXT NOT NULL,
  rule_id TEXT NOT NULL,
  strategy TEXT NOT NULL,
  window TEXT NOT NULL,
  action TEXT NOT NULL,
  usage_value INTEGER NOT NULL,
  limit_value INTEGER NOT NULL,
  blocked INTEGER NOT NULL,
  reason TEXT NOT NULL,
  triggered_at_unix_ms INTEGER NOT NULL
);
INSERT INTO rate_limit_rules VALUES ('legacy-rule', 'account-1', 'legacy-match', 'request-window', '1h', 1, 'block', 1, '', 1, 1);
INSERT INTO rate_limit_events VALUES ('legacy-event', 'account-1', 'legacy-match', 'legacy-rule', 'request-window', '1h', 'block', 1, 1, 1, 'legacy', 1);
`)
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	store, err := newRateLimitStore(dbPath)
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}

	for _, table := range []string{"rate_limit_rules", "rate_limit_events"} {
		if hasDeprecatedMatchKeyColumn(t, store.db, table) {
			t.Fatalf("%s still has match_key after destructive migration", table)
		}
		var count int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d legacy rows, want destructive cleanup", table, count)
		}
	}
}

func TestRateLimitEvaluatorUsesOnlyAccountKey(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	if err := store.upsertRule(RateLimitRule{
		ID:         "rule-1",
		AccountKey: "acct_00000000-0000-4000-8000-000000000001",
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, now); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	if err := store.insertUsageAttributionEvent(usageAttributionEvent{
		ID:                "event-matching-evidence-only",
		CompletedAtUnixMs: now.Add(1 * time.Minute).UnixMilli(),
		AttributionKey:    "codex-api-key:stable-001",
		AttributionKind:   "legacy_evidence",
		AccountKey:        "codex-api-key:other-card",
		Provider:          "codex",
		RequestedModel:    "gpt-5.4",
		TotalTokens:       50,
		EvidenceKind:      "legacy_evidence",
	}); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	evaluator := NewRateLimitEvaluator(store, RateLimitEvaluatorOptions{
		Now: func() time.Time { return now.Add(30 * time.Minute) },
	})
	if err := evaluator.EvaluateNow(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	state, ok := evaluator.StateForAccount("acct_00000000-0000-4000-8000-000000000001")
	if !ok {
		t.Fatal("missing account state")
	}
	if state.Blocked {
		t.Fatalf("state = %#v, want no block because usage belongs to another account card", state)
	}
}

func TestRateLimitRuleRejectsLegacyAccountKeys(t *testing.T) {
	store, err := newRateLimitStore(filepath.Join(t.TempDir(), "usage-attribution-v1.sqlite"))
	if err != nil {
		t.Fatalf("new rate limit store: %v", err)
	}
	err = store.upsertRule(RateLimitRule{
		ID:         "legacy-key-rule",
		AccountKey: "codex-api-key:stable-001",
		Strategy:   RateLimitStrategyRequestWindow,
		Window:     "1h",
		LimitValue: 1,
		Action:     RateLimitActionBlock,
		Enabled:    true,
	}, time.Now().UTC())
	if err == nil {
		t.Fatal("upsert legacy account key succeeded, want acct_* validation error")
	}
	if !strings.Contains(err.Error(), "acct_") {
		t.Fatalf("error = %v, want acct_* validation detail", err)
	}
}

func hasDeprecatedMatchKeyColumn(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan %s column: %v", table, err)
		}
		if name == "match_key" {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s columns: %v", table, err)
	}
	return false
}

func TestRateLimitSourceDoesNotUseMatchKeyOrAttributionFallback(t *testing.T) {
	source, err := os.ReadFile("rate_limit.go")
	if err != nil {
		t.Fatalf("read rate_limit.go: %v", err)
	}
	text := string(source)
	if strings.Contains(text, "match_key") || strings.Contains(text, "MatchKey") {
		t.Fatal("rate_limit.go still contains deprecated match_key/MatchKey")
	}
	if strings.Contains(text, "attribution_key = ?") {
		t.Fatal("rate-limit evaluator must not match usage by attribution_key")
	}
}
