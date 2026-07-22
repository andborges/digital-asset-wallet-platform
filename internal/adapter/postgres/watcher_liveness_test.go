// This file exercises WatcherLiveness (Story 3.5) against a real PostgreSQL container —
// reusing newTestPool already established in withdrawal_repo_test.go (same postgres_test
// package).
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/postgres"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

func TestWatcherLiveness_IsLive_NoRowAtAll_ReturnsFalse(t *testing.T) {
	pool := newTestPool(t)
	liveness := postgres.NewWatcherLiveness(pool)

	live, err := liveness.IsLive(context.Background(), core.ChainBase, 30*time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if live {
		t.Fatal("live = true, want false — a chain the watcher has never polled has no watcher_cursors row at all")
	}
}

func TestWatcherLiveness_IsLive_FreshCursor_ReturnsTrue(t *testing.T) {
	pool := newTestPool(t)
	liveness := postgres.NewWatcherLiveness(pool)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO watcher_cursors (chain, tier, last_block, updated_at) VALUES ($1, $2, $3, $4)`,
		"base", core.CursorTierObserved, 100, time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert watcher_cursors fixture: %v", err)
	}

	live, err := liveness.IsLive(context.Background(), core.ChainBase, 30*time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !live {
		t.Fatal("live = false, want true — the observed cursor was just updated")
	}
}

func TestWatcherLiveness_IsLive_StaleCursor_ReturnsFalse(t *testing.T) {
	pool := newTestPool(t)
	liveness := postgres.NewWatcherLiveness(pool)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO watcher_cursors (chain, tier, last_block, updated_at) VALUES ($1, $2, $3, $4)`,
		"base", core.CursorTierObserved, 100, time.Now().UTC().Add(-time.Hour),
	); err != nil {
		t.Fatalf("insert watcher_cursors fixture: %v", err)
	}

	live, err := liveness.IsLive(context.Background(), core.ChainBase, 30*time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if live {
		t.Fatal("live = true, want false — the observed cursor was last updated an hour ago, well past the 30s staleness threshold")
	}
}

// TestWatcherLiveness_IsLive_ScopedToChainAndObservedTier proves IsLive never confuses a
// different chain's or a different tier's cursor with the requested (chain, "observed")
// pair.
func TestWatcherLiveness_IsLive_ScopedToChainAndObservedTier(t *testing.T) {
	pool := newTestPool(t)
	liveness := postgres.NewWatcherLiveness(pool)

	now := time.Now().UTC()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO watcher_cursors (chain, tier, last_block, updated_at) VALUES ($1, $2, $3, $4)`,
		"arbitrum", core.CursorTierObserved, 50, now,
	); err != nil {
		t.Fatalf("insert arbitrum watcher_cursors fixture: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO watcher_cursors (chain, tier, last_block, updated_at) VALUES ($1, $2, $3, $4)`,
		"base", core.CursorTierFinalized, 50, now,
	); err != nil {
		t.Fatalf("insert base finalized-tier watcher_cursors fixture: %v", err)
	}

	live, err := liveness.IsLive(context.Background(), core.ChainBase, 30*time.Second)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if live {
		t.Fatal("live = true, want false — base has no 'observed'-tier cursor row, only arbitrum's observed and base's own finalized")
	}
}
