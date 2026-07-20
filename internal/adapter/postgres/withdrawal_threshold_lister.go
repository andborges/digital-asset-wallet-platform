package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// ErrNoWithdrawalApprovalThreshold is returned by WithdrawalThresholdLister.
// GetApprovalThreshold when no withdrawal_approval_thresholds row exists for the
// requested (chain, asset) pair — a registry gap (Story 3.3's own I/O matrix: "should
// never happen in a correctly configured deployment"). Never a guessed default: a
// withdrawal must never be silently auto-approved or silently blocked because a threshold
// row is missing.
var ErrNoWithdrawalApprovalThreshold = errors.New("no withdrawal approval threshold configured for this chain/asset")

// WithdrawalThresholdLister implements core.WithdrawalThresholdLister against PostgreSQL —
// the same small-repo shape as TokenRegistry: it holds its own *pgxpool.Pool and queries
// it directly rather than via txFromContext, since CreateWithdrawal (its sole caller)
// reads the threshold BEFORE opening any write, exactly like TokenRegistryLister is read
// before TrackDeposits opens its own write transaction.
type WithdrawalThresholdLister struct {
	pool *pgxpool.Pool
}

// NewWithdrawalThresholdLister constructs a core.WithdrawalThresholdLister backed by pool.
func NewWithdrawalThresholdLister(pool *pgxpool.Pool) *WithdrawalThresholdLister {
	return &WithdrawalThresholdLister{pool: pool}
}

// GetApprovalThreshold returns chain/asset's configured threshold amount, or
// ErrNoWithdrawalApprovalThreshold if no row exists for the pair.
func (l *WithdrawalThresholdLister) GetApprovalThreshold(ctx context.Context, chain core.Chain, asset core.Asset) (*big.Int, error) {
	var thresholdText string
	if err := l.pool.QueryRow(ctx,
		`SELECT threshold_amount::text FROM withdrawal_approval_thresholds WHERE chain = $1 AND asset = $2`,
		string(chain), string(asset),
	).Scan(&thresholdText); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: chain %q, asset %q", ErrNoWithdrawalApprovalThreshold, chain, asset)
		}
		return nil, fmt.Errorf("query withdrawal approval threshold: %w", err)
	}
	threshold, ok := new(big.Int).SetString(thresholdText, 10)
	if !ok {
		return nil, fmt.Errorf("parse withdrawal approval threshold %q as integer", thresholdText)
	}
	return threshold, nil
}

var _ core.WithdrawalThresholdLister = (*WithdrawalThresholdLister)(nil)
