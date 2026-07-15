package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// customerServer implements the generated ServerInterface. It holds no state of its
// own beyond the use cases it delegates to — all persistence and transaction handling
// happens through the core use cases and the ports they were constructed with.
type customerServer struct {
	createCustomer *core.CreateCustomer
	getBalances    *core.GetCustomerBalances
	createTransfer *core.CreateTransfer
}

// NewServerInterface constructs the generated ServerInterface implementation. Later
// stories add their own use cases here as this service grows.
func NewServerInterface(createCustomer *core.CreateCustomer, getBalances *core.GetCustomerBalances, createTransfer *core.CreateTransfer) ServerInterface {
	return &customerServer{createCustomer: createCustomer, getBalances: getBalances, createTransfer: createTransfer}
}

// CreateCustomer implements ServerInterface.CreateCustomer (POST /v1/customers).
// By the time this runs, AuthMiddleware and IdempotencyMiddleware have already
// authenticated the caller and opened the transaction carried on r.Context() — this
// handler and the use case it calls operate on that transaction, never their own (AD-4).
func (s *customerServer) CreateCustomer(w http.ResponseWriter, r *http.Request, _ CreateCustomerParams) {
	customer, err := s.createCustomer.Execute(r.Context())
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "customer-creation-failed", err.Error(), r.URL.Path)
		return
	}

	id, err := uuid.Parse(customer.ID)
	if err != nil {
		// core.Customer.ID is always a UUIDv7 string this service generated itself
		// (core.newUUIDv7) — a parse failure here is a bug upstream, not bad external
		// input, hence 500 rather than a normal validation error path.
		WriteProblem(w, http.StatusInternalServerError, "invalid-customer-id", err.Error(), r.URL.Path)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(Customer{
		Id:        id,
		CreatedAt: customer.CreatedAt,
	})
}

// GetCustomerBalances implements ServerInterface.GetCustomerBalances
// (GET /v1/customers/{id}/balances). This route is non-mutating: IdempotencyMiddleware
// passes it straight through without opening a transaction (Story 1.1's non-mutating
// bypass), so s.getBalances reads independently via its own pool, not r.Context()'s tx.
func (s *customerServer) GetCustomerBalances(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	balances, err := s.getBalances.Execute(r.Context(), id.String())
	if errors.Is(err, core.ErrCustomerNotFound) {
		WriteProblem(w, http.StatusNotFound, "customer-not-found", err.Error(), r.URL.Path)
		return
	}
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "get-balances-failed", err.Error(), r.URL.Path)
		return
	}

	resp := BalancesResponse{Balances: make([]Balance, 0, len(balances))}
	for _, b := range balances {
		resp.Balances = append(resp.Balances, Balance{
			Chain:   BalanceChain(b.Chain),
			Asset:   BalanceAsset(b.Asset),
			Balance: b.Balance.String(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
