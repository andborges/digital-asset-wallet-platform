package core

import (
	"context"
	"errors"
	"math/big"
)

// ErrAmountTooLarge is returned when amount exceeds the maximum value a uint256 (the EVM's
// native integer width) can represent (Story 3.1 adversarial review). Without this check,
// an oversized amount would reach the EVM adapter's ABI encoder, which wraps a too-large
// value modulo 2^256 silently rather than erroring — producing a "successful" estimate
// computed against a completely different, wrapped amount.
var ErrAmountTooLarge = errors.New("amount exceeds the maximum representable on-chain value")

// EstimateFee computes a withdrawal's fee estimate for the given chain, asset, and amount
// (Story 3.1) — a pure, unpersisted, read-only computation: no withdrawal resource exists
// until Story 3.2, and this use case never writes anything.
type EstimateFee struct {
	estimator FeeEstimator
}

// NewEstimateFee constructs the use case against the given port.
func NewEstimateFee(estimator FeeEstimator) *EstimateFee {
	return &EstimateFee{estimator: estimator}
}

// Execute rejects a non-positive amount with ErrNonPositiveAmount (mirroring
// CreateTransfer's own amount-validation convention exactly), rejects an amount too large
// for a uint256 with ErrAmountTooLarge, and otherwise delegates to the chain-specific
// FeeEstimator port.
func (uc *EstimateFee) Execute(ctx context.Context, chain Chain, asset Asset, amount *big.Int) (FeeEstimate, error) {
	if amount == nil || amount.Sign() <= 0 {
		return FeeEstimate{}, ErrNonPositiveAmount
	}
	if amount.BitLen() > 256 {
		return FeeEstimate{}, ErrAmountTooLarge
	}

	return uc.estimator.EstimateFee(ctx, chain, asset, amount)
}
