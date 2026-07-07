package gettokenshooks

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const liveSessionHistorySchema = `
CREATE TABLE IF NOT EXISTS live_session_requests (
  request_key TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  sequence INTEGER NOT NULL DEFAULT 0,
  started_at_unix_ms INTEGER NOT NULL,
  completed_at_unix_ms INTEGER NOT NULL DEFAULT 0,
  last_event_unix_ms INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  auth_id TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  downstream_transport TEXT NOT NULL DEFAULT '',
  upstream_transport TEXT NOT NULL DEFAULT '',
  request_json TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_live_session_requests_session_time
  ON live_session_requests(session_id, last_event_unix_ms);
CREATE INDEX IF NOT EXISTS idx_live_session_requests_time
  ON live_session_requests(last_event_unix_ms);`

const (
	liveSessionHistoryRetention     = 14 * 24 * time.Hour
	liveSessionHistoryPruneInterval = 5 * time.Minute
)

type LiveSessionHistoryOptions struct {
	ConfigFilePath string
	WritableBase   string
}

type LiveSessionHistoryResponse struct {
	Window      string        `json:"window"`
	GeneratedAt string        `json:"generatedAt"`
	Limit       int           `json:"limit"`
	Offset      int           `json:"offset"`
	Items       []LiveRequest `json:"items"`
}

type liveSessionHistoryStore struct {
	db           *sql.DB
	pruneMu      sync.Mutex
	lastPrunedAt time.Time
}

var (
	liveSessionHistoryMu      sync.RWMutex
	defaultLiveSessionHistory *liveSessionHistoryStore
)

func InstallLiveSessionHistoryHook(opts LiveSessionHistoryOptions) error {
	dbPath, err := resolveLiveSessionHistoryPath(opts)
	if err != nil {
		return fmt.Errorf("resolve live session history path: %w", err)
	}
	store, err := newLiveSessionHistoryStore(dbPath)
	if err != nil {
		return fmt.Errorf("open live session history store: %w", err)
	}
	liveSessionHistoryMu.Lock()
	defaultLiveSessionHistory = store
	liveSessionHistoryMu.Unlock()
	return nil
}

func resolveLiveSessionHistoryPath(opts LiveSessionHistoryOptions) (string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv("GETTOKENS_LIVE_SESSIONS_SQLITE_PATH")); fromEnv != "" {
		return fromEnv, nil
	}
	if configPath := strings.TrimSpace(opts.ConfigFilePath); configPath != "" {
		return filepath.Join(filepath.Dir(configPath), "live-sessions-v1.sqlite"), nil
	}
	if writableBase := strings.TrimSpace(opts.WritableBase); writableBase != "" {
		return filepath.Join(writableBase, "live-sessions-v1.sqlite"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gettokens", "live-sessions-v1.sqlite"), nil
}

func newLiveSessionHistoryStore(path string) (*liveSessionHistoryStore, error) {
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
	if _, err := db.Exec(liveSessionHistorySchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &liveSessionHistoryStore{db: db}
	if err := store.pruneExpired(time.Now().UTC(), true); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func currentLiveSessionHistoryStore() *liveSessionHistoryStore {
	liveSessionHistoryMu.RLock()
	store := defaultLiveSessionHistory
	liveSessionHistoryMu.RUnlock()
	return store
}

func persistLiveRequestHistory(session LiveSession, request LiveRequest) {
	store := currentLiveSessionHistoryStore()
	if store == nil {
		return
	}
	_ = store.upsert(session, request)
}

func (s *liveSessionHistoryStore) upsert(session LiveSession, request LiveRequest) error {
	if s == nil || s.db == nil {
		return errors.New("live session history store is not initialized")
	}
	sessionID := firstNonEmptyString(strings.TrimSpace(request.SessionID), strings.TrimSpace(session.SessionID))
	requestID := strings.TrimSpace(request.RequestID)
	if sessionID == "" || requestID == "" {
		return nil
	}
	request.SessionID = sessionID
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	startedAt := parseLiveTime(request.StartedAt)
	if startedAt.IsZero() {
		startedAt = parseLiveTime(session.StartedAt)
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	completedAt := parseLiveTime(request.CompletedAt)
	lastEventAt := completedAt
	if lastEventAt.IsZero() {
		lastEventAt = parseLiveTime(session.LastEventAt)
	}
	if lastEventAt.IsZero() {
		lastEventAt = startedAt
	}
	_, err = s.db.Exec(
		`INSERT INTO live_session_requests (
		  request_key, session_id, request_id, sequence, started_at_unix_ms, completed_at_unix_ms,
		  last_event_unix_ms, status, model, auth_id, provider, downstream_transport, upstream_transport, request_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(request_key) DO UPDATE SET
		  session_id = excluded.session_id,
		  request_id = excluded.request_id,
		  sequence = excluded.sequence,
		  started_at_unix_ms = excluded.started_at_unix_ms,
		  completed_at_unix_ms = excluded.completed_at_unix_ms,
		  last_event_unix_ms = excluded.last_event_unix_ms,
		  status = excluded.status,
		  model = excluded.model,
		  auth_id = excluded.auth_id,
		  provider = excluded.provider,
		  downstream_transport = excluded.downstream_transport,
		  upstream_transport = excluded.upstream_transport,
		  request_json = excluded.request_json`,
		liveRequestHistoryKey(sessionID, requestID),
		sessionID,
		requestID,
		request.Sequence,
		startedAt.UTC().UnixMilli(),
		completedAt.UTC().UnixMilli(),
		lastEventAt.UTC().UnixMilli(),
		request.Status,
		request.Model,
		request.AuthID,
		request.Provider,
		request.DownstreamTransport,
		request.UpstreamTransport,
		string(payload),
	)
	if err != nil {
		return err
	}
	return s.pruneExpired(time.Now().UTC(), false)
}

func (s *liveSessionHistoryStore) pruneExpired(now time.Time, force bool) error {
	if s == nil || s.db == nil || liveSessionHistoryRetention <= 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	if !force && !s.lastPrunedAt.IsZero() && now.Sub(s.lastPrunedAt) < liveSessionHistoryPruneInterval {
		return nil
	}
	cutoff := now.Add(-liveSessionHistoryRetention).UnixMilli()
	if _, err := s.db.Exec(`DELETE FROM live_session_requests WHERE last_event_unix_ms < ?`, cutoff); err != nil {
		return err
	}
	if !force {
		s.lastPrunedAt = now
	}
	return nil
}

func (s *liveSessionHistoryStore) history(window time.Duration, limit int, offset int, sessionID string) (LiveSessionHistoryResponse, error) {
	if s == nil || s.db == nil {
		return LiveSessionHistoryResponse{}, errors.New("live session history store is not initialized")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	now := time.Now().UTC()
	since := int64(0)
	windowLabel := formatUsageAttributionWindow(window)
	if window == 0 {
		window = liveSessionsRetention
		windowLabel = liveSessionsRetention.String()
	}
	if window > 0 {
		since = now.Add(-window).UnixMilli()
	}
	query := strings.Builder{}
	query.WriteString(`SELECT request_json FROM live_session_requests WHERE last_event_unix_ms >= ?`)
	args := []any{since}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		query.WriteString(" AND session_id = ?")
		args = append(args, sessionID)
	}
	query.WriteString(" ORDER BY last_event_unix_ms DESC, sequence DESC, request_id DESC LIMIT ? OFFSET ?")
	args = append(args, limit, offset)
	rows, err := s.db.Query(query.String(), args...)
	if err != nil {
		return LiveSessionHistoryResponse{}, err
	}
	defer rows.Close()
	out := LiveSessionHistoryResponse{
		Window:      windowLabel,
		GeneratedAt: now.Format(time.RFC3339),
		Limit:       limit,
		Offset:      offset,
		Items:       []LiveRequest{},
	}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return LiveSessionHistoryResponse{}, err
		}
		var request LiveRequest
		if err := json.Unmarshal([]byte(payload), &request); err != nil {
			return LiveSessionHistoryResponse{}, err
		}
		out.Items = append(out.Items, request)
	}
	if err := rows.Err(); err != nil {
		return LiveSessionHistoryResponse{}, err
	}
	return out, nil
}

func liveRequestHistoryKey(sessionID string, requestID string) string {
	return strings.TrimSpace(sessionID) + "\x00" + strings.TrimSpace(requestID)
}
