package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// WatcherLiveness implements core.WatcherLivenessChecker against PostgreSQL's existing
// watcher_cursors table (Story 2.1) — the same small-repo shape as WithdrawalThresholdLister/
// TokenRegistryLister: it holds its own *pgxpool.Pool and queries it directly rather than
// via txFromContext, since IsLive's sole caller (Story 3.5's runBroadcaster poll loop)
// checks liveness BEFORE opening any write transaction, exactly like those two readers are
// consulted before their own callers open theirs.
type WatcherLiveness struct {
	pool *pgxpool.Pool
}

// NewWatcherLiveness constructs a core.WatcherLivenessChecker backed by pool.
func NewWatcherLiveness(pool *pgxpool.Pool) *WatcherLiveness {
	return &WatcherLiveness{pool: pool}
}

// IsLive implements core.WatcherLivenessChecker: reads MAX(watcher_cursors.updated_at) for
// chain's "observed" tier (Code Map's exact query) and compares it against now()-staleAfter.
// No row at all — a chain the watcher has never polled — returns false, never true (Code
// Map: "no row at all means not live"), which MAX's own NULL-on-no-rows behavior gives for
// free: scanning a NULL max(updated_at) into a nullable *time.Time leaves it nil, handled
// below as the same "not live" outcome as a merely-stale timestamp.
func (l *WatcherLiveness) IsLive(ctx context.Context, chain core.Chain, staleAfter time.Duration) (bool, error) {
	var lastUpdated *time.Time
	if err := l.pool.QueryRow(ctx,
		`SELECT max(updated_at) FROM watcher_cursors WHERE chain = $1 AND tier = $2`,
		string(chain), core.CursorTierObserved,
	).Scan(&lastUpdated); err != nil {
		return false, fmt.Errorf("query watcher cursor liveness for chain %q: %w", chain, err)
	}
	if lastUpdated == nil {
		return false, nil
	}
	return time.Since(*lastUpdated) <= staleAfter, nil
}

var _ core.WatcherLivenessChecker = (*WatcherLiveness)(nil)
