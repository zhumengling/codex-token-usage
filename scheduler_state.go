package main

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"
)

// schedulerStateCache lets the common, healthy-account path avoid SQLite.
// Unknown state deliberately falls back to the database so failures never
// disable account filtering.
type schedulerStateCache struct {
	mu              sync.RWMutex
	initialized     bool
	codexRestricted bool
	xaiRestricted   bool
	codexResetAt    int64
	xaiResetAt      int64
}

var globalSchedulerState schedulerStateCache

func (c *schedulerStateCache) invalidate() {
	c.mu.Lock()
	c.initialized = false
	c.mu.Unlock()
}

func (c *schedulerStateCache) needsDatabase(provider string, protectionEnabled bool) bool {
	if protectionEnabled && strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.initialized {
		return true
	}
	now := time.Now().Unix()
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		if c.codexRestricted && c.codexResetAt > 0 && c.codexResetAt <= now {
			return true
		}
		return c.codexRestricted
	case "xai":
		if c.xaiRestricted && c.xaiResetAt > 0 && c.xaiResetAt <= now {
			return true
		}
		return c.xaiRestricted
	default:
		return false
	}
}

func (c *schedulerStateCache) refresh(ctx context.Context, db *sql.DB) error {
	now := time.Now().Unix()
	var activeBans, activeInvalids, activeXAI int
	var codexResetAt, xaiResetAt int64
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MIN(reset_at),0) FROM autoban_bans WHERE active=1 AND reset_at>?`, now).Scan(&activeBans, &codexResetAt); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM invalid_auths WHERE active=1`).Scan(&activeInvalids); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MIN(CASE WHEN reset_at>0 THEN reset_at END),0)
FROM xai_account_states WHERE active=1 AND (reset_at=0 OR reset_at>?)`, now).Scan(&activeXAI, &xaiResetAt); err != nil {
		return err
	}
	c.mu.Lock()
	c.initialized = true
	c.codexRestricted = activeBans > 0 || activeInvalids > 0
	c.xaiRestricted = activeXAI > 0
	c.codexResetAt = codexResetAt
	c.xaiResetAt = xaiResetAt
	c.mu.Unlock()
	return nil
}

func (c *schedulerStateCache) setRestricted(provider string, restricted bool) {
	c.mu.Lock()
	c.initialized = true
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		c.codexRestricted = restricted
		if !restricted {
			c.codexResetAt = 0
		}
	case "xai":
		c.xaiRestricted = restricted
		if !restricted {
			c.xaiResetAt = 0
		}
	}
	c.mu.Unlock()
}

func (s *store) refreshSchedulerState(ctx context.Context) error {
	db, _, err := s.open(ctx)
	if err != nil {
		globalSchedulerState.invalidate()
		return err
	}
	if err := globalSchedulerState.refresh(ctx, db); err != nil {
		globalSchedulerState.invalidate()
		return err
	}
	return nil
}

func resetSchedulerStateForTest() {
	globalSchedulerState = schedulerStateCache{}
}
