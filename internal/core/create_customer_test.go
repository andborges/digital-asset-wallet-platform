package core

import (
	"context"
	"errors"
	"testing"
)

type fakeCustomerRepository struct {
	created        bool
	customer       Customer
	accounts       []Account
	depositAddress string
	err            error
}

func (f *fakeCustomerRepository) CreateCustomer(ctx context.Context, customer Customer, accounts []Account, depositAddress string) error {
	if f.err != nil {
		return f.err
	}
	f.created = true
	f.customer = customer
	f.accounts = accounts
	f.depositAddress = depositAddress
	return nil
}

// fakeDepositAddressDeriver is a test double for core.DepositAddressDeriver. It records
// the salt it was actually called with and returns a deterministic, recognizable address
// derived from it, so tests can assert the salt reaching the port and the address
// reaching the repository without needing a real CREATE2 implementation.
type fakeDepositAddressDeriver struct {
	gotSalt [32]byte
	called  bool
	err     error
}

func (f *fakeDepositAddressDeriver) DeriveAddress(salt [32]byte) (string, error) {
	f.called = true
	f.gotSalt = salt
	if f.err != nil {
		return "", f.err
	}
	return "0xfakeaddress", nil
}

func newTestCreateCustomer(repo *fakeCustomerRepository, deriver *fakeDepositAddressDeriver) *CreateCustomer {
	if deriver == nil {
		deriver = &fakeDepositAddressDeriver{}
	}
	return NewCreateCustomer(repo, deriver)
}

func TestCreateCustomer_ProvisionsFourFixedAccounts(t *testing.T) {
	repo := &fakeCustomerRepository{}
	uc := newTestCreateCustomer(repo, nil)

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
	uc := newTestCreateCustomer(repo, nil)

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
	uc := newTestCreateCustomer(repo, nil)

	_, err := uc.Execute(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Execute() error = %v, want %v", err, wantErr)
	}
}

func TestCreateCustomer_DerivesDepositAddressFromTheCustomersOwnSalt(t *testing.T) {
	repo := &fakeCustomerRepository{}
	deriver := &fakeDepositAddressDeriver{}
	uc := newTestCreateCustomer(repo, deriver)

	customer, err := uc.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}

	if !deriver.called {
		t.Fatal("expected DepositAddressDeriver.DeriveAddress to be called")
	}
	wantSalt, err := customerSalt(customer.ID)
	if err != nil {
		t.Fatalf("customerSalt(%q) error = %v", customer.ID, err)
	}
	if deriver.gotSalt != wantSalt {
		t.Fatalf("deriver received salt %x, want %x (salt of the generated customer id)", deriver.gotSalt, wantSalt)
	}

	if customer.DepositAddress != "0xfakeaddress" {
		t.Fatalf("customer.DepositAddress = %q, want %q", customer.DepositAddress, "0xfakeaddress")
	}
	if repo.depositAddress != "0xfakeaddress" {
		t.Fatalf("repo received depositAddress = %q, want %q", repo.depositAddress, "0xfakeaddress")
	}
}

func TestCreateCustomer_PropagatesDeriverError(t *testing.T) {
	wantErr := errors.New("derive failed")
	repo := &fakeCustomerRepository{}
	deriver := &fakeDepositAddressDeriver{err: wantErr}
	uc := newTestCreateCustomer(repo, deriver)

	_, err := uc.Execute(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Execute() error = %v, want %v", err, wantErr)
	}
	if repo.created {
		t.Fatal("repository.CreateCustomer must not be called when address derivation fails")
	}
}
