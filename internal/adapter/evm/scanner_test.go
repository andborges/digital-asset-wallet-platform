package evm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// testERC20CreationBytecode is contracts/src/TestERC20.sol's compiled creation
// bytecode, pinned the same way address.go pins Forwarder/Factory's init-code hashes:
// compile once (`forge build` from contracts/), then copy
// contracts/out/TestERC20.sol/TestERC20.json's ".bytecode.object" field here (minus its
// "0x" prefix). TestERC20 is a throwaway fixture used only by this file, never deployed
// anywhere real and unrelated to AD-8's CREATE2 scheme — it exists so ScanDeposits' ERC-20
// Transfer-log path (USDC detection) can be exercised against a real, standard-shaped
// Transfer(address indexed, address indexed, uint256) event on a real chain, without
// depending on a real USDC deployment.
const testERC20CreationBytecode = "6080604052348015600e575f5ffd5b506040516102c63803806102c6833981016040819052602b916071565b335f81815260208181526040808320859055518481527fddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef910160405180910390a3506087565b5f602082840312156080575f5ffd5b5051919050565b610232806100945f395ff3fe608060405234801561000f575f5ffd5b5060043610610034575f3560e01c806370a0823114610038578063a9059cbb1461006a575b5f5ffd5b6100576100463660046101a3565b5f6020819052908152604090205481565b6040519081526020015b60405180910390f35b61007d6100783660046101c3565b61008d565b6040519015158152602001610061565b335f908152602081905260408120548211156100ef5760405162461bcd60e51b815260206004820152601f60248201527f5465737445524332303a20696e73756666696369656e742062616c616e636500604482015260640160405180910390fd5b335f908152602081905260408120805484929061010d9084906101ff565b90915550506001600160a01b0383165f9081526020819052604081208054849290610139908490610212565b90915550506040518281526001600160a01b0384169033907fddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef9060200160405180910390a35060015b92915050565b80356001600160a01b038116811461019e575f5ffd5b919050565b5f602082840312156101b3575f5ffd5b6101bc82610188565b9392505050565b5f5f604083850312156101d4575f5ffd5b6101dd83610188565b946020939093013593505050565b634e487b7160e01b5f52601160045260245ffd5b81810381811115610182576101826101eb565b80820180821115610182576101826101eb56fea164736f6c6343000824000a"

// anvilChainID is anvil's default chain id (used unless anvil is started with a
// non-default --chain-id).
const anvilChainID = 31337

// anvilDefaultPrivateKeyHex is anvil's well-known first dev account (mnemonic "test
// test test test test test test test test test test junk", account #0) — printed by
// every anvil instance started without --silent, and stable across versions since
// anvil's default mnemonic/derivation path never changes (empirically confirmed against
// this environment's installed anvil before pinning it here).
const anvilDefaultPrivateKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

// TestScanner_RealAnvil_FindsNativeAndUSDCTransfers exercises the full Scanner path (real
// ethclient + real RPC) against a locally-installed `anvil` instance: it deploys a
// throwaway ERC-20 (TestERC20), sends both a native ETH value transfer and an ERC-20
// transfer() to a fixed "known deposit address", then asserts ScanDeposits finds both —
// proving the native tx.To() scan and the eth_getLogs Transfer filter both work over a
// real chain, not just against fakes (deployer_test.go's real-anvil test is the model).
func TestScanner_RealAnvil_FindsNativeAndUSDCTransfers(t *testing.T) {
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
	waitForAnvilReady(t, rpcURL)

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatalf("dial anvil: %v", err)
	}
	defer client.Close()

	privKey, err := crypto.HexToECDSA(anvilDefaultPrivateKeyHex)
	if err != nil {
		t.Fatalf("parse anvil dev private key: %v", err)
	}
	deployer := crypto.PubkeyToAddress(privKey.PublicKey)
	signer := types.NewEIP155Signer(big.NewInt(anvilChainID))

	// A fixed, arbitrary "known deposit address" (anvil's dev account #1) standing in
	// for a customer's real CREATE2 forwarder address — attribution in this test is via
	// the knownAddresses argument alone, exactly as AD-8 requires of the real watcher.
	depositAddr := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")

	nonce, err := client.PendingNonceAt(ctx, deployer)
	if err != nil {
		t.Fatalf("get deployer nonce: %v", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("suggest gas price: %v", err)
	}

	// Deploy TestERC20 with an initial supply minted to the deployer, so it has a
	// balance to send in the transfer() call below.
	creationCode, err := hex.DecodeString(testERC20CreationBytecode)
	if err != nil {
		t.Fatalf("decode TestERC20 creation bytecode: %v", err)
	}
	initialSupply := big.NewInt(1_000_000)
	deployData := append(append([]byte{}, creationCode...), common.LeftPadBytes(initialSupply.Bytes(), 32)...)

	deployTx := types.NewContractCreation(nonce, big.NewInt(0), 2_000_000, gasPrice, deployData)
	signedDeployTx, err := types.SignTx(deployTx, signer, privKey)
	if err != nil {
		t.Fatalf("sign deploy tx: %v", err)
	}
	if err := client.SendTransaction(ctx, signedDeployTx); err != nil {
		t.Fatalf("send deploy tx: %v", err)
	}
	deployReceipt, err := waitForReceipt(ctx, client, signedDeployTx.Hash())
	if err != nil {
		t.Fatalf("wait for deploy receipt: %v", err)
	}
	if deployReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("TestERC20 deployment failed (status %d)", deployReceipt.Status)
	}
	tokenAddr := deployReceipt.ContractAddress

	// Native ETH transfer straight to the known deposit address — the plain
	// value-transfer scanNativeTransfers must find via tx.To() scanning (it has no log
	// of its own).
	nonce++
	nativeAmount := big.NewInt(5_000_000_000_000_000) // 0.005 ETH in wei
	nativeTx := types.NewTransaction(nonce, depositAddr, nativeAmount, 21_000, gasPrice, nil)
	signedNativeTx, err := types.SignTx(nativeTx, signer, privKey)
	if err != nil {
		t.Fatalf("sign native transfer tx: %v", err)
	}
	if err := client.SendTransaction(ctx, signedNativeTx); err != nil {
		t.Fatalf("send native transfer tx: %v", err)
	}
	nativeReceipt, err := waitForReceipt(ctx, client, signedNativeTx.Hash())
	if err != nil {
		t.Fatalf("wait for native transfer receipt: %v", err)
	}
	if nativeReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("native transfer failed (status %d)", nativeReceipt.Status)
	}

	// ERC-20 transfer() call on TestERC20 to the known deposit address — the standard
	// Transfer log scanERC20Transfers must find via eth_getLogs.
	nonce++
	erc20Amount := big.NewInt(42_000)
	transferSelector := crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]
	callData := append(append([]byte{}, transferSelector...), common.LeftPadBytes(depositAddr.Bytes(), 32)...)
	callData = append(callData, common.LeftPadBytes(erc20Amount.Bytes(), 32)...)
	erc20Tx := types.NewTransaction(nonce, tokenAddr, big.NewInt(0), 100_000, gasPrice, callData)
	signedERC20Tx, err := types.SignTx(erc20Tx, signer, privKey)
	if err != nil {
		t.Fatalf("sign erc20 transfer tx: %v", err)
	}
	if err := client.SendTransaction(ctx, signedERC20Tx); err != nil {
		t.Fatalf("send erc20 transfer tx: %v", err)
	}
	erc20Receipt, err := waitForReceipt(ctx, client, signedERC20Tx.Hash())
	if err != nil {
		t.Fatalf("wait for erc20 transfer receipt: %v", err)
	}
	if erc20Receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("erc20 transfer failed (status %d)", erc20Receipt.Status)
	}

	chain := Chain{Name: "anvil", RPCURL: rpcURL, ChainID: anvilChainID}
	scanner, err := NewScanner(ctx, chain)
	if err != nil {
		t.Fatalf("NewScanner() error = %v, want nil", err)
	}
	defer scanner.Close()

	latest, safe, finalized, err := scanner.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v, want nil (anvil supports the safe and finalized tags)", err)
	}
	if latest == 0 {
		t.Fatal("Head() latest = 0, want > 0 after mining several transactions")
	}
	t.Logf("Head() latest=%d safe=%d finalized=%d", latest, safe, finalized)

	// Story 2.3: the token registry snapshot is what makes tokenAddr's Transfer log an
	// ordinary observed deposit rather than an unsupported-token observation — registered
	// under its lowercase hex form, the same canonical case
	// postgres.TokenRegistry.UpsertToken stores and the scanner's own lookup normalizes to.
	tokenRegistry := map[string]core.Asset{strings.ToLower(tokenAddr.Hex()): core.AssetUSDC}
	transfers, unsupported, err := scanner.ScanDeposits(ctx, []string{depositAddr.Hex()}, tokenRegistry, 0, latest)
	if err != nil {
		t.Fatalf("ScanDeposits() error = %v, want nil", err)
	}
	if len(unsupported) != 0 {
		t.Fatalf("unsupported observations = %+v, want none (tokenAddr is registered)", unsupported)
	}

	var gotNative, gotUSDC *core.ObservedTransfer
	for i := range transfers {
		tr := &transfers[i]
		switch {
		case tr.Asset == core.AssetETH && tr.TxHash == signedNativeTx.Hash().Hex():
			gotNative = tr
		case tr.Asset == core.AssetUSDC && tr.TxHash == signedERC20Tx.Hash().Hex():
			gotUSDC = tr
		}
	}

	if gotNative == nil {
		t.Fatalf("no native ETH transfer observed among %+v", transfers)
	}
	if gotNative.LogIndex != nativeTransferLogIndex {
		t.Fatalf("native transfer log index = %d, want %d (sentinel)", gotNative.LogIndex, nativeTransferLogIndex)
	}
	if gotNative.Amount.Cmp(nativeAmount) != 0 {
		t.Fatalf("native transfer amount = %s, want %s", gotNative.Amount, nativeAmount)
	}
	if !strings.EqualFold(gotNative.Address, depositAddr.Hex()) {
		t.Fatalf("native transfer address = %s, want %s", gotNative.Address, depositAddr.Hex())
	}
	if gotNative.Chain != core.Chain("anvil") {
		t.Fatalf("native transfer chain = %q, want %q", gotNative.Chain, "anvil")
	}

	if gotUSDC == nil {
		t.Fatalf("no USDC transfer observed among %+v", transfers)
	}
	if gotUSDC.LogIndex < 0 {
		t.Fatalf("USDC transfer log index = %d, want a real (non-negative) EVM log index", gotUSDC.LogIndex)
	}
	if gotUSDC.Amount.Cmp(erc20Amount) != 0 {
		t.Fatalf("USDC transfer amount = %s, want %s", gotUSDC.Amount, erc20Amount)
	}
	if !strings.EqualFold(gotUSDC.Address, depositAddr.Hex()) {
		t.Fatalf("USDC transfer address = %s, want %s", gotUSDC.Address, depositAddr.Hex())
	}
}

// TestScanner_RealAnvil_ClassifiesUnregisteredTokenAsUnsupported proves Story 2.3's core
// classification rule against a real chain: an ERC-20 Transfer log landing on a known
// deposit address, emitted by a contract NOT present in the tokenRegistry map passed to
// ScanDeposits, is returned as an UnsupportedTokenObservation — never as an
// ObservedTransfer, never silently dropped. Reuses the exact same deploy/transfer setup
// as TestScanner_RealAnvil_FindsNativeAndUSDCTransfers above, the only difference being
// an empty (rather than tokenAddr-populated) token registry passed to ScanDeposits.
func TestScanner_RealAnvil_ClassifiesUnregisteredTokenAsUnsupported(t *testing.T) {
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
	waitForAnvilReady(t, rpcURL)

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatalf("dial anvil: %v", err)
	}
	defer client.Close()

	privKey, err := crypto.HexToECDSA(anvilDefaultPrivateKeyHex)
	if err != nil {
		t.Fatalf("parse anvil dev private key: %v", err)
	}
	deployer := crypto.PubkeyToAddress(privKey.PublicKey)
	signer := types.NewEIP155Signer(big.NewInt(anvilChainID))

	depositAddr := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")

	nonce, err := client.PendingNonceAt(ctx, deployer)
	if err != nil {
		t.Fatalf("get deployer nonce: %v", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("suggest gas price: %v", err)
	}

	creationCode, err := hex.DecodeString(testERC20CreationBytecode)
	if err != nil {
		t.Fatalf("decode TestERC20 creation bytecode: %v", err)
	}
	initialSupply := big.NewInt(1_000_000)
	deployData := append(append([]byte{}, creationCode...), common.LeftPadBytes(initialSupply.Bytes(), 32)...)

	deployTx := types.NewContractCreation(nonce, big.NewInt(0), 2_000_000, gasPrice, deployData)
	signedDeployTx, err := types.SignTx(deployTx, signer, privKey)
	if err != nil {
		t.Fatalf("sign deploy tx: %v", err)
	}
	if err := client.SendTransaction(ctx, signedDeployTx); err != nil {
		t.Fatalf("send deploy tx: %v", err)
	}
	deployReceipt, err := waitForReceipt(ctx, client, signedDeployTx.Hash())
	if err != nil {
		t.Fatalf("wait for deploy receipt: %v", err)
	}
	if deployReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("TestERC20 deployment failed (status %d)", deployReceipt.Status)
	}
	tokenAddr := deployReceipt.ContractAddress

	nonce++
	erc20Amount := big.NewInt(7_000)
	transferSelector := crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]
	callData := append(append([]byte{}, transferSelector...), common.LeftPadBytes(depositAddr.Bytes(), 32)...)
	callData = append(callData, common.LeftPadBytes(erc20Amount.Bytes(), 32)...)
	erc20Tx := types.NewTransaction(nonce, tokenAddr, big.NewInt(0), 100_000, gasPrice, callData)
	signedERC20Tx, err := types.SignTx(erc20Tx, signer, privKey)
	if err != nil {
		t.Fatalf("sign erc20 transfer tx: %v", err)
	}
	if err := client.SendTransaction(ctx, signedERC20Tx); err != nil {
		t.Fatalf("send erc20 transfer tx: %v", err)
	}
	erc20Receipt, err := waitForReceipt(ctx, client, signedERC20Tx.Hash())
	if err != nil {
		t.Fatalf("wait for erc20 transfer receipt: %v", err)
	}
	if erc20Receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("erc20 transfer failed (status %d)", erc20Receipt.Status)
	}

	chain := Chain{Name: "anvil", RPCURL: rpcURL, ChainID: anvilChainID}
	scanner, err := NewScanner(ctx, chain)
	if err != nil {
		t.Fatalf("NewScanner() error = %v, want nil", err)
	}
	defer scanner.Close()

	latest, _, _, err := scanner.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v, want nil", err)
	}

	// The key difference from TestScanner_RealAnvil_FindsNativeAndUSDCTransfers: an empty
	// token registry, so tokenAddr's Transfer log has no registry match at all.
	emptyRegistry := map[string]core.Asset{}
	transfers, unsupported, err := scanner.ScanDeposits(ctx, []string{depositAddr.Hex()}, emptyRegistry, 0, latest)
	if err != nil {
		t.Fatalf("ScanDeposits() error = %v, want nil", err)
	}

	for _, tr := range transfers {
		if tr.TxHash == signedERC20Tx.Hash().Hex() {
			t.Fatalf("unregistered token transfer %+v was returned as an ObservedTransfer — want it classified as unsupported instead", tr)
		}
	}

	var got *core.UnsupportedTokenObservation
	for i := range unsupported {
		if unsupported[i].TxHash == signedERC20Tx.Hash().Hex() {
			got = &unsupported[i]
		}
	}
	if got == nil {
		t.Fatalf("no unsupported-token observation found for the unregistered transfer among %+v", unsupported)
	}
	if !strings.EqualFold(got.ContractAddress, tokenAddr.Hex()) {
		t.Fatalf("unsupported observation contract address = %s, want %s", got.ContractAddress, tokenAddr.Hex())
	}
	if !strings.EqualFold(got.Address, depositAddr.Hex()) {
		t.Fatalf("unsupported observation address = %s, want %s", got.Address, depositAddr.Hex())
	}
	if got.Amount.Cmp(erc20Amount) != 0 {
		t.Fatalf("unsupported observation amount = %s, want %s", got.Amount, erc20Amount)
	}
	if got.LogIndex < 0 {
		t.Fatalf("unsupported observation log index = %d, want a real (non-negative) EVM log index", got.LogIndex)
	}
	if got.Chain != core.Chain("anvil") {
		t.Fatalf("unsupported observation chain = %q, want %q", got.Chain, "anvil")
	}
}

// TestScanner_RealAnvil_BlockHash_ReflectsReorg proves Story 2.4's core detection
// mechanism against a real chain, not just fakes (AC3): a real anvil_reorg call replaces
// the canonical block at a given height with a different one, and BlockHash's return
// value for that same height changes to match — exactly the signal TrackDeposits.Execute's
// reorg-check phase relies on to orphan a deposit. It also proves BlockHash's other
// contractual case over real RPC: a height beyond the chain's current head reports
// exists=false, never an error (the "chain got shorter" case, Design Notes' I/O matrix).
func TestScanner_RealAnvil_BlockHash_ReflectsReorg(t *testing.T) {
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
	waitForAnvilReady(t, rpcURL)

	ctx := context.Background()
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatalf("dial anvil: %v", err)
	}
	defer client.Close()

	// Mine several empty blocks via anvil_mine (a raw RPC call — no real transaction
	// needed to advance the chain) so there is real history for anvil_reorg to replace.
	const minedBlocks = 5
	for i := 0; i < minedBlocks; i++ {
		var result any
		// anvil_mine requires its params to be present as an empty JSON array, not
		// omitted/null (confirmed empirically: a bare CallContext with no variadic args
		// sends "params":null per go-ethereum's rpc.Client.newMessage, which anvil's
		// EthRequest::Mine deserializer rejects with "invalid type: null, expected tuple
		// variant") — spreading an explicit empty []any keeps args non-nil so
		// json.Marshal produces "[]" instead.
		if err := client.Client().CallContext(ctx, &result, "anvil_mine", []any{}...); err != nil {
			t.Fatalf("anvil_mine (block %d): %v", i, err)
		}
	}

	chain := Chain{Name: "anvil", RPCURL: rpcURL, ChainID: anvilChainID}
	scanner, err := NewScanner(ctx, chain)
	if err != nil {
		t.Fatalf("NewScanner() error = %v, want nil", err)
	}
	defer scanner.Close()

	latest, _, _, err := scanner.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v, want nil", err)
	}
	if latest < minedBlocks {
		t.Fatalf("latest = %d, want >= %d after mining", latest, minedBlocks)
	}

	// targetHeight is within the last 3 blocks — anvil_reorg(3, ...) below replaces
	// exactly the blocks at [latest-2, latest].
	targetHeight := latest - 1

	beforeHash, exists, err := scanner.BlockHash(ctx, targetHeight)
	if err != nil {
		t.Fatalf("BlockHash(%d) before reorg: error = %v, want nil", targetHeight, err)
	}
	if !exists {
		t.Fatalf("BlockHash(%d) before reorg: exists = false, want true", targetHeight)
	}

	if err := anvilReorg(ctx, client, 3); err != nil {
		t.Fatalf("anvil_reorg: %v", err)
	}

	latestAfter, _, _, err := scanner.Head(ctx)
	if err != nil {
		t.Fatalf("Head() after reorg: error = %v, want nil", err)
	}
	if latestAfter != latest {
		t.Fatalf("latest after reorg = %d, want unchanged %d (a reorg replaces history, it doesn't shorten or extend it)", latestAfter, latest)
	}

	afterHash, exists, err := scanner.BlockHash(ctx, targetHeight)
	if err != nil {
		t.Fatalf("BlockHash(%d) after reorg: error = %v, want nil", targetHeight, err)
	}
	if !exists {
		t.Fatalf("BlockHash(%d) after reorg: exists = false, want true (the height still exists, just with a different block)", targetHeight)
	}
	if afterHash == beforeHash {
		t.Fatalf("BlockHash(%d) after reorg = %s, want different from the pre-reorg hash %s (anvil_reorg must have replaced this block)", targetHeight, afterHash, beforeHash)
	}

	// A height beyond the chain's current head reports exists=false, never an error —
	// confirmed here over real RPC (go-ethereum's ethclient.HeaderByNumber surfaces this
	// as the ethereum.NotFound sentinel when eth_getBlockByNumber answers null).
	_, exists, err = scanner.BlockHash(ctx, latestAfter+1000)
	if err != nil {
		t.Fatalf("BlockHash(%d) beyond chain head: error = %v, want nil", latestAfter+1000, err)
	}
	if exists {
		t.Fatalf("BlockHash(%d) beyond chain head: exists = true, want false", latestAfter+1000)
	}
}

// anvilReorg forces a real reorg via the anvil_reorg RPC method: it re-mines the last
// depth blocks on top of the (unchanged) chain head, producing a NEW canonical history for
// those blocks — a different set of block hashes at the same heights, height count
// unchanged. The exact param shape (a 2-element ReorgOptions struct: depth, then an array
// of transaction/block-number pairs to replay into the new blocks) was confirmed
// empirically against this environment's installed anvil (1.7.1) via `cast rpc
// anvil_reorg` before writing this — passing zero params, or a single param, both errored
// with "invalid length N, expected struct ReorgOptions with 2 elements", and a bare
// `anvil_reorg(depth, [])` reliably changed the block hash at a height within depth of the
// tip while leaving the chain's height unchanged. An empty second element (no specific
// transactions to replay into the reorged blocks) is all this test needs — anvil fills the
// reorged blocks itself.
func anvilReorg(ctx context.Context, client *ethclient.Client, depth int) error {
	var result any
	return client.Client().CallContext(ctx, &result, "anvil_reorg", depth, []any{})
}

// waitForAnvilReady polls rpcURL until it accepts connections and answers a real
// eth_blockNumber call, tracking whichever error (dial or call) happened last — mirrors
// deployer_test.go's TestVerifyDeployerPresence_RealAnvil readiness gate.
func waitForAnvilReady(t *testing.T, rpcURL string) {
	t.Helper()
	ctx := context.Background()

	var lastErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		client, dialErr := ethclient.DialContext(ctx, rpcURL)
		if dialErr != nil {
			lastErr = dialErr
		} else {
			_, blockErr := client.BlockNumber(ctx)
			client.Close()
			if blockErr == nil {
				return
			}
			lastErr = blockErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("anvil did not become ready: %v", lastErr)
}

// waitForReceipt polls for tx's receipt, bounding the wait so a stuck transaction fails
// the test loudly instead of hanging it.
func waitForReceipt(ctx context.Context, client *ethclient.Client, txHash common.Hash) (*types.Receipt, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := client.TransactionReceipt(ctx, txHash)
		if err == nil {
			return receipt, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for receipt of %s", txHash.Hex())
}

// fakeRawBlockClient is a minimal scanClient fake that returns a fixed raw JSON payload
// for eth_getBlockByNumber, regardless of the requested block number — just enough to
// prove scanNativeTransfers tolerates a transaction type go-ethereum's own decoder would
// reject (production bug fix, 2026-07-17: see scanClient's doc comment). Every other
// scanClient method is unused by that test and panics if called, so a test that
// accidentally relies on them fails loudly instead of silently returning zero values.
type fakeRawBlockClient struct {
	blockJSON []byte
}

func (f *fakeRawBlockClient) BlockNumber(ctx context.Context) (uint64, error) {
	panic("not used by this test")
}

func (f *fakeRawBlockClient) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	panic("not used by this test")
}

func (f *fakeRawBlockClient) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	panic("not used by this test")
}

func (f *fakeRawBlockClient) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	if method != "eth_getBlockByNumber" {
		return fmt.Errorf("unexpected method %q", method)
	}
	return json.Unmarshal(f.blockJSON, result)
}

func (f *fakeRawBlockClient) Close() {}

// TestScanNativeTransfers_TolerantOfUnrecognizedTransactionType proves the production bug
// fix (2026-07-17): a block containing a transaction whose "type" byte go-ethereum's own
// decoder doesn't recognize — e.g. Arbitrum's 0x64-0x6a deposit/internal transaction types
// (an ArbitrumInternalTx is the first transaction of nearly every Nitro block) or
// Base/OP-stack's 0x7e deposit transaction type (emitted whenever a user bridges from L1)
// — no longer fails the whole scan. Before the fix, ethclient.BlockByNumber's typed
// transaction decoding rejected any unrecognized type outright ("transaction type not
// supported"), breaking native-ETH scanning on real chains entirely. This can't be
// exercised against real anvil, since anvil never emits these chain-specific transaction
// types — hence a fake at the raw-JSON level.
func TestScanNativeTransfers_TolerantOfUnrecognizedTransactionType(t *testing.T) {
	t.Parallel()

	depositAddress := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")

	// Two transactions to the same known address: one with an unrecognized type byte
	// (0x64, Arbitrum's ArbitrumDepositTx), one with a standard type (0x2, EIP-1559) —
	// both must be decoded and returned as transfers, proving the unrecognized-type
	// transaction's own data (not just its presence) is handled correctly, not merely
	// tolerated without a crash.
	blockJSON := fmt.Sprintf(`{
		"hash": "0x%s",
		"transactions": [
			{"hash": "0x%s", "type": "0x64", "to": "%s", "value": "0x1"},
			{"hash": "0x%s", "type": "0x2",  "to": "%s", "value": "0xde0b6b3a7640000"}
		]
	}`,
		strings.Repeat("aa", 32),
		strings.Repeat("11", 32), depositAddress.Hex(),
		strings.Repeat("22", 32), depositAddress.Hex(),
	)

	scanner := &Scanner{
		chain:  Chain{Name: "arbitrum"},
		client: &fakeRawBlockClient{blockJSON: []byte(blockJSON)},
	}

	known := map[common.Address]string{depositAddress: depositAddress.Hex()}
	transfers, err := scanner.scanNativeTransfers(context.Background(), known, 1, 1)
	if err != nil {
		t.Fatalf("scanNativeTransfers() error = %v, want nil — an unrecognized transaction type must not fail the scan", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("transfers = %+v, want exactly 2 (both the unrecognized-type and the standard-type transaction to the known address)", transfers)
	}
	byAmount := map[string]bool{}
	for _, tr := range transfers {
		if tr.Address != depositAddress.Hex() {
			t.Fatalf("transfer address = %q, want %q", tr.Address, depositAddress.Hex())
		}
		byAmount[tr.Amount.String()] = true
	}
	if !byAmount["1"] || !byAmount["1000000000000000000"] {
		t.Fatalf("transfer amounts = %v, want both \"1\" (from the unrecognized-type tx) and \"1000000000000000000\" (from the standard tx)", byAmount)
	}
}
