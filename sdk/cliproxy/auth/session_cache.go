package auth

import (
	"sync"
	"time"
)

// sessionEntry stores auth binding with expiration.
type sessionEntry struct {
	authID       string
	expiresAt    time.Time
	failureCount int
	poolEpoch    int64
}

// SessionCache provides TTL-based session to auth mapping with automatic cleanup.
type SessionCache struct {
	mu      sync.RWMutex
	entries map[string]sessionEntry
	ttl     time.Duration
	stopCh  chan struct{}
}

// NewSessionCache creates a cache with the specified TTL.
// A background goroutine periodically cleans expired entries.
func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	c := &SessionCache{
		entries: make(map[string]sessionEntry),
		ttl:     ttl,
		stopCh:  make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

// Get retrieves the auth ID bound to a session, if still valid.
// Does NOT refresh the TTL on access.
func (c *SessionCache) Get(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	return entry.authID, true
}

// GetAndRefresh retrieves the auth ID bound to a session and refreshes TTL on hit.
// This extends the binding lifetime for active sessions.
func (c *SessionCache) GetAndRefresh(sessionID string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	entry, ok := c.entries[sessionID]
	if !ok {
		c.mu.Unlock()
		return "", false
	}
	if now.After(entry.expiresAt) {
		delete(c.entries, sessionID)
		c.mu.Unlock()
		return "", false
	}
	// Refresh TTL on successful access
	entry.expiresAt = now.Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
	return entry.authID, true
}

// Set binds a session to an auth ID with TTL refresh.
func (c *SessionCache) Set(sessionID, authID string) {
	if sessionID == "" || authID == "" {
		return
	}
	c.mu.Lock()
	entry := c.entries[sessionID]
	if entry.authID != authID {
		entry.failureCount = 0
	}
	entry.authID = authID
	entry.expiresAt = time.Now().Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
}

func (c *SessionCache) SetWithEpoch(sessionID, authID string, epoch int64) {
	if sessionID == "" || authID == "" {
		return
	}
	c.mu.Lock()
	entry := c.entries[sessionID]
	if entry.authID != authID {
		entry.failureCount = 0
	}
	entry.authID = authID
	entry.poolEpoch = epoch
	entry.expiresAt = time.Now().Add(c.ttl)
	c.entries[sessionID] = entry
	c.mu.Unlock()
}

func (c *SessionCache) GetEpoch(sessionID string) (string, int64, bool) {
	if sessionID == "" {
		return "", 0, false
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) {
		c.Invalidate(sessionID)
		return "", 0, false
	}
	return entry.authID, entry.poolEpoch, true
}

func (c *SessionCache) FailureCount(sessionID, authID string) int {
	if sessionID == "" || authID == "" {
		return 0
	}
	c.mu.RLock()
	entry, ok := c.entries[sessionID]
	c.mu.RUnlock()
	if !ok || entry.authID != authID {
		return 0
	}
	if time.Now().After(entry.expiresAt) {
		c.Invalidate(sessionID)
		return 0
	}
	return entry.failureCount
}

func (c *SessionCache) IncrementFailure(sessionID, authID string) int {
	if sessionID == "" || authID == "" {
		return 0
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[sessionID]
	if !ok || entry.authID != authID || now.After(entry.expiresAt) {
		return 0
	}
	entry.failureCount++
	entry.expiresAt = now.Add(c.ttl)
	c.entries[sessionID] = entry
	return entry.failureCount
}

func (c *SessionCache) ResetFailure(sessionID, authID string) {
	if sessionID == "" || authID == "" {
		return
	}
	c.mu.Lock()
	entry, ok := c.entries[sessionID]
	if ok && entry.authID == authID {
		entry.failureCount = 0
		entry.expiresAt = time.Now().Add(c.ttl)
		c.entries[sessionID] = entry
	}
	c.mu.Unlock()
}

// Invalidate removes a specific session binding.
func (c *SessionCache) Invalidate(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	delete(c.entries, sessionID)
	c.mu.Unlock()
}

// InvalidateAuth removes all sessions bound to a specific auth ID.
// Used when an auth becomes unavailable.
func (c *SessionCache) InvalidateAuth(authID string) {
	if authID == "" {
		return
	}
	c.mu.Lock()
	for sid, entry := range c.entries {
		if entry.authID == authID {
			delete(c.entries, sid)
		}
	}
	c.mu.Unlock()
}

// Stop terminates the background cleanup goroutine.
func (c *SessionCache) Stop() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
}

func (c *SessionCache) cleanupLoop() {
	ticker := time.NewTicker(c.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

func (c *SessionCache) cleanup() {
	now := time.Now()
	c.mu.Lock()
	for sid, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, sid)
		}
	}
	c.mu.Unlock()
}
