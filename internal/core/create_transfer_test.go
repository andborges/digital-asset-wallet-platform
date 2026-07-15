package core_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeTransferRepository is a test double for core.TransferRepository. It records
// whether CreateTransfer was ever invoked, so tests can assert that use-case-level
// validation (self-transfer, non-positive amount) rejects a request before the
// repository — and therefore before any account lookup or lock — is ever reached.
type fakeTransferRepository struct {
	called bool
	result core.Transfer
	err    error
}

func (f *fakeTransferRepository) CreateTransfer(_ context.Context, _ core.TransferRequest) (core.Transfer, error) {
	f.called = true
	return f.result, f.err
}

func validTransferRequest() core.TransferRequest {
	return core.TransferRequest{
		SourceCustomerID:      "source-id",
		DestinationCustomerID: "destination-id",
		Chain:                 core.ChainBase,
		Asset:                 core.AssetETH,
		Amount:                big.NewInt(100),
		IdempotencyKey:        "key-1",
	}
}

func TestCreateTransfer_Execute(t *testing.T) {
	t.Run("rejects self-transfer before calling the repository", func(t *testing.T) {
		repo := &fakeTransferRepository{}
		uc := core.NewCreateTransfer(repo)

		req := validTransferRequest()
		req.DestinationCustomerID = req.SourceCustomerID

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrSelfTransfer) {
			t.Fatalf("err = %v, want ErrSelfTransfer", err)
		}
		if repo.called {
			t.Fatal("repository CreateTransfer must not be called for a self-transfer")
		}
	})

	t.Run("rejects zero amount before calling the repository", func(t *testing.T) {
		repo := &fakeTransferRepository{}
		uc := core.NewCreateTransfer(repo)

		req := validTransferRequest()
		req.Amount = big.NewInt(0)

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("err = %v, want ErrNonPositiveAmount", err)
		}
		if repo.called {
			t.Fatal("repository CreateTransfer must not be called for a zero amount")
		}
	})

	t.Run("rejects negative amount before calling the repository", func(t *testing.T) {
		repo := &fakeTransferRepository{}
		uc := core.NewCreateTransfer(repo)

		req := validTransferRequest()
		req.Amount = big.NewInt(-1)

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("err = %v, want ErrNonPositiveAmount", err)
		}
		if repo.called {
			t.Fatal("repository CreateTransfer must not be called for a negative amount")
		}
	})

	t.Run("rejects a nil amount before calling the repository", func(t *testing.T) {
		repo := &fakeTransferRepository{}
		uc := core.NewCreateTransfer(repo)

		req := validTransferRequest()
		req.Amount = nil

		_, err := uc.Execute(context.Background(), req)

		if !errors.Is(err, core.ErrNonPositiveAmount) {
			t.Fatalf("err = %v, want ErrNonPositiveAmount", err)
		}
		if repo.called {
			t.Fatal("repository CreateTransfer must not be called for a nil amount")
		}
	})

	t.Run("passes through a successful repository result", func(t *testing.T) {
		want := core.Transfer{
			ID:                    "journal-entry-id",
			SourceCustomerID:      "source-id",
			DestinationCustomerID: "destination-id",
			Chain:                 core.ChainBase,
			Asset:                 core.AssetETH,
			Amount:                big.NewInt(100),
		}
		repo := &fakeTransferRepository{result: want}
		uc := core.NewCreateTransfer(repo)

		got, err := uc.Execute(context.Background(), validTransferRequest())

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

	t.Run("passes through core.ErrCustomerNotFound unchanged", func(t *testing.T) {
		repo := &fakeTransferRepository{err: core.ErrCustomerNotFound}
		uc := core.NewCreateTransfer(repo)

		_, err := uc.Execute(context.Background(), validTransferRequest())

		if !errors.Is(err, core.ErrCustomerNotFound) {
			t.Fatalf("err = %v, want ErrCustomerNotFound", err)
		}
	})

	t.Run("passes through core.ErrInsufficientBalance unchanged", func(t *testing.T) {
		repo := &fakeTransferRepository{err: core.ErrInsufficientBalance}
		uc := core.NewCreateTransfer(repo)

		_, err := uc.Execute(context.Background(), validTransferRequest())

		if !errors.Is(err, core.ErrInsufficientBalance) {
			t.Fatalf("err = %v, want ErrInsufficientBalance", err)
		}
	})

	t.Run("passes through core.ErrDuplicateTransferCause unchanged", func(t *testing.T) {
		repo := &fakeTransferRepository{err: core.ErrDuplicateTransferCause}
		uc := core.NewCreateTransfer(repo)

		_, err := uc.Execute(context.Background(), validTransferRequest())

		if !errors.Is(err, core.ErrDuplicateTransferCause) {
			t.Fatalf("err = %v, want ErrDuplicateTransferCause", err)
		}
	})
}
