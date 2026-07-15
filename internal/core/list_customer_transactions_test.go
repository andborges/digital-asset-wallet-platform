package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeTransactionRepository is a test double for core.TransactionRepository. It records
// the pageSize/cursor it was actually called with, so tests can assert the use case's
// pageSize policy (default substitution, clamping, rejection) runs before the repository
// is ever reached.
type fakeTransactionRepository struct {
	called      bool
	gotPageSize int
	gotCursor   string
	result      core.TransactionPage
	err         error
}

func (f *fakeTransactionRepository) ListCustomerTransactions(_ context.Context, _ string, pageSize int, cursor string) (core.TransactionPage, error) {
	f.called = true
	f.gotPageSize = pageSize
	f.gotCursor = cursor
	return f.result, f.err
}

func intPtr(i int) *int { return &i }

func TestListCustomerTransactions_Execute(t *testing.T) {
	t.Run("substitutes the default page size when omitted (nil)", func(t *testing.T) {
		repo := &fakeTransactionRepository{}
		uc := core.NewListCustomerTransactions(repo)

		_, err := uc.Execute(context.Background(), "customer-id", nil, "")

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !repo.called {
			t.Fatal("expected the repository to be called")
		}
		if repo.gotPageSize != 20 {
			t.Fatalf("pageSize passed to repository = %d, want default 20", repo.gotPageSize)
		}
	})

	t.Run("rejects an explicit zero page size before calling the repository", func(t *testing.T) {
		repo := &fakeTransactionRepository{}
		uc := core.NewListCustomerTransactions(repo)

		// An explicit pageSize=0 is "present and not a positive integer" (AC8) — distinct
		// from an omitted pageSize (nil), which defaults. It must be rejected, not defaulted.
		_, err := uc.Execute(context.Background(), "customer-id", intPtr(0), "")

		if !errors.Is(err, core.ErrInvalidPageSize) {
			t.Fatalf("err = %v, want ErrInvalidPageSize", err)
		}
		if repo.called {
			t.Fatal("repository ListCustomerTransactions must not be called for an explicit zero page size")
		}
	})

	t.Run("rejects a negative page size before calling the repository", func(t *testing.T) {
		repo := &fakeTransactionRepository{}
		uc := core.NewListCustomerTransactions(repo)

		_, err := uc.Execute(context.Background(), "customer-id", intPtr(-1), "")

		if !errors.Is(err, core.ErrInvalidPageSize) {
			t.Fatalf("err = %v, want ErrInvalidPageSize", err)
		}
		if repo.called {
			t.Fatal("repository ListCustomerTransactions must not be called for a negative page size")
		}
	})

	t.Run("clamps a page size above the maximum to 100 before calling the repository", func(t *testing.T) {
		repo := &fakeTransactionRepository{}
		uc := core.NewListCustomerTransactions(repo)

		_, err := uc.Execute(context.Background(), "customer-id", intPtr(1000), "")

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repo.gotPageSize != 100 {
			t.Fatalf("pageSize passed to repository = %d, want clamped 100", repo.gotPageSize)
		}
	})

	t.Run("passes a valid page size through unchanged", func(t *testing.T) {
		repo := &fakeTransactionRepository{}
		uc := core.NewListCustomerTransactions(repo)

		_, err := uc.Execute(context.Background(), "customer-id", intPtr(5), "")

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repo.gotPageSize != 5 {
			t.Fatalf("pageSize passed to repository = %d, want 5", repo.gotPageSize)
		}
	})

	t.Run("passes the cursor through unchanged", func(t *testing.T) {
		repo := &fakeTransactionRepository{}
		uc := core.NewListCustomerTransactions(repo)

		_, err := uc.Execute(context.Background(), "customer-id", intPtr(20), "opaque-cursor-value")

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repo.gotCursor != "opaque-cursor-value" {
			t.Fatalf("cursor passed to repository = %q, want %q", repo.gotCursor, "opaque-cursor-value")
		}
	})

	t.Run("passes through a successful repository result", func(t *testing.T) {
		want := core.TransactionPage{
			Transactions: []core.Transaction{{ID: "txn-1", Type: "internal_transfer", Status: "completed"}},
			NextCursor:   "next-page-cursor",
		}
		repo := &fakeTransactionRepository{result: want}
		uc := core.NewListCustomerTransactions(repo)

		got, err := uc.Execute(context.Background(), "customer-id", intPtr(20), "")

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Transactions) != 1 || got.Transactions[0].ID != "txn-1" {
			t.Fatalf("got = %+v, want %+v", got, want)
		}
		if got.NextCursor != "next-page-cursor" {
			t.Fatalf("NextCursor = %q, want %q", got.NextCursor, "next-page-cursor")
		}
	})

	t.Run("passes through core.ErrCustomerNotFound unchanged", func(t *testing.T) {
		repo := &fakeTransactionRepository{err: core.ErrCustomerNotFound}
		uc := core.NewListCustomerTransactions(repo)

		_, err := uc.Execute(context.Background(), "customer-id", intPtr(20), "")

		if !errors.Is(err, core.ErrCustomerNotFound) {
			t.Fatalf("err = %v, want ErrCustomerNotFound", err)
		}
	})

	t.Run("passes through core.ErrInvalidCursor unchanged", func(t *testing.T) {
		repo := &fakeTransactionRepository{err: core.ErrInvalidCursor}
		uc := core.NewListCustomerTransactions(repo)

		_, err := uc.Execute(context.Background(), "customer-id", intPtr(20), "garbage")

		if !errors.Is(err, core.ErrInvalidCursor) {
			t.Fatalf("err = %v, want ErrInvalidCursor", err)
		}
	})
}
