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

// withdrawalRoutedPayload is the jsonb payload recorded alongside every outbox event this
// story writes (approvalRequiredEventType, and withdrawalApprovedEventType from EITHER
// producer — CreateWithdrawal's auto-approval branch or ApproveWithdrawal's operator
// action) — one fixed shape regardless of event_type or write path (re-review 2026-07-21:
// the two withdrawalApprovedEventType producers previously wrote structurally different
// payloads under the identical event_type — CreateWithdrawal's carried
// chain/asset/amount/destinationAddress/customerId but no approvedBy/approvalReason,
// ApproveWithdrawal's carried only approvedBy/approvalReason — an undocumented schema-drift
// hazard for any consumer, e.g. Story 3.4's future broadcaster, subscribed to
// "withdrawal.approved" and expecting one decodable shape). ApprovedBy/ApprovalReason are
// omitted entirely (via omitempty), not just empty, on approvalRequiredEventType and on
// CreateWithdrawal's auto-approval branch — neither has an operator action to record.
type withdrawalRoutedPayload struct {
	WithdrawalID       string `json:"withdrawalId"`
	CustomerID         string `json:"customerId"`
	Chain              string `json:"chain"`
	Asset              string `json:"asset"`
	Amount             string `json:"amount"`
	DestinationAddress string `json:"destinationAddress"`
	ApprovedBy         string `json:"approvedBy,omitempty"`
	ApprovalReason     string `json:"approvalReason,omitempty"`
}

// withdrawalSettlementCauseType and withdrawalFailureCauseType are the journal_entries.
// cause_type values SettleConfirmedWithdrawal/SettleFailedWithdrawal write (Story 3.4).
// cause_id is the withdrawal's own id in both cases — a withdrawal's natural settlement
// dedup key, globally unique, one-to-one with exactly one settlement ever (mirrors
// CreditFinalizedDeposits' identical "cause_id = deposit.id" reasoning), backed by the same
// UNIQUE(cause_type, cause_id) constraint (AD-3) as every other journal entry in this
// codebase.
const (
	withdrawalSettlementCauseType = "withdrawal_settlement"
	withdrawalFailureCauseType    = "withdrawal_failure"
)

// withdrawalConfirmedEventType and withdrawalFailedEventType are the outbox event_types
// SettleConfirmedWithdrawal/SettleFailedWithdrawal write in the same transaction as their
// own settlement (AD-4), per the Acceptance Criteria's explicit "withdrawal.confirmed"/
// "withdrawal.failed" outbox events.
const (
	withdrawalConfirmedEventType = "withdrawal.confirmed"
	withdrawalFailedEventType    = "withdrawal.failed"
)

// ErrNoChainNonceState is returned by ClaimApprovedWithdrawal when no chain_nonce_state row
// exists for the requested chain — a registry gap that should never happen in a correctly
// migrated deployment (migration 0011 seeds one row per supported chain), mirroring
// ErrNoWithdrawalApprovalThreshold's own "never a guessed default, fail loud" principle.
var ErrNoChainNonceState = errors.New("no chain_nonce_state row configured for this chain")

// ErrNoTreasuryAccount is returned by SettleConfirmedWithdrawal when no platform treasury
// account row exists for the withdrawal's (chain, asset) — a registry gap that should never
// happen in a correctly migrated deployment (migration 0011 seeds one treasury row per
// core.SupportedChainAssetPairs entry), mirroring Story 3.3's identical "registry gap, fail
// loud" principle (I/O & Edge-Case Matrix).
var ErrNoTreasuryAccount = errors.New("no platform treasury account configured for this chain/asset")

// ErrNoHoldAccount and ErrNoAvailableAccount are returned by settleWithdrawal when the
// customer's own hold or available account for the withdrawal's (chain, asset) is missing
// — should be unreachable in practice (CreateCustomer always provisions all four per
// (chain, asset) account types, Story 1.1/3.2), but given dedicated sentinels (re-review
// 2026-07-21) rather than ad hoc fmt.Errorf, mirroring ErrNoTreasuryAccount's/
// ErrNoChainNonceState's own "registry gap gets a matchable sentinel" precedent so a
// caller/test can errors.Is-match these exact failure modes too, not only the platform-side
// one.
var (
	ErrNoHoldAccount      = errors.New("customer has no hold account for this chain/asset")
	ErrNoAvailableAccount = errors.New("customer has no available account for this chain/asset")
)

// withdrawalSettledPayload is the jsonb payload recorded alongside every settlement outbox
// event this story writes (withdrawalConfirmedEventType or withdrawalFailedEventType) — one
// fixed shape regardless of which settlement path produced it, mirroring
// withdrawalRoutedPayload's own "one fixed shape" discipline above.
type withdrawalSettledPayload struct {
	WithdrawalID   string `json:"withdrawalId"`
	JournalEntryID string `json:"journalEntryId"`
	CustomerID     string `json:"customerId"`
	Chain          string `json:"chain"`
	Asset          string `json:"asset"`
	Amount         string `json:"amount"`
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
		rows.Close()
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
	tag, err := tx.Exec(ctx,
		`UPDATE withdrawals
		 SET status = $1, approved_at = $2, approved_by = $3, approval_reason = $4, updated_at = $2
		 WHERE id = $5 AND status = $6`,
		core.WithdrawalStatusApproved, now, actor, reason, id, core.WithdrawalStatusAwaitingApproval,
	)
	if err != nil {
		return core.Withdrawal{}, fmt.Errorf("update withdrawal to approved: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Currently unreachable — the SELECT ... FOR UPDATE above already re-verified
		// status = 'awaiting-approval' inside this same transaction, so this WHERE clause
		// can only fail to match if that check's own row somehow no longer satisfies it,
		// which the row lock rules out. Checked anyway (re-review 2026-07-21, mirrors
		// Story 2.4's identical OrphanDeposit fix): defense-in-depth against a future
		// refactor that removes or weakens the SELECT check while leaving this UPDATE's
		// WHERE clause as the only remaining guard — a silent 0-row update must never be
		// reported as a successful approval.
		return core.Withdrawal{}, fmt.Errorf("%w: withdrawal %s status changed concurrently", core.ErrWithdrawalNotAwaitingApproval, id)
	}

	payload, err := json.Marshal(withdrawalRoutedPayload{
		WithdrawalID:       id,
		CustomerID:         customerID,
		Chain:              chain,
		Asset:              asset,
		Amount:             amountText,
		DestinationAddress: destinationAddress,
		ApprovedBy:         actor,
		ApprovalReason:     reason,
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

// ClaimApprovedWithdrawal implements core.WithdrawalRepository (Story 3.4): it locks one
// approved withdrawal on chain (FOR UPDATE SKIP LOCKED — a defensive concurrency guard, not
// load-bearing, since AD-11 pins exactly one broadcaster process per chain), allocates the
// next nonce from chain_nonce_state (also locked FOR UPDATE, incremented in this same
// transaction), inserts the broadcast_attempts row, and transitions the withdrawal to
// WithdrawalStatusSigned — ALL of which the caller commits BEFORE any sign/broadcast call
// happens (AD-11's exact wording; see core.SignAndBroadcastWithdrawal.Execute).
func (r *WithdrawalRepository) ClaimApprovedWithdrawal(ctx context.Context, chain core.Chain) (core.Withdrawal, bool, error) {
	tx := txFromContext(ctx)

	var (
		id, customerID, asset, amountText, destinationAddress string
		createdAt                                             time.Time
	)
	err := tx.QueryRow(ctx,
		`SELECT id, customer_id, asset, amount::text, destination_address, created_at
		 FROM withdrawals
		 WHERE chain = $1 AND status = $2
		 ORDER BY created_at, id
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`,
		string(chain), core.WithdrawalStatusApproved,
	).Scan(&id, &customerID, &asset, &amountText, &destinationAddress, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Withdrawal{}, false, nil
	}
	if err != nil {
		return core.Withdrawal{}, false, fmt.Errorf("claim approved withdrawal: %w", err)
	}

	amount, ok := new(big.Int).SetString(amountText, 10)
	if !ok {
		return core.Withdrawal{}, false, fmt.Errorf("parse withdrawal amount %q as integer", amountText)
	}

	var nextNonce int64
	if err := tx.QueryRow(ctx,
		`SELECT next_nonce FROM chain_nonce_state WHERE chain = $1 FOR UPDATE`,
		string(chain),
	).Scan(&nextNonce); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Withdrawal{}, false, fmt.Errorf("%w: chain %q", ErrNoChainNonceState, chain)
		}
		return core.Withdrawal{}, false, fmt.Errorf("lock chain nonce state: %w", err)
	}

	now := time.Now().UTC()
	nonceTag, err := tx.Exec(ctx,
		`UPDATE chain_nonce_state SET next_nonce = next_nonce + 1, updated_at = $2 WHERE chain = $1`,
		string(chain), now,
	)
	if err != nil {
		return core.Withdrawal{}, false, fmt.Errorf("advance chain nonce state: %w", err)
	}
	if nonceTag.RowsAffected() == 0 {
		// Currently unreachable — the SELECT ... FOR UPDATE immediately above already
		// locked this exact row, so this UPDATE's unconditional WHERE chain = $1 can only
		// fail to match if that row vanished between the two statements, which the lock
		// held across this same transaction rules out. Checked anyway (re-review
		// 2026-07-21, mirrors this function's own RowsAffected check on the withdrawals
		// UPDATE below): a silent 0-row update must never let a nonce be reported as
		// allocated when it wasn't.
		return core.Withdrawal{}, false, fmt.Errorf("advance chain nonce state for chain %q: no row updated", chain)
	}

	attemptID, err := uuid.NewV7()
	if err != nil {
		return core.Withdrawal{}, false, fmt.Errorf("generate broadcast attempt id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO broadcast_attempts (id, withdrawal_id, chain, nonce, created_at) VALUES ($1, $2, $3, $4, $5)`,
		attemptID.String(), id, string(chain), nextNonce, now,
	); err != nil {
		return core.Withdrawal{}, false, fmt.Errorf("insert broadcast attempt: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawals SET status = $1, nonce = $2, updated_at = $3 WHERE id = $4 AND status = $5`,
		core.WithdrawalStatusSigned, nextNonce, now, id, core.WithdrawalStatusApproved,
	)
	if err != nil {
		return core.Withdrawal{}, false, fmt.Errorf("update withdrawal to signed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Currently unreachable — the SELECT ... FOR UPDATE SKIP LOCKED above already
		// locked and re-verified status = 'approved' inside this same transaction, so this
		// WHERE clause can only fail to match if that check's own row somehow no longer
		// satisfies it, which the row lock rules out. Checked anyway (mirrors
		// ApproveWithdrawal's identical defense-in-depth): a silent 0-row update must never
		// be reported as a successful claim.
		return core.Withdrawal{}, false, fmt.Errorf("withdrawal %s status changed concurrently while claiming", id)
	}

	nonce := nextNonce
	return core.Withdrawal{
		ID:                 id,
		CustomerID:         customerID,
		Chain:              chain,
		Asset:              core.Asset(asset),
		Amount:             amount,
		DestinationAddress: destinationAddress,
		Status:             core.WithdrawalStatusSigned,
		CreatedAt:          createdAt,
		Nonce:              &nonce,
	}, true, nil
}

// RecordBroadcastTxHash implements core.WithdrawalRepository (Story 3.4): records txHash
// on both broadcast_attempts and withdrawals (the latter a denormalized read-convenience
// column, Design Notes) and transitions the withdrawal to WithdrawalStatusBroadcast —
// called only after TransactionBroadcaster.SendRawTransaction has already succeeded.
func (r *WithdrawalRepository) RecordBroadcastTxHash(ctx context.Context, withdrawalID, txHash string) error {
	tx := txFromContext(ctx)

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM withdrawals WHERE id = $1 FOR UPDATE`, withdrawalID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: withdrawal %s", core.ErrWithdrawalNotFound, withdrawalID)
		}
		return fmt.Errorf("lock withdrawal row: %w", err)
	}
	if status != core.WithdrawalStatusSigned {
		return fmt.Errorf("%w: withdrawal %s has status %q", core.ErrWithdrawalNotSigned, withdrawalID, status)
	}

	now := time.Now().UTC()
	attemptTag, err := tx.Exec(ctx,
		`UPDATE broadcast_attempts SET tx_hash = $1 WHERE withdrawal_id = $2`,
		txHash, withdrawalID,
	)
	if err != nil {
		return fmt.Errorf("record broadcast attempt tx hash: %w", err)
	}
	if attemptTag.RowsAffected() == 0 {
		// Currently unreachable — ClaimApprovedWithdrawal always inserts exactly one
		// broadcast_attempts row for this withdrawal_id before it can ever reach
		// WithdrawalStatusSigned, which the status check above already re-verified inside
		// this same transaction. Checked anyway (re-review 2026-07-21): without this, a
		// missing/deleted broadcast_attempts row would let withdrawals.status/tx_hash
		// advance below while broadcast_attempts — this repository's own documented
		// source of truth for tx_hash — silently stayed empty.
		return fmt.Errorf("record broadcast attempt tx hash for withdrawal %s: no broadcast_attempts row found", withdrawalID)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawals SET status = $1, tx_hash = $2, updated_at = $3 WHERE id = $4 AND status = $5`,
		core.WithdrawalStatusBroadcast, txHash, now, withdrawalID, core.WithdrawalStatusSigned,
	)
	if err != nil {
		return fmt.Errorf("update withdrawal to broadcast: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Same defense-in-depth reasoning as ClaimApprovedWithdrawal's identical check
		// above — the SELECT ... FOR UPDATE already re-verified status = 'signed' inside
		// this same transaction.
		return fmt.Errorf("%w: withdrawal %s status changed concurrently", core.ErrWithdrawalNotSigned, withdrawalID)
	}
	return nil
}

// ListBroadcastWithdrawals implements core.WithdrawalRepository (Story 3.4): a plain read
// of every withdrawal on chain currently at WithdrawalStatusBroadcast with a known tx_hash
// — PollWithdrawalReceipts' own input set each poll cycle.
func (r *WithdrawalRepository) ListBroadcastWithdrawals(ctx context.Context, chain core.Chain) ([]core.Withdrawal, error) {
	tx := txFromContext(ctx)

	rows, err := tx.Query(ctx,
		`SELECT id, customer_id, asset, amount::text, destination_address, created_at, tx_hash, nonce
		 FROM withdrawals
		 WHERE chain = $1 AND status = $2 AND tx_hash IS NOT NULL
		 ORDER BY created_at`,
		string(chain), core.WithdrawalStatusBroadcast,
	)
	if err != nil {
		return nil, fmt.Errorf("list broadcast withdrawals: %w", err)
	}
	defer rows.Close()

	var out []core.Withdrawal
	for rows.Next() {
		var (
			id, customerID, asset, amountText, destinationAddress, txHash string
			createdAt                                                     time.Time
			nonce                                                         int64
		)
		if err := rows.Scan(&id, &customerID, &asset, &amountText, &destinationAddress, &createdAt, &txHash, &nonce); err != nil {
			return nil, fmt.Errorf("scan broadcast withdrawal: %w", err)
		}
		amount, ok := new(big.Int).SetString(amountText, 10)
		if !ok {
			return nil, fmt.Errorf("parse withdrawal amount %q as integer", amountText)
		}
		out = append(out, core.Withdrawal{
			ID:                 id,
			CustomerID:         customerID,
			Chain:              chain,
			Asset:              core.Asset(asset),
			Amount:             amount,
			DestinationAddress: destinationAddress,
			Status:             core.WithdrawalStatusBroadcast,
			CreatedAt:          createdAt,
			TxHash:             txHash,
			Nonce:              &nonce,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate broadcast withdrawals: %w", err)
	}
	return out, nil
}

// settleWithdrawal is the shared implementation behind SettleConfirmedWithdrawal
// (targetStatus=confirmed, counterpartAccountType=treasury, counterpartIsPlatform=true) and
// SettleFailedWithdrawal (targetStatus=failed, counterpartAccountType=available,
// counterpartIsPlatform=false) — both lock the withdrawal row FOR UPDATE, verify
// status=broadcast, lock the customer's hold account and the counterparty account (either
// the platform-wide treasury row for this (chain, asset), or the same customer's own
// available account) in one deterministic-order statement, write one balanced journal
// entry (debit hold, credit counterparty) plus its two postings, transition the
// withdrawal, and write the paired outbox event — all atomically, mirroring
// CreateWithdrawal's own lock-then-write shape exactly.
//
// Lock ordering (re-review 2026-07-21): `ORDER BY id FOR UPDATE` below is deterministic
// per call, but that alone does not make concurrent calls to this method deadlock-safe in
// general — it only avoids deadlocking against ANOTHER call whose own two target account
// ids happen to overlap in the opposite order, which two DIFFERENT customers' settlements
// never do (each locks one customer's own hold account plus either the single, global
// treasury row or that SAME customer's own available account — no two distinct
// settlements ever contend for the same pair of rows). The actual, load-bearing safety
// property is AD-11: exactly one broadcaster process per chain, so settleWithdrawal is
// never invoked concurrently with itself in the first place. Unlike CreateWithdrawal (whose
// two customers CAN be arbitrary, so its own `ORDER BY id` ordering is the thing doing the
// work), this comment exists so a future reader doesn't assume the same about this method.
func (r *WithdrawalRepository) settleWithdrawal(ctx context.Context, withdrawalID, targetStatus, causeType, eventType, counterpartAccountType string, counterpartIsPlatform bool) error {
	tx := txFromContext(ctx)

	var customerID, chain, asset, amountText, status string
	if err := tx.QueryRow(ctx,
		`SELECT customer_id, chain, asset, amount::text, status FROM withdrawals WHERE id = $1 FOR UPDATE`,
		withdrawalID,
	).Scan(&customerID, &chain, &asset, &amountText, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: withdrawal %s", core.ErrWithdrawalNotFound, withdrawalID)
		}
		return fmt.Errorf("lock withdrawal row: %w", err)
	}
	if status != core.WithdrawalStatusBroadcast {
		return fmt.Errorf("%w: withdrawal %s has status %q", core.ErrWithdrawalNotBroadcast, withdrawalID, status)
	}
	amount, ok := new(big.Int).SetString(amountText, 10)
	if !ok {
		return fmt.Errorf("parse withdrawal amount %q as integer", amountText)
	}

	// Lock the customer's hold account and the counterparty account in ONE
	// deterministic-order statement (mirrors CreateWithdrawal's own lock-ordering
	// discipline). The two settlement paths need different counterparty scoping — the
	// platform-wide treasury row (customer_id IS NULL) for confirmation, or the SAME
	// customer's own available row for failure — so each gets its own query shape rather
	// than one query trying to express both via a dynamic predicate.
	var rows pgx.Rows
	var queryErr error
	if counterpartIsPlatform {
		rows, queryErr = tx.Query(ctx,
			`SELECT id, account_type FROM accounts
			 WHERE chain = $1 AND asset = $2 AND (
			     (customer_id = $3 AND account_type = 'hold')
			     OR (customer_id IS NULL AND account_type = $4)
			 )
			 ORDER BY id
			 FOR UPDATE`,
			chain, asset, customerID, counterpartAccountType,
		)
	} else {
		rows, queryErr = tx.Query(ctx,
			`SELECT id, account_type FROM accounts
			 WHERE chain = $1 AND asset = $2 AND customer_id = $3 AND account_type IN ('hold', $4)
			 ORDER BY id
			 FOR UPDATE`,
			chain, asset, customerID, counterpartAccountType,
		)
	}
	if queryErr != nil {
		return fmt.Errorf("lock hold/counterparty accounts: %w", queryErr)
	}
	accountByType := make(map[string]string, 2)
	for rows.Next() {
		var accID, accType string
		if err := rows.Scan(&accID, &accType); err != nil {
			rows.Close()
			return fmt.Errorf("scan locked account: %w", err)
		}
		accountByType[accType] = accID
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate locked accounts: %w", err)
	}
	rows.Close()

	holdAccountID, ok := accountByType[string(core.AccountTypeHold)]
	if !ok {
		return fmt.Errorf("%w: customer %s, chain %q, asset %q", ErrNoHoldAccount, customerID, chain, asset)
	}
	counterpartAccountID, ok := accountByType[counterpartAccountType]
	if !ok {
		if counterpartIsPlatform {
			return fmt.Errorf("%w: chain %q, asset %q", ErrNoTreasuryAccount, chain, asset)
		}
		return fmt.Errorf("%w: customer %s, chain %q, asset %q", ErrNoAvailableAccount, customerID, chain, asset)
	}

	journalEntryID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate journal entry id: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx,
		`INSERT INTO journal_entries (id, cause_type, cause_id, created_at) VALUES ($1, $2, $3, $4)`,
		journalEntryID.String(), causeType, withdrawalID, now,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			// A settlement journal entry already exists for this withdrawal (cause_id =
			// withdrawalID) — should be unreachable given the status check above already
			// guards against re-settling an already-settled withdrawal, but the real,
			// database-enforced guarantee is UNIQUE(cause_type, cause_id) (AD-3); this is
			// defense in depth, not the primary guard.
			return fmt.Errorf("withdrawal %s already has a %s journal entry", withdrawalID, causeType)
		}
		return fmt.Errorf("insert %s journal entry: %w", causeType, err)
	}

	holdPostingID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate posting id: %w", err)
	}
	counterpartPostingID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate posting id: %w", err)
	}
	negAmount := new(big.Int).Neg(amount)

	batch := &pgx.Batch{}
	batch.Queue(
		`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
		holdPostingID.String(), journalEntryID.String(), holdAccountID, negAmount.String(), now,
	)
	batch.Queue(
		`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
		counterpartPostingID.String(), journalEntryID.String(), counterpartAccountID, amount.String(), now,
	)
	br := tx.SendBatch(ctx, batch)
	for range 2 {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("insert posting: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("close posting batch: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE withdrawals SET status = $1, updated_at = $2 WHERE id = $3 AND status = $4`,
		targetStatus, now, withdrawalID, core.WithdrawalStatusBroadcast,
	)
	if err != nil {
		return fmt.Errorf("update withdrawal to %s: %w", targetStatus, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("withdrawal %s status changed concurrently while settling", withdrawalID)
	}

	payload, err := json.Marshal(withdrawalSettledPayload{
		WithdrawalID:   withdrawalID,
		JournalEntryID: journalEntryID.String(),
		CustomerID:     customerID,
		Chain:          chain,
		Asset:          asset,
		Amount:         amountText,
	})
	if err != nil {
		return fmt.Errorf("marshal %s outbox payload: %w", eventType, err)
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate outbox event id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, event_type, payload, created_at) VALUES ($1, $2, $3, $4)`,
		eventID.String(), eventType, payload, now,
	); err != nil {
		return fmt.Errorf("insert %s outbox event: %w", eventType, err)
	}

	return nil
}

// SettleConfirmedWithdrawal implements core.WithdrawalRepository (Story 3.4): debit hold,
// credit treasury (see settleWithdrawal).
func (r *WithdrawalRepository) SettleConfirmedWithdrawal(ctx context.Context, withdrawalID string) error {
	return r.settleWithdrawal(ctx, withdrawalID, core.WithdrawalStatusConfirmed, withdrawalSettlementCauseType, withdrawalConfirmedEventType, string(core.AccountTypeTreasury), true)
}

// SettleFailedWithdrawal implements core.WithdrawalRepository (Story 3.4): debit hold,
// credit available (see settleWithdrawal).
func (r *WithdrawalRepository) SettleFailedWithdrawal(ctx context.Context, withdrawalID string) error {
	return r.settleWithdrawal(ctx, withdrawalID, core.WithdrawalStatusFailed, withdrawalFailureCauseType, withdrawalFailedEventType, string(core.AccountTypeAvailable), false)
}

var _ core.WithdrawalRepository = (*WithdrawalRepository)(nil)
