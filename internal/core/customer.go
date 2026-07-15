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
type Customer struct {
	ID        string
	CreatedAt time.Time
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
