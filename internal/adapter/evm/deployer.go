package evm

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// chainClient is the minimal surface VerifyDeployerPresence needs from an RPC client —
// small enough to fake in unit tests without a real chain (see deployer_test.go).
type chainClient interface {
	ChainID(ctx context.Context) (*big.Int, error)
	CodeAt(ctx context.Context, account common.Address, blockNumber *big.Int) ([]byte, error)
	Close()
}

// VerifyDeployerPresence confirms the configured RPC endpoint is actually the chain it
// claims to be (eth_chainId vs chain.ChainID — a swapped or mis-pasted RPC URL would
// otherwise pass, since the canonical deployer is live on most EVM chains) and that
// canonicalDeployerAddress has contract code there (AC3) — the precondition for any
// future CREATE2 deployment to be trustworthy. Called once per configured chain at api
// startup; the caller is expected to fail startup loudly on a non-nil error, per AC3 —
// this function does not retry or degrade.
func VerifyDeployerPresence(ctx context.Context, chain Chain) error {
	client, err := ethclient.DialContext(ctx, chain.RPCURL)
	if err != nil {
		return fmt.Errorf("connect to %s RPC %q: %w", chain.Name, chain.RPCURL, err)
	}
	defer client.Close()

	return verifyDeployerPresence(ctx, client, chain)
}

// verifyDeployerPresence holds the testable logic: given anything satisfying
// chainClient, confirm chain identity and canonical-deployer code. Split out from
// VerifyDeployerPresence so unit tests can exercise every branch (wrong chain,
// present, absent, RPC error) with a fake client, without dialing a real chain.
func verifyDeployerPresence(ctx context.Context, client chainClient, chain Chain) error {
	gotID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("query %s for chain id: %w", chain.Name, err)
	}
	if wantID := new(big.Int).SetUint64(chain.ChainID); gotID.Cmp(wantID) != 0 {
		return fmt.Errorf(
			"chain %q: RPC endpoint reports chain id %s, want %d — mis-wired RPC URL? refusing to start against an unverified chain",
			chain.Name, gotID, chain.ChainID,
		)
	}

	code, err := client.CodeAt(ctx, canonicalDeployerAddress, nil)
	if err != nil {
		return fmt.Errorf("query %s for canonical deployer code: %w", chain.Name, err)
	}
	if len(code) == 0 {
		return fmt.Errorf(
			"canonical CREATE2 deployer (%s) has no code on chain %q — refusing to serve deposit addresses that could collide or diverge",
			canonicalDeployerAddress.Hex(), chain.Name,
		)
	}
	return nil
}
