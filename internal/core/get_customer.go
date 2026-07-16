package core

import "context"

// GetCustomer reads a customer's own attributes, including its deposit address (FR7).
type GetCustomer struct {
	reader CustomerReader
}

// NewGetCustomer constructs the use case against the given reader port.
func NewGetCustomer(reader CustomerReader) *GetCustomer {
	return &GetCustomer{reader: reader}
}

// Execute returns customerID's own record, or ErrCustomerNotFound if no such customer
// exists.
func (uc *GetCustomer) Execute(ctx context.Context, customerID string) (Customer, error) {
	return uc.reader.GetCustomer(ctx, customerID)
}
