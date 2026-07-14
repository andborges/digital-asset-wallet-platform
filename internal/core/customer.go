// Package core is the domain: ledger, deposit/withdrawal/sweep machines, policy, ports.
// It imports nothing from internal/adapter/* (AD-1, AD-2) — adapters import core, never the reverse.
package core

import "time"

// Customer is a platform customer. Balances are never stored on Customer or Account —
// they are derived from postings starting Story 1.3 (AD-3).
type Customer struct {
	ID        string
	CreatedAt time.Time
}

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
// field: balances are always derived from postings (AD-3), which don't exist until Story 1.3.
type Account struct {
	ID         string
	CustomerID string
	Chain      Chain
	Asset      Asset
	CreatedAt  time.Time
}
