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
