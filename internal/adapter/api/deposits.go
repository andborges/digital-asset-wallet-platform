package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// GetCustomerDeposits implements ServerInterface.GetCustomerDeposits
// (GET /v1/customers/{id}/deposits). This route is non-mutating, like GetCustomerBalances:
// IdempotencyMiddleware passes it straight through without opening a transaction, so
// s.getDeposits reads independently via its own pool, not r.Context()'s tx.
func (s *customerServer) GetCustomerDeposits(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	deposits, err := s.getDeposits.Execute(r.Context(), id.String())
	if errors.Is(err, core.ErrCustomerNotFound) {
		WriteProblem(w, http.StatusNotFound, "customer-not-found", err.Error(), r.URL.Path)
		return
	}
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "get-deposits-failed", err.Error(), r.URL.Path)
		return
	}

	resp := DepositsResponse{Deposits: make([]Deposit, 0, len(deposits))}
	for _, d := range deposits {
		depositID, err := uuid.Parse(d.ID)
		if err != nil {
			// core.Deposit.ID is always a UUIDv7 string this service generated itself
			// (core.newUUIDv7, in TrackDeposits.Execute) — a parse failure here is a bug
			// upstream, not bad external input, hence 500 (mirrors GetCustomer's handling
			// of its own generated ids).
			WriteProblem(w, http.StatusInternalServerError, "invalid-deposit-id", err.Error(), r.URL.Path)
			return
		}
		// Observed/safe deposits are exposed as status "pending"; orphaned deposits
		// (Story 2.4) as status "orphaned" — a customer must be able to see a deposit
		// was reorged away, not have it silently vanish (AC1). Finalized/credited
		// deposits never reach here: DepositReader's query is scoped to
		// observed/safe/orphaned only.
		status := DepositStatusPending
		if d.State == core.DepositOrphaned {
			status = DepositStatusOrphaned
		}
		resp.Deposits = append(resp.Deposits, Deposit{
			Id:         depositID,
			Chain:      DepositChain(d.Chain),
			Asset:      DepositAsset(d.Asset),
			Amount:     d.Amount.String(),
			TxHash:     d.TxHash,
			Status:     status,
			Tier:       DepositTier(d.State),
			ObservedAt: d.ObservedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
