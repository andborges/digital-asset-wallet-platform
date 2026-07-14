package core

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CreateCustomer provisions a new customer plus one account per supported
// (chain, asset) pair, as a single call to the repository (FR1). The repository
// implementation is responsible for making that provisioning atomic (AD-4).
type CreateCustomer struct {
	repo CustomerRepository
}

// NewCreateCustomer constructs the use case against the given repository port.
func NewCreateCustomer(repo CustomerRepository) *CreateCustomer {
	return &CreateCustomer{repo: repo}
}

// Execute creates a new customer and its fixed set of per-asset accounts.
func (uc *CreateCustomer) Execute(ctx context.Context) (Customer, error) {
	now := time.Now().UTC()

	customerID, err := newUUIDv7()
	if err != nil {
		return Customer{}, fmt.Errorf("generate customer id: %w", err)
	}

	customer := Customer{
		ID:        customerID,
		CreatedAt: now,
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

	if err := uc.repo.CreateCustomer(ctx, customer, accounts); err != nil {
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
