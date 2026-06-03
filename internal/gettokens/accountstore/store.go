package accountstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = "1"
const sqliteBusyTimeoutMs = 5000

var accountKeyPattern = regexp.MustCompile(`^acct_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Store struct {
	path string
	db   *sql.DB
}

func Open(path string) (*Store, error) {
	clean := filepath.Clean(path)
	if clean == "." || clean == "" {
		return nil, errors.New("empty account store path")
	}
	dir := filepath.Dir(clean)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create account store dir: %w", err)
	}
	_ = os.Chmod(dir, 0700)

	file, err := os.OpenFile(clean, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("create account store db: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close account store db: %w", err)
	}
	_ = os.Chmod(clean, 0600)

	db, err := sql.Open("sqlite", sqliteDSN(clean))
	if err != nil {
		return nil, fmt.Errorf("open account store sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping account store sqlite: %w", err)
	}
	_ = os.Chmod(clean, 0600)

	return &Store{path: clean, db: db}, nil
}

func sqliteDSN(path string) string {
	openPath := path
	if abs, err := filepath.Abs(path); err == nil {
		openPath = abs
	}
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(openPath)}
	query := dsn.Query()
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMs))
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable account store foreign keys: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, accountStoreSchema); err != nil {
		return fmt.Errorf("ensure account store schema: %w", err)
	}
	if err := ensureTextColumn(ctx, s.db, "openai_compatible_accounts", "format_base_urls_json", "'{}'"); err != nil {
		return fmt.Errorf("ensure openai-compatible format base URLs column: %w", err)
	}
	if err := ensureTextColumn(ctx, s.db, "codex_api_key_accounts", "platform_cookie", "''"); err != nil {
		return fmt.Errorf("ensure codex api key platform cookie column: %w", err)
	}
	now := fmt.Sprintf("%d", time.Now().UnixMilli())
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO account_store_meta(key, value) VALUES
  ('schema_version', ?),
  ('created_at_unix_ms', ?),
  ('owner', 'sidecar')`, schemaVersion, now)
	if err != nil {
		return fmt.Errorf("ensure account store metadata: %w", err)
	}
	return nil
}

func ensureTextColumn(ctx context.Context, db *sql.DB, table string, column string, defaultValue string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s TEXT NOT NULL DEFAULT %s", table, column, defaultValue))
	return err
}

func IsAccountKey(value string) bool {
	return accountKeyPattern.MatchString(value)
}

func newAccountKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"acct_%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}

const accountStoreSchema = `
CREATE TABLE IF NOT EXISTS account_store_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_cards (
  account_key TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  credential_source TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0,
  revision INTEGER NOT NULL DEFAULT 1,
  metadata_json TEXT NOT NULL DEFAULT '{}',
  created_at_unix_ms INTEGER NOT NULL,
  updated_at_unix_ms INTEGER NOT NULL,
  deleted_at_unix_ms INTEGER
);

CREATE INDEX IF NOT EXISTS idx_account_cards_kind
  ON account_cards(kind);

CREATE INDEX IF NOT EXISTS idx_account_cards_deleted
  ON account_cards(deleted_at_unix_ms);

CREATE INDEX IF NOT EXISTS idx_account_cards_updated
  ON account_cards(updated_at_unix_ms);

CREATE TABLE IF NOT EXISTS codex_api_key_accounts (
  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
  api_key TEXT NOT NULL,
  api_key_fingerprint TEXT NOT NULL DEFAULT '',
  base_url TEXT NOT NULL,
  prefix TEXT NOT NULL DEFAULT '',
  proxy_url TEXT NOT NULL DEFAULT '',
  websockets INTEGER NOT NULL DEFAULT 1,
  quota_curl TEXT NOT NULL DEFAULT '',
  quota_enabled INTEGER NOT NULL DEFAULT 0,
  billing_curl TEXT NOT NULL DEFAULT '',
  billing_enabled INTEGER NOT NULL DEFAULT 0,
  platform_cookie TEXT NOT NULL DEFAULT '', 
  curl_variables_json TEXT NOT NULL DEFAULT '{}',
  format_base_urls_json TEXT NOT NULL DEFAULT '{}',
  headers_json TEXT NOT NULL DEFAULT '{}',
  models_json TEXT NOT NULL DEFAULT '[]',
  excluded_models_json TEXT NOT NULL DEFAULT '[]',
  updated_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_codex_api_key_runtime_config
  ON codex_api_key_accounts(base_url, prefix);

CREATE INDEX IF NOT EXISTS idx_codex_api_key_fingerprint
  ON codex_api_key_accounts(api_key_fingerprint);

CREATE TABLE IF NOT EXISTS auth_file_accounts (
  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
  source_file_name TEXT NOT NULL DEFAULT '',
  auth_json TEXT NOT NULL,
  auth_fingerprint TEXT NOT NULL DEFAULT '',
  auth_type TEXT NOT NULL DEFAULT '',
  email TEXT NOT NULL DEFAULT '',
  plan_type TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  status_message TEXT NOT NULL DEFAULT '',
  modified_unix_ms INTEGER NOT NULL DEFAULT 0,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  updated_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auth_file_source_file_name
  ON auth_file_accounts(source_file_name);

CREATE INDEX IF NOT EXISTS idx_auth_file_fingerprint
  ON auth_file_accounts(auth_fingerprint);

CREATE TABLE IF NOT EXISTS openai_compatible_accounts (
  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
  provider_name TEXT NOT NULL DEFAULT '',
  runtime_provider_key TEXT NOT NULL,
  base_url TEXT NOT NULL,
  prefix TEXT NOT NULL DEFAULT '',
  api_key_entries_json TEXT NOT NULL DEFAULT '[]',
  headers_json TEXT NOT NULL DEFAULT '{}',
  format_base_urls_json TEXT NOT NULL DEFAULT '{}',
  models_json TEXT NOT NULL DEFAULT '[]',
  updated_at_unix_ms INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_openai_compatible_runtime_provider_key
  ON openai_compatible_accounts(runtime_provider_key);

CREATE TABLE IF NOT EXISTS account_runtime_identities (
  identity_key TEXT PRIMARY KEY,
  account_key TEXT NOT NULL REFERENCES account_cards(account_key) ON DELETE CASCADE,
  identity_kind TEXT NOT NULL,
  created_at_unix_ms INTEGER NOT NULL,
  updated_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_account_runtime_identities_account
  ON account_runtime_identities(account_key);

CREATE TABLE IF NOT EXISTS account_runtime_apply_state (
  account_key TEXT PRIMARY KEY REFERENCES account_cards(account_key) ON DELETE CASCADE,
  revision INTEGER NOT NULL,
  status TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  applied_at_unix_ms INTEGER NOT NULL DEFAULT 0,
  updated_at_unix_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_account_runtime_apply_state_status
  ON account_runtime_apply_state(status);

CREATE TABLE IF NOT EXISTS account_migration_sources (
  id TEXT PRIMARY KEY,
  account_key TEXT NOT NULL REFERENCES account_cards(account_key) ON DELETE CASCADE,
  source_kind TEXT NOT NULL,
  source_path TEXT NOT NULL DEFAULT '',
  source_key TEXT NOT NULL DEFAULT '',
  source_fingerprint TEXT NOT NULL DEFAULT '',
  imported_at_unix_ms INTEGER NOT NULL,
  deleted_at_unix_ms INTEGER NOT NULL DEFAULT 0,
  backup_path TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_account_migration_sources_account
  ON account_migration_sources(account_key);

CREATE INDEX IF NOT EXISTS idx_account_migration_sources_kind
  ON account_migration_sources(source_kind);
`
