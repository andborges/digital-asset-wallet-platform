package core

import (
	"context"
	"errors"
	"strings"
)

// ErrMissingApprovalActor is returned when an approve request omits actor (NFR11:
// "operator actions... logged with actor, timestamp, and reason" — this system has no
// separate operator-identity/auth tier, so actor is caller-supplied and required, never
// derived from the request's auth context, Design Notes).
var ErrMissingApprovalActor = errors.New("actor is required to approve a withdrawal")

// ErrMissingApprovalReason is returned when an approve request omits reason (NFR11, same
// reasoning as ErrMissingApprovalActor).
var ErrMissingApprovalReason = errors.New("reason is required to approve a withdrawal")

// ApproveWithdrawal transitions an awaiting-approval withdrawal to approved (Story 3.3):
// an operator's explicit sign-off on a withdrawal Story 3.3's own threshold check routed
// there for exceeding its (chain, asset)'s configured amount. This is the only path (other
// than CreateWithdrawal's own auto-approval branch) that ever produces
// WithdrawalStatusApproved — there is no poller (Boundaries & Constraints).
type ApproveWithdrawal struct {
	repo WithdrawalRepository
}

// NewApproveWithdrawal constructs the use case against the given repository port.
func NewApproveWithdrawal(repo WithdrawalRepository) *ApproveWithdrawal {
	return &ApproveWithdrawal{repo: repo}
}

// Execute rejects an empty or whitespace-only actor or reason (NFR11) before ever reaching
// the repository — mirroring CreateWithdrawal's own validate-then-delegate shape — and
// otherwise delegates to WithdrawalRepository.ApproveWithdrawal, which locks the row,
// verifies its status, and performs the transition atomically. Both values are trimmed
// before validation AND before being persisted (re-review: a whitespace-only value like
// " " previously passed the == "" check and was stored verbatim as the audit trail's
// approved_by/approval_reason, undercutting NFR11's "logged with a meaningful actor"
// intent) — the trimmed value, not the raw one, is what reaches the repository.
func (uc *ApproveWithdrawal) Execute(ctx context.Context, id, actor, reason string) (Withdrawal, error) {
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if actor == "" {
		return Withdrawal{}, ErrMissingApprovalActor
	}
	if reason == "" {
		return Withdrawal{}, ErrMissingApprovalReason
	}

	return uc.repo.ApproveWithdrawal(ctx, id, actor, reason)
}
