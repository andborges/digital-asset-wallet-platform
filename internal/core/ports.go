package core

import (
	"context"
	"errors"
)

// CustomerRepository persists a customer and its accounts. Implementations must run
// their writes against whatever transaction is already open on ctx (established by
// the calling adapter's idempotency middleware) rather than opening their own —
// this is what makes "customer + accounts, one transaction" (AD-4) true.
type CustomerRepository interface {
	CreateCustomer(ctx context.Context, customer Customer, accounts []Account) error
}

// BalanceRepository reads a customer's per-(chain, asset) balances, derived by summing
// postings (AD-3). Unlike CustomerRepository, implementations query independently of
// any transaction on ctx — this is a plain read with no state change to commit, and the
// idempotency middleware never opens a transaction for the non-mutating GET route this
// port serves.
type BalanceRepository interface {
	// CustomerBalances returns one AccountBalance per (chain, asset) pair provisioned
	// for customerID. Returns ErrCustomerNotFound if no such customer exists.
	CustomerBalances(ctx context.Context, customerID string) ([]AccountBalance, error)
}

// Tx, TxBeginner, and IdempotencyStore below are cross-cutting architectural ports
// (AD-4's one-transaction-per-state-change rule, AD-5's idempotency-by-constraint rule)
// rather than ledger domain concepts. They live in core, not in internal/adapter/api,
// because AD-1/AD-2 forbid adapters from importing each other: the api adapter's
// idempotency middleware and the postgres adapter's implementations both need to share
// these types, and core is the only package both are allowed to import. This is a
// deliberate, narrow exception — it does not open the door to putting HTTP or SQL
// specifics here, only the shared *shape* of "a transaction" and "an idempotency store."

// Tx is an in-flight transaction handle, opaque to its callers.
type Tx interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// TxBeginner starts a transaction and returns a context carrying it. Repository
// implementations (e.g. the postgres CustomerRepository) extract the concrete
// transaction from that context themselves — callers never see the driver type.
type TxBeginner interface {
	Begin(ctx context.Context) (context.Context, Tx, error)
}

// ErrKeyConflict is returned by IdempotencyStore.Insert when key was already inserted
// by a concurrent request that committed first — the loser rolls back and re-Lookups.
var ErrKeyConflict = errors.New("idempotency key already inserted by a concurrent request")

// StoredResponse is a previously captured, byte-exact HTTP response.
type StoredResponse struct {
	Status      int
	Body        []byte
	ContentType string
}

// StoredEntry is a previously recorded idempotency-key row.
type StoredEntry struct {
	RequestHash []byte
	Response    StoredResponse
}

// IdempotencyStore records and looks up idempotency keys. Implementations must enforce
// uniqueness on key via a database constraint (AD-5): Insert is expected to return
// ErrKeyConflict on a concurrent duplicate, never preceded by an application-level check.
type IdempotencyStore interface {
	// Lookup returns the stored entry for key, if any exists. Called before any
	// transaction is opened — a plain read, not part of the eventual write transaction.
	Lookup(ctx context.Context, key string) (StoredEntry, bool, error)
	// Insert records key's requestHash and resp inside ctx's open transaction, as part
	// of the same commit as the handler's own writes. Returns ErrKeyConflict if key was
	// already inserted by a concurrent request that won the race.
	Insert(ctx context.Context, key string, requestHash []byte, resp StoredResponse) error
}
