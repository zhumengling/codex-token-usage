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
	mu               sync.RWMutex
	codexInitialized bool
	xaiInitialized   bool
	codexGeneration  uint64
	xaiGeneration    uint64
	codexRestricted  bool
	xaiRestricted    bool
	codexResetAt     int64
	xaiResetAt       int64
}

type schedulerRestrictionState struct {
	codexRestricted bool
	xaiRestricted   bool
	codexResetAt    int64
	xaiResetAt      int64
}

var globalSchedulerState schedulerStateCache

func (c *schedulerStateCache) invalidate() {
	c.mu.Lock()
	c.codexInitialized = false
	c.xaiInitialized = false
	c.codexGeneration++
	c.xaiGeneration++
	c.mu.Unlock()
}

func (c *schedulerStateCache) needsDatabase(provider string, protectionEnabled bool) bool {
	if protectionEnabled && strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now().Unix()
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		if !c.codexInitialized {
			return true
		}
		if c.codexRestricted && c.codexResetAt > 0 && c.codexResetAt <= now {
			return true
		}
		return c.codexRestricted
	case "xai":
		if !c.xaiInitialized {
			return true
		}
		if c.xaiRestricted && c.xaiResetAt > 0 && c.xaiResetAt <= now {
			return true
		}
		return c.xaiRestricted
	default:
		return false
	}
}

func (c *schedulerStateCache) refresh(ctx context.Context, db *sql.DB) error {
	return c.refreshWithLoader(func() (schedulerRestrictionState, error) {
		now := time.Now().Unix()
		var state schedulerRestrictionState
		var activeBans, activeInvalids, activeXAI int
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MIN(reset_at),0) FROM autoban_bans WHERE active=1 AND reset_at>?`, now).Scan(&activeBans, &state.codexResetAt); err != nil {
			return schedulerRestrictionState{}, err
		}
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM invalid_auths WHERE active=1`).Scan(&activeInvalids); err != nil {
			return schedulerRestrictionState{}, err
		}
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MIN(CASE WHEN reset_at>0 THEN reset_at END),0)
FROM xai_account_states WHERE active=1 AND (reset_at=0 OR reset_at>?)`, now).Scan(&activeXAI, &state.xaiResetAt); err != nil {
			return schedulerRestrictionState{}, err
		}
		state.codexRestricted = activeBans > 0 || activeInvalids > 0
		state.xaiRestricted = activeXAI > 0
		return state, nil
	})
}

func (c *schedulerStateCache) refreshWithLoader(loader func() (schedulerRestrictionState, error)) error {
	c.mu.RLock()
	codexGeneration := c.codexGeneration
	xaiGeneration := c.xaiGeneration
	c.mu.RUnlock()

	state, err := loader()
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.codexGeneration == codexGeneration {
		c.codexInitialized = true
		c.codexRestricted = state.codexRestricted
		c.codexResetAt = state.codexResetAt
	}
	if c.xaiGeneration == xaiGeneration {
		c.xaiInitialized = true
		c.xaiRestricted = state.xaiRestricted
		c.xaiResetAt = state.xaiResetAt
	}
	c.mu.Unlock()
	return nil
}

func (c *schedulerStateCache) setRestricted(provider string, restricted bool) {
	c.mu.Lock()
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		c.codexInitialized = true
		c.codexGeneration++
		c.codexRestricted = restricted
		if !restricted {
			c.codexResetAt = 0
		}
	case "xai":
		c.xaiInitialized = true
		c.xaiGeneration++
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
