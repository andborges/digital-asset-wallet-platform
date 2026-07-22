package api

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"

	"github.com/google/uuid"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// CreateWithdrawal implements ServerInterface.CreateWithdrawal (POST /v1/withdrawals,
// Story 3.2, extended by Story 3.3's policy-check-and-route step). By the time this runs,
// AuthMiddleware and IdempotencyMiddleware have already authenticated the caller and
// opened the transaction carried on r.Context() — this handler and the use case it calls
// operate on that transaction, never their own (AD-4). Mirrors CreateTransfer's own
// decode-validate-delegate shape exactly.
func (s *customerServer) CreateWithdrawal(w http.ResponseWriter, r *http.Request, params CreateWithdrawalParams) {
	var body WithdrawalRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid-withdrawal-request", "request body could not be decoded as JSON", r.URL.Path)
		return
	}

	// A JSON body that omits customerId decodes to the zero UUID (openapi_types.UUID
	// unmarshals a missing field to uuid.Nil rather than erroring), which is a
	// syntactically valid id — so without this guard an absent required field would sail
	// past decode and the account lookup and surface as a misleading 404, not the 400 the
	// OpenAPI contract documents for a malformed body (mirrors CreateTransfer's identical
	// guard).
	if body.CustomerId == uuid.Nil {
		WriteProblem(w, http.StatusBadRequest, "invalid-withdrawal-request", "customerId is required", r.URL.Path)
		return
	}

	// chain/asset are externally supplied, not internally generated — a bogus value must
	// be rejected here as 400, not fall through to a misleading 404 from the account
	// lookup (mirrors CreateTransfer's identical validation).
	if !body.Chain.Valid() || !body.Asset.Valid() {
		WriteProblem(w, http.StatusBadRequest, "invalid-chain-or-asset", "chain and asset must be supported enum values", r.URL.Path)
		return
	}

	amount, ok := new(big.Int).SetString(body.Amount, 10)
	if !ok {
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", "amount must be a base-10 integer string", r.URL.Path)
		return
	}

	req := core.WithdrawalRequest{
		CustomerID:         body.CustomerId.String(),
		Chain:              core.Chain(body.Chain),
		Asset:              core.Asset(body.Asset),
		Amount:             amount,
		DestinationAddress: body.DestinationAddress,
		IdempotencyKey:     params.IdempotencyKey,
	}

	withdrawal, err := s.createWithdrawal.Execute(r.Context(), req)
	switch {
	case errors.Is(err, core.ErrNonPositiveAmount):
		// A fixed, endpoint-appropriate message, not err.Error() (mirrors
		// GetWithdrawalFeeEstimate's identical handling): ErrNonPositiveAmount's own text
		// is CreateTransfer's ("transfer amount must be a positive integer"), reused here
		// only for errors.Is identity, never for display on this endpoint.
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", "amount must be a positive integer", r.URL.Path)
		return
	case errors.Is(err, core.ErrAmountTooLarge):
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", "amount exceeds the maximum representable on-chain value", r.URL.Path)
		return
	case errors.Is(err, core.ErrMalformedDestinationAddress):
		WriteProblem(w, http.StatusBadRequest, "invalid-destination-address", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrInvalidDestinationAddress):
		// Story 3.3: the zero-address denylist check, distinct from the shape check above
		// — both are 400s, but a distinct problem "title" keeps the two failure reasons
		// distinguishable to a caller inspecting the response (re-review: the title itself
		// must actually differ, not just this comment's claim that it does).
		WriteProblem(w, http.StatusBadRequest, "denylisted-destination-address", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrCustomerNotFound):
		WriteProblem(w, http.StatusNotFound, "customer-not-found", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrInsufficientBalance):
		WriteProblem(w, http.StatusUnprocessableEntity, "insufficient-balance", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrInsufficientBalanceForFee):
		// Story 3.3: the fee-inclusive check, distinct from the pre-hold "can't even cover
		// amount" case above — both 422s, distinct problem "title".
		WriteProblem(w, http.StatusUnprocessableEntity, "insufficient-balance-for-fee", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrDuplicateWithdrawalCause):
		WriteProblem(w, http.StatusConflict, "duplicate-withdrawal-cause", err.Error(), r.URL.Path)
		return
	case err != nil:
		// Logged server-side, never forwarded to the caller (mirrors
		// GetWithdrawalFeeEstimate's identical handling, re-review adversarial review): err
		// here can wrap raw Postgres failures ("lock available/hold accounts: %w", "sum
		// available balance: %w", etc.) as well as a FeeEstimator/WithdrawalThresholdLister
		// failure (a transient RPC error, or a "no threshold configured" registry gap) — an
		// information-disclosure surface if returned verbatim in the response body, and
		// exactly the "fail loud, log server-side" case the story's own I/O matrix calls for
		// on a registry gap.
		s.logger.Error("withdrawal request failed", "error", err)
		WriteProblem(w, http.StatusInternalServerError, "withdrawal-failed", "failed to process withdrawal request", r.URL.Path)
		return
	}

	resp, err := s.toWithdrawalResponse(withdrawal)
	if err != nil {
		s.logger.Error("withdrawal has invalid generated ids", "error", err)
		WriteProblem(w, http.StatusInternalServerError, "invalid-withdrawal-id", "failed to process withdrawal request", r.URL.Path)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// ApproveWithdrawal implements ServerInterface.ApproveWithdrawal (POST
// /v1/withdrawals/{id}/approve, Story 3.3): an operator's explicit sign-off on an
// awaiting-approval withdrawal (FR17, NFR11). Like CreateWithdrawal, this handler runs
// inside the transaction IdempotencyMiddleware already opened on r.Context() (AD-4) —
// this is a mutating route on the same bearer-token + Idempotency-Key stack as every
// other one (Boundaries & Constraints).
func (s *customerServer) ApproveWithdrawal(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ ApproveWithdrawalParams) {
	var body ApproveWithdrawalRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid-approve-withdrawal-request", "request body could not be decoded as JSON", r.URL.Path)
		return
	}

	// NFR11: actor and reason are both required in the request body — this system has no
	// separate operator-identity/auth tier, so actor is caller-supplied, never derived from
	// the request's auth context (Design Notes). core.ApproveWithdrawal.Execute re-checks
	// this too (defense in depth for any other caller of that use case), but checking here
	// lets this handler map straight to core.ErrMissingApprovalActor/
	// ErrMissingApprovalReason below with no ambiguity about which layer rejected it.
	withdrawal, err := s.approveWithdrawal.Execute(r.Context(), id.String(), body.Actor, body.Reason)
	switch {
	case errors.Is(err, core.ErrMissingApprovalActor):
		WriteProblem(w, http.StatusBadRequest, "missing-approval-actor", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrMissingApprovalReason):
		WriteProblem(w, http.StatusBadRequest, "missing-approval-reason", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrWithdrawalNotFound):
		WriteProblem(w, http.StatusNotFound, "withdrawal-not-found", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrWithdrawalNotAwaitingApproval):
		WriteProblem(w, http.StatusConflict, "withdrawal-not-awaiting-approval", err.Error(), r.URL.Path)
		return
	case err != nil:
		// Logged server-side, never forwarded to the caller (mirrors CreateWithdrawal's
		// identical handling): err here can wrap raw Postgres failures.
		s.logger.Error("approve withdrawal request failed", "error", err)
		WriteProblem(w, http.StatusInternalServerError, "approve-withdrawal-failed", "failed to process approve withdrawal request", r.URL.Path)
		return
	}

	resp, err := s.toWithdrawalResponse(withdrawal)
	if err != nil {
		s.logger.Error("withdrawal has invalid generated ids", "error", err)
		WriteProblem(w, http.StatusInternalServerError, "invalid-withdrawal-id", "failed to process approve withdrawal request", r.URL.Path)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// toWithdrawalResponse converts a core.Withdrawal to its generated wire shape, shared by
// CreateWithdrawal and ApproveWithdrawal (both return the same resource, at different
// points in its lifecycle). core.Withdrawal.ID/CustomerID are always UUIDv7/validated
// strings this service generated or validated on the way in (mirrors CreateTransfer's
// identical handling of its own generated ids) — a parse failure here is a bug upstream,
// not bad external input, hence the caller maps a non-nil error to 500 rather than a
// normal validation error path.
func (s *customerServer) toWithdrawalResponse(withdrawal core.Withdrawal) (Withdrawal, error) {
	id, err := uuid.Parse(withdrawal.ID)
	if err != nil {
		return Withdrawal{}, err
	}
	customerID, err := uuid.Parse(withdrawal.CustomerID)
	if err != nil {
		return Withdrawal{}, err
	}

	resp := Withdrawal{
		Id:                 id,
		CustomerId:         customerID,
		Chain:              WithdrawalChain(withdrawal.Chain),
		Asset:              WithdrawalAsset(withdrawal.Asset),
		Amount:             withdrawal.Amount.String(),
		DestinationAddress: withdrawal.DestinationAddress,
		Status:             WithdrawalStatus(withdrawal.Status),
		CreatedAt:          withdrawal.CreatedAt,
	}
	// ApprovedAt/ApprovedBy/ApprovalReason stay nil/omitted for a withdrawal that was
	// auto-approved by the threshold check (never touched by an operator) or is still
	// awaiting approval — populated only once ApproveWithdrawal has transitioned the row
	// (Story 3.3, NFR11).
	if withdrawal.ApprovedAt != nil {
		resp.ApprovedAt = withdrawal.ApprovedAt
	}
	if withdrawal.ApprovedBy != "" {
		resp.ApprovedBy = &withdrawal.ApprovedBy
	}
	if withdrawal.ApprovalReason != "" {
		resp.ApprovalReason = &withdrawal.ApprovalReason
	}
	return resp, nil
}
