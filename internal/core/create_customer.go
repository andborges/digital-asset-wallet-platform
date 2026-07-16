package core

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CreateCustomer provisions a new customer plus one account per supported
// (chain, asset) pair and its CREATE2 deposit address (FR1, FR6, AD-8), as a single call
// to the repository. The repository implementation is responsible for making that
// provisioning atomic (AD-4).
type CreateCustomer struct {
	repo           CustomerRepository
	addressDeriver DepositAddressDeriver
}

// NewCreateCustomer constructs the use case against the given repository and deposit
// address derivation ports.
func NewCreateCustomer(repo CustomerRepository, addressDeriver DepositAddressDeriver) *CreateCustomer {
	return &CreateCustomer{repo: repo, addressDeriver: addressDeriver}
}

// Execute creates a new customer, its fixed set of per-asset accounts, and its deposit
// address.
func (uc *CreateCustomer) Execute(ctx context.Context) (Customer, error) {
	now := time.Now().UTC()

	customerID, err := newUUIDv7()
	if err != nil {
		return Customer{}, fmt.Errorf("generate customer id: %w", err)
	}

	// A derivation failure here means a bug in this story's own salt/CREATE2 wiring
	// against an id this service just generated itself — not bad external input — so it
	// is treated as an unexpected error, the same way newUUIDv7 failures are, rather than
	// a new sentinel error.
	salt, err := customerSalt(customerID)
	if err != nil {
		return Customer{}, fmt.Errorf("compute salt for generated customer id: %w", err)
	}
	depositAddress, err := uc.addressDeriver.DeriveAddress(salt)
	if err != nil {
		return Customer{}, fmt.Errorf("derive deposit address: %w", err)
	}

	customer := Customer{
		ID:             customerID,
		CreatedAt:      now,
		DepositAddress: depositAddress,
	}

	accounts := make([]Account, 0, len(SupportedChainAssetPairs))
	for _, pair := range SupportedChainAssetPairs {
		accountID, err := newUUIDv7()
		if err != nil {
			return Customer{}, fmt.Errorf("generate account id: %w", err)
		}
		accounts = append(accounts, Account{
			ID:         accountID,
			CustomerID: customer.ID,
			Chain:      pair.Chain,
			Asset:      pair.Asset,
			CreatedAt:  now,
		})
	}

	if err := uc.repo.CreateCustomer(ctx, customer, accounts, depositAddress); err != nil {
		return Customer{}, err
	}

	return customer, nil
}

func newUUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
