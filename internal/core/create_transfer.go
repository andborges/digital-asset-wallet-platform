package core

import (
	"context"
	"errors"
)

// ErrSelfTransfer is returned when the source and destination customer are the same.
// Validated here, in the use case, rather than left to TransferRepository: a
// self-referential account lookup would otherwise return a single row for what the
// caller thinks are two, corrupting the repository's two-account locking assumption.
var ErrSelfTransfer = errors.New("source and destination customer must differ")

// ErrNonPositiveAmount is returned when the requested transfer amount is not a
// strictly positive integer — a zero or negative amount is not a valid money movement.
var ErrNonPositiveAmount = errors.New("transfer amount must be a positive integer")

// CreateTransfer moves balance between two customers' accounts ledger-only — no chain
// interaction, no on-chain transaction — as a single balanced journal entry (FR4, AD-3,
// AD-4).
type CreateTransfer struct {
	repo TransferRepository
}

// NewCreateTransfer constructs the use case against the given repository port.
func NewCreateTransfer(repo TransferRepository) *CreateTransfer {
	return &CreateTransfer{repo: repo}
}

// Execute validates req's ledger-domain invariants and, if they hold, delegates to the
// repository to perform the locked, balance-checked transfer.
func (uc *CreateTransfer) Execute(ctx context.Context, req TransferRequest) (Transfer, error) {
	if req.SourceCustomerID == req.DestinationCustomerID {
		return Transfer{}, ErrSelfTransfer
	}
	if req.Amount == nil || req.Amount.Sign() <= 0 {
		return Transfer{}, ErrNonPositiveAmount
	}

	return uc.repo.CreateTransfer(ctx, req)
}
