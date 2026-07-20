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

// CreateCustomer inserts customer, all of accounts, and its deposit address using the
// single transaction on ctx — one round-trip's worth of statements, one commit, per
// AD-4. depositAddress is already computed by the caller (core.CreateCustomer); this
// repository only persists it.
func (r *CustomerRepository) CreateCustomer(ctx context.Context, customer core.Customer, accounts []core.Account, depositAddress string) error {
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
			`INSERT INTO accounts (id, customer_id, chain, asset, account_type, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			acc.ID, acc.CustomerID, string(acc.Chain), string(acc.Asset), string(acc.Type), acc.CreatedAt,
		)
	}
	batch.Queue(
		`INSERT INTO deposit_addresses (customer_id, address, created_at) VALUES ($1, $2, $3)`,
		customer.ID, depositAddress, customer.CreatedAt,
	)
	br := tx.SendBatch(ctx, batch)
	for i := 0; i < len(accounts)+1; i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("insert account or deposit address: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("close account/deposit-address batch: %w", err)
	}

	return nil
}
