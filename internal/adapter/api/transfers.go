package api

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"

	"github.com/google/uuid"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// CreateTransfer implements ServerInterface.CreateTransfer (POST /v1/transfers). By the
// time this runs, AuthMiddleware and IdempotencyMiddleware have already authenticated
// the caller and opened the transaction carried on r.Context() — this handler and the
// use case it calls operate on that transaction, never their own (AD-4).
//
// This is the first mutating endpoint with a JSON request body: std-http-server mode
// does not auto-decode it, so it is decoded here explicitly.
func (s *customerServer) CreateTransfer(w http.ResponseWriter, r *http.Request, params CreateTransferParams) {
	var body TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(w, http.StatusBadRequest, "invalid-transfer-request", "request body could not be decoded as JSON", r.URL.Path)
		return
	}

	// A JSON body that omits sourceCustomerId/destinationCustomerId decodes to the zero
	// UUID (openapi_types.UUID unmarshals a missing field to uuid.Nil rather than
	// erroring), which is a syntactically valid id — so without this guard an absent
	// required field would sail past decode and the account lookup and surface as a
	// misleading 404, not the 400 the OpenAPI contract documents for a malformed body.
	if body.SourceCustomerId == uuid.Nil || body.DestinationCustomerId == uuid.Nil {
		WriteProblem(w, http.StatusBadRequest, "invalid-transfer-request", "sourceCustomerId and destinationCustomerId are required", r.URL.Path)
		return
	}

	// chain/asset are externally supplied for the first time in this service (Stories
	// 1.1/1.2 only ever generated them internally from SupportedChainAssetPairs) — a
	// bogus value must be rejected here as 400, not silently fall through to a
	// misleading 404 from the account lookup.
	if !body.Chain.Valid() || !body.Asset.Valid() {
		WriteProblem(w, http.StatusBadRequest, "invalid-chain-or-asset", "chain and asset must be supported enum values", r.URL.Path)
		return
	}

	amount, ok := new(big.Int).SetString(body.Amount, 10)
	if !ok {
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", "amount must be a base-10 integer string", r.URL.Path)
		return
	}

	req := core.TransferRequest{
		SourceCustomerID:      body.SourceCustomerId.String(),
		DestinationCustomerID: body.DestinationCustomerId.String(),
		Chain:                 core.Chain(body.Chain),
		Asset:                 core.Asset(body.Asset),
		Amount:                amount,
		IdempotencyKey:        params.IdempotencyKey,
	}

	transfer, err := s.createTransfer.Execute(r.Context(), req)
	switch {
	case errors.Is(err, core.ErrSelfTransfer):
		WriteProblem(w, http.StatusBadRequest, "self-transfer-not-allowed", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrNonPositiveAmount):
		WriteProblem(w, http.StatusBadRequest, "invalid-amount", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrCustomerNotFound):
		WriteProblem(w, http.StatusNotFound, "customer-not-found", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrInsufficientBalance):
		WriteProblem(w, http.StatusUnprocessableEntity, "insufficient-balance", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrDuplicateTransferCause):
		WriteProblem(w, http.StatusConflict, "duplicate-transfer-cause", err.Error(), r.URL.Path)
		return
	case err != nil:
		WriteProblem(w, http.StatusInternalServerError, "transfer-failed", err.Error(), r.URL.Path)
		return
	}

	// core.Transfer's ids are always UUIDv7 strings this service generated itself
	// (uuid.NewV7 in TransferRepository) or validated on the way in via
	// openapi_types.UUID — a parse failure here is a bug upstream, not bad external
	// input, hence 500 rather than a normal validation error path (mirrors
	// CreateCustomer's handling of its own generated id).
	sourceID, err := uuid.Parse(transfer.SourceCustomerID)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "invalid-customer-id", err.Error(), r.URL.Path)
		return
	}
	destID, err := uuid.Parse(transfer.DestinationCustomerID)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "invalid-customer-id", err.Error(), r.URL.Path)
		return
	}
	journalEntryID, err := uuid.Parse(transfer.ID)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "invalid-transfer-id", err.Error(), r.URL.Path)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(Transfer{
		Id:                    journalEntryID,
		SourceCustomerId:      sourceID,
		DestinationCustomerId: destID,
		Chain:                 TransferChain(transfer.Chain),
		Asset:                 TransferAsset(transfer.Asset),
		Amount:                transfer.Amount.String(),
		CreatedAt:             transfer.CreatedAt,
	})
}
