package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// withdrawalHoldCauseType is the journal_entries.cause_type WithdrawalRepository.
// CreateWithdrawal writes when placing a withdrawal's hold (Story 3.2). cause_id is the
// request's own Idempotency-Key — the same belt-and-suspenders ledger-level dedup
// CreateTransfer already relies on for its own narrow pre-commit race window (Intent).
const withdrawalHoldCauseType = "withdrawal_hold"

// approvalRequiredEventType and withdrawalApprovedEventType are the outbox event_types
// CreateWithdrawal writes (Story 3.3), one or the other depending on the target status
// core.CreateWithdrawal already decided — never both, and never the Story 3.2
// "withdrawal.created" event, which this story's CreateWithdrawal no longer writes on its
// own: a withdrawal is now never observably at rest in WithdrawalStatusCreated (Design
// Notes, AD-6 "api-through-core, single writer"), so its own outbox event would be
// indistinguishable from noise. ApproveWithdrawal writes withdrawalApprovedEventType too,
// for the operator-approval path.
const (
	approvalRequiredEventType   = "approval.required"
	withdrawalApprovedEventType = "withdrawal.approved"
)

// withdrawalRoutedPayload is the jsonb payload recorded alongside a newly created
// withdrawal's outbox event (either approvalRequiredEventType or
// withdrawalApprovedEventType, Story 3.3).
type withdrawalRoutedPayload struct {
	WithdrawalID       string `json:"withdrawalId"`
	CustomerID         string `json:"customerId"`
	Chain              string `json:"chain"`
	Asset              string `json:"asset"`
	Amount             string `json:"amount"`
	DestinationAddress string `json:"destinationAddress"`
}

// withdrawalApprovedByOperatorPayload is the jsonb payload recorded alongside the
// "withdrawal.approved" outbox event ApproveWithdrawal writes — distinct from
// withdrawalRoutedPayload because it also carries the approval's own actor/reason
// (NFR11), which an auto-approved withdrawal's payload never has.
type withdrawalApprovedByOperatorPayload struct {
	WithdrawalID   string `json:"withdrawalId"`
	ApprovedBy     string `json:"approvedBy"`
	ApprovalReason string `json:"approvalReason"`
}

// WithdrawalRepository implements core.WithdrawalRepository against PostgreSQL. Like
// TransferRepository, it carries no pool of its own — every call runs against the
// transaction already open on ctx (AD-4), obtained via TxBeginner.Begin by
// IdempotencyMiddleware.
type WithdrawalRepository struct{}

// NewWithdrawalRepository constructs a core.WithdrawalRepository.
func NewWithdrawalRepository() *WithdrawalRepository {
	return &WithdrawalRepository{}
}

// CreateWithdrawal locks the customer's available and hold accounts for (req.Chain,
// req.Asset) in ONE deterministic-order statement (mirrors TransferRepository.
// CreateTransfer's exact lock-ordering pattern), verifies the available account's derived
// balance covers req.Amount, then (Story 3.3) verifies that SAME already-locked balance
// read, minus req.Amount (i.e. the post-hold available balance — no second lock or second
// read needed, Design Notes), covers feeEstimate — and only then writes the withdrawal
// hold: one balanced journal entry (debit available, credit hold), its two postings, the
// withdrawals row (with status = targetStatus, computed by the caller BEFORE this method
// is ever invoked, never by this method calling FeeEstimator/WithdrawalThresholdLister
// itself, AD-1), and the matching paired outbox event — all inside the transaction already
// open on ctx (AD-4). A failed fee check returns before any of these writes, and since the
// entire call runs inside the caller's own transaction (never one this method opens or
// commits itself), returning an error here leaves nothing committed: the whole request
// transaction rolls back (IdempotencyMiddleware's own defer, AD-4).
func (r *WithdrawalRepository) CreateWithdrawal(ctx context.Context, req core.WithdrawalRequest, feeEstimate *big.Int, targetStatus string) (core.Withdrawal, error) {
	tx := txFromContext(ctx)

	// Both of this customer's own accounts for (chain, asset) — available and hold — are
	// locked in one statement, ordered by id. Unlike CreateTransfer's two-different-
	// customers case, a single customer's own two accounts can never race in opposite
	// directions against themselves, but the ORDER BY still matters: it is what makes this
	// lock acquisition order consistent with every other statement in this package that
	// locks rows from the accounts table, so no other repository can ever deadlock against
	// this one. Holding both locks for the rest of the transaction is what makes the
	// balance check below race-free against a second concurrent withdrawal request for
	// this same customer, chain, and asset.
	rows, err := tx.Query(ctx,
		`SELECT id, account_type FROM accounts
		 WHERE customer_id = $1 AND chain = $2 AND asset = $3 AND account_type IN ('available', 'hold')
		 ORDER BY id
		 FOR UPDATE`,
		req.CustomerID, string(req.Chain), string(req.Asset),
	)
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("lock available/hold accounts: %w", err)
	}
	accountByType := make(map[string]string, 2)
	rowCount := 0
	for rows.Next() {
		var accountID, accountType string
		if err := rows.Scan(&accountID, &accountType); err != nil {
			rows.Close()
			return core.Withdrawal{}, fmt.Errorf("scan locked account: %w", err)
		}
		accountByType[accountType] = accountID
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return core.Withdrawal{}, fmt.Errorf("iterate locked accounts: %w", err)
	}
	rows.Close()
	// The accounts table's own UNIQUE(customer_id, chain, asset, account_type) constraint
	// (migration 0009) should make more than one row per account_type unreachable — this
	// is a defensive check, not expected validation (re-review, adversarial review): if it
	// ever fired, silently keeping whichever row the map assignment above scanned last
	// would be the wrong failure mode for a ledger operation.
	if rowCount != len(accountByType) {
		return core.Withdrawal{}, fmt.Errorf("expected at most one account per account_type for customer %s (%s, %s), got %d rows for %d types", req.CustomerID, req.Chain, req.Asset, rowCount, len(accountByType))
	}

	availableAccountID, ok := accountByType[string(core.AccountTypeAvailable)]
	if !ok {
		return core.Withdrawal{}, fmt.Errorf("%w: customer %s has no (%s, %s) available account", core.ErrCustomerNotFound, req.CustomerID, req.Chain, req.Asset)
	}
	holdAccountID, ok := accountByType[string(core.AccountTypeHold)]
	if !ok {
		return core.Withdrawal{}, fmt.Errorf("%w: customer %s has no (%s, %s) hold account", core.ErrCustomerNotFound, req.CustomerID, req.Chain, req.Asset)
	}

	var balanceText string
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0)::text FROM postings WHERE account_id = $1`,
		availableAccountID,
	).Scan(&balanceText); err != nil {
		return core.Withdrawal{}, fmt.Errorf("sum available balance: %w", err)
	}
	balance, ok := new(big.Int).SetString(balanceText, 10)
	if !ok {
		return core.Withdrawal{}, fmt.Errorf("parse available balance %q as integer", balanceText)
	}
	if balance.Cmp(req.Amount) < 0 {
		return core.Withdrawal{}, core.ErrInsufficientBalance
	}

	// Story 3.3's fee-inclusive balance check: available_post_hold = available_pre_hold -
	// amount (Design Notes: arithmetically identical to requiring available_pre_hold >=
	// amount + fee). Reuses the SAME balance value already read above — no second SELECT,
	// no second lock — since the lock acquired at the top of this method is still held for
	// the rest of this transaction.
	postHoldAvailable := new(big.Int).Sub(balance, req.Amount)
	if postHoldAvailable.Cmp(feeEstimate) < 0 {
		return core.Withdrawal{}, core.ErrInsufficientBalanceForFee
	}

	journalEntryID, err := uuid.NewV7()
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("generate journal entry id: %w", err)
	}
	now := time.Now().UTC()

	if _, err := tx.Exec(ctx,
		`INSERT INTO journal_entries (id, cause_type, cause_id, created_at) VALUES ($1, $2, $3, $4)`,
		journalEntryID.String(), withdrawalHoldCauseType, req.IdempotencyKey, now,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return core.Withdrawal{}, fmt.Errorf("%w: idempotency key %s", core.ErrDuplicateWithdrawalCause, req.IdempotencyKey)
		}
		return core.Withdrawal{}, fmt.Errorf("insert withdrawal_hold journal entry: %w", err)
	}

	availablePostingID, err := uuid.NewV7()
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("generate posting id: %w", err)
	}
	holdPostingID, err := uuid.NewV7()
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("generate posting id: %w", err)
	}
	negAmount := new(big.Int).Neg(req.Amount)

	batch := &pgx.Batch{}
	batch.Queue(
		`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
		availablePostingID.String(), journalEntryID.String(), availableAccountID, negAmount.String(), now,
	)
	batch.Queue(
		`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
		holdPostingID.String(), journalEntryID.String(), holdAccountID, req.Amount.String(), now,
	)
	br := tx.SendBatch(ctx, batch)
	for range 2 {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return core.Withdrawal{}, fmt.Errorf("insert posting: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return core.Withdrawal{}, fmt.Errorf("close posting batch: %w", err)
	}

	withdrawalID, err := uuid.NewV7()
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("generate withdrawal id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawals (id, customer_id, chain, asset, amount, destination_address, status, hold_journal_entry_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8, $9, $9)`,
		withdrawalID.String(), req.CustomerID, string(req.Chain), string(req.Asset), req.Amount.String(), req.DestinationAddress,
		targetStatus, journalEntryID.String(), now,
	); err != nil {
		return core.Withdrawal{}, fmt.Errorf("insert withdrawal: %w", err)
	}

	// Story 3.3: exactly one of these two outbox events is written, depending on
	// targetStatus (the caller's own already-decided routing outcome) — never both, and
	// never Story 3.2's "withdrawal.created" (a withdrawal is never observably at rest in
	// WithdrawalStatusCreated from this story onward, Design Notes).
	var eventType string
	switch targetStatus {
	case core.WithdrawalStatusAwaitingApproval:
		eventType = approvalRequiredEventType
	case core.WithdrawalStatusApproved:
		eventType = withdrawalApprovedEventType
	default:
		// Unreachable: core.CreateWithdrawal.Execute only ever computes one of the two
		// values above — a defensive check, not expected validation, so a future bug
		// introducing a third target status fails loudly here rather than silently writing
		// no outbox event at all.
		return core.Withdrawal{}, fmt.Errorf("unrecognized withdrawal target status %q", targetStatus)
	}
	payload, err := json.Marshal(withdrawalRoutedPayload{
		WithdrawalID:       withdrawalID.String(),
		CustomerID:         req.CustomerID,
		Chain:              string(req.Chain),
		Asset:              string(req.Asset),
		Amount:             req.Amount.String(),
		DestinationAddress: req.DestinationAddress,
	})
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("marshal %s outbox payload: %w", eventType, err)
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("generate outbox event id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, event_type, payload, created_at) VALUES ($1, $2, $3, $4)`,
		eventID.String(), eventType, payload, now,
	); err != nil {
		return core.Withdrawal{}, fmt.Errorf("insert %s outbox event: %w", eventType, err)
	}

	return core.Withdrawal{
		ID:                 withdrawalID.String(),
		CustomerID:         req.CustomerID,
		Chain:              req.Chain,
		Asset:              req.Asset,
		Amount:             req.Amount,
		DestinationAddress: req.DestinationAddress,
		Status:             targetStatus,
		CreatedAt:          now,
	}, nil
}

// ApproveWithdrawal locks the withdrawal row FOR UPDATE, verifies it is currently
// WithdrawalStatusAwaitingApproval, and transitions it to WithdrawalStatusApproved,
// recording actor/reason/timestamp (NFR11) and writing a paired "withdrawal.approved"
// outbox event — atomically, in the transaction already open on ctx (AD-4). The row lock
// is what makes a concurrent double-approve race deterministic rather than a race: the
// loser's UPDATE (scoped to WHERE status = 'awaiting-approval') affects zero rows once the
// winner's UPDATE has already moved the row to 'approved', surfacing as
// ErrWithdrawalNotAwaitingApproval rather than a lost or double update.
func (r *WithdrawalRepository) ApproveWithdrawal(ctx context.Context, id, actor, reason string) (core.Withdrawal, error) {
	tx := txFromContext(ctx)

	var (
		customerID, chain, asset, amountText, destinationAddress, status string
		createdAt                                                        time.Time
	)
	if err := tx.QueryRow(ctx,
		`SELECT customer_id, chain, asset, amount::text, destination_address, status, created_at
		 FROM withdrawals WHERE id = $1 FOR UPDATE`,
		id,
	).Scan(&customerID, &chain, &asset, &amountText, &destinationAddress, &status, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Withdrawal{}, fmt.Errorf("%w: withdrawal %s", core.ErrWithdrawalNotFound, id)
		}
		return core.Withdrawal{}, fmt.Errorf("lock withdrawal row: %w", err)
	}
	if status != core.WithdrawalStatusAwaitingApproval {
		return core.Withdrawal{}, fmt.Errorf("%w: withdrawal %s has status %q", core.ErrWithdrawalNotAwaitingApproval, id, status)
	}

	amount, ok := new(big.Int).SetString(amountText, 10)
	if !ok {
		return core.Withdrawal{}, fmt.Errorf("parse withdrawal amount %q as integer", amountText)
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx,
		`UPDATE withdrawals
		 SET status = $1, approved_at = $2, approved_by = $3, approval_reason = $4, updated_at = $2
		 WHERE id = $5 AND status = $6`,
		core.WithdrawalStatusApproved, now, actor, reason, id, core.WithdrawalStatusAwaitingApproval,
	); err != nil {
		return core.Withdrawal{}, fmt.Errorf("update withdrawal to approved: %w", err)
	}

	payload, err := json.Marshal(withdrawalApprovedByOperatorPayload{
		WithdrawalID:   id,
		ApprovedBy:     actor,
		ApprovalReason: reason,
	})
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("marshal withdrawal.approved outbox payload: %w", err)
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("generate outbox event id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, event_type, payload, created_at) VALUES ($1, $2, $3, $4)`,
		eventID.String(), withdrawalApprovedEventType, payload, now,
	); err != nil {
		return core.Withdrawal{}, fmt.Errorf("insert withdrawal.approved outbox event: %w", err)
	}

	return core.Withdrawal{
		ID:                 id,
		CustomerID:         customerID,
		Chain:              core.Chain(chain),
		Asset:              core.Asset(asset),
		Amount:             amount,
		DestinationAddress: destinationAddress,
		Status:             core.WithdrawalStatusApproved,
		CreatedAt:          createdAt,
		ApprovedAt:         &now,
		ApprovedBy:         actor,
		ApprovalReason:     reason,
	}, nil
}

var _ core.WithdrawalRepository = (*WithdrawalRepository)(nil)
