package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

type fakeCustomerReader struct {
	customer core.Customer
	err      error
}

func (f *fakeCustomerReader) GetCustomer(ctx context.Context, customerID string) (core.Customer, error) {
	if f.err != nil {
		return core.Customer{}, f.err
	}
	return f.customer, nil
}

func TestGetCustomer_Execute(t *testing.T) {
	t.Parallel()

	t.Run("returns the customer from the reader", func(t *testing.T) {
		t.Parallel()

		want := core.Customer{ID: "some-customer-id", CreatedAt: time.Now().UTC(), DepositAddress: "0xabc"}
		reader := &fakeCustomerReader{customer: want}
		uc := core.NewGetCustomer(reader)

		got, err := uc.Execute(context.Background(), "some-customer-id")
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if got != want {
			t.Fatalf("Execute() = %+v, want %+v", got, want)
		}
	})

	t.Run("propagates ErrCustomerNotFound unchanged", func(t *testing.T) {
		t.Parallel()

		reader := &fakeCustomerReader{err: core.ErrCustomerNotFound}
		uc := core.NewGetCustomer(reader)

		_, err := uc.Execute(context.Background(), "nonexistent-id")
		if !errors.Is(err, core.ErrCustomerNotFound) {
			t.Fatalf("Execute() error = %v, want ErrCustomerNotFound", err)
		}
	})
}
