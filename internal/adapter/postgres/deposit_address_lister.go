package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// DepositAddressLister implements core.DepositAddressLister against PostgreSQL. Like
// BalanceRepository, it holds its own *pgxpool.Pool and queries it directly rather than
// via txFromContext: listing known addresses is a plain read TrackDeposits performs
// before it opens its write transaction, not a write that needs to share it.
type DepositAddressLister struct {
	pool *pgxpool.Pool
}

// NewDepositAddressLister constructs a core.DepositAddressLister backed by pool.
func NewDepositAddressLister(pool *pgxpool.Pool) *DepositAddressLister {
	return &DepositAddressLister{pool: pool}
}

// ListDepositAddresses returns every customer deposit address currently provisioned
// (Story 1.5's deposit_addresses table) — the known-address set TrackDeposits scans
// against. Reloaded from this table every poll cycle (simple and correct; scaling this
// is not this story's concern).
func (r *DepositAddressLister) ListDepositAddresses(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT address FROM deposit_addresses`)
	if err != nil {
		return nil, fmt.Errorf("query deposit addresses: %w", err)
	}
	defer rows.Close()

	var addresses []string
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, fmt.Errorf("scan deposit address: %w", err)
		}
		addresses = append(addresses, address)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deposit addresses: %w", err)
	}
	return addresses, nil
}

var _ core.DepositAddressLister = (*DepositAddressLister)(nil)
