package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// TransferRepository implements core.TransferRepository against PostgreSQL. Like
// CustomerRepository, it carries no pool of its own — this serves a mutating POST, so
// every call runs against the transaction already open on ctx (AD-4), obtained via
// TxBeginner.Begin by IdempotencyMiddleware.
type TransferRepository struct{}

// NewTransferRepository constructs a core.TransferRepository.
func NewTransferRepository() *TransferRepository {
	return &TransferRepository{}
}

// CreateTransfer locks both accounts in one deterministic-order statement, verifies the
// source account's derived balance covers req.Amount, and writes the journal entry plus
// its two postings — all inside the transaction already open on ctx.
func (r *TransferRepository) CreateTransfer(ctx context.Context, req core.TransferRequest) (core.Transfer, error) {
	tx := txFromContext(ctx)

	// Lock both accounts in ONE statement, ordered by id, so two opposite-direction
	// concurrent transfers (A→B racing B→A) can never deadlock. The FOR UPDATE row locks
	// are acquired by the plan's LockRows node, which sits ABOVE the Sort — so rows are
	// locked in ORDER BY id order, the same global order for every transfer regardless of
	// direction. (The ORDER BY is load-bearing for this guarantee, not cosmetic: drop it
	// and lock-acquisition order becomes plan-dependent and deadlock reappears.) Holding
	// this lock for the rest of the transaction is also what makes the balance check below
	// race-free: a concurrent transfer against the same source account cannot even read
	// the balance until this transaction commits or rolls back.
	// account_type = 'available' (Story 3.2): since that story, every customer has TWO
	// accounts per (chain, asset) — available and hold — so an unfiltered lookup here
	// would return up to 4 rows instead of 2, and accountByCustomer's map assignment below
	// would nondeterministically pick whichever account (available or hold) happened to
	// sort last per customer. An internal transfer only ever moves a customer's available
	// balance; it never touches the hold account, which exists solely for withdrawal holds.
	rows, err := tx.Query(ctx,
		`SELECT id, customer_id FROM accounts
		 WHERE chain = $1 AND asset = $2 AND customer_id = ANY($3::uuid[]) AND account_type = 'available'
		 ORDER BY id
		 FOR UPDATE`,
		string(req.Chain), string(req.Asset), []string{req.SourceCustomerID, req.DestinationCustomerID},
	)
	if err != nil {
		return core.Transfer{}, fmt.Errorf("lock accounts: %w", err)
	}
	accountByCustomer := make(map[string]string, 2)
	for rows.Next() {
		var accountID, customerID string
		if err := rows.Scan(&accountID, &customerID); err != nil {
			rows.Close()
			return core.Transfer{}, fmt.Errorf("scan locked account: %w", err)
		}
		accountByCustomer[customerID] = accountID
	}
	if err := rows.Err(); err != nil {
		return core.Transfer{}, fmt.Errorf("iterate locked accounts: %w", err)
	}
	rows.Close()

	sourceAccountID, ok := accountByCustomer[req.SourceCustomerID]
	if !ok {
		return core.Transfer{}, fmt.Errorf("%w: source customer %s has no (%s, %s) account", core.ErrCustomerNotFound, req.SourceCustomerID, req.Chain, req.Asset)
	}
	destAccountID, ok := accountByCustomer[req.DestinationCustomerID]
	if !ok {
		return core.Transfer{}, fmt.Errorf("%w: destination customer %s has no (%s, %s) account", core.ErrCustomerNotFound, req.DestinationCustomerID, req.Chain, req.Asset)
	}

	var balanceText string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0)::text FROM postings WHERE account_id = $1`,
		sourceAccountID,
	).Scan(&balanceText); err != nil {
		return core.Transfer{}, fmt.Errorf("sum source balance: %w", err)
	}
	balance, ok := new(big.Int).SetString(balanceText, 10)
	if !ok {
		return core.Transfer{}, fmt.Errorf("parse source balance %q as integer", balanceText)
	}
	if balance.Cmp(req.Amount) < 0 {
		return core.Transfer{}, core.ErrInsufficientBalance
	}

	journalEntryID, err := uuid.NewV7()
	if err != nil {
		return core.Transfer{}, fmt.Errorf("generate journal entry id: %w", err)
	}
	now := time.Now().UTC()

	if _, err := tx.Exec(ctx,
		`INSERT INTO journal_entries (id, cause_type, cause_id, created_at) VALUES ($1, 'internal_transfer', $2, $3)`,
		journalEntryID.String(), req.IdempotencyKey, now,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return core.Transfer{}, fmt.Errorf("%w: idempotency key %s", core.ErrDuplicateTransferCause, req.IdempotencyKey)
		}
		return core.Transfer{}, fmt.Errorf("insert journal entry: %w", err)
	}

	sourcePostingID, err := uuid.NewV7()
	if err != nil {
		return core.Transfer{}, fmt.Errorf("generate posting id: %w", err)
	}
	destPostingID, err := uuid.NewV7()
	if err != nil {
		return core.Transfer{}, fmt.Errorf("generate posting id: %w", err)
	}
	negAmount := new(big.Int).Neg(req.Amount)

	batch := &pgx.Batch{}
	batch.Queue(
		`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
		sourcePostingID.String(), journalEntryID.String(), sourceAccountID, negAmount.String(), now,
	)
	batch.Queue(
		`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
		destPostingID.String(), journalEntryID.String(), destAccountID, req.Amount.String(), now,
	)
	br := tx.SendBatch(ctx, batch)
	for range 2 {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return core.Transfer{}, fmt.Errorf("insert posting: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return core.Transfer{}, fmt.Errorf("close posting batch: %w", err)
	}

	return core.Transfer{
		ID:                    journalEntryID.String(),
		SourceCustomerID:      req.SourceCustomerID,
		DestinationCustomerID: req.DestinationCustomerID,
		Chain:                 req.Chain,
		Asset:                 req.Asset,
		Amount:                req.Amount,
		CreatedAt:             now,
	}, nil
}
