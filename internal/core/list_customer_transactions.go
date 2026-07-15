package core

import "context"

// defaultTransactionPageSize and maxTransactionPageSize bound ListCustomerTransactions'
// pageSize policy (AC8): an omitted pageSize (nil) substitutes the default; a present but
// non-positive value (zero or negative) is rejected; anything above the maximum is
// silently clamped, never rejected.
const (
	defaultTransactionPageSize = 20
	maxTransactionPageSize     = 100
)

// ListCustomerTransactions reads a customer's transaction history, newest first,
// generically from the cause-tagged journal (FR3).
type ListCustomerTransactions struct {
	repo TransactionRepository
}

// NewListCustomerTransactions constructs the use case against the given repository port.
func NewListCustomerTransactions(repo TransactionRepository) *ListCustomerTransactions {
	return &ListCustomerTransactions{repo: repo}
}

// Execute applies the pageSize policy — omitted (nil) substitutes the default, a present
// non-positive value (zero or negative) is rejected, above the maximum is clamped — then
// delegates to the repository. pageSize is a *int so the use case can tell a caller who
// omitted the parameter (nil → default) apart from one who explicitly sent pageSize=0
// (present and not a positive integer → ErrInvalidPageSize, AC8); collapsing both to 0
// would silently accept an explicit zero.
func (uc *ListCustomerTransactions) Execute(ctx context.Context, customerID string, pageSize *int, cursor string) (TransactionPage, error) {
	resolved := defaultTransactionPageSize
	if pageSize != nil {
		switch {
		case *pageSize <= 0:
			return TransactionPage{}, ErrInvalidPageSize
		case *pageSize > maxTransactionPageSize:
			resolved = maxTransactionPageSize
		default:
			resolved = *pageSize
		}
	}

	return uc.repo.ListCustomerTransactions(ctx, customerID, resolved, cursor)
}
