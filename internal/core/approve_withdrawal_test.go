package core_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeApproveWithdrawalRepository is a test double for core.WithdrawalRepository, used
// only for ApproveWithdrawal's own tests — CreateWithdrawal is never called here, so it
// panics if invoked, catching an accidental cross-wire.
type fakeApproveWithdrawalRepository struct {
	called                     bool
	gotID, gotActor, gotReason string
	result                     core.Withdrawal
	err                        error
}

func (f *fakeApproveWithdrawalRepository) CreateWithdrawal(context.Context, core.WithdrawalRequest, *big.Int, string) (core.Withdrawal, error) {
	panic("fakeApproveWithdrawalRepository.CreateWithdrawal must not be called by ApproveWithdrawal tests")
}

func (f *fakeApproveWithdrawalRepository) ApproveWithdrawal(_ context.Context, id, actor, reason string) (core.Withdrawal, error) {
	f.called = true
	f.gotID, f.gotActor, f.gotReason = id, actor, reason
	return f.result, f.err
}

func (f *fakeApproveWithdrawalRepository) ClaimApprovedWithdrawal(context.Context, core.Chain) (core.Withdrawal, bool, error) {
	panic("fakeApproveWithdrawalRepository.ClaimApprovedWithdrawal must not be called by ApproveWithdrawal tests")
}

func (f *fakeApproveWithdrawalRepository) RecordBroadcastTxHash(context.Context, string, string) error {
	panic("fakeApproveWithdrawalRepository.RecordBroadcastTxHash must not be called by ApproveWithdrawal tests")
}

func (f *fakeApproveWithdrawalRepository) ListBroadcastWithdrawals(context.Context, core.Chain) ([]core.Withdrawal, error) {
	panic("fakeApproveWithdrawalRepository.ListBroadcastWithdrawals must not be called by ApproveWithdrawal tests")
}

func (f *fakeApproveWithdrawalRepository) SettleConfirmedWithdrawal(context.Context, string) error {
	panic("fakeApproveWithdrawalRepository.SettleConfirmedWithdrawal must not be called by ApproveWithdrawal tests")
}

func (f *fakeApproveWithdrawalRepository) SettleFailedWithdrawal(context.Context, string) error {
	panic("fakeApproveWithdrawalRepository.SettleFailedWithdrawal must not be called by ApproveWithdrawal tests")
}

func TestApproveWithdrawal_Execute(t *testing.T) {
	t.Run("rejects an empty actor before calling the repository", func(t *testing.T) {
		repo := &fakeApproveWithdrawalRepository{}
		uc := core.NewApproveWithdrawal(repo)

		_, err := uc.Execute(context.Background(), "withdrawal-id", "", "reason")

		if !errors.Is(err, core.ErrMissingApprovalActor) {
			t.Fatalf("err = %v, want ErrMissingApprovalActor", err)
		}
		if repo.called {
			t.Fatal("repository ApproveWithdrawal must not be called for an empty actor")
		}
	})

	t.Run("rejects an empty reason before calling the repository", func(t *testing.T) {
		repo := &fakeApproveWithdrawalRepository{}
		uc := core.NewApproveWithdrawal(repo)

		_, err := uc.Execute(context.Background(), "withdrawal-id", "ops-alice", "")

		if !errors.Is(err, core.ErrMissingApprovalReason) {
			t.Fatalf("err = %v, want ErrMissingApprovalReason", err)
		}
		if repo.called {
			t.Fatal("repository ApproveWithdrawal must not be called for an empty reason")
		}
	})

	t.Run("rejects both empty actor and reason with the actor error first", func(t *testing.T) {
		repo := &fakeApproveWithdrawalRepository{}
		uc := core.NewApproveWithdrawal(repo)

		_, err := uc.Execute(context.Background(), "withdrawal-id", "", "")

		if !errors.Is(err, core.ErrMissingApprovalActor) {
			t.Fatalf("err = %v, want ErrMissingApprovalActor", err)
		}
		if repo.called {
			t.Fatal("repository ApproveWithdrawal must not be called")
		}
	})

	t.Run("delegates to the repository with a non-empty actor and reason", func(t *testing.T) {
		want := core.Withdrawal{ID: "withdrawal-id", Status: core.WithdrawalStatusApproved}
		repo := &fakeApproveWithdrawalRepository{result: want}
		uc := core.NewApproveWithdrawal(repo)

		got, err := uc.Execute(context.Background(), "withdrawal-id", "ops-alice", "manually reviewed, looks fine")

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !repo.called {
			t.Fatal("expected the repository to be called for a valid request")
		}
		if repo.gotID != "withdrawal-id" || repo.gotActor != "ops-alice" || repo.gotReason != "manually reviewed, looks fine" {
			t.Fatalf("repository received (%q, %q, %q), want (%q, %q, %q)", repo.gotID, repo.gotActor, repo.gotReason, "withdrawal-id", "ops-alice", "manually reviewed, looks fine")
		}
		if got.ID != want.ID || got.Status != want.Status {
			t.Fatalf("got = %+v, want %+v", got, want)
		}
	})

	t.Run("passes through core.ErrWithdrawalNotFound unchanged", func(t *testing.T) {
		repo := &fakeApproveWithdrawalRepository{err: core.ErrWithdrawalNotFound}
		uc := core.NewApproveWithdrawal(repo)

		_, err := uc.Execute(context.Background(), "withdrawal-id", "ops-alice", "reason")

		if !errors.Is(err, core.ErrWithdrawalNotFound) {
			t.Fatalf("err = %v, want ErrWithdrawalNotFound", err)
		}
	})

	t.Run("passes through core.ErrWithdrawalNotAwaitingApproval unchanged", func(t *testing.T) {
		repo := &fakeApproveWithdrawalRepository{err: core.ErrWithdrawalNotAwaitingApproval}
		uc := core.NewApproveWithdrawal(repo)

		_, err := uc.Execute(context.Background(), "withdrawal-id", "ops-alice", "reason")

		if !errors.Is(err, core.ErrWithdrawalNotAwaitingApproval) {
			t.Fatalf("err = %v, want ErrWithdrawalNotAwaitingApproval", err)
		}
	})
}
