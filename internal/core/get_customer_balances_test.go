package core_test

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

type fakeBalanceRepository struct {
	balances []core.AccountBalance
	err      error
}

func (f *fakeBalanceRepository) CustomerBalances(ctx context.Context, customerID string) ([]core.AccountBalance, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.balances, nil
}

func TestGetCustomerBalances_Execute(t *testing.T) {
	t.Parallel()

	t.Run("returns balances from the repository", func(t *testing.T) {
		t.Parallel()

		want := []core.AccountBalance{
			{Chain: core.ChainBase, Asset: core.AssetETH, Balance: big.NewInt(0)},
			{Chain: core.ChainBase, Asset: core.AssetUSDC, Balance: big.NewInt(0)},
			{Chain: core.ChainArbitrum, Asset: core.AssetETH, Balance: big.NewInt(0)},
			{Chain: core.ChainArbitrum, Asset: core.AssetUSDC, Balance: big.NewInt(0)},
		}
		repo := &fakeBalanceRepository{balances: want}
		uc := core.NewGetCustomerBalances(repo)

		got, err := uc.Execute(context.Background(), "some-customer-id")
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if len(got) != len(want) {
			t.Fatalf("Execute() returned %d balances, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].Chain != want[i].Chain || got[i].Asset != want[i].Asset || got[i].Balance.Cmp(want[i].Balance) != 0 {
				t.Errorf("Execute()[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("propagates ErrCustomerNotFound unchanged", func(t *testing.T) {
		t.Parallel()

		repo := &fakeBalanceRepository{err: core.ErrCustomerNotFound}
		uc := core.NewGetCustomerBalances(repo)

		_, err := uc.Execute(context.Background(), "nonexistent-id")
		if !errors.Is(err, core.ErrCustomerNotFound) {
			t.Fatalf("Execute() error = %v, want ErrCustomerNotFound", err)
		}
	})

	t.Run("propagates other repository errors unchanged", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("boom")
		repo := &fakeBalanceRepository{err: sentinel}
		uc := core.NewGetCustomerBalances(repo)

		_, err := uc.Execute(context.Background(), "some-customer-id")
		if !errors.Is(err, sentinel) {
			t.Fatalf("Execute() error = %v, want sentinel", err)
		}
	})
}
