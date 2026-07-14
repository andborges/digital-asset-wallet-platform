package core

import (
	"context"
	"errors"
	"testing"
)

type fakeCustomerRepository struct {
	created  bool
	customer Customer
	accounts []Account
	err      error
}

func (f *fakeCustomerRepository) CreateCustomer(ctx context.Context, customer Customer, accounts []Account) error {
	if f.err != nil {
		return f.err
	}
	f.created = true
	f.customer = customer
	f.accounts = accounts
	return nil
}

func TestCreateCustomer_ProvisionsFourFixedAccounts(t *testing.T) {
	repo := &fakeCustomerRepository{}
	uc := NewCreateCustomer(repo)

	customer, err := uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if !repo.created {
		t.Fatal("expected repository.CreateCustomer to be called")
	}
	if customer.ID == "" {
		t.Fatal("expected a non-empty customer ID")
	}
	if repo.customer.ID != customer.ID {
		t.Fatalf("repo received customer ID %q, want %q", repo.customer.ID, customer.ID)
	}

	if len(repo.accounts) != len(SupportedChainAssetPairs) {
		t.Fatalf("got %d accounts, want %d", len(repo.accounts), len(SupportedChainAssetPairs))
	}

	seen := map[string]bool{}
	for _, acc := range repo.accounts {
		if acc.CustomerID != customer.ID {
			t.Fatalf("account %+v has CustomerID %q, want %q", acc, acc.CustomerID, customer.ID)
		}
		if acc.ID == "" {
			t.Fatalf("account %+v has empty ID", acc)
		}
		key := string(acc.Chain) + "/" + string(acc.Asset)
		if seen[key] {
			t.Fatalf("duplicate (chain, asset) pair provisioned: %s", key)
		}
		seen[key] = true
	}

	for _, pair := range SupportedChainAssetPairs {
		key := string(pair.Chain) + "/" + string(pair.Asset)
		if !seen[key] {
			t.Fatalf("missing expected account for (chain, asset) = %s", key)
		}
	}
}

func TestCreateCustomer_EachInvocationGetsAUniqueID(t *testing.T) {
	repo := &fakeCustomerRepository{}
	uc := NewCreateCustomer(repo)

	first, err := uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	second, err := uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if first.ID == second.ID {
		t.Fatalf("expected distinct customer IDs across invocations, got the same ID twice: %s", first.ID)
	}
}

func TestCreateCustomer_PropagatesRepositoryError(t *testing.T) {
	wantErr := errors.New("insert failed")
	repo := &fakeCustomerRepository{err: wantErr}
	uc := NewCreateCustomer(repo)

	_, err := uc.Execute(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Execute() error = %v, want %v", err, wantErr)
	}
}
