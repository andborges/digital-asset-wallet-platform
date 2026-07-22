package core_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeWithdrawalRepository is a test double for core.WithdrawalRepository. It records
// whether CreateWithdrawal was ever invoked, and captures the feeEstimate/targetStatus it
// was given, so tests can assert both that use-case-level validation (non-positive
// amount, malformed/denylisted destination address) rejects a request before the
// repository — and therefore before any account lookup or lock — is ever reached, and
// that CreateWithdrawal.Execute computed the right fee estimate and target status before
// delegating (Story 3.3).
type fakeWithdrawalRepository struct {
	called          bool
	gotFeeEstimate  *big.Int
	gotTargetStatus string
	result          core.Withdrawal
	err             error
	approveCalled   bool
}

func (f *fakeWithdrawalRepository) CreateWithdrawal(_ context.Context, _ core.WithdrawalRequest, feeEstimate *big.Int, targetStatus string) (core.Withdrawal, error) {
	f.called = true
	f.gotFeeEstimate = feeEstimate
	f.gotTargetStatus = targetStatus
	return f.result, f.err
}

func (f *fakeWithdrawalRepository) ApproveWithdrawal(context.Context, string, string, string) (core.Withdrawal, error) {
	f.approveCalled = true
	panic("fakeWithdrawalRepository.ApproveWithdrawal must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) ClaimApprovedWithdrawal(context.Context, core.Chain) (core.Withdrawal, bool, error) {
	panic("fakeWithdrawalRepository.ClaimApprovedWithdrawal must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) RecordSignedTx(context.Context, string, string, string) error {
	panic("fakeWithdrawalRepository.RecordSignedTx must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) MarkBroadcast(context.Context, string) error {
	panic("fakeWithdrawalRepository.MarkBroadcast must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) ListSignedWithdrawals(context.Context, core.Chain) ([]core.Withdrawal, error) {
	panic("fakeWithdrawalRepository.ListSignedWithdrawals must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) ListBroadcastWithdrawals(context.Context, core.Chain) ([]core.Withdrawal, error) {
	panic("fakeWithdrawalRepository.ListBroadcastWithdrawals must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) SettleConfirmedWithdrawal(context.Context, string) error {
	panic("fakeWithdrawalRepository.SettleConfirmedWithdrawal must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) SettleFailedWithdrawal(context.Context, string) error {
	panic("fakeWithdrawalRepository.SettleFailedWithdrawal must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) ListStuckCandidates(context.Context, core.Chain, time.Duration) ([]core.Withdrawal, error) {
	panic("fakeWithdrawalRepository.ListStuckCandidates must not be called by CreateWithdrawal tests")
}

func (f *fakeWithdrawalRepository) MarkStuckAlerted(context.Context, string) error {
	panic("fakeWithdrawalRepository.MarkStuckAlerted must not be called by CreateWithdrawal tests")
}

// fakeFeeEstimator (core.FeeEstimator's test double) is defined once, in
// estimate_fee_test.go, and reused here.

// fakeWithdrawalThresholdLister is a test double for core.WithdrawalThresholdLister,
// returning a fixed threshold or error regardless of its arguments.
type fakeWithdrawalThresholdLister struct {
	threshold *big.Int
	err       error
}

func (f *fakeWithdrawalThresholdLister) GetApprovalThreshold(context.Context, core.Chain, core.Asset) (*big.Int, error) {
	return f.threshold, f.err
}

// defaultFeeEstimate/defaultThreshold give most tests a fee/threshold pair that never
// itself blocks a request (feeEstimate = 0, threshold far above every test amount) so
// each test can focus on the one behavior it's asserting.
func defaultFeeEstimator() *fakeFeeEstimator {
	return &fakeFeeEstimator{result: core.FeeEstimate{L2Fee: big.NewInt(0), L1Fee: big.NewInt(0), TotalFee: big.NewInt(0)}}
}

func defaultThresholdLister() *fakeWithdrawalThresholdLister {
	return &fakeWithdrawalThresholdLister{threshold: big.NewInt(1_000_000_000_000)}
}

func newCreateWithdrawal(repo core.WithdrawalRepository, feeEstimator core.FeeEstimator, thresholds core.WithdrawalThresholdLister) *core.CreateWithdrawal {
	return core.NewCreateWithdrawal(repo, feeEstimator, thresholds)
}

func validWithdrawalRequest() core.WithdrawalRequest {
	return core.WithdrawalRequest{
		CustomerID:         "customer-id",
		Chain:              core.ChainBase,
		Asset:              core.AssetETH,
		Amount:             big.NewInt(100),
		DestinationAddress: "0x00000000000000000000000000000000000000AA",
		IdempotencyKey:     "key-1",
	}
}

func TestCreateWithdrawal_Execute(t *testing.T) {
	t.Run("rejects zero amount before calling the repository", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		req := validWithdrawalRequest()
		req.Amount = big.NewInt(0)

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("err = %v, want ErrNonPositiveAmount", err)
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called for a zero amount")
		}
	})

	t.Run("rejects negative amount before calling the repository", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		req := validWithdrawalRequest()
		req.Amount = big.NewInt(-1)

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("err = %v, want ErrNonPositiveAmount", err)
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called for a negative amount")
		}
	})

	t.Run("rejects a nil amount before calling the repository", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		req := validWithdrawalRequest()
		req.Amount = nil

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("err = %v, want ErrNonPositiveAmount", err)
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called for a nil amount")
		}
	})

	t.Run("rejects an amount exceeding a uint256's maximum before calling the repository", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		req := validWithdrawalRequest()
		req.Amount = new(big.Int).Lsh(big.NewInt(1), 256) // 2^256, one past uint256's max

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrAmountTooLarge) {
			t.Fatalf("err = %v, want ErrAmountTooLarge", err)
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called for an oversized amount")
		}
	})

	t.Run("accepts an amount exactly at a uint256's maximum", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{result: core.Withdrawal{ID: "withdrawal-id"}}
		// The threshold lister's fixed threshold is far below 2^256-1, so this amount
		// routes to awaiting-approval — irrelevant to what this test asserts (that the
		// repository is reached at all), but the threshold lister must still return
		// something so the boundary check itself isn't what's being exercised here.
		thresholds := &fakeWithdrawalThresholdLister{threshold: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), thresholds)

		req := validWithdrawalRequest()
		req.Amount = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

		_, err := uc.Execute(context.Background(), req)

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !repo.called {
			t.Fatal("expected the repository to be called for an amount at the uint256 boundary")
		}
	})

	t.Run("rejects a malformed destination address before calling the repository", func(t *testing.T) {
		cases := []string{
			"",
			"not-an-address",
			"0x000000000000000000000000000000000000000",   // 39 hex chars: one short
			"0x00000000000000000000000000000000000000000", // 41 hex chars: one long
			"0xZZ00000000000000000000000000000000000000",  // non-hex characters
			"0000000000000000000000000000000000000000",    // missing 0x prefix
		}
		for _, addr := range cases {
			repo := &fakeWithdrawalRepository{}
			uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

			req := validWithdrawalRequest()
			req.DestinationAddress = addr

			_, err := uc.Execute(context.Background(), req)

			if !errors.Is(err, core.ErrMalformedDestinationAddress) {
				t.Fatalf("address %q: err = %v, want ErrMalformedDestinationAddress", addr, err)
			}
			if repo.called {
				t.Fatalf("address %q: repository CreateWithdrawal must not be called for a malformed address", addr)
			}
		}
	})

	t.Run("rejects the zero address before calling the repository", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		req := validWithdrawalRequest()
		req.DestinationAddress = "0x0000000000000000000000000000000000000000"

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrInvalidDestinationAddress) {
			t.Fatalf("err = %v, want ErrInvalidDestinationAddress", err)
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called for the zero address")
		}
	})

	t.Run("accepts a well-formed, non-zero destination address and delegates to the repository", func(t *testing.T) {
		want := core.Withdrawal{
			ID:                 "withdrawal-id",
			CustomerID:         "customer-id",
			Chain:              core.ChainBase,
			Asset:              core.AssetETH,
			Amount:             big.NewInt(100),
			DestinationAddress: "0x00000000000000000000000000000000000000AA",
			Status:             core.WithdrawalStatusApproved,
		}
		repo := &fakeWithdrawalRepository{result: want}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		got, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !repo.called {
			t.Fatal("expected the repository to be called for a valid request")
		}
		if got.ID != want.ID {
			t.Fatalf("got = %+v, want %+v", got, want)
		}
	})

	t.Run("routes to approved when amount is at or below the threshold", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		thresholds := &fakeWithdrawalThresholdLister{threshold: big.NewInt(100)}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), thresholds)

		req := validWithdrawalRequest()
		req.Amount = big.NewInt(100) // exactly at the threshold

		if _, err := uc.Execute(context.Background(), req); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repo.gotTargetStatus != core.WithdrawalStatusApproved {
			t.Fatalf("targetStatus = %q, want %q (amount at threshold)", repo.gotTargetStatus, core.WithdrawalStatusApproved)
		}
	})

	t.Run("routes to awaiting-approval when amount exceeds the threshold", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		thresholds := &fakeWithdrawalThresholdLister{threshold: big.NewInt(99)}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), thresholds)

		req := validWithdrawalRequest()
		req.Amount = big.NewInt(100) // one above the threshold

		if _, err := uc.Execute(context.Background(), req); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repo.gotTargetStatus != core.WithdrawalStatusAwaitingApproval {
			t.Fatalf("targetStatus = %q, want %q (amount exceeds threshold)", repo.gotTargetStatus, core.WithdrawalStatusAwaitingApproval)
		}
	})

	t.Run("routes to approved at the exact threshold even with a nonzero fee estimate", func(t *testing.T) {
		// re-review 2026-07-21: the two existing threshold-boundary tests above both use
		// defaultFeeEstimator() (fee = 0) — this proves the threshold comparison
		// (req.Amount.Cmp(threshold)) and the fee estimate are genuinely independent
		// inputs that don't interact unexpectedly at the boundary: a nonzero fee must
		// never push an at-threshold amount into awaiting-approval (the fee-inclusive
		// balance check is a separate, later concern the repository enforces, not part of
		// this routing decision), and the correct TotalFee must still reach the repository
		// unchanged.
		repo := &fakeWithdrawalRepository{}
		feeEstimator := &fakeFeeEstimator{result: core.FeeEstimate{L2Fee: big.NewInt(3), L1Fee: big.NewInt(4), TotalFee: big.NewInt(7)}}
		thresholds := &fakeWithdrawalThresholdLister{threshold: big.NewInt(100)}
		uc := newCreateWithdrawal(repo, feeEstimator, thresholds)

		req := validWithdrawalRequest()
		req.Amount = big.NewInt(100) // exactly at the threshold

		if _, err := uc.Execute(context.Background(), req); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repo.gotTargetStatus != core.WithdrawalStatusApproved {
			t.Fatalf("targetStatus = %q, want %q (amount at threshold, regardless of a nonzero fee)", repo.gotTargetStatus, core.WithdrawalStatusApproved)
		}
		if repo.gotFeeEstimate == nil || repo.gotFeeEstimate.Cmp(big.NewInt(7)) != 0 {
			t.Fatalf("feeEstimate passed to repository = %v, want 7 (TotalFee)", repo.gotFeeEstimate)
		}
	})

	t.Run("passes the fee estimator's TotalFee to the repository, not L2Fee or L1Fee alone", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		feeEstimator := &fakeFeeEstimator{result: core.FeeEstimate{L2Fee: big.NewInt(10), L1Fee: big.NewInt(20), TotalFee: big.NewInt(30)}}
		uc := newCreateWithdrawal(repo, feeEstimator, defaultThresholdLister())

		if _, err := uc.Execute(context.Background(), validWithdrawalRequest()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repo.gotFeeEstimate == nil || repo.gotFeeEstimate.Cmp(big.NewInt(30)) != 0 {
			t.Fatalf("feeEstimate passed to repository = %v, want 30 (TotalFee)", repo.gotFeeEstimate)
		}
	})

	t.Run("propagates a FeeEstimator failure without calling the repository", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		feeEstimator := &fakeFeeEstimator{err: errors.New("rpc unavailable")}
		uc := newCreateWithdrawal(repo, feeEstimator, defaultThresholdLister())

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if err == nil {
			t.Fatal("expected an error when the fee estimator fails")
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called when fee estimation fails")
		}
	})

	t.Run("fails loud, without calling the repository, if the fee estimator returns a nil TotalFee with no error", func(t *testing.T) {
		// re-review 2026-07-21: a well-behaved FeeEstimator never does this, but a future
		// adapter bug returning {TotalFee: nil, err: nil} must produce a clean error here,
		// not a big.Int.Cmp(nil) panic crashing the request handler.
		repo := &fakeWithdrawalRepository{}
		feeEstimator := &fakeFeeEstimator{result: core.FeeEstimate{L2Fee: big.NewInt(0), L1Fee: big.NewInt(0), TotalFee: nil}}
		uc := newCreateWithdrawal(repo, feeEstimator, defaultThresholdLister())

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if err == nil {
			t.Fatal("expected an error when the fee estimator returns a nil TotalFee")
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called when TotalFee is nil")
		}
	})

	t.Run("fails loud, without calling the repository, if the threshold lister returns a nil threshold with no error", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		thresholds := &fakeWithdrawalThresholdLister{threshold: nil}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), thresholds)

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if err == nil {
			t.Fatal("expected an error when the threshold lister returns a nil threshold")
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called when threshold is nil")
		}
	})

	t.Run("propagates a WithdrawalThresholdLister failure (registry gap) without calling the repository", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{}
		thresholds := &fakeWithdrawalThresholdLister{err: errors.New("no threshold configured")}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), thresholds)

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if err == nil {
			t.Fatal("expected an error when the threshold lister fails")
		}
		if repo.called {
			t.Fatal("repository CreateWithdrawal must not be called when the threshold lookup fails")
		}
	})

	t.Run("passes through core.ErrCustomerNotFound unchanged", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{err: core.ErrCustomerNotFound}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if !errors.Is(err, core.ErrCustomerNotFound) {
			t.Fatalf("err = %v, want ErrCustomerNotFound", err)
		}
	})

	t.Run("passes through core.ErrInsufficientBalance unchanged", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{err: core.ErrInsufficientBalance}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if !errors.Is(err, core.ErrInsufficientBalance) {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("passes through core.ErrInsufficientBalanceForFee unchanged", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{err: core.ErrInsufficientBalanceForFee}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if !errors.Is(err, core.ErrInsufficientBalanceForFee) {
			t.Fatalf("err = %v, want ErrInsufficientBalanceForFee", err)
		}
	})

	t.Run("passes through core.ErrDuplicateWithdrawalCause unchanged", func(t *testing.T) {
		repo := &fakeWithdrawalRepository{err: core.ErrDuplicateWithdrawalCause}
		uc := newCreateWithdrawal(repo, defaultFeeEstimator(), defaultThresholdLister())

		_, err := uc.Execute(context.Background(), validWithdrawalRequest())

		if !errors.Is(err, core.ErrDuplicateWithdrawalCause) {
			t.Fatalf("err = %v, want ErrDuplicateWithdrawalCause", err)
		}
	})
}
