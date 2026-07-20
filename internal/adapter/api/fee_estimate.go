package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// feeEstimateRPCTimeout bounds the live chain RPC round-trips GetWithdrawalFeeEstimate's
// use case makes (Story 3.1 adversarial review) — without it, a stalled/unreachable RPC
// endpoint hangs the request until the server's global WriteTimeout instead of failing
// fast with a clear error (mirrors main.go's deployerCheckTimeout pattern).
const feeEstimateRPCTimeout = 10 * time.Second

// GetWithdrawalFeeEstimate implements ServerInterface.GetWithdrawalFeeEstimate
// (GET /v1/withdrawals/fee-estimate, Story 3.1). This route is non-mutating, like
// GetCustomerBalances: IdempotencyMiddleware passes it straight through without opening a
// transaction, so s.estimateFee reads independently, never against r.Context()'s tx. No
// withdrawal resource exists until Story 3.2 — this is a pure, unpersisted computation,
// never a database write.
func (s *customerServer) GetWithdrawalFeeEstimate(w http.ResponseWriter, r *http.Request, params GetWithdrawalFeeEstimateParams) {
	// chain/asset are externally supplied query parameters, not internally generated
	// (the same reasoning CreateTransfer's own chain/asset validation documents) — a
	// bogus value must be rejected here as 400, not passed through to the estimator.
	// Checked separately (re-review, adversarial review) so the response tells the caller
	// which parameter was actually invalid, rather than one generic message for either.
	if !params.Chain.Valid() {
		WriteProblem(w, http.StatusBadRequest, "invalid-chain", "chain must be a supported enum value", r.URL.Path)
		return
	}
	if !params.Asset.Valid() {
		WriteProblem(w, http.StatusBadRequest, "invalid-asset", "asset must be a supported enum value", r.URL.Path)
		return
	}

	amount, ok := new(big.Int).SetString(params.Amount, 10)
	if !ok {
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", "amount must be a base-10 integer string", r.URL.Path)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), feeEstimateRPCTimeout)
	defer cancel()

	estimate, err := s.estimateFee.Execute(ctx, core.Chain(params.Chain), core.Asset(params.Asset), amount)
	switch {
	case errors.Is(err, core.ErrNonPositiveAmount):
		// A fixed, endpoint-appropriate message, not err.Error() (re-review, adversarial
		// review): ErrNonPositiveAmount's own text is CreateTransfer's ("transfer amount
		// must be a positive integer"), reused here only for errors.Is identity, not for
		// display on this non-transfer endpoint.
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", "amount must be a positive integer", r.URL.Path)
		return
	case errors.Is(err, core.ErrAmountTooLarge):
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", "amount exceeds the maximum representable on-chain value", r.URL.Path)
		return
	case err != nil:
		// Logged server-side, never forwarded to the caller (re-review, adversarial
		// review): err wraps raw RPC/node internals (e.g. "call NodeInterface.
		// gasEstimateComponents: ..."), which the story's own I/O matrix requires be
		// "clearly logged server-side" rather than exposed as a public information-
		// disclosure surface in the response body.
		s.logger.Error("fee estimate failed", "chain", params.Chain, "asset", params.Asset, "error", err)
		WriteProblem(w, http.StatusInternalServerError, "fee-estimate-failed", "failed to compute fee estimate", r.URL.Path)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(FeeEstimate{
		L2Fee:    estimate.L2Fee.String(),
		L1Fee:    estimate.L1Fee.String(),
		TotalFee: estimate.TotalFee.String(),
	})
}
