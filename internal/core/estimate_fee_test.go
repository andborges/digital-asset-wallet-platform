package core_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeFeeEstimator is a test double for core.FeeEstimator. It records whether
// EstimateFee was ever invoked, so tests can assert that use-case-level amount
// validation rejects a request before the port — and therefore before any RPC call — is
// ever reached (mirroring fakeTransferRepository's exact pattern in create_transfer_test.go).
type fakeFeeEstimator struct {
	calls  int
	result core.FeeEstimate
	err    error
}

func (f *fakeFeeEstimator) EstimateFee(_ context.Context, _ core.Chain, _ core.Asset, _ *big.Int) (core.FeeEstimate, error) {
	f.calls++
	return f.result, f.err
}

func TestEstimateFee_Execute(t *testing.T) {
	t.Run("rejects a nil amount before calling the port", func(t *testing.T) {
		estimator := &fakeFeeEstimator{}
		uc := core.NewEstimateFee(estimator)

		_, err := uc.Execute(context.Background(), core.ChainBase, core.AssetETH, nil)
		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("Execute() error = %v, want ErrNonPositiveAmount", err)
		}
		if estimator.calls != 0 {
			t.Fatalf("port called %d times, want 0 (rejected before delegating)", estimator.calls)
		}
	})

	t.Run("rejects a zero amount before calling the port", func(t *testing.T) {
		estimator := &fakeFeeEstimator{}
		uc := core.NewEstimateFee(estimator)

		_, err := uc.Execute(context.Background(), core.ChainBase, core.AssetETH, big.NewInt(0))
		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("Execute() error = %v, want ErrNonPositiveAmount", err)
		}
		if estimator.calls != 0 {
			t.Fatalf("port called %d times, want 0 (rejected before delegating)", estimator.calls)
		}
	})

	t.Run("rejects a negative amount before calling the port", func(t *testing.T) {
		estimator := &fakeFeeEstimator{}
		uc := core.NewEstimateFee(estimator)

		_, err := uc.Execute(context.Background(), core.ChainArbitrum, core.AssetUSDC, big.NewInt(-1))
		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("Execute() error = %v, want ErrNonPositiveAmount", err)
		}
		if estimator.calls != 0 {
			t.Fatalf("port called %d times, want 0 (rejected before delegating)", estimator.calls)
		}
	})

	t.Run("rejects an amount exceeding a uint256's maximum before calling the port", func(t *testing.T) {
		estimator := &fakeFeeEstimator{}
		uc := core.NewEstimateFee(estimator)

		tooLarge := new(big.Int).Lsh(big.NewInt(1), 256) // 2^256, one past uint256's max
		_, err := uc.Execute(context.Background(), core.ChainBase, core.AssetETH, tooLarge)
		if !errors.Is(err, core.ErrAmountTooLarge) {
			t.Fatalf("Execute() error = %v, want ErrAmountTooLarge", err)
		}
		if estimator.calls != 0 {
			t.Fatalf("port called %d times, want 0 (rejected before delegating)", estimator.calls)
		}
	})

	t.Run("accepts an amount exactly at a uint256's maximum", func(t *testing.T) {
		want := core.FeeEstimate{L2Fee: big.NewInt(1), L1Fee: big.NewInt(1), TotalFee: big.NewInt(2)}
		estimator := &fakeFeeEstimator{result: want}
		uc := core.NewEstimateFee(estimator)

		maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
		_, err := uc.Execute(context.Background(), core.ChainBase, core.AssetETH, maxUint256)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if estimator.calls != 1 {
			t.Fatalf("port called %d times, want exactly 1", estimator.calls)
		}
	})

	t.Run("delegates to the port on a valid positive amount and returns its result", func(t *testing.T) {
		want := core.FeeEstimate{L2Fee: big.NewInt(100), L1Fee: big.NewInt(50), TotalFee: big.NewInt(150)}
		estimator := &fakeFeeEstimator{result: want}
		uc := core.NewEstimateFee(estimator)

		got, err := uc.Execute(context.Background(), core.ChainArbitrum, core.AssetUSDC, big.NewInt(100))
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if estimator.calls != 1 {
			t.Fatalf("port called %d times, want exactly 1", estimator.calls)
		}
		if got.L2Fee.Cmp(want.L2Fee) != 0 || got.L1Fee.Cmp(want.L1Fee) != 0 || got.TotalFee.Cmp(want.TotalFee) != 0 {
			t.Fatalf("Execute() = %+v, want %+v", got, want)
		}
	})

	t.Run("propagates a port error unchanged", func(t *testing.T) {
		portErr := errors.New("no token_registry entry for USDC")
		estimator := &fakeFeeEstimator{err: portErr}
		uc := core.NewEstimateFee(estimator)

		_, err := uc.Execute(context.Background(), core.ChainBase, core.AssetUSDC, big.NewInt(1))
		if !errors.Is(err, portErr) {
			t.Fatalf("Execute() error = %v, want %v", err, portErr)
		}
	})
}
