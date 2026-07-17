package core

import "context"

// GetCustomerDeposits reads a customer's own deposits: observed/safe tiers exposed as
// "pending", and orphaned deposits exposed as "orphaned" (Story 2.4, AC1's "provisional
// visibility reflects this" — a customer must be able to see a deposit was reorged away,
// not have it silently vanish). Finalized/credited deposits surface only through the
// transaction history endpoint (Story 2.2), never here.
type GetCustomerDeposits struct {
	reader DepositReader
}

// NewGetCustomerDeposits constructs the use case against the given reader port.
func NewGetCustomerDeposits(reader DepositReader) *GetCustomerDeposits {
	return &GetCustomerDeposits{reader: reader}
}

// Execute returns customerID's deposits, or ErrCustomerNotFound if no such customer
// exists.
func (uc *GetCustomerDeposits) Execute(ctx context.Context, customerID string) ([]Deposit, error) {
	return uc.reader.ListCustomerDeposits(ctx, customerID)
}
