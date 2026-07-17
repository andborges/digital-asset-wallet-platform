package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// TokenRegistry implements core.TokenRegistryLister against PostgreSQL, plus UpsertToken
// — the watcher-startup write path that keeps the registry in sync with the operator-
// configured *_USDC_ADDRESS env vars (Story 2.3, Design Notes). Like BalanceRepository,
// it holds its own *pgxpool.Pool and queries it directly rather than via txFromContext:
// UpsertToken runs once at watcher startup, not as part of any per-poll transaction, and
// ListTokenRegistry is a plain read TrackDeposits performs before it opens its write
// transaction, the same shape as DepositAddressLister.
type TokenRegistry struct {
	pool *pgxpool.Pool
}

// NewTokenRegistry constructs a core.TokenRegistryLister (and its UpsertToken
// counterpart) backed by pool.
func NewTokenRegistry(pool *pgxpool.Pool) *TokenRegistry {
	return &TokenRegistry{pool: pool}
}

// ListTokenRegistry returns chain's configured (contract_address -> asset) map, keyed by
// lowercase-normalized hex contract address — the same canonical case UpsertToken stores
// and evm.Scanner's lookup queries against, so a checksum-casing mismatch between the two
// sides never causes a spurious miss.
func (r *TokenRegistry) ListTokenRegistry(ctx context.Context, chain core.Chain) (map[string]core.Asset, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT contract_address, asset FROM token_registry WHERE chain = $1`,
		string(chain),
	)
	if err != nil {
		return nil, fmt.Errorf("query token registry: %w", err)
	}
	defer rows.Close()

	registry := make(map[string]core.Asset)
	for rows.Next() {
		var contractAddress, asset string
		if err := rows.Scan(&contractAddress, &asset); err != nil {
			return nil, fmt.Errorf("scan token registry row: %w", err)
		}
		// Defense in depth alongside the DB's own CHECK (asset = 'usdc') (re-review
		// 2026-07-17): reject a value the rest of the system can't interpret rather than
		// silently casting an unrecognized string into core.Asset and letting it flow
		// into a real ObservedTransfer/Deposit downstream.
		if core.Asset(asset) != core.AssetUSDC {
			return nil, fmt.Errorf("token_registry row for %s has unrecognized asset %q", contractAddress, asset)
		}
		registry[strings.ToLower(contractAddress)] = core.Asset(asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token registry rows: %w", err)
	}
	return registry, nil
}

// UpsertToken records that chain's contractAddress identifies asset, called once at
// watcher startup (cmd/walletd/main.go's runWatcher) — never part of the per-poll
// transaction TrackDeposits opens. contractAddress is stored lowercased, matching
// ListTokenRegistry's own canonical case. A restart re-upserts the same row
// (ON CONFLICT DO UPDATE), keeping the registry in sync with the operator's current
// *_USDC_ADDRESS configuration on every restart, while an operator's own manually
// inserted row for a genuinely new ERC-20 is left untouched (FR34).
func (r *TokenRegistry) UpsertToken(ctx context.Context, chain, contractAddress, asset string) error {
	if _, err := r.pool.Exec(ctx,
		`INSERT INTO token_registry (chain, contract_address, asset) VALUES ($1, $2, $3)
		 ON CONFLICT (chain, contract_address) DO UPDATE SET asset = EXCLUDED.asset`,
		chain, strings.ToLower(contractAddress), asset,
	); err != nil {
		return fmt.Errorf("upsert token registry row: %w", err)
	}
	return nil
}

var _ core.TokenRegistryLister = (*TokenRegistry)(nil)
