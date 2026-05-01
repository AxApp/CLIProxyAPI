package gettokenshooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

const (
	usageSnapshotSchema = `
CREATE TABLE IF NOT EXISTS usage_snapshots (
  singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
  version INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  payload_json TEXT NOT NULL
);`
	usageSnapshotVersion = 1
	flushDebounce        = 1500 * time.Millisecond
	flushInterval        = 5 * time.Second
)

type UsagePersistenceOptions struct {
	ConfigFilePath string
	WritableBase   string
}

// InstallUsagePersistenceHook installs a GetTokens-specific usage persistence hook.
// The hook restores the latest snapshot from SQLite into the in-memory usage store,
// then keeps persisting subsequent updates back into the same database.
func InstallUsagePersistenceHook(opts UsagePersistenceOptions) error {
	dbPath, err := resolveUsageSnapshotPath(opts)
	if err != nil {
		return fmt.Errorf("resolve usage snapshot path: %w", err)
	}

	store, err := newUsageSnapshotStore(dbPath)
	if err != nil {
		return fmt.Errorf("open usage snapshot store: %w", err)
	}

	if restored, err := store.load(); err != nil {
		return fmt.Errorf("load usage snapshot: %w", err)
	} else if restored != nil {
		result := usage.GetRequestStatistics().MergeSnapshot(*restored)
		log.Infof(
			"gettokenshooks: restored usage snapshot from %s (added=%d skipped=%d)",
			dbPath,
			result.Added,
			result.Skipped,
		)
	} else {
		log.Infof("gettokenshooks: usage snapshot store ready at %s", dbPath)
	}

	coreusage.RegisterPlugin(newUsagePersistencePlugin(store))
	return nil
}

func resolveUsageSnapshotPath(opts UsagePersistenceOptions) (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("GETTOKENS_USAGE_SQLITE_PATH")); fromEnv != "" {
		return fromEnv, nil
	}
	if configPath := strings.TrimSpace(opts.ConfigFilePath); configPath != "" {
		return filepath.Join(filepath.Dir(configPath), "usage-observed-v1.sqlite"), nil
	}
	if writableBase := strings.TrimSpace(opts.WritableBase); writableBase != "" {
		return filepath.Join(writableBase, "usage-observed-v1.sqlite"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gettokens", "usage-observed-v1.sqlite"), nil
}

type usagePersistencePlugin struct {
	store     *usageSnapshotStore
	flushCh   chan struct{}
	startOnce sync.Once
}

func newUsagePersistencePlugin(store *usageSnapshotStore) *usagePersistencePlugin {
	plugin := &usagePersistencePlugin{
		store:   store,
		flushCh: make(chan struct{}, 1),
	}
	plugin.startOnce.Do(func() {
		go plugin.run()
	})
	return plugin
}

func (p *usagePersistencePlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.store == nil {
		return
	}
	select {
	case p.flushCh <- struct{}{}:
	default:
	}
}

func (p *usagePersistencePlugin) run() {
	ticker := time.NewTicker(resolveFlushInterval())
	defer ticker.Stop()
	var timer *time.Timer
	var timerCh <-chan time.Time
	for {
		select {
		case _, ok := <-p.flushCh:
			if !ok {
				if timer != nil {
					timer.Stop()
				}
				return
			}
			if timer == nil {
				timer = time.NewTimer(flushDebounce)
				timerCh = timer.C
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(flushDebounce)
		case <-timerCh:
			if err := p.store.save(usage.GetRequestStatistics().Snapshot()); err != nil {
				log.WithError(err).Warn("gettokenshooks: persist usage snapshot failed")
			}
			timerCh = nil
			timer = nil
		case <-ticker.C:
			if err := p.store.save(usage.GetRequestStatistics().Snapshot()); err != nil {
				log.WithError(err).Warn("gettokenshooks: periodic usage snapshot persist failed")
			}
		}
	}
}

func resolveFlushInterval() time.Duration {
	if fromEnv := strings.TrimSpace(os.Getenv("GETTOKENS_USAGE_FLUSH_INTERVAL")); fromEnv != "" {
		if parsed, err := time.ParseDuration(fromEnv); err == nil && parsed > 0 {
			return parsed
		}
	}
	return flushInterval
}

type usageSnapshotStore struct {
	db *sql.DB
}

func newUsageSnapshotStore(path string) (*usageSnapshotStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty sqlite path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(usageSnapshotSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &usageSnapshotStore{db: db}, nil
}

func (s *usageSnapshotStore) load() (*usage.StatisticsSnapshot, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("usage snapshot store is not initialized")
	}
	row := s.db.QueryRow(`SELECT payload_json FROM usage_snapshots WHERE singleton_id = 1`)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var snapshot usage.StatisticsSnapshot
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *usageSnapshotStore) save(snapshot usage.StatisticsSnapshot) error {
	if s == nil || s.db == nil {
		return errors.New("usage snapshot store is not initialized")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO usage_snapshots (singleton_id, version, updated_at, payload_json)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(singleton_id) DO UPDATE SET
		   version = excluded.version,
		   updated_at = excluded.updated_at,
		   payload_json = excluded.payload_json`,
		usageSnapshotVersion,
		time.Now().UTC().Format(time.RFC3339Nano),
		string(payload),
	)
	return err
}
