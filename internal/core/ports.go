package core

import (
	"context"
	"errors"
	"math/big"
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

// ChainScanner reads one configured chain's head/safe tags and scans its blocks for
// ETH/ERC-20 transfers landing on known deposit addresses. Implemented in
// internal/adapter/evm — all go-ethereum imports, RPC calls, and chain-ID references are
// confined there (AD-1). One ChainScanner is bound to a single chain (one OS process per
// chain, AD-2), which is why neither method takes a chain parameter.
type ChainScanner interface {
	// Head returns the chain's current head block, its current "safe" block (via
	// eth_getBlockByNumber("safe", false)), and its current "finalized" block (via
	// eth_getBlockByNumber("finalized", false), Story 2.2). Returns an error if the RPC
	// endpoint does not support either tag — never a silent "head minus N blocks"
	// approximation.
	Head(ctx context.Context) (latest, safe, finalized uint64, err error)
	// BlockHash returns the chain's CURRENT block hash at blockNumber (Story 2.4) — the
	// value TrackDeposits.Execute's reorg-check phase compares against each pending
	// deposit's stored block_hash. exists is false, with no error, when the chain no
	// longer has a block at that height at all (the "chain got shorter than the
	// deposit's height" case, Design Notes' I/O matrix) — this is a normal, expected
	// outcome of a reorg, never a failed poll.
	BlockHash(ctx context.Context, blockNumber uint64) (hash string, exists bool, err error)
	// ScanDeposits scans the inclusive block range [fromBlock, toBlock] for ETH/ERC-20
	// transfers landing on any address in knownAddresses. Native ETH transfers are found
	// by scanning each block's transactions for tx.To() in knownAddresses (no log exists
	// for a plain value transfer); ERC-20 transfers are found via a single, unfiltered-by-
	// contract eth_getLogs Transfer topic filter (Story 2.3: filtering by contract address
	// up front would filter unsupported transfers out before classification ever ran) and
	// classified per log against tokenRegistry (keyed by lowercase contract address, Story
	// 2.3's FR34 — a registry hit is an ordinary ObservedTransfer, a miss is an
	// UnsupportedTokenObservation). The zero-amount guard applies to both branches.
	ScanDeposits(ctx context.Context, knownAddresses []string, tokenRegistry map[string]Asset, fromBlock, toBlock uint64) ([]ObservedTransfer, []UnsupportedTokenObservation, error)
}

// DepositAddressLister lists every customer deposit address currently provisioned
// (Story 1.5's deposit_addresses table) — the known-address set TrackDeposits scans
// against. Reloaded every poll cycle (simple and correct; scaling this is not this
// story's concern).
type DepositAddressLister interface {
	ListDepositAddresses(ctx context.Context) ([]string, error)
}

// TokenRegistryLister lists chain's configured ERC-20 token registry — the
// (contract_address -> asset) snapshot ChainScanner.ScanDeposits classifies each Transfer
// log against (Story 2.3, FR34). Scoped to chain (unlike DepositAddressLister's
// platform-wide list, a token contract address genuinely differs per chain — the same
// reasoning that made evm.Chain.USDCAddress chain-specific before this story). Reloaded
// every poll cycle, the same "simple and correct" choice as DepositAddressLister. Keys
// are lowercase-normalized hex contract addresses: Ethereum addresses are
// case-insensitive at the byte level, but log addresses and stored strings can differ in
// checksum casing, so both the write side (postgres.TokenRegistry.UpsertToken) and the
// read side (evm.Scanner's lookup) must agree on one canonical case.
type TokenRegistryLister interface {
	ListTokenRegistry(ctx context.Context, chain Chain) (map[string]Asset, error)
}

// DepositRepository is the watcher's sole write path for deposits, cursors, and their
// paired outbox events. Implementations must run every method's writes against whatever
// transaction is already open on ctx (established by TrackDeposits via TxBeginner)
// rather than opening their own, the same AD-4 contract as CustomerRepository.
type DepositRepository interface {
	// RecordObserved inserts deposit in the observed state and, only when a row was
	// actually inserted, a paired "deposit.pending" outbox event — both in the caller's
	// open transaction (AD-4). Re-observing the same (chain, tx_hash, log_index) on a
	// repoll relies on the DB's UNIQUE constraint (INSERT ... ON CONFLICT DO NOTHING,
	// AD-5), never an application-level existence check; inserted is false on that
	// no-op path.
	RecordObserved(ctx context.Context, deposit Deposit) (inserted bool, err error)
	// PromoteToSafe transitions every observed deposit on chain whose block_number is
	// at or below safeBlock to the safe state, in one bulk statement. Returns the
	// number of rows transitioned.
	PromoteToSafe(ctx context.Context, chain Chain, safeBlock uint64) (int, error)
	// PromoteToFinalized transitions every safe deposit on chain whose block_number is
	// at or below finalizedBlock to the finalized state, in one bulk statement (Story
	// 2.2), mirroring PromoteToSafe exactly one tier up. Returns the number of rows
	// transitioned.
	PromoteToFinalized(ctx context.Context, chain Chain, finalizedBlock uint64) (int, error)
	// CreditFinalizedDeposits credits every finalized deposit on chain whose
	// (chain, asset) crediting policy is 'finalized' (Story 2.2, FR9): for each eligible
	// row it writes one balanced journal entry (cause_type='deposit_credit',
	// cause_id=deposit.id) debiting the chain/asset forwarder-float platform account and
	// crediting the customer's own account, transitions the deposit to credited, and
	// writes a paired "deposit.credited" outbox event — all in the caller's open
	// transaction (AD-4). A deposit already credited is never re-selected (the query is
	// scoped to state='finalized'), so this is naturally idempotent across repeated
	// polls. Returns the number of deposits credited.
	CreditFinalizedDeposits(ctx context.Context, chain Chain) (int, error)
	// ListPendingDeposits returns every observed/safe deposit on chain (Story 2.4) —
	// exactly the states TrackDeposits.Execute's reorg-check phase must re-verify each
	// poll. finalized/credited deposits are never candidates for orphaning and are never
	// returned here (this is what makes AC1's "no balance ever affected" true by
	// construction, not a runtime guard).
	ListPendingDeposits(ctx context.Context, chain Chain) ([]Deposit, error)
	// OrphanDeposit transitions depositID to the orphaned state and writes a paired
	// "deposit.orphaned" outbox event, both in the transaction already open on ctx
	// (AD-4) — mirroring RecordObserved's paired-write pattern exactly.
	OrphanDeposit(ctx context.Context, depositID string) error
	// Cursor returns the last block persisted for (chain, tier) — "observed", "safe",
	// or "finalized" — or 0 if no cursor has ever been set for that pair.
	Cursor(ctx context.Context, chain Chain, tier string) (uint64, error)
	// SetCursor persists the last block processed for (chain, tier).
	SetCursor(ctx context.Context, chain Chain, tier string, block uint64) error
}

// DepositReader reads a customer's own deposits — observed and safe tiers count as
// "pending" (Story 2.2 introduces finalized/credited, never surfaced here) and orphaned
// deposits are surfaced as their own status (Story 2.4, AC1: a customer must be able to
// see a deposit was reorged away, not have it silently vanish). Like BalanceRepository,
// implementations query independently of any transaction on ctx: this serves a
// non-mutating GET route, and IdempotencyMiddleware never opens a transaction for it.
type DepositReader interface {
	// ListCustomerDeposits returns customerID's deposits, resolved via a join against
	// deposit_addresses (deposits has no customer_id column by design, AD-8). Returns
	// ErrCustomerNotFound if no such customer exists.
	ListCustomerDeposits(ctx context.Context, customerID string) ([]Deposit, error)
}

// UnsupportedTokenRepository is the watcher's write path for unsupported-token
// observations (Story 2.3, FR11) and the operator-facing read path over them.
// RecordObservation must run against whatever transaction is already open on ctx
// (established by TrackDeposits via TxBeginner), mirroring DepositRepository's AD-4
// contract exactly — an unsupported-token observation is recorded in the SAME
// transaction as everything else that poll cycle. ListObservations is a plain read,
// scoped to no customer (this is operator-facing and platform-wide, not customer-scoped),
// so it queries independently of any transaction on ctx like DepositReader.
type UnsupportedTokenRepository interface {
	// RecordObservation inserts observation, relying entirely on the DB's UNIQUE
	// (chain, tx_hash, log_index) constraint (INSERT ... ON CONFLICT DO NOTHING, AD-5) —
	// never an application-level existence check — so a repoll of an already-recorded
	// event is a no-op reported via inserted=false, never an error. Mirrors
	// DepositRepository.RecordObserved's exact idempotency pattern.
	RecordObservation(ctx context.Context, observation UnsupportedTokenObservation) (inserted bool, err error)
	// ListObservations returns every recorded unsupported-token observation, newest
	// first — a flat, platform-wide list for manual operator triage (AC3), never filtered
	// or scoped to a customer.
	ListObservations(ctx context.Context) ([]UnsupportedTokenObservation, error)
}

// FeeEstimator estimates a withdrawal's on-chain fee for chain, split into its L2
// execution and L1 data-posting components (Story 3.1) — never collapsed into one
// undifferentiated number. Implemented in internal/adapter/evm (AD-1): Arbitrum via
// NodeInterface.gasEstimateComponents() (an ArbOS-native precompile, called via raw RPC,
// never a typed contract binding — it has no deployed bytecode), Base via the
// GasPriceOracle predeploy (eth_estimateGas/eth_gasPrice for the L2 component,
// getL1FeeUpperBound for the L1 component). Both implementations estimate against a
// fixed representative transaction — empty data for ETH, an ABI-encoded
// transfer(placeholder, amount) against the chain's registered USDC contract for USDC —
// since this endpoint's inputs carry no real withdrawal destination and no withdrawal
// resource exists until Story 3.2 (this is a pure, unpersisted, read-only computation).
type FeeEstimator interface {
	EstimateFee(ctx context.Context, chain Chain, asset Asset, amount *big.Int) (FeeEstimate, error)
}

// WithdrawalRepository persists a requested withdrawal's immediate hold placement (Story
// 3.2) as one balanced journal entry (debit the customer's available account, credit its
// hold account, same (chain, asset) pair) plus the withdrawals row and its paired outbox
// event — all atomically — and (Story 3.3) the subsequent policy-check-and-route
// transition into either awaiting-approval or approved, in that SAME transaction. Like
// TransferRepository, implementations must run their writes against whatever transaction
// is already open on ctx (established by IdempotencyMiddleware for this mutating POST)
// rather than opening their own.
type WithdrawalRepository interface {
	// CreateWithdrawal locks the customer's available and hold accounts for
	// (req.Chain, req.Asset) in a single deterministic-order statement (mirroring
	// TransferRepository.CreateTransfer's own lock-ordering discipline), verifies the
	// available account's derived balance covers req.Amount, and writes the journal
	// entry, its two postings, and the withdrawals row atomically. feeEstimate and
	// targetStatus are computed by the caller (CreateWithdrawal, core) BEFORE this method
	// is ever invoked — this method never calls FeeEstimator or WithdrawalThresholdLister
	// itself (AD-1's adapters-don't-call-adapters rule) — and are used only to (a) verify
	// the SAME already-locked available-account balance read, post-hold, covers
	// feeEstimate (ErrInsufficientBalanceForFee if not, with no partial write: the whole
	// call returns an error and nothing commits) and (b) write targetStatus
	// (WithdrawalStatusAwaitingApproval or WithdrawalStatusApproved) as the withdrawals
	// row's status, together with the matching paired outbox event ("approval.required"
	// or "withdrawal.approved"). Returns ErrCustomerNotFound if the customer has no
	// available/hold accounts for (req.Chain, req.Asset), ErrInsufficientBalance if the
	// available account's derived balance is less than req.Amount, ErrInsufficientBalanceForFee
	// per the above, or ErrDuplicateWithdrawalCause on a duplicate cause_id (a narrow
	// concurrent-request race — see internal/adapter/postgres/withdrawal_repo.go).
	CreateWithdrawal(ctx context.Context, req WithdrawalRequest, feeEstimate *big.Int, targetStatus string) (Withdrawal, error)
	// ApproveWithdrawal locks the withdrawal row FOR UPDATE, verifies its status is
	// WithdrawalStatusAwaitingApproval, and transitions it to WithdrawalStatusApproved,
	// recording actor/reason/timestamp on the row (approved_by, approval_reason,
	// approved_at, NFR11) and writing a paired "withdrawal.approved" outbox event —
	// atomically, in the transaction already open on ctx. Returns ErrWithdrawalNotFound if
	// no withdrawal with id exists, or ErrWithdrawalNotAwaitingApproval if it exists but is
	// not currently awaiting approval (already approved — including the losing side of a
	// concurrent double-approve race, made deterministic by the row lock — or still
	// created).
	ApproveWithdrawal(ctx context.Context, id, actor, reason string) (Withdrawal, error)
}

// WithdrawalThresholdLister reads a (chain, asset) pair's configured withdrawal approval
// threshold (Story 3.3, FR17) from the withdrawal_approval_thresholds data table — never a
// Go constant, mirroring migration 0006's crediting_policy precedent (FR9-style "policy is
// data, not code"). Implemented in internal/adapter/postgres, the same small-repo shape as
// TokenRegistryLister.
type WithdrawalThresholdLister interface {
	// GetApprovalThreshold returns chain/asset's configured threshold amount. Returns a
	// "no threshold configured" error — never a guessed default — if no row exists for the
	// pair (a registry gap, the I/O matrix's own "should never happen in a correctly
	// configured deployment" case): a withdrawal must never be silently auto-approved or
	// silently blocked because a threshold row is missing.
	GetApprovalThreshold(ctx context.Context, chain Chain, asset Asset) (*big.Int, error)
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
