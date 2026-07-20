package postgres_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/postgres"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// TestWithdrawalThresholdLister_GetApprovalThreshold exercises the small repo against a
// real Postgres container (newTestPool, shared with withdrawal_repo_test.go in this same
// package) — migration 0010 seeds one placeholder row per core.SupportedChainAssetPairs
// entry, so every supported (chain, asset) pair should resolve without any test-specific
// fixture.
func TestWithdrawalThresholdLister_GetApprovalThreshold(t *testing.T) {
	pool := newTestPool(t)
	lister := postgres.NewWithdrawalThresholdLister(pool)

	t.Run("returns the seeded threshold for every supported (chain, asset) pair", func(t *testing.T) {
		for _, pair := range core.SupportedChainAssetPairs {
			got, err := lister.GetApprovalThreshold(context.Background(), pair.Chain, pair.Asset)
			if err != nil {
				t.Fatalf("(%s, %s): unexpected error: %v", pair.Chain, pair.Asset, err)
			}
			if got == nil || got.Sign() <= 0 {
				t.Fatalf("(%s, %s): threshold = %v, want a positive seeded placeholder value", pair.Chain, pair.Asset, got)
			}
		}
	})

	t.Run("returns ErrNoWithdrawalApprovalThreshold for an unrecognized (chain, asset) pair", func(t *testing.T) {
		// core.Chain/core.Asset are just strings at the port boundary — the CHECK
		// constraint on withdrawal_approval_thresholds rejects an unsupported chain/asset
		// outright, but a well-formed, supported chain crossed with an asset that has no
		// row (should never actually happen given the 4-pair seed, but the registry-gap
		// path itself must still be provably reachable and never silently wrong).
		_, err := lister.GetApprovalThreshold(context.Background(), core.ChainBase, core.Asset("nonexistent"))
		if !errors.Is(err, postgres.ErrNoWithdrawalApprovalThreshold) {
			t.Fatalf("err = %v, want ErrNoWithdrawalApprovalThreshold", err)
		}
	})

	t.Run("updating a threshold row is reflected on the next read", func(t *testing.T) {
		if _, err := pool.Exec(context.Background(),
			`UPDATE withdrawal_approval_thresholds SET threshold_amount = $1 WHERE chain = 'base' AND asset = 'eth'`,
			"42",
		); err != nil {
			t.Fatalf("update threshold fixture: %v", err)
		}

		got, err := lister.GetApprovalThreshold(context.Background(), core.ChainBase, core.AssetETH)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Cmp(big.NewInt(42)) != 0 {
			t.Fatalf("threshold = %s, want 42", got)
		}
	})
}
