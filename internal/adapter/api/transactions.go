package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// ListCustomerTransactions implements ServerInterface.ListCustomerTransactions
// (GET /v1/customers/{id}/transactions). This route is non-mutating, like
// GetCustomerBalances: IdempotencyMiddleware passes it straight through without opening
// a transaction, so s.listTransactions reads independently via its own pool, not
// r.Context()'s tx.
func (s *customerServer) ListCustomerTransactions(w http.ResponseWriter, r *http.Request, id uuid.UUID, params ListCustomerTransactionsParams) {
	// params.PageSize is passed through as *int (not collapsed to 0 when nil): the use case
	// distinguishes an omitted pageSize (nil → default) from an explicit pageSize=0 (which
	// AC8 requires be rejected with 400). A non-numeric pageSize never reaches here — the
	// generated router's decode failure is routed through main.go's ErrorHandlerFunc → 400.
	var cursor string
	if params.Cursor != nil {
		cursor = *params.Cursor
	}

	page, err := s.listTransactions.Execute(r.Context(), id.String(), params.PageSize, cursor)
	switch {
	case errors.Is(err, core.ErrCustomerNotFound):
		WriteProblem(w, http.StatusNotFound, "customer-not-found", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrInvalidCursor):
		WriteProblem(w, http.StatusBadRequest, "invalid-cursor", err.Error(), r.URL.Path)
		return
	case errors.Is(err, core.ErrInvalidPageSize):
		WriteProblem(w, http.StatusBadRequest, "invalid-page-size", err.Error(), r.URL.Path)
		return
	case err != nil:
		WriteProblem(w, http.StatusInternalServerError, "list-transactions-failed", err.Error(), r.URL.Path)
		return
	}

	resp := TransactionsResponse{Transactions: make([]Transaction, 0, len(page.Transactions))}
	for _, t := range page.Transactions {
		txnID, err := uuid.Parse(t.ID)
		if err != nil {
			// core.Transaction.ID is always a journal entry id this service generated
			// itself (uuid.NewV7 in TransferRepository) — a parse failure here is a bug
			// upstream, not bad external input, hence 500 (mirrors CreateTransfer's
			// handling of its own generated ids).
			WriteProblem(w, http.StatusInternalServerError, "invalid-transaction-id", err.Error(), r.URL.Path)
			return
		}
		resp.Transactions = append(resp.Transactions, Transaction{
			Id:        txnID,
			Type:      t.Type,
			Amount:    t.Amount.String(),
			Chain:     TransactionChain(t.Chain),
			Asset:     TransactionAsset(t.Asset),
			Status:    t.Status,
			CreatedAt: t.CreatedAt,
		})
	}
	if page.NextCursor != "" {
		resp.NextCursor = &page.NextCursor
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
