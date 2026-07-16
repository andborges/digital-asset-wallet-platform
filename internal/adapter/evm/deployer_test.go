package evm

import (
	"context"
	"errors"
	"math/big"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

type fakeChainClient struct {
	chainID    *big.Int
	chainIDErr error
	code       []byte
	codeErr    error
	closed     bool
}

func (f *fakeChainClient) ChainID(ctx context.Context) (*big.Int, error) {
	if f.chainIDErr != nil {
		return nil, f.chainIDErr
	}
	return f.chainID, nil
}

func (f *fakeChainClient) CodeAt(ctx context.Context, account common.Address, blockNumber *big.Int) ([]byte, error) {
	if f.codeErr != nil {
		return nil, f.codeErr
	}
	return f.code, nil
}

func (f *fakeChainClient) Close() {
	f.closed = true
}

func TestVerifyDeployerPresence_SucceedsWhenChainMatchesAndCodeIsPresent(t *testing.T) {
	client := &fakeChainClient{chainID: big.NewInt(84532), code: []byte{0x60, 0x80}}

	if err := verifyDeployerPresence(context.Background(), client, Chain{Name: "base", ChainID: 84532}); err != nil {
		t.Fatalf("verifyDeployerPresence() error = %v, want nil", err)
	}
}

func TestVerifyDeployerPresence_FailsWhenChainIDMismatches(t *testing.T) {
	// The endpoint reports Arbitrum Sepolia while the config says Base Sepolia — the
	// swapped-URL misconfiguration the chain-identity check exists to catch (re-review
	// 2026-07-16): deployer code IS present, so the old presence-only check would pass.
	client := &fakeChainClient{chainID: big.NewInt(421614), code: []byte{0x60, 0x80}}

	err := verifyDeployerPresence(context.Background(), client, Chain{Name: "base", ChainID: 84532})
	if err == nil {
		t.Fatal("verifyDeployerPresence() error = nil, want a non-nil error (chain id mismatch)")
	}
}

func TestVerifyDeployerPresence_FailsOnChainIDError(t *testing.T) {
	client := &fakeChainClient{chainIDErr: errors.New("connection refused")}

	err := verifyDeployerPresence(context.Background(), client, Chain{Name: "base", ChainID: 84532})
	if err == nil {
		t.Fatal("verifyDeployerPresence() error = nil, want a non-nil error (eth_chainId failure)")
	}
}

func TestVerifyDeployerPresence_FailsWhenNoCodeAtAddress(t *testing.T) {
	client := &fakeChainClient{chainID: big.NewInt(84532), code: nil}

	err := verifyDeployerPresence(context.Background(), client, Chain{Name: "base", ChainID: 84532})
	if err == nil {
		t.Fatal("verifyDeployerPresence() error = nil, want a non-nil error (deployer absent)")
	}
}

func TestVerifyDeployerPresence_FailsOnRPCError(t *testing.T) {
	client := &fakeChainClient{chainID: big.NewInt(84532), codeErr: errors.New("connection refused")}

	err := verifyDeployerPresence(context.Background(), client, Chain{Name: "base", ChainID: 84532})
	if err == nil {
		t.Fatal("verifyDeployerPresence() error = nil, want a non-nil error (RPC failure)")
	}
}

// freeLocalPort asks the kernel for an unused TCP port and releases it for anvil to
// claim. The listen-then-close handoff is inherently racy, but the window is milliseconds
// on a local machine — unlike the previous hardcoded port, which collided deterministically
// with anything else using it on a shared CI runner (re-review 2026-07-16).
func freeLocalPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return strconv.Itoa(port)
}

// TestVerifyDeployerPresence_RealAnvil exercises the full VerifyDeployerPresence path
// (real ethclient.DialContext + real RPC) against a locally-installed `anvil` instance.
// Foundry's anvil preinstalls the canonical CREATE2 deployer at genesis by default (and
// reports chain id 31337 by default), so the fully-passing branch is exercised for real,
// over real RPC, with zero manual setup — confirming the fake-based unit tests above
// match real-world behavior.
func TestVerifyDeployerPresence_RealAnvil(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping anvil-backed test in -short mode")
	}
	anvilPath, err := exec.LookPath("anvil")
	if err != nil {
		t.Skip("anvil not found on PATH — install Foundry (foundryup) to run this test")
	}

	port := freeLocalPort(t)
	cmd := exec.Command(anvilPath, "--port", port, "--silent")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start anvil: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	rpcURL := "http://127.0.0.1:" + port

	// Wait for anvil to accept connections and answer a real call rather than assuming
	// it's ready immediately after Start returns. Track whichever error happened last —
	// dial OR call — so a node that accepts connections but never becomes able to answer
	// is reported accurately (the previous version overwrote the call error with the nil
	// dial error, skipping this gate entirely — re-review 2026-07-16).
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	var lastErr error
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		client, dialErr := ethclient.DialContext(readyCtx, rpcURL)
		if dialErr != nil {
			lastErr = dialErr
		} else {
			_, blockErr := client.BlockNumber(readyCtx)
			client.Close()
			if blockErr == nil {
				ready = true
				break
			}
			lastErr = blockErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("anvil did not become ready: %v", lastErr)
	}

	// Fresh timeout for the actual verification, so readiness polling can't eat its budget.
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelVerify()
	chain := Chain{Name: "anvil", RPCURL: rpcURL, ChainID: 31337}
	if err := VerifyDeployerPresence(verifyCtx, chain); err != nil {
		t.Fatalf("VerifyDeployerPresence() error = %v, want nil (anvil preinstalls the canonical deployer)", err)
	}
}
