package evm

// Chain identifies one of the platform's configured EVM chains for RPC purposes: a name
// (for logging/errors), the RPC endpoint used to reach it, and the chain ID that endpoint
// is expected to report — chain-specific config that AD-1 confines to this package. This
// is the api role's single RPC provider per chain, not the dual-provider split AD-12
// requires for recon (Epic 5).
type Chain struct {
	Name   string
	RPCURL string
	// ChainID is the EIP-155 chain ID the RPC endpoint must report (verified at startup
	// via eth_chainId, re-review 2026-07-16). Operator-configured per environment
	// (BASE_CHAIN_ID/ARBITRUM_CHAIN_ID) rather than hardcoded: the same binary runs
	// against Sepolia testnets, mainnets, and local anvil (31337) — but a probe that
	// only checked deployer presence would pass against ANY chain (the canonical
	// deployer is live on most of them), silently accepting swapped or mis-pasted
	// RPC URLs.
	ChainID uint64
}
