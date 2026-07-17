package postgres

import (
	"context"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// UnsupportedTokenRepository implements core.UnsupportedTokenRepository against
// PostgreSQL. RecordObservation runs against whatever transaction is already open on ctx
// (established by TrackDeposits via TxBeginner, the same AD-4 contract as
// DepositRepository), while ListObservations — like DepositReader — queries
// independently via its own pool, since it serves a non-mutating, platform-wide GET
// route IdempotencyMiddleware never opens a transaction for.
type UnsupportedTokenRepository struct {
	pool *pgxpool.Pool
}

// NewUnsupportedTokenRepository constructs a core.UnsupportedTokenRepository backed by
// pool. pool is used only by ListObservations; RecordObservation always runs against the
// transaction on ctx via txFromContext.
func NewUnsupportedTokenRepository(pool *pgxpool.Pool) *UnsupportedTokenRepository {
	return &UnsupportedTokenRepository{pool: pool}
}

// RecordObservation inserts observation in the transaction already open on ctx (AD-4),
// mirroring DepositRepository.RecordObserved's exact idempotency pattern: re-observing
// the same (chain, tx_hash, log_index) on a repoll relies entirely on the DB's UNIQUE
// constraint (INSERT ... ON CONFLICT DO NOTHING, AD-5), never an application-level
// existence check, so a conflict is reported as inserted=false, not an error. Unlike a
// deposit, an unsupported-token observation has no paired outbox event — it never
// triggers any downstream action (FR11).
func (r *UnsupportedTokenRepository) RecordObservation(ctx context.Context, observation core.UnsupportedTokenObservation) (bool, error) {
	tx := txFromContext(ctx)

	tag, err := tx.Exec(ctx,
		`INSERT INTO unsupported_token_observations (id, chain, address, contract_address, tx_hash, log_index, amount, block_number, observed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9)
		 ON CONFLICT (chain, tx_hash, log_index) DO NOTHING`,
		observation.ID, string(observation.Chain), observation.Address, observation.ContractAddress,
		observation.TxHash, observation.LogIndex, observation.Amount.String(), observation.BlockNumber, observation.ObservedAt,
	)
	if err != nil {
		return false, fmt.Errorf("insert unsupported token observation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// ON CONFLICT DO NOTHING fired — this exact (chain, tx_hash, log_index) was
		// already recorded by an earlier poll. A no-op by construction (AD-5).
		return false, nil
	}
	return true, nil
}

// maxUnsupportedObservationsPerList caps how many rows ListObservations returns
// (re-review 2026-07-17): this is a platform-wide, unpaginated GET route, and unfiltered-
// by-contract scanning means an attacker can cheaply generate many distinct observations
// (each a different tx_hash, so the UNIQUE constraint doesn't dedupe across transactions).
// A fixed cap bounds worst-case response size and query cost; full cursor-based pagination
// (Story 1.4's pattern) is a larger follow-up if actual volume ever warrants it — AC3 only
// requires visibility for manual triage, not scale.
const maxUnsupportedObservationsPerList = 500

// ListObservations returns up to maxUnsupportedObservationsPerList recorded
// unsupported-token observations, newest first — a flat, platform-wide list (no customer
// scoping: this is operator-facing, Story 2.3 AC3) for manual triage.
func (r *UnsupportedTokenRepository) ListObservations(ctx context.Context) ([]core.UnsupportedTokenObservation, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, chain, address, contract_address, tx_hash, log_index, amount::text, block_number, observed_at
		 FROM unsupported_token_observations
		 ORDER BY observed_at DESC
		 LIMIT $1`,
		maxUnsupportedObservationsPerList,
	)
	if err != nil {
		return nil, fmt.Errorf("query unsupported token observations: %w", err)
	}
	defer rows.Close()

	var observations []core.UnsupportedTokenObservation
	for rows.Next() {
		var (
			o          core.UnsupportedTokenObservation
			chain      string
			amountText string
		)
		if err := rows.Scan(&o.ID, &chain, &o.Address, &o.ContractAddress, &o.TxHash, &o.LogIndex, &amountText, &o.BlockNumber, &o.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan unsupported token observation row: %w", err)
		}
		amount, ok := new(big.Int).SetString(amountText, 10)
		if !ok {
			return nil, fmt.Errorf("parse unsupported token observation amount %q as integer", amountText)
		}
		o.Chain = core.Chain(chain)
		o.Amount = amount
		observations = append(observations, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unsupported token observation rows: %w", err)
	}
	return observations, nil
}

var _ core.UnsupportedTokenRepository = (*UnsupportedTokenRepository)(nil)
