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

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// depositPendingEventType is the outbox event_type written in the same transaction as a
// deposit's observed transition (AD-4). The outbox table itself is generic (event_type +
// jsonb payload, AD-13) — Story 2.2's credit event and Epic 4's dispatcher reuse it
// without a schema change.
const depositPendingEventType = "deposit.pending"

// depositCreditCauseType is the journal_entries.cause_type CreditFinalizedDeposits
// writes. cause_id is the deposit's own id (Design Notes: "cause_id = deposit.id" is a
// deposit's natural dedup key — globally unique, one-to-one with exactly one credit
// event ever, backed by the same UNIQUE(cause_type, cause_id) constraint Story 1.3's
// transfers rely on).
const depositCreditCauseType = "deposit_credit"

// depositCreditedEventType is the outbox event_type written in the same transaction as a
// deposit's credit transition (AD-4).
const depositCreditedEventType = "deposit.credited"

// depositOrphanedEventType is the outbox event_type written in the same transaction as a
// deposit's orphan transition (Story 2.4, AD-4) — the same generic outbox table
// (event_type + jsonb payload) as deposit.pending/deposit.credited.
const depositOrphanedEventType = "deposit.orphaned"

// depositOrphanedPayload is the jsonb payload recorded alongside a newly orphaned
// deposit's outbox event.
type depositOrphanedPayload struct {
	DepositID string `json:"depositId"`
}

// depositCreditedPayload is the jsonb payload recorded alongside a newly credited
// deposit's outbox event.
type depositCreditedPayload struct {
	DepositID      string `json:"depositId"`
	JournalEntryID string `json:"journalEntryId"`
	Chain          string `json:"chain"`
	Asset          string `json:"asset"`
	Amount         string `json:"amount"`
	CustomerID     string `json:"customerId"`
}

// depositPendingPayload is the jsonb payload recorded alongside a newly observed
// deposit's outbox event.
type depositPendingPayload struct {
	DepositID string `json:"depositId"`
	Chain     string `json:"chain"`
	Asset     string `json:"asset"`
	Address   string `json:"address"`
	TxHash    string `json:"txHash"`
	LogIndex  int    `json:"logIndex"`
	Amount    string `json:"amount"`
}

// DepositRepository implements core.DepositRepository against PostgreSQL. Like
// CustomerRepository, it carries no pool of its own — every call runs against the
// transaction already open on ctx (AD-4), obtained via TxBeginner.Begin by
// TrackDeposits.Execute. It is the watcher's sole write path for deposits, cursors, and
// their paired outbox events.
type DepositRepository struct{}

// NewDepositRepository constructs a core.DepositRepository.
func NewDepositRepository() *DepositRepository {
	return &DepositRepository{}
}

// RecordObserved inserts deposit in the observed state and, only when a row was actually
// inserted, a paired "deposit.pending" outbox event — both in the transaction on ctx
// (AD-4). Re-observing the same (chain, tx_hash, log_index) on a repoll relies entirely
// on the UNIQUE constraint (INSERT ... ON CONFLICT DO NOTHING, AD-5) — never an
// application-level existence check — so a conflict is reported as inserted=false, not
// an error.
func (r *DepositRepository) RecordObserved(ctx context.Context, deposit core.Deposit) (bool, error) {
	tx := txFromContext(ctx)

	// ON CONFLICT targets the partial unique index added by migration 0008
	// (idx_deposits_active_chain_tx_hash_log_index), so its WHERE clause must match that
	// index's predicate exactly (Postgres requires this) — confirmed empirically against
	// a real Postgres instance. Scoping to non-orphaned rows (Story 2.4, AD-5) is what
	// lets a re-broadcast of the same transaction after a reorg insert a brand-new row
	// once the prior one is orphaned, instead of being silently swallowed as "already
	// recorded."
	tag, err := tx.Exec(ctx,
		`INSERT INTO deposits (id, chain, asset, address, tx_hash, log_index, amount, block_number, block_hash, state, observed_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10, $11, $12)
		 ON CONFLICT (chain, tx_hash, log_index) WHERE state != 'orphaned' DO NOTHING`,
		deposit.ID, string(deposit.Chain), string(deposit.Asset), deposit.Address, deposit.TxHash, deposit.LogIndex,
		deposit.Amount.String(), deposit.BlockNumber, deposit.BlockHash, string(deposit.State), deposit.ObservedAt, deposit.UpdatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("insert deposit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// ON CONFLICT DO NOTHING fired — this exact (chain, tx_hash, log_index) was
		// already recorded by an earlier poll. A no-op by construction (AD-5): the
		// existing row is left untouched and no outbox event is written for it again.
		return false, nil
	}

	payload, err := json.Marshal(depositPendingPayload{
		DepositID: deposit.ID,
		Chain:     string(deposit.Chain),
		Asset:     string(deposit.Asset),
		Address:   deposit.Address,
		TxHash:    deposit.TxHash,
		LogIndex:  deposit.LogIndex,
		Amount:    deposit.Amount.String(),
	})
	if err != nil {
		return false, fmt.Errorf("marshal deposit.pending outbox payload: %w", err)
	}

	eventID, err := uuid.NewV7()
	if err != nil {
		return false, fmt.Errorf("generate outbox event id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, event_type, payload, created_at) VALUES ($1, $2, $3, $4)`,
		eventID.String(), depositPendingEventType, payload, deposit.ObservedAt,
	); err != nil {
		return false, fmt.Errorf("insert deposit.pending outbox event: %w", err)
	}

	return true, nil
}

// PromoteToSafe transitions every observed deposit on chain whose block_number is at or
// below safeBlock to the safe state, in one bulk statement, and returns the number of
// rows transitioned.
func (r *DepositRepository) PromoteToSafe(ctx context.Context, chain core.Chain, safeBlock uint64) (int, error) {
	tx := txFromContext(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE deposits SET state = $1, updated_at = now()
		 WHERE state = $2 AND chain = $3 AND block_number <= $4`,
		string(core.DepositSafe), string(core.DepositObserved), string(chain), safeBlock,
	)
	if err != nil {
		return 0, fmt.Errorf("promote deposits to safe: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PromoteToFinalized transitions every safe deposit on chain whose block_number is at or
// below finalizedBlock to the finalized state, in one bulk statement — mirrors
// PromoteToSafe exactly, one tier up (Story 2.2) — and returns the number of rows
// transitioned.
func (r *DepositRepository) PromoteToFinalized(ctx context.Context, chain core.Chain, finalizedBlock uint64) (int, error) {
	tx := txFromContext(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE deposits SET state = $1, updated_at = now()
		 WHERE state = $2 AND chain = $3 AND block_number <= $4`,
		string(core.DepositFinalized), string(core.DepositSafe), string(chain), finalizedBlock,
	)
	if err != nil {
		return 0, fmt.Errorf("promote deposits to finalized: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// maxPendingDepositsPerReorgCheck caps how many observed/safe deposits a single poll's
// reorg-check phase re-verifies (re-review 2026-07-17) — mirrors CreditFinalizedDeposits'
// maxCreditsPerPoll guard against the identical class of risk: an unbounded backlog
// (first deploy of this story against months of history, or extended watcher downtime)
// would otherwise mean an unbounded number of BlockHash RPC calls in one cycle. A backlog
// larger than this is simply finished across subsequent polls.
const maxPendingDepositsPerReorgCheck = 500

// ListPendingDeposits returns up to maxPendingDepositsPerReorgCheck observed/safe deposits
// on chain (Story 2.4), oldest block first — exactly the states TrackDeposits.Execute's
// reorg-check phase must re-verify each poll. finalized/credited deposits are never
// candidates for orphaning, so this query never selects them (the same "true by
// construction" pattern as CreditFinalizedDeposits' WHERE state='finalized').
func (r *DepositRepository) ListPendingDeposits(ctx context.Context, chain core.Chain) ([]core.Deposit, error) {
	tx := txFromContext(ctx)

	// COALESCE(block_hash, '') keeps core.Deposit.BlockHash a plain string rather than a
	// pointer (re-review 2026-07-17): a legacy row from before this story's migration has
	// no historical hash to compare (block_hash is nullable precisely because it couldn't
	// be backfilled), and checkForReorgs treats an empty string as "nothing to check,"
	// never as a real hash value — '' can never collide with a genuine 66-character hash.
	rows, err := tx.Query(ctx,
		`SELECT id, chain, asset, address, tx_hash, log_index, amount::text, block_number, COALESCE(block_hash, ''), state, observed_at, updated_at
		 FROM deposits
		 WHERE chain = $1 AND state IN ($2, $3)
		 ORDER BY block_number
		 LIMIT $4`,
		string(chain), string(core.DepositObserved), string(core.DepositSafe), maxPendingDepositsPerReorgCheck,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending deposits: %w", err)
	}
	defer rows.Close()

	var deposits []core.Deposit
	for rows.Next() {
		var (
			d          core.Deposit
			chainStr   string
			asset      string
			state      string
			amountText string
		)
		if err := rows.Scan(&d.ID, &chainStr, &asset, &d.Address, &d.TxHash, &d.LogIndex, &amountText, &d.BlockNumber, &d.BlockHash, &state, &d.ObservedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan pending deposit row: %w", err)
		}
		amount, ok := new(big.Int).SetString(amountText, 10)
		if !ok {
			return nil, fmt.Errorf("parse deposit amount %q as integer", amountText)
		}
		d.Chain = core.Chain(chainStr)
		d.Asset = core.Asset(asset)
		d.State = core.DepositState(state)
		d.Amount = amount
		deposits = append(deposits, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending deposit rows: %w", err)
	}

	return deposits, nil
}

// OrphanDeposit transitions depositID to the orphaned state and writes a paired
// "deposit.orphaned" outbox event, both in the transaction already open on ctx (AD-4) —
// mirroring RecordObserved's paired-write pattern exactly (Story 2.4).
func (r *DepositRepository) OrphanDeposit(ctx context.Context, depositID string) error {
	tx := txFromContext(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE deposits SET state = $1, updated_at = now() WHERE id = $2`,
		string(core.DepositOrphaned), depositID,
	)
	if err != nil {
		return fmt.Errorf("orphan deposit %s: %w", depositID, err)
	}
	if tag.RowsAffected() == 0 {
		// A non-matching depositID must fail loud, not write a paired outbox event for a
		// transition that never happened (re-review 2026-07-17) — this exact gap let the
		// production code diverge from its own test fake, which already returned an error
		// here, so no unit test could have caught it.
		return fmt.Errorf("orphan deposit %s: no such deposit", depositID)
	}

	payload, err := json.Marshal(depositOrphanedPayload{DepositID: depositID})
	if err != nil {
		return fmt.Errorf("marshal deposit.orphaned outbox payload: %w", err)
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate outbox event id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (id, event_type, payload, created_at) VALUES ($1, $2, $3, $4)`,
		eventID.String(), depositOrphanedEventType, payload, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("insert deposit.orphaned outbox event for deposit %s: %w", depositID, err)
	}

	return nil
}

// maxCreditsPerPoll caps how many deposits a single CreditFinalizedDeposits call
// processes (re-review 2026-07-17) — mirrors track_deposits.go's maxBlocksPerScan: an
// unbounded backlog (extended downtime, a burst of deposits) would otherwise be credited
// entirely within one long-held transaction instead of incrementally across polls. A
// backlog larger than this is simply finished on the next poll — every write here already
// commits per poll, so this is purely a per-cycle batch size, not a correctness boundary.
const maxCreditsPerPoll = 500

// eligibleCreditDeposit is one row of CreditFinalizedDeposits' policy-joined selection —
// a finalized deposit whose (chain, asset) crediting policy is 'finalized', together with
// the two account ids its credit's postings must touch.
type eligibleCreditDeposit struct {
	depositID       string
	chain           string
	asset           string
	amountText      string
	customerID      string
	customerAccount string
	floatAccount    string
}

// CreditFinalizedDeposits credits every finalized deposit on chain whose (chain, asset)
// crediting policy is 'finalized' (Story 2.2, FR9): for each eligible row it writes one
// balanced journal entry (cause_type='deposit_credit', cause_id=deposit.id) debiting the
// chain/asset forwarder-float platform account (customer_id IS NULL) and crediting the
// customer's own account, transitions the deposit to credited, and writes a paired
// "deposit.credited" outbox event — all inside the transaction already open on ctx
// (AD-4). The query is scoped to state='finalized', so a deposit already credited is
// never re-selected on a later poll (no runtime check needed — true by construction).
// Returns the number of deposits credited.
func (r *DepositRepository) CreditFinalizedDeposits(ctx context.Context, chain core.Chain) (int, error) {
	tx := txFromContext(ctx)

	rows, err := tx.Query(ctx,
		`SELECT d.id, d.chain, d.asset, d.amount::text, da.customer_id, cust.id, float.id
		 FROM deposits d
		 JOIN deposit_addresses da ON da.address = d.address
		 JOIN crediting_policy cp ON cp.chain = d.chain AND cp.asset = d.asset
		 JOIN accounts cust ON cust.customer_id = da.customer_id AND cust.chain = d.chain AND cust.asset = d.asset AND cust.account_type = 'available'
		 JOIN accounts float ON float.customer_id IS NULL AND float.chain = d.chain AND float.asset = d.asset AND float.account_type = 'available'
		 WHERE d.chain = $1 AND d.state = $2 AND cp.credit_tier = $3
		 ORDER BY d.block_number
		 LIMIT $4`,
		// $2 and $3 are two independently-declared vocabularies (core.DepositState vs.
		// crediting_policy's CHECK) that happen to share the literal "finalized" — bound
		// separately (re-review 2026-07-17) so a future rename of either doesn't silently
		// break this join via a single reused placeholder.
		string(chain), string(core.DepositFinalized), string(core.DepositFinalized), maxCreditsPerPoll,
	)
	if err != nil {
		return 0, fmt.Errorf("query finalized deposits eligible for crediting: %w", err)
	}

	// Collect every eligible row before issuing any further statement on tx: pgx does not
	// allow a new query/exec on the same transaction while a Rows cursor from an earlier
	// query is still open (the same reason CreateTransfer's account-locking SELECT is
	// fully drained before its own subsequent statements).
	var eligible []eligibleCreditDeposit
	for rows.Next() {
		var d eligibleCreditDeposit
		if err := rows.Scan(&d.depositID, &d.chain, &d.asset, &d.amountText, &d.customerID, &d.customerAccount, &d.floatAccount); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan eligible deposit row: %w", err)
		}
		eligible = append(eligible, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate eligible deposit rows: %w", err)
	}
	rows.Close()

	now := time.Now().UTC()
	for _, d := range eligible {
		amount, ok := new(big.Int).SetString(d.amountText, 10)
		if !ok {
			return 0, fmt.Errorf("parse deposit %s amount %q as integer", d.depositID, d.amountText)
		}
		// Defense in depth alongside the new deposits_amount_positive DB CHECK (re-review
		// 2026-07-17): a zero or negative amount here would silently write a no-op-but-
		// "credited" deposit, or reverse the debit/credit direction — fail loud instead,
		// mirroring CreateTransfer's ErrNonPositiveAmount validation.
		if amount.Sign() <= 0 {
			return 0, fmt.Errorf("deposit %s has a non-positive amount %q — refusing to credit", d.depositID, d.amountText)
		}

		journalEntryID, err := uuid.NewV7()
		if err != nil {
			return 0, fmt.Errorf("generate journal entry id: %w", err)
		}
		// A unique-violation on (cause_type, cause_id) here is left to fail loudly, not
		// translated to a special sentinel error: unlike transfers, there is no legitimate
		// client-retry scenario for a deposit credit — a real hit would mean a genuine
		// double-credit bug (Design Notes).
		if _, err := tx.Exec(ctx,
			`INSERT INTO journal_entries (id, cause_type, cause_id, created_at) VALUES ($1, $2, $3, $4)`,
			journalEntryID.String(), depositCreditCauseType, d.depositID, now,
		); err != nil {
			return 0, fmt.Errorf("insert deposit_credit journal entry for deposit %s: %w", d.depositID, err)
		}

		floatPostingID, err := uuid.NewV7()
		if err != nil {
			return 0, fmt.Errorf("generate posting id: %w", err)
		}
		custPostingID, err := uuid.NewV7()
		if err != nil {
			return 0, fmt.Errorf("generate posting id: %w", err)
		}
		negAmount := new(big.Int).Neg(amount)

		batch := &pgx.Batch{}
		batch.Queue(
			`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
			floatPostingID.String(), journalEntryID.String(), d.floatAccount, negAmount.String(), now,
		)
		batch.Queue(
			`INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at) VALUES ($1, $2, $3, $4::numeric, $5)`,
			custPostingID.String(), journalEntryID.String(), d.customerAccount, amount.String(), now,
		)
		br := tx.SendBatch(ctx, batch)
		for range 2 {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return 0, fmt.Errorf("insert posting for deposit %s: %w", d.depositID, err)
			}
		}
		if err := br.Close(); err != nil {
			return 0, fmt.Errorf("close posting batch for deposit %s: %w", d.depositID, err)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE deposits SET state = $1, updated_at = now() WHERE id = $2`,
			string(core.DepositCredited), d.depositID,
		); err != nil {
			return 0, fmt.Errorf("transition deposit %s to credited: %w", d.depositID, err)
		}

		payload, err := json.Marshal(depositCreditedPayload{
			DepositID:      d.depositID,
			JournalEntryID: journalEntryID.String(),
			Chain:          d.chain,
			Asset:          d.asset,
			Amount:         amount.String(),
			CustomerID:     d.customerID,
		})
		if err != nil {
			return 0, fmt.Errorf("marshal deposit.credited outbox payload: %w", err)
		}
		eventID, err := uuid.NewV7()
		if err != nil {
			return 0, fmt.Errorf("generate outbox event id: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbox_events (id, event_type, payload, created_at) VALUES ($1, $2, $3, $4)`,
			eventID.String(), depositCreditedEventType, payload, now,
		); err != nil {
			return 0, fmt.Errorf("insert deposit.credited outbox event for deposit %s: %w", d.depositID, err)
		}
	}

	return len(eligible), nil
}

// Cursor returns the last block persisted for (chain, tier), or 0 if no cursor has ever
// been set for that pair — the natural "scan from just after the very beginning" starting
// point for a chain the watcher has never polled before.
func (r *DepositRepository) Cursor(ctx context.Context, chain core.Chain, tier string) (uint64, error) {
	tx := txFromContext(ctx)

	var lastBlock uint64
	err := tx.QueryRow(ctx,
		`SELECT last_block FROM watcher_cursors WHERE chain = $1 AND tier = $2`,
		string(chain), tier,
	).Scan(&lastBlock)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read cursor: %w", err)
	}
	return lastBlock, nil
}

// SetCursor persists the last block processed for (chain, tier), upserting so the first
// poll for a (chain, tier) pair and every subsequent one use the same statement.
func (r *DepositRepository) SetCursor(ctx context.Context, chain core.Chain, tier string, block uint64) error {
	tx := txFromContext(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO watcher_cursors (chain, tier, last_block, updated_at) VALUES ($1, $2, $3, now())
		 ON CONFLICT (chain, tier) DO UPDATE SET last_block = EXCLUDED.last_block, updated_at = now()`,
		string(chain), tier, block,
	); err != nil {
		return fmt.Errorf("set cursor: %w", err)
	}
	return nil
}

var _ core.DepositRepository = (*DepositRepository)(nil)
