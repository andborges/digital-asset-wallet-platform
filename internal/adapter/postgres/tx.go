// Package postgres implements the ports declared in internal/core against
// PostgreSQL (pgx v5). It implements core.TxBeginner and core.CustomerRepository
// so that internal/adapter/api's middleware can share a transaction with this
// package's repositories without either adapter importing the other (AD-1, AD-2).
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

type txContextKey struct{}

// TxBeginner implements core.TxBeginner against a pgxpool.Pool.
type TxBeginner struct {
	pool *pgxpool.Pool
}

// NewTxBeginner constructs a core.TxBeginner backed by pool.
func NewTxBeginner(pool *pgxpool.Pool) *TxBeginner {
	return &TxBeginner{pool: pool}
}

// Begin starts a pgx transaction and returns a context carrying it. Repositories in
// this package extract it via txFromContext — callers never see *pgx.Tx directly.
func (b *TxBeginner) Begin(ctx context.Context) (context.Context, core.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return ctx, nil, fmt.Errorf("begin transaction: %w", err)
	}
	return context.WithValue(ctx, txContextKey{}, tx), &txHandle{tx: tx}, nil
}

// txHandle adapts *pgx.Tx to the core.Tx port.
type txHandle struct {
	tx pgx.Tx
}

func (h *txHandle) Commit(ctx context.Context) error {
	return h.tx.Commit(ctx)
}

func (h *txHandle) Rollback(ctx context.Context) error {
	return h.tx.Rollback(ctx)
}

// txFromContext returns the *pgx.Tx placed on ctx by TxBeginner.Begin. Every
// repository in this package requires one to already be present — running a
// mutating query outside a transaction opened by the calling adapter's idempotency
// middleware would violate AD-4, so this is a programming error, not a runtime
// condition to recover from gracefully.
func txFromContext(ctx context.Context) pgx.Tx {
	tx, ok := ctx.Value(txContextKey{}).(pgx.Tx)
	if !ok {
		panic("postgres: no transaction on context — every mutating repository call must run inside a transaction opened via TxBeginner.Begin (AD-4)")
	}
	return tx
}
