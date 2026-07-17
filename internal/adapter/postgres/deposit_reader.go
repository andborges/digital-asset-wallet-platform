package postgres

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// DepositReader implements core.DepositReader against PostgreSQL. Like
// BalanceRepository, it holds its own *pgxpool.Pool and queries it directly rather than
// via txFromContext: the GET route this reader serves is non-mutating, and
// IdempotencyMiddleware never opens a transaction for it.
type DepositReader struct {
	pool *pgxpool.Pool
}

// NewDepositReader constructs a core.DepositReader backed by pool.
func NewDepositReader(pool *pgxpool.Pool) *DepositReader {
	return &DepositReader{pool: pool}
}

// ListCustomerDeposits confirms customerID exists, then reads its deposits joined
// against deposit_addresses — deposits has no customer_id column by design (AD-8): the
// watcher's only attribution key is the address itself, so customer_id is resolved here
// at read time, never looked up mid-scan. Filtered to the observed/safe/orphaned tiers
// this endpoint surfaces (re-review 2026-07-16, widened Story 2.4): without this filter,
// a future finalized/credited row (Story 2.2 writes to this same table) would be
// returned here and the API handler would serialize it as status "pending" with a tier
// value outside the OpenAPI enum — this query, not the handler, is where that must be
// prevented. orphaned is deliberately included (Story 2.4, AC1's "provisional visibility
// reflects this") — a customer must be able to see a deposit was reorged away, not have
// it silently vanish.
func (r *DepositReader) ListCustomerDeposits(ctx context.Context, customerID string) ([]core.Deposit, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)`,
		customerID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check customer exists: %w", err)
	}
	if !exists {
		return nil, core.ErrCustomerNotFound
	}

	rows, err := r.pool.Query(ctx,
		`SELECT d.id, d.chain, d.asset, d.address, d.tx_hash, d.log_index, d.amount::text, d.block_number, d.state, d.observed_at, d.updated_at
		 FROM deposits d
		 JOIN deposit_addresses da ON da.address = d.address
		 WHERE da.customer_id = $1 AND d.state IN ($2, $3, $4)
		 ORDER BY d.observed_at DESC`,
		customerID, string(core.DepositObserved), string(core.DepositSafe), string(core.DepositOrphaned),
	)
	if err != nil {
		return nil, fmt.Errorf("query deposits: %w", err)
	}
	defer rows.Close()

	var deposits []core.Deposit
	for rows.Next() {
		var (
			d          core.Deposit
			chain      string
			asset      string
			state      string
			amountText string
		)
		if err := rows.Scan(&d.ID, &chain, &asset, &d.Address, &d.TxHash, &d.LogIndex, &amountText, &d.BlockNumber, &state, &d.ObservedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan deposit row: %w", err)
		}

		amount, ok := new(big.Int).SetString(amountText, 10)
		if !ok {
			return nil, fmt.Errorf("parse deposit amount %q as integer", amountText)
		}

		d.Chain = core.Chain(chain)
		d.Asset = core.Asset(asset)
		d.State = core.DepositState(state)
		d.Amount = amount
		deposits = append(deposits, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deposit rows: %w", err)
	}

	return deposits, nil
}

var _ core.DepositReader = (*DepositReader)(nil)
