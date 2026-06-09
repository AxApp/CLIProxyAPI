package accountstore

import (
	"context"
	"errors"
	"fmt"
)

// PurgeSoftDeletedAccounts hard-deletes soft-deleted account cards older than
// cutoffUnixMs. Child credential/runtime rows are removed by SQLite cascades.
func (s *Store) PurgeSoftDeletedAccounts(ctx context.Context, cutoffUnixMs int64, limit int) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("account store is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cutoffUnixMs <= 0 || limit <= 0 {
		return 0, nil
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return 0, fmt.Errorf("enable account store foreign keys: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `
DELETE FROM account_cards
WHERE account_key IN (
  SELECT account_key
  FROM account_cards
  WHERE deleted_at_unix_ms IS NOT NULL
    AND deleted_at_unix_ms > 0
    AND deleted_at_unix_ms <= ?
  ORDER BY deleted_at_unix_ms ASC
  LIMIT ?
)`, cutoffUnixMs, limit)
	if err != nil {
		return 0, fmt.Errorf("purge soft-deleted accounts: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge soft-deleted accounts rows affected: %w", err)
	}
	return int(rows), nil
}
