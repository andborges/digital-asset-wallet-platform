package core

import "context"

// GetCustomerBalances reads a customer's current per-(chain, asset) balances, each
// always derived from postings rather than a stored value (AD-3, FR2).
type GetCustomerBalances struct {
	repo BalanceRepository
}

// NewGetCustomerBalances constructs the use case against the given repository port.
func NewGetCustomerBalances(repo BalanceRepository) *GetCustomerBalances {
	return &GetCustomerBalances{repo: repo}
}

// Execute returns customerID's balances, or ErrCustomerNotFound if no such customer exists.
func (uc *GetCustomerBalances) Execute(ctx context.Context, customerID string) ([]AccountBalance, error) {
	return uc.repo.CustomerBalances(ctx, customerID)
}
