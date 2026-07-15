package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// TransactionRepository implements core.TransactionRepository against PostgreSQL. Like
// BalanceRepository, it holds its own *pgxpool.Pool and queries it directly rather than
// via txFromContext: the GET route this repository serves is non-mutating, and
// IdempotencyMiddleware never opens a transaction for non-mutating methods, so
// txFromContext would panic here. It also holds cursorKey, the HMAC secret that
// authenticates page cursors (see encodeCursor/decodeCursor).
type TransactionRepository struct {
	pool      *pgxpool.Pool
	cursorKey []byte
}

// NewTransactionRepository constructs a core.TransactionRepository backed by pool.
// cursorKey is the secret used to sign page cursors; it must be non-empty and stable
// across the process's lifetime (a restart with a new key invalidates outstanding
// cursors, which for pagination degrades gracefully to "first page again").
func NewTransactionRepository(pool *pgxpool.Pool, cursorKey []byte) *TransactionRepository {
	return &TransactionRepository{pool: pool, cursorKey: cursorKey}
}

// cursorFieldSeparator joins the fields inside a cursor's payload. None of the fields can
// contain it: customer/journal-entry/posting ids are UUIDs and created_at is RFC 3339
// nanosecond text.
const cursorFieldSeparator = "|"

// cursorPartSeparator splits a cursor's base64 payload from its base64 HMAC tag.
const cursorPartSeparator = "."

// encodeCursor builds a signed, customer-bound opaque page cursor from the last row of the
// current page — the row a subsequent request must resume strictly after, in
// (createdAt, journalEntryID, postingID) order. The payload embeds the owning customerID
// and is authenticated with an HMAC-SHA256 tag, so decodeCursor can reject a cursor that
// was tampered with or minted for a different customer (AC7) instead of silently honoring
// it as a page origin.
func (r *TransactionRepository) encodeCursor(customerID string, createdAt time.Time, journalEntryID, postingID string) string {
	payload := strings.Join(
		[]string{customerID, createdAt.Format(time.RFC3339Nano), journalEntryID, postingID},
		cursorFieldSeparator,
	)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) +
		cursorPartSeparator +
		base64.RawURLEncoding.EncodeToString(r.cursorMAC([]byte(payload)))
}

// cursorMAC computes the HMAC-SHA256 tag over a cursor payload using the repository's key.
func (r *TransactionRepository) cursorMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, r.cursorKey)
	mac.Write(payload)
	return mac.Sum(nil)
}

// decodeCursor reverses encodeCursor and authenticates the result. It returns
// core.ErrInvalidCursor for anything that is not a cursor this endpoint minted for this
// exact customer: bad base64, wrong shape, a failed HMAC (tampered), a customerID that
// does not match the caller (a cursor "from a different customer"), an unparseable
// timestamp, or a non-UUID id. A cursor is only ever produced by this endpoint, so
// anything that fails these checks is treated as attacker/bug input — never as "first
// page." The HMAC check is what makes "tampered" detectable at all: without it a client
// could forge any well-formed (timestamp, uuid) pair and shift its own page window; the
// customerID check is what makes "from a different customer" detectable, since the id
// itself is not otherwise re-derivable from the cursor.
func (r *TransactionRepository) decodeCursor(customerID, cursor string) (createdAt time.Time, journalEntryID, postingID string, err error) {
	fail := func(reason string) (time.Time, string, string, error) {
		return time.Time{}, "", "", fmt.Errorf("%w: %s", core.ErrInvalidCursor, reason)
	}

	encodedPayload, encodedMAC, ok := strings.Cut(cursor, cursorPartSeparator)
	if !ok {
		return fail("wrong shape")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return fail("payload is not valid base64")
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return fail("signature is not valid base64")
	}
	// hmac.Equal is constant-time and safe on unequal-length inputs.
	if !hmac.Equal(gotMAC, r.cursorMAC(payload)) {
		return fail("signature mismatch")
	}

	fields := strings.Split(string(payload), cursorFieldSeparator)
	if len(fields) != 4 {
		return fail("wrong field count")
	}
	if fields[0] != customerID {
		return fail("cursor belongs to a different customer")
	}
	createdAt, err = time.Parse(time.RFC3339Nano, fields[1])
	if err != nil {
		return fail("unparseable timestamp")
	}
	if _, err := uuid.Parse(fields[2]); err != nil {
		return fail("journal entry id is not a valid uuid")
	}
	if _, err := uuid.Parse(fields[3]); err != nil {
		return fail("posting id is not a valid uuid")
	}
	return createdAt, fields[2], fields[3], nil
}

// pagedTransaction pairs a domain Transaction with the posting id that produced it. The
// posting id is a persistence detail (deliberately not surfaced on core.Transaction) and
// exists here only to build the keyset cursor: it is the finest component of the sort key
// because (created_at, journal_entry_id) alone is NOT unique per result row. The query
// emits one row per (journal_entry, posting), and a single journal entry can produce more
// than one posting on this customer's own accounts. No such cause type exists today
// (internal_transfer writes exactly one posting per customer), but the query is generic by
// construction (AC4), so a future multi-posting cause type would yield two rows sharing an
// identical (created_at, je.id); the row-wise cursor comparison would then drop or repeat
// one of them across a page boundary. The posting id, unique per row, closes that gap.
type pagedTransaction struct {
	txn       core.Transaction
	postingID string
}

// ListCustomerTransactions confirms customerID exists, then reads its transaction
// history generically from the cause-tagged journal — journal_entries joined to
// postings, restricted to accounts owned by this customer — with no cause_type filter
// anywhere, so future cause types appear automatically (FR3, AC4). Pagination is keyset
// on (created_at, journal_entry_id, posting_id) DESC, encoded in a signed opaque cursor
// (see encodeCursor/decodeCursor).
func (r *TransactionRepository) ListCustomerTransactions(ctx context.Context, customerID string, pageSize int, cursor string) (core.TransactionPage, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)`,
		customerID,
	).Scan(&exists); err != nil {
		return core.TransactionPage{}, fmt.Errorf("check customer exists: %w", err)
	}
	if !exists {
		return core.TransactionPage{}, core.ErrCustomerNotFound
	}

	// The cursor bind slots must always carry syntactically valid values of the type
	// Postgres infers for them (uuid, from je.id/p.id), even when hasCursor is false:
	// Postgres validates a parameter's text form against its inferred type at bind time,
	// before execution — so an empty placeholder would fail with "invalid input syntax for
	// type uuid" even though the "(je.created_at, je.id, p.id) < (...)" branch is never
	// logically reached once "$2::boolean IS FALSE" short-circuits the OR.
	hasCursor := false
	cursorCreatedAt := time.Time{}
	cursorJournalEntryID := uuid.Nil.String()
	cursorPostingID := uuid.Nil.String()
	if cursor != "" {
		hasCursor = true
		var err error
		cursorCreatedAt, cursorJournalEntryID, cursorPostingID, err = r.decodeCursor(customerID, cursor)
		if err != nil {
			return core.TransactionPage{}, err
		}
	}

	rows, err := r.pool.Query(ctx,
		`SELECT je.id, je.cause_type, je.created_at, p.id, p.amount::text, a.chain, a.asset
		 FROM journal_entries je
		 JOIN postings p ON p.journal_entry_id = je.id
		 JOIN accounts a ON a.id = p.account_id
		 WHERE a.customer_id = $1
		   AND ($2::boolean IS FALSE OR (je.created_at, je.id, p.id) < ($3, $4, $5))
		 ORDER BY je.created_at DESC, je.id DESC, p.id DESC
		 LIMIT $6`,
		customerID, hasCursor, cursorCreatedAt, cursorJournalEntryID, cursorPostingID, pageSize+1,
	)
	if err != nil {
		return core.TransactionPage{}, fmt.Errorf("query transactions: %w", err)
	}
	defer rows.Close()

	var paged []pagedTransaction
	for rows.Next() {
		var journalEntryID, causeType, postingID, amountText, chain, asset string
		var createdAt time.Time
		if err := rows.Scan(&journalEntryID, &causeType, &createdAt, &postingID, &amountText, &chain, &asset); err != nil {
			return core.TransactionPage{}, fmt.Errorf("scan transaction row: %w", err)
		}

		amount, ok := new(big.Int).SetString(amountText, 10)
		if !ok {
			return core.TransactionPage{}, fmt.Errorf("parse transaction amount %q as integer", amountText)
		}

		paged = append(paged, pagedTransaction{
			txn: core.Transaction{
				ID:        journalEntryID,
				Type:      causeType,
				Amount:    amount,
				Chain:     core.Chain(chain),
				Asset:     core.Asset(asset),
				Status:    "completed",
				CreatedAt: createdAt,
			},
			postingID: postingID,
		})
	}
	if err := rows.Err(); err != nil {
		return core.TransactionPage{}, fmt.Errorf("iterate transaction rows: %w", err)
	}

	// Over-fetching pageSize+1 rows lets us detect a next page without a second round-trip:
	// if the extra row is present, build the cursor from the last row of the page proper
	// (the pageSize-th, 0-indexed pageSize-1) and drop the overflow.
	var nextCursor string
	if len(paged) > pageSize {
		last := paged[pageSize-1]
		nextCursor = r.encodeCursor(customerID, last.txn.CreatedAt, last.txn.ID, last.postingID)
		paged = paged[:pageSize]
	}

	transactions := make([]core.Transaction, len(paged))
	for i := range paged {
		transactions[i] = paged[i].txn
	}
	return core.TransactionPage{Transactions: transactions, NextCursor: nextCursor}, nil
}
