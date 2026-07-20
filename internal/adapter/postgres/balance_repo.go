package postgres

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// BalanceRepository implements core.BalanceRepository against PostgreSQL. Unlike
// CustomerRepository, it holds its own *pgxpool.Pool and queries it directly rather
// than via txFromContext: the GET route this repository serves is non-mutating, and
// IdempotencyMiddleware never opens a transaction for non-mutating methods, so
// txFromContext would panic here. A plain read needs no transaction (AD-4 governs
// state changes, not reads) — see IdempotencyStore.Lookup for the same pattern applied
// to its own pre-transaction read.
type BalanceRepository struct {
	pool *pgxpool.Pool
}

// NewBalanceRepository constructs a core.BalanceRepository backed by pool.
func NewBalanceRepository(pool *pgxpool.Pool) *BalanceRepository {
	return &BalanceRepository{pool: pool}
}

// CustomerBalances confirms customerID exists, then derives each of its accounts'
// balances by summing postings (AD-3). Returns core.ErrCustomerNotFound if no customer
// with that id exists — a bare accounts/postings join can't distinguish "customer
// doesn't exist" from "customer exists with no postings yet," since both produce zero
// rows, so existence is checked explicitly first.
func (r *BalanceRepository) CustomerBalances(ctx context.Context, customerID string) ([]core.AccountBalance, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)`,
		customerID,
	).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("check customer exists: %w", err)
	}
	if !exists {
		return nil, core.ErrCustomerNotFound
	}

	// account_type = 'available' (Story 3.2): since that story, every customer has TWO
	// accounts per (chain, asset) — available and hold — so an unfiltered join here would
	// silently net a withdrawal's hold reclassification back to zero (its debit on
	// available and credit on hold are equal and opposite), making a held withdrawal
	// invisible on this endpoint instead of decreasing the visible balance. This endpoint
	// surfaces only the available balance; the hold account is not exposed here (Story
	// 3.2 Verification notes: "introduces no new balances endpoint output for the hold
	// account itself").
	rows, err := r.pool.Query(ctx,
		`SELECT a.chain, a.asset, COALESCE(SUM(p.amount), 0)::text
		 FROM accounts a
		 LEFT JOIN postings p ON p.account_id = a.id
		 WHERE a.customer_id = $1 AND a.account_type = 'available'
		 GROUP BY a.chain, a.asset
		 ORDER BY a.chain, a.asset`,
		customerID,
	)
	if err != nil {
		return nil, fmt.Errorf("query balances: %w", err)
	}
	defer rows.Close()

	var balances []core.AccountBalance
	for rows.Next() {
		var chain, asset, amountText string
		if err := rows.Scan(&chain, &asset, &amountText); err != nil {
			return nil, fmt.Errorf("scan balance row: %w", err)
		}

		amount, ok := new(big.Int).SetString(amountText, 10)
		if !ok {
			return nil, fmt.Errorf("parse balance amount %q as integer", amountText)
		}

		balances = append(balances, core.AccountBalance{
			Chain:   core.Chain(chain),
			Asset:   core.Asset(asset),
			Balance: amount,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate balance rows: %w", err)
	}

	return balances, nil
}
