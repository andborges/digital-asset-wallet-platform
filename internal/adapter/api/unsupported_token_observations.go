package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// GetUnsupportedTokenObservations implements ServerInterface.GetUnsupportedTokenObservations
// (GET /v1/unsupported-token-observations). Unlike every other GET route in this
// package, this one takes no path parameter — it is a top-level, platform-wide route
// (Story 2.3, FR11): an unsupported-token observation carries no customer attribution of
// its own, only the deposit address it landed on. It is non-mutating like
// GetCustomerBalances: IdempotencyMiddleware passes it straight through without opening
// a transaction, so it reads independently via its own pool, not r.Context()'s tx. It
// still requires the same bearer auth as every other route (no new role concept).
func (s *customerServer) GetUnsupportedTokenObservations(w http.ResponseWriter, r *http.Request) {
	observations, err := s.listUnsupportedTokenObservations.Execute(r.Context())
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "list-unsupported-token-observations-failed", err.Error(), r.URL.Path)
		return
	}

	resp := UnsupportedTokenObservationsResponse{Observations: make([]UnsupportedTokenObservation, 0, len(observations))}
	for _, o := range observations {
		id, err := uuid.Parse(o.ID)
		if err != nil {
			// core.UnsupportedTokenObservation.ID is always a UUIDv7 string this service
			// generated itself (core.newUUIDv7, in TrackDeposits.Execute) — a parse
			// failure here is a bug upstream, not bad external input, hence 500 (mirrors
			// GetCustomerDeposits' handling of its own generated ids).
			WriteProblem(w, http.StatusInternalServerError, "invalid-observation-id", err.Error(), r.URL.Path)
			return
		}
		resp.Observations = append(resp.Observations, UnsupportedTokenObservation{
			Id:              id,
			Chain:           UnsupportedTokenObservationChain(o.Chain),
			DepositAddress:  o.Address,
			ContractAddress: o.ContractAddress,
			TxHash:          o.TxHash,
			Amount:          o.Amount.String(),
			BlockNumber:     int64(o.BlockNumber),
			ObservedAt:      o.ObservedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
