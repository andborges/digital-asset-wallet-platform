package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// CustomerRepository implements core.CustomerRepository against PostgreSQL.
type CustomerRepository struct{}

// NewCustomerRepository constructs a core.CustomerRepository. It carries no pool of
// its own — every call runs against the transaction already open on ctx (AD-4),
// obtained via TxBeginner.Begin by the calling adapter's idempotency middleware.
func NewCustomerRepository() *CustomerRepository {
	return &CustomerRepository{}
}

// CreateCustomer inserts customer and all of accounts using the single transaction
// on ctx — one round-trip's worth of statements, one commit, per AD-4.
func (r *CustomerRepository) CreateCustomer(ctx context.Context, customer core.Customer, accounts []core.Account) error {
	tx := txFromContext(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO customers (id, created_at) VALUES ($1, $2)`,
		customer.ID, customer.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert customer: %w", err)
	}

	batch := &pgx.Batch{}
	for _, acc := range accounts {
		batch.Queue(
			`INSERT INTO accounts (id, customer_id, chain, asset, created_at) VALUES ($1, $2, $3, $4, $5)`,
			acc.ID, acc.CustomerID, string(acc.Chain), string(acc.Asset), acc.CreatedAt,
		)
	}
	br := tx.SendBatch(ctx, batch)
	for range accounts {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("insert account: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("close account batch: %w", err)
	}

	return nil
}
