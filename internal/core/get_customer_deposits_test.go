package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

type fakeDepositReader struct {
	deposits []core.Deposit
	err      error
}

func (f *fakeDepositReader) ListCustomerDeposits(ctx context.Context, customerID string) ([]core.Deposit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.deposits, nil
}

func TestGetCustomerDeposits_Execute(t *testing.T) {
	t.Parallel()

	t.Run("returns the deposits from the reader", func(t *testing.T) {
		t.Parallel()

		want := []core.Deposit{
			{ID: "dep-1", Chain: core.ChainBase, Asset: core.AssetETH, State: core.DepositObserved},
			{ID: "dep-2", Chain: core.ChainBase, Asset: core.AssetUSDC, State: core.DepositSafe},
		}
		reader := &fakeDepositReader{deposits: want}
		uc := core.NewGetCustomerDeposits(reader)

		got, err := uc.Execute(context.Background(), "some-customer-id")
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if len(got) != len(want) || got[0].ID != want[0].ID || got[1].ID != want[1].ID {
			t.Fatalf("Execute() = %+v, want %+v", got, want)
		}
	})

	t.Run("propagates ErrCustomerNotFound unchanged", func(t *testing.T) {
		t.Parallel()

		reader := &fakeDepositReader{err: core.ErrCustomerNotFound}
		uc := core.NewGetCustomerDeposits(reader)

		_, err := uc.Execute(context.Background(), "nonexistent-id")
		if !errors.Is(err, core.ErrCustomerNotFound) {
			t.Fatalf("Execute() error = %v, want ErrCustomerNotFound", err)
		}
	})
}
