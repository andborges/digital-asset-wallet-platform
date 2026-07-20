package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// destinationAddressPattern matches a structurally well-formed 20-byte hex EVM address —
// the same convention as unsupported_token_observations.address's existing CHECK (Story
// 2.3). This is a SHAPE check only: no denylist, no checksum validation — the denylist
// check (the zero address) is a separate, later step in Execute (Story 3.3).
var destinationAddressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// zeroDestinationAddress is v1's only destination-address denylist entry (Story 3.3, FR18:
// "e.g. the zero address," not an exhaustive list) — confirmed to be exactly 20 bytes
// (2 "0x" characters + 40 hex characters, one per nibble) before being hardcoded here,
// mirroring Story 3.1's own "verify byte length empirically" discipline after its
// address-transcription bug (adversarial review). Lowercase, matching
// destinationAddressPattern's own case-insensitive match — compared case-insensitively
// below rather than assuming callers always send lowercase.
const zeroDestinationAddress = "0x0000000000000000000000000000000000000000"

// CreateWithdrawal places an immediate ledger-only hold on a customer's available balance
// for a requested withdrawal (Story 3.2, AD-4), then — in the SAME request, the SAME
// transaction (Story 3.3, Design Notes: AD-6 "api-through-core, single writer", no
// separate poller) — evaluates FR18's minimal pre-signing policy set (fee-inclusive
// balance coverage, destination-address denylist) and FR17's threshold routing, computing
// the fee estimate and target status BEFORE calling the repository: the repository only
// executes what this use case already decided, never calling FeeEstimator or
// WithdrawalThresholdLister itself (AD-1's adapters-don't-call-adapters rule).
type CreateWithdrawal struct {
	repo         WithdrawalRepository
	feeEstimator FeeEstimator
	thresholds   WithdrawalThresholdLister
}

// NewCreateWithdrawal constructs the use case against the given repository and policy
// ports.
func NewCreateWithdrawal(repo WithdrawalRepository, feeEstimator FeeEstimator, thresholds WithdrawalThresholdLister) *CreateWithdrawal {
	return &CreateWithdrawal{repo: repo, feeEstimator: feeEstimator, thresholds: thresholds}
}

// Execute validates req's amount and destination address (shape, then the zero-address
// denylist), estimates the withdrawal's fee and looks up its (chain, asset) approval
// threshold, computes the target status (WithdrawalStatusApproved if req.Amount is at or
// below the threshold, WithdrawalStatusAwaitingApproval otherwise), and delegates to the
// repository to place the locked, balance-checked hold and write that target status —
// all before any hold is placed, so a rejection here never touches the ledger. Mirrors
// CreateTransfer's own validate-then-delegate shape, extended with Story 3.3's policy
// step.
func (uc *CreateWithdrawal) Execute(ctx context.Context, req WithdrawalRequest) (Withdrawal, error) {
	if req.Amount == nil || req.Amount.Sign() <= 0 {
		return Withdrawal{}, ErrNonPositiveAmount
	}
	// Reuses ErrAmountTooLarge from EstimateFee (Story 3.1 adversarial review) rather than
	// a parallel sentinel: the same on-chain representability limit applies here — a hold
	// this large would land in withdrawals.amount NUMERIC(78,0) (which permits values far
	// beyond uint256's max) only to become unbroadcastable once Story 3.4 tries to encode
	// it into a real transaction.
	if req.Amount.BitLen() > 256 {
		return Withdrawal{}, ErrAmountTooLarge
	}
	if !destinationAddressPattern.MatchString(req.DestinationAddress) {
		return Withdrawal{}, ErrMalformedDestinationAddress
	}
	if strings.EqualFold(req.DestinationAddress, zeroDestinationAddress) {
		return Withdrawal{}, ErrInvalidDestinationAddress
	}

	feeEstimate, err := uc.feeEstimator.EstimateFee(ctx, req.Chain, req.Asset, req.Amount)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("estimate withdrawal fee: %w", err)
	}
	threshold, err := uc.thresholds.GetApprovalThreshold(ctx, req.Chain, req.Asset)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("get withdrawal approval threshold: %w", err)
	}

	targetStatus := WithdrawalStatusAwaitingApproval
	if req.Amount.Cmp(threshold) <= 0 {
		targetStatus = WithdrawalStatusApproved
	}

	return uc.repo.CreateWithdrawal(ctx, req, feeEstimate.TotalFee, targetStatus)
}
