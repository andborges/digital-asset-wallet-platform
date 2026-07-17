package core

import (
	"math/big"
	"time"
)

// DepositState is a deposit's position in the observed->safe->finalized->credited state
// machine (AD-6's fixed vocabulary). Orphaned is the reorg-eviction terminal state.
// Story 2.1 writes Observed and Safe; Story 2.2 adds Finalized and Credited (never
// backward, and never re-entered once Credited — see track_deposits.go); Orphaned is
// Story 2.4's — but the full enum was declared from Story 2.1 on so the column's valid
// values never need a later migration.
type DepositState string

const (
	DepositObserved  DepositState = "observed"
	DepositSafe      DepositState = "safe"
	DepositFinalized DepositState = "finalized"
	DepositOrphaned  DepositState = "orphaned"
	DepositCredited  DepositState = "credited"
)

// Cursor tiers: one watcher_cursors row per (chain, tier), so the observed-scan cursor,
// the safe-promotion cursor, and the finalized-promotion cursor (Story 2.2) advance
// independently (AD-5 groundwork for Story 2.5's crash/downtime recovery). The
// finalized-tier cursor is written purely for observability bookkeeping, not
// correctness — PromoteToFinalized and CreditFinalizedDeposits are both idempotent bulk
// WHERE state=... operations over already-persisted rows (see track_deposits.go).
const (
	CursorTierObserved  = "observed"
	CursorTierSafe      = "safe"
	CursorTierFinalized = "finalized"
)

// Deposit is a single tracked on-chain transfer landing on a customer's deposit address
// (FR-adjacent Epic 2). It carries no customer_id (AD-8): the address is the only
// attribution key the watcher ever uses, and DepositReader resolves customer_id at read
// time via a join against deposit_addresses, never re-derived or looked up mid-scan.
type Deposit struct {
	ID      string
	Chain   Chain
	Asset   Asset
	Address string
	TxHash  string
	// LogIndex is -1 for a native ETH transfer (which has no log to key on) and the
	// real EVM log index for an ERC-20 Transfer event — the sentinel that lets both
	// share one (chain, tx_hash, log_index) uniqueness key (AD-5).
	LogIndex    int
	Amount      *big.Int
	BlockNumber uint64
	// BlockHash is the hash of the block this deposit was observed in, captured at
	// observation time (Story 2.4). Every poll, TrackDeposits.Execute re-checks this
	// stored hash against the chain's CURRENT hash at BlockNumber — a mismatch (or the
	// height no longer existing at all) is what detects a reorg (Design Notes: "a
	// stored-hash comparison, not a depth heuristic").
	BlockHash  string
	State      DepositState
	ObservedAt time.Time
	UpdatedAt  time.Time
}

// ObservedTransfer is a single ETH/USDC transfer a ChainScanner found landing on a known
// deposit address — the scanner's output, before it becomes a persisted Deposit row.
type ObservedTransfer struct {
	Chain       Chain
	Asset       Asset
	Address     string
	TxHash      string
	LogIndex    int
	Amount      *big.Int
	BlockNumber uint64
	// BlockHash is the hash of the block the scanner found this transfer in (Story 2.4)
	// — populated from the same block/log data the scanner already fetched to find the
	// transfer itself, never a second RPC round-trip.
	BlockHash string
}

// UnsupportedTokenObservation is a single ERC-20 Transfer log a ChainScanner found
// landing on a known deposit address, emitted by a contract NOT in the token_registry
// snapshot it was given (Story 2.3, FR11). It is a visible, historical record only —
// never a Deposit, never credited, never touching journal_entries/postings. Unlike
// ObservedTransfer it carries ContractAddress (the token contract that emitted the log)
// instead of Asset: by definition, an unsupported token's asset identity is not one this
// platform can currently interpret.
type UnsupportedTokenObservation struct {
	ID              string
	Chain           Chain
	Address         string
	ContractAddress string
	TxHash          string
	LogIndex        int
	Amount          *big.Int
	BlockNumber     uint64
	ObservedAt      time.Time
}
