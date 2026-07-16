package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// CustomerReader implements core.CustomerReader against PostgreSQL. Like
// BalanceRepository, it holds its own *pgxpool.Pool and queries it directly rather than
// via txFromContext: the GET route this reader serves is non-mutating, and
// IdempotencyMiddleware never opens a transaction for non-mutating methods.
type CustomerReader struct {
	pool *pgxpool.Pool
}

// NewCustomerReader constructs a core.CustomerReader backed by pool.
func NewCustomerReader(pool *pgxpool.Pool) *CustomerReader {
	return &CustomerReader{pool: pool}
}

// GetCustomer reads customerID's own record, joined with its deposit address. A single
// joined SELECT — not an existence-check-then-query — because every customer has
// exactly one deposit_addresses row by construction (CustomerRepository.CreateCustomer
// inserts both in the same transaction, AD-4), so "no rows" here unambiguously means "no
// such customer."
func (r *CustomerReader) GetCustomer(ctx context.Context, customerID string) (core.Customer, error) {
	var customer core.Customer
	err := r.pool.QueryRow(ctx,
		`SELECT c.id, c.created_at, d.address
		 FROM customers c
		 JOIN deposit_addresses d ON d.customer_id = c.id
		 WHERE c.id = $1`,
		customerID,
	).Scan(&customer.ID, &customer.CreatedAt, &customer.DepositAddress)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Customer{}, core.ErrCustomerNotFound
		}
		return core.Customer{}, fmt.Errorf("query customer: %w", err)
	}
	return customer, nil
}
