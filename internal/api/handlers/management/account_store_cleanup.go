package management

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	accountStoreSoftDeleteRetention      = 24 * time.Hour
	accountStoreSoftDeleteCleanupDelay   = 10 * time.Minute
	accountStoreSoftDeleteCleanupEvery   = 1 * time.Hour
	accountStoreSoftDeleteCleanupTimeout = 2 * time.Second
	accountStoreSoftDeleteCleanupLimit   = 500
)

func (h *Handler) startAccountStoreSoftDeleteCleanup() {
	if h == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.accountStoreCleanupCancel = cancel
	go h.accountStoreSoftDeleteCleanupLoop(ctx)
}

func (h *Handler) StopBackgroundTasks() {
	if h == nil {
		return
	}
	if h.accountStoreCleanupCancel != nil {
		h.accountStoreCleanupCancel()
	}
}

func (h *Handler) accountStoreSoftDeleteCleanupLoop(ctx context.Context) {
	timer := time.NewTimer(accountStoreSoftDeleteCleanupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			purged, err := h.purgeSoftDeletedAccountsOnce(ctx, time.Now())
			if err != nil {
				log.Debugf("account store soft-delete cleanup skipped: %v", err)
			} else if purged > 0 {
				log.Infof("purged %d soft-deleted account store rows", purged)
			}
			timer.Reset(accountStoreSoftDeleteCleanupEvery)
		}
	}
}

func (h *Handler) purgeSoftDeletedAccountsOnce(parent context.Context, now time.Time) (int, error) {
	if h == nil {
		return 0, nil
	}
	if parent == nil {
		parent = context.Background()
	}
	cutoff := now.Add(-accountStoreSoftDeleteRetention).UnixMilli()
	ctx, cancel := context.WithTimeout(parent, accountStoreSoftDeleteCleanupTimeout)
	defer cancel()

	h.accountStoreMu.Lock()
	defer h.accountStoreMu.Unlock()
	if h.accountStore == nil {
		return 0, nil
	}
	return h.accountStore.PurgeSoftDeletedAccounts(ctx, cutoff, accountStoreSoftDeleteCleanupLimit)
}
