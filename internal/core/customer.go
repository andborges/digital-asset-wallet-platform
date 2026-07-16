// Package core is the domain: ledger, deposit/withdrawal/sweep machines, policy, ports.
// It imports nothing from internal/adapter/* (AD-1, AD-2) — adapters import core, never the reverse.
package core

import (
	"errors"
	"math/big"
	"time"
)

// Customer is a platform customer. Balances are never stored on Customer or Account —
// they are always derived from postings (AD-3), starting this story's derivation query.
// DepositAddress is computed once at customer creation (AD-8) and always required —
// there is no "pending" state, since deriving it is pure math with no chain interaction.
type Customer struct {
	ID             string
	CreatedAt      time.Time
	DepositAddress string
}

// ErrCustomerNotFound is returned by BalanceRepository.CustomerBalances when no
// customer with the given id exists.
var ErrCustomerNotFound = errors.New("customer not found")

// ErrInsufficientBalance is returned by TransferRepository.CreateTransfer when the
// source account's derived balance is less than the requested transfer amount.
var ErrInsufficientBalance = errors.New("source account balance is less than the requested transfer amount")

// ErrDuplicateTransferCause is returned by TransferRepository.CreateTransfer when a
// journal entry already exists for the given idempotency key — a narrow race between
// two concurrent requests carrying the same Idempotency-Key (AD-3, AD-5). The caller
// should retry; a retry lands after the winning request's commit and is served by
// IdempotencyMiddleware's own dedup.
var ErrDuplicateTransferCause = errors.New("a journal entry already exists for this idempotency key")

// ErrInvalidCursor is returned by TransactionRepository.ListCustomerTransactions when a
// non-empty cursor does not decode to a valid page marker (tampered, truncated, or
// otherwise malformed).
var ErrInvalidCursor = errors.New("cursor is not a valid page marker")

// ErrInvalidPageSize is returned by ListCustomerTransactions.Execute when pageSize is
// negative — zero is treated as "omitted" (substituted with a default), never an error.
var ErrInvalidPageSize = errors.New("page size must be a positive integer")

// Chain identifies a supported EVM chain.
type Chain string

const (
	ChainBase     Chain = "base"
	ChainArbitrum Chain = "arbitrum"
)

// Asset identifies a supported asset.
type Asset string

const (
	AssetETH  Asset = "eth"
	AssetUSDC Asset = "usdc"
)

// SupportedChainAssetPairs is the fixed v1 set of (chain, asset) pairs every customer
// is provisioned with. Base/ETH, Base/USDC, Arbitrum/ETH, Arbitrum/USDC — see PRD FR11.
var SupportedChainAssetPairs = []struct {
	Chain Chain
	Asset Asset
}{
	{ChainBase, AssetETH},
	{ChainBase, AssetUSDC},
	{ChainArbitrum, AssetETH},
	{ChainArbitrum, AssetUSDC},
}

// Account is a per-customer, per-(chain, asset) ledger account. It carries no balance
// field: balances are always derived from postings (AD-3) via BalanceRepository.
type Account struct {
	ID         string
	CustomerID string
	Chain      Chain
	Asset      Asset
	CreatedAt  time.Time
}

// AccountBalance is a (chain, asset) balance derived from summing an account's
// postings (AD-3) — never a stored value.
type AccountBalance struct {
	Chain   Chain
	Asset   Asset
	Balance *big.Int
}

// TransferRequest is the input to CreateTransfer (FR4). Source and destination are
// scoped to the same (Chain, Asset) pair — internal transfers move balance between two
// customers' accounts for one specific supported pair, never across chains.
type TransferRequest struct {
	SourceCustomerID      string
	DestinationCustomerID string
	Chain                 Chain
	Asset                 Asset
	Amount                *big.Int
	// IdempotencyKey becomes the created journal entry's cause_id (FR5).
	IdempotencyKey string
}

// Transfer is a completed ledger-only internal transfer (FR4) — the journal entry
// TransferRepository.CreateTransfer wrote.
type Transfer struct {
	// ID is the journal entry's id.
	ID                    string
	SourceCustomerID      string
	DestinationCustomerID string
	Chain                 Chain
	Asset                 Asset
	Amount                *big.Int
	CreatedAt             time.Time
}

// Transaction is one entry in a customer's transaction history (FR3) — one row per
// (journal entry, this customer's own posting on it). Read generically from the
// cause-tagged journal: Type is the journal entry's cause_type verbatim, and Amount is
// this customer's own posting amount, signed (negative when debited, positive when
// credited) — never the transfer's unsigned magnitude.
type Transaction struct {
	// ID is the journal entry's id.
	ID        string
	Type      string
	Amount    *big.Int
	Chain     Chain
	Asset     Asset
	Status    string
	CreatedAt time.Time
}

// TransactionPage is one page of a customer's transaction history, newest first.
// NextCursor is "" when there is no further page.
type TransactionPage struct {
	Transactions []Transaction
	NextCursor   string
}
