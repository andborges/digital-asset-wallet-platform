package core

import "context"

// ListUnsupportedTokenObservations is an operator-facing read use case over
// unsupported-token observations (Story 2.3, FR11, AC3) — a thin wrapper over
// UnsupportedTokenRepository.ListObservations, the same shape as GetCustomer over
// CustomerReader. Platform-wide, never scoped to a customer: an unsupported-token
// observation carries no customer attribution of its own, only the deposit address it
// landed on.
type ListUnsupportedTokenObservations struct {
	repo UnsupportedTokenRepository
}

// NewListUnsupportedTokenObservations constructs the use case against the given
// repository port.
func NewListUnsupportedTokenObservations(repo UnsupportedTokenRepository) *ListUnsupportedTokenObservations {
	return &ListUnsupportedTokenObservations{repo: repo}
}

// Execute returns every recorded unsupported-token observation, newest first.
func (uc *ListUnsupportedTokenObservations) Execute(ctx context.Context) ([]UnsupportedTokenObservation, error) {
	return uc.repo.ListObservations(ctx)
}
