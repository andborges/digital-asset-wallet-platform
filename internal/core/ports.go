package core

import (
	"context"
	"errors"
)

// CustomerRepository persists a customer, its accounts, and its deposit address.
// Implementations must run their writes against whatever transaction is already open on
// ctx (established by the calling adapter's idempotency middleware) rather than opening
// their own — this is what makes "customer + accounts + deposit address, one
// transaction" (AD-4) true. depositAddress is customer.DepositAddress, already computed
// by the time this is called — the repository persists it, it does not derive it.
type CustomerRepository interface {
	CreateCustomer(ctx context.Context, customer Customer, accounts []Account, depositAddress string) error
}

// CustomerReader reads a customer's own attributes (id, creation time, deposit address),
// unlike CustomerRepository which writes them. Like BalanceRepository/TransactionRepository,
// implementations query independently of any transaction on ctx — GET /customers/{id} is
// non-mutating, and IdempotencyMiddleware never opens a transaction for it.
type CustomerReader interface {
	// GetCustomer returns customerID's own record. Returns ErrCustomerNotFound if no such
	// customer exists.
	GetCustomer(ctx context.Context, customerID string) (Customer, error)
}

// DepositAddressDeriver computes a customer's CREATE2 deposit address from its salt
// (AD-8). This is a port, not inline math in core, deliberately: the CREATE2 formula
// (keccak256(0xff ++ factory ++ salt ++ initCodeHash)[12:]) is a correctness-critical
// primitive where a single byte-order or padding bug would permanently corrupt every
// customer address ever issued. Rather than reimplementing that formula by hand in core
// with a second crypto library, this port is implemented in internal/adapter/evm using
// go-ethereum's own crypto.CreateAddress2 — the same battle-tested helper the wider
// Ethereum ecosystem relies on for this exact formula. This also keeps go-ethereum
// imports and chain-ID references confined to internal/adapter/evm (AD-1) while keeping
// core free of any adapter import (AD-1/AD-2) — the same shape as every other repository
// port in this codebase, not a special case.
type DepositAddressDeriver interface {
	DeriveAddress(salt [32]byte) (string, error)
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

// TransferRepository persists a ledger-only internal transfer (FR4) as one balanced
// journal entry (debit source, credit destination) plus its postings (AD-3, AD-4).
// Like CustomerRepository, implementations must run their writes against whatever
// transaction is already open on ctx (established by IdempotencyMiddleware for this
// mutating POST) rather than opening their own.
type TransferRepository interface {
	// CreateTransfer locks both accounts in a single deterministic-order statement
	// (preventing lock-ordering deadlocks between opposite-direction concurrent
	// transfers), verifies the source account's derived balance covers req.Amount, and
	// writes the journal entry plus two postings atomically. Returns
	// ErrCustomerNotFound if either customer has no account for (req.Chain, req.Asset),
	// ErrInsufficientBalance if the source's derived balance is less than req.Amount,
	// or ErrDuplicateTransferCause on a duplicate cause_id (a narrow concurrent-request
	// race — see internal/adapter/postgres/transfer_repo.go).
	CreateTransfer(ctx context.Context, req TransferRequest) (Transfer, error)
}

// TransactionRepository reads a customer's transaction history generically from the
// cause-tagged journal (journal_entries joined to postings, restricted to this
// customer's own accounts) — never filtered or switched on cause_type, so future cause
// types appear automatically with no repository changes (FR3). Like BalanceRepository,
// implementations query independently of any transaction on ctx: this is a plain read
// with no state change to commit, and the idempotency middleware never opens a
// transaction for the non-mutating GET route this port serves.
type TransactionRepository interface {
	// ListCustomerTransactions returns one page of customerID's transaction history,
	// newest first. cursor == "" means "first page." Returns ErrCustomerNotFound if no
	// such customer exists, or ErrInvalidCursor if cursor is non-empty but doesn't
	// decode to a valid page marker.
	ListCustomerTransactions(ctx context.Context, customerID string, pageSize int, cursor string) (TransactionPage, error)
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
