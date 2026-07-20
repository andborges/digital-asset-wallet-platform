package evm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
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

// forwarderCreationCodePrefixHex / forwarderCreationCodeSuffixHex are raw, hand-assembled
// EVM bytecode (no solc involved — contracts/ has no throwaway "forwards to an arbitrary
// runtime address" fixture, and pulling one into the pinned, bytecode-sacred contracts/
// project just for this test would be the wrong place for it per contracts/README.md) that
// sandwich a 20-byte address argument: a minimal contract whose entire runtime logic is
// "forward my full msg.value to <address>, unconditionally, on any call" — exactly the
// "receives ETH and immediately forwards it via a low-level call to a target address" shape
// TestScanner_RealAnvil_FindsInternalTransferViaTrace needs to produce a genuine nested CALL
// trace frame.
//
// Disassembly (verified opcode-by-opcode by hand, since nothing compiled this):
//
//	Creation code (runs once, at deploy time):
//	  6022        PUSH1 0x22        ; runtime code length (34 bytes)
//	  80          DUP1
//	  600b        PUSH1 0x0b        ; runtime code's offset within this creation code (11)
//	  6000        PUSH1 0x00        ; destination offset in memory
//	  39          CODECOPY          ; copy the runtime code (below) into memory
//	  6000        PUSH1 0x00        ; return offset
//	  f3          RETURN            ; return the copied bytes as this contract's runtime code
//	Runtime code (34 bytes total, executed on every call to the deployed contract):
//	  6000 6000 6000 6000            ; retSize=0, retOffset=0, argsSize=0, argsOffset=0
//	  34          CALLVALUE          ; push msg.value — the ETH this contract was just sent
//	  73<addr>    PUSH20 <address>   ; the destination, baked directly into the bytecode
//	  5a          GAS                ; forward all remaining gas
//	  f1          CALL               ; CALL(gas, addr, value, 0, 0, 0, 0)
//	  50          POP                ; discard the call's success/failure return value
//	  00          STOP
const forwarderCreationCodePrefixHex = "602280600b6000396000f360006000600060003473"
const forwarderCreationCodeSuffixHex = "5af15000"

// forwarderCreationCode returns the creation bytecode for a throwaway low-level-call
// forwarder targeting to — baked directly into the bytecode (raw bytes, no ABI encoding),
// the same "append bytes the constructor consumes" trick testERC20CreationBytecode's caller
// uses for its uint256 constructor argument above, just simpler since a raw 20-byte address
// needs no padding.
func forwarderCreationCode(to common.Address) []byte {
	prefix, err := hex.DecodeString(forwarderCreationCodePrefixHex)
	if err != nil {
		panic(err)
	}
	suffix, err := hex.DecodeString(forwarderCreationCodeSuffixHex)
	if err != nil {
		panic(err)
	}
	code := make([]byte, 0, len(prefix)+common.AddressLength+len(suffix))
	code = append(code, prefix...)
	code = append(code, to.Bytes()...)
	code = append(code, suffix...)
	return code
}

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
	scanner, err := NewScanner(ctx, chain, slog.New(slog.DiscardHandler))
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
	scanner, err := NewScanner(ctx, chain, slog.New(slog.DiscardHandler))
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
	scanner, err := NewScanner(ctx, chain, slog.New(slog.DiscardHandler))
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

// TestScanner_RealAnvil_FindsInternalTransferViaTrace proves this spec's core claim against
// a real chain, not just fakes: ETH that reaches a known deposit address only via an
// internal CALL — value moved by a contract's own code, never appearing as a top-level
// tx.To()/tx.Value() — is found by the new debug_traceBlockByNumber pass even though the
// top-level scan above it cannot see it. This also empirically confirms recent Foundry
// anvil supports debug_traceBlockByNumber with callTracer out of the box (Intent/Design
// Notes): if it didn't, scanInternalTransfers' graceful-degradation path would silently
// swallow the failure and this test's assertion on transfers would fail loudly instead of
// erroring out.
func TestScanner_RealAnvil_FindsInternalTransferViaTrace(t *testing.T) {
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

	// ultimateRecipient is the ONLY address passed as "known" to ScanDeposits below — the
	// forwarder contract's own address is deliberately excluded. A plain top-level ETH send
	// lands on the forwarder contract, not on ultimateRecipient, so the top-level
	// tx.To()/tx.Value() scan must find nothing for this tx; only the new trace-based pass
	// can attribute the transfer to ultimateRecipient.
	ultimateRecipient := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")

	nonce, err := client.PendingNonceAt(ctx, deployer)
	if err != nil {
		t.Fatalf("get deployer nonce: %v", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("suggest gas price: %v", err)
	}

	deployTx := types.NewContractCreation(nonce, big.NewInt(0), 500_000, gasPrice, forwarderCreationCode(ultimateRecipient))
	signedDeployTx, err := types.SignTx(deployTx, signer, privKey)
	if err != nil {
		t.Fatalf("sign forwarder deploy tx: %v", err)
	}
	if err := client.SendTransaction(ctx, signedDeployTx); err != nil {
		t.Fatalf("send forwarder deploy tx: %v", err)
	}
	deployReceipt, err := waitForReceipt(ctx, client, signedDeployTx.Hash())
	if err != nil {
		t.Fatalf("wait for forwarder deploy receipt: %v", err)
	}
	if deployReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("forwarder deployment failed (status %d)", deployReceipt.Status)
	}
	forwarderAddr := deployReceipt.ContractAddress

	// Plain top-level ETH send to the forwarder contract — this is what triggers its
	// runtime code, which immediately re-sends the full value onward to ultimateRecipient
	// via a low-level CALL. The top-level scan sees to=forwarderAddr (not known); only the
	// trace pass can see the nested to=ultimateRecipient CALL.
	nonce++
	sentAmount := big.NewInt(3_000_000_000_000_000) // 0.003 ETH in wei
	sendTx := types.NewTransaction(nonce, forwarderAddr, sentAmount, 100_000, gasPrice, nil)
	signedSendTx, err := types.SignTx(sendTx, signer, privKey)
	if err != nil {
		t.Fatalf("sign send-to-forwarder tx: %v", err)
	}
	if err := client.SendTransaction(ctx, signedSendTx); err != nil {
		t.Fatalf("send send-to-forwarder tx: %v", err)
	}
	sendReceipt, err := waitForReceipt(ctx, client, signedSendTx.Hash())
	if err != nil {
		t.Fatalf("wait for send-to-forwarder receipt: %v", err)
	}
	if sendReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("send to forwarder failed (status %d) — the forwarder's low-level call to ultimateRecipient must have reverted", sendReceipt.Status)
	}

	chain := Chain{Name: "anvil", RPCURL: rpcURL, ChainID: anvilChainID}
	scanner, err := NewScanner(ctx, chain, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewScanner() error = %v, want nil", err)
	}
	defer scanner.Close()

	latest, _, _, err := scanner.Head(ctx)
	if err != nil {
		t.Fatalf("Head() error = %v, want nil", err)
	}

	transfers, unsupported, err := scanner.ScanDeposits(ctx, []string{ultimateRecipient.Hex()}, map[string]core.Asset{}, 0, latest)
	if err != nil {
		t.Fatalf("ScanDeposits() error = %v, want nil", err)
	}
	if len(unsupported) != 0 {
		t.Fatalf("unsupported observations = %+v, want none", unsupported)
	}

	var got *core.ObservedTransfer
	for i := range transfers {
		if transfers[i].TxHash == signedSendTx.Hash().Hex() {
			got = &transfers[i]
		}
	}
	if got == nil {
		t.Fatalf("no internal transfer observed for tx %s among %+v — anvil's debug_traceBlockByNumber may not behave as expected, or scanInternalTransfers has a bug", signedSendTx.Hash().Hex(), transfers)
	}
	if got.Asset != core.AssetETH {
		t.Fatalf("internal transfer asset = %q, want %q", got.Asset, core.AssetETH)
	}
	if got.Amount.Cmp(sentAmount) != 0 {
		t.Fatalf("internal transfer amount = %s, want %s", got.Amount, sentAmount)
	}
	if !strings.EqualFold(got.Address, ultimateRecipient.Hex()) {
		t.Fatalf("internal transfer address = %s, want %s", got.Address, ultimateRecipient.Hex())
	}
	if got.LogIndex >= -1 {
		t.Fatalf("internal transfer log index = %d, want <= -2 (synthetic, never colliding with -1 or a real ERC-20 log index)", got.LogIndex)
	}
	if got.Chain != core.Chain("anvil") {
		t.Fatalf("internal transfer chain = %q, want %q", got.Chain, "anvil")
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

// bigHex builds a *hexutil.Big fixture value inline — every fake-trace test below
// constructs rawBlock/callFrame fixtures as Go values (marshaled to JSON via
// json.Marshal), never as hand-written JSON text, so a fixture's shape is checked by the
// compiler against the exact same types scanInternalTransfers itself decodes into.
func bigHex(v int64) *hexutil.Big {
	b := hexutil.Big(*big.NewInt(v))
	return &b
}

// fakeTraceClient is a scanClient fake that serves canned eth_getBlockByNumber and
// debug_traceBlockByNumber responses — the call-tree analogue of fakeRawBlockClient above,
// extended to also exercise scanInternalTransfers (traceJSON/traceErr) and, for
// TestScanDeposits_TraceFailure_StillReturnsTopLevelAndERC20, the full ScanDeposits path
// end-to-end (erc20Logs/erc20Err via FilterLogs). traceCalls counts how many times
// debug_traceBlockByNumber was actually invoked, proving the sticky disabled flag really
// does stop it being retried on a later poll cycle, not just that the test's own assertions
// happen to still pass.
type fakeTraceClient struct {
	blockJSON []byte

	traceJSON []byte
	traceErr  error

	erc20Logs []types.Log
	erc20Err  error

	mu         sync.Mutex
	traceCalls int
}

func (f *fakeTraceClient) BlockNumber(ctx context.Context) (uint64, error) {
	panic("not used by this test")
}

func (f *fakeTraceClient) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	panic("not used by this test")
}

func (f *fakeTraceClient) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	return f.erc20Logs, f.erc20Err
}

func (f *fakeTraceClient) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	switch method {
	case "eth_getBlockByNumber":
		return json.Unmarshal(f.blockJSON, result)
	case "debug_traceBlockByNumber":
		f.mu.Lock()
		f.traceCalls++
		f.mu.Unlock()
		if f.traceErr != nil {
			return f.traceErr
		}
		return json.Unmarshal(f.traceJSON, result)
	default:
		return fmt.Errorf("unexpected method %q", method)
	}
}

func (f *fakeTraceClient) Close() {}

// warnCounter is a minimal slog.Handler that counts Warn-level records — just enough to
// assert scanInternalTransfers' "exactly one warning, ever, per Scanner instance" contract
// (Boundaries & Constraints, AC2) without depending on any particular log message text.
type warnCounter struct {
	mu    sync.Mutex
	warns int
}

func newWarnCountingLogger() (*slog.Logger, *warnCounter) {
	c := &warnCounter{}
	return slog.New(c), c
}

func (c *warnCounter) Enabled(context.Context, slog.Level) bool { return true }

func (c *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		c.mu.Lock()
		c.warns++
		c.mu.Unlock()
	}
	return nil
}

func (c *warnCounter) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *warnCounter) WithGroup(_ string) slog.Handler      { return c }

func (c *warnCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.warns
}

// TestScanNativeTransfers_InternalCallDetection is the fake-client counterpart to
// TestScanner_RealAnvil_FindsInternalTransferViaTrace, covering the I/O & Edge-Case
// Matrix's per-frame rules precisely, in one block with two transactions:
//   - tx1 is a plain top-level transfer to knownA. Its trace root frame ALSO reports
//     to=knownA/value>0 (a real root-frame value transfer) but has no nested calls at all —
//     proving the trace pass produces NOTHING extra for it (root frame skipped entirely,
//     matrix row 2): if scanInternalTransfers ever inspected the root frame itself instead
//     of only its Calls, this would silently double-count tx1.
//   - tx2's top-level tx.To is an unrelated contract with value=0 (never itself a
//     transfer), but its trace root has three child frames: a STATICCALL reporting
//     to=knownA/value>0 (matrix row 4 — skipped, STATICCALL never really carries value),
//     a zero-value CALL to knownA (matrix row 3 — skipped), and a genuine CALL to knownB
//     with value>0 (matrix row 1 — the only one that should produce an ObservedTransfer).
func TestScanNativeTransfers_InternalCallDetection(t *testing.T) {
	t.Parallel()

	knownA := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")
	knownB := common.HexToAddress("0xBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBb")
	contractX := common.HexToAddress("0xCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCc")
	dev := common.HexToAddress("0xDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDd")

	blockHash := common.HexToHash("0x" + strings.Repeat("aa", 32))
	tx1Hash := common.HexToHash("0x" + strings.Repeat("11", 32))
	tx2Hash := common.HexToHash("0x" + strings.Repeat("22", 32))

	block := rawBlock{
		Hash: blockHash,
		Transactions: []rawBlockTransaction{
			{Hash: tx1Hash, To: &knownA, Value: bigHex(100)},
			{Hash: tx2Hash, To: &contractX, Value: bigHex(0)},
		},
	}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal fixture block: %v", err)
	}

	traces := []txCallTrace{
		{
			TxHash: tx1Hash,
			Result: callFrame{Type: "CALL", From: dev, To: &knownA, Value: bigHex(100)},
		},
		{
			TxHash: tx2Hash,
			Result: callFrame{
				Type: "CALL", From: dev, To: &contractX, Value: bigHex(0),
				Calls: []callFrame{
					{Type: "STATICCALL", From: contractX, To: &knownA, Value: bigHex(100)},
					{Type: "CALL", From: contractX, To: &knownA, Value: bigHex(0)},
					{Type: "CALL", From: contractX, To: &knownB, Value: bigHex(42)},
				},
			},
		},
	}
	traceJSON, err := json.Marshal(traces)
	if err != nil {
		t.Fatalf("marshal fixture traces: %v", err)
	}

	scanner := &Scanner{
		chain:  Chain{Name: "faketrace"},
		client: &fakeTraceClient{blockJSON: blockJSON, traceJSON: traceJSON},
		logger: slog.New(slog.DiscardHandler),
	}

	known := map[common.Address]string{knownA: knownA.Hex(), knownB: knownB.Hex()}
	transfers, err := scanner.scanNativeTransfers(context.Background(), known, 1, 1)
	if err != nil {
		t.Fatalf("scanNativeTransfers() error = %v, want nil", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("transfers = %+v, want exactly 2 (tx1's top-level transfer + tx2's one matching nested CALL — no double-count of tx1's root, no false positive from the STATICCALL or zero-value frames)", transfers)
	}

	var gotTopLevel, gotInternal *core.ObservedTransfer
	for i := range transfers {
		switch transfers[i].TxHash {
		case tx1Hash.Hex():
			gotTopLevel = &transfers[i]
		case tx2Hash.Hex():
			gotInternal = &transfers[i]
		}
	}

	if gotTopLevel == nil {
		t.Fatalf("no top-level transfer found for tx1 among %+v", transfers)
	}
	if gotTopLevel.LogIndex != nativeTransferLogIndex {
		t.Fatalf("tx1 log index = %d, want %d (top-level sentinel, unaffected by the internal-transfer pass)", gotTopLevel.LogIndex, nativeTransferLogIndex)
	}
	if gotTopLevel.Amount.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("tx1 amount = %s, want 100", gotTopLevel.Amount)
	}

	if gotInternal == nil {
		t.Fatalf("no internal transfer found for tx2 among %+v — the nested matching CALL frame to knownB was not detected", transfers)
	}
	if gotInternal.Address != knownB.Hex() {
		t.Fatalf("tx2 internal transfer address = %s, want %s", gotInternal.Address, knownB.Hex())
	}
	if gotInternal.Amount.Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("tx2 internal transfer amount = %s, want 42", gotInternal.Amount)
	}
	const wantLogIndex = -2 - 2 // dfsIndex 2: STATICCALL(0, skipped), zero-value CALL(1, skipped), matching CALL(2)
	if gotInternal.LogIndex != wantLogIndex {
		t.Fatalf("tx2 internal transfer log index = %d, want %d (synthetic -2-dfsIndex, never colliding with -1 or a real ERC-20 log index)", gotInternal.LogIndex, wantLogIndex)
	}
	if gotInternal.Chain != core.Chain("faketrace") {
		t.Fatalf("tx2 internal transfer chain = %q, want %q", gotInternal.Chain, "faketrace")
	}
	if gotInternal.BlockHash != blockHash.Hex() {
		t.Fatalf("tx2 internal transfer block hash = %s, want %s (from the same already-fetched block, no second RPC round-trip)", gotInternal.BlockHash, blockHash.Hex())
	}
}

// TestScanNativeTransfers_TwoInternalTransfersInSameTx_DistinctLogIndex covers the I/O &
// Edge-Case Matrix's last row directly: one transaction whose trace has two separate
// matching nested CALL frames (siblings, to two different known addresses) must produce two
// separate ObservedTransfers, each with its own distinct synthetic LogIndex — never
// conflated into one, never sharing an index.
func TestScanNativeTransfers_TwoInternalTransfersInSameTx_DistinctLogIndex(t *testing.T) {
	t.Parallel()

	knownA := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")
	knownB := common.HexToAddress("0xBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBb")
	contractX := common.HexToAddress("0xCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCc")
	dev := common.HexToAddress("0xDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDd")

	blockHash := common.HexToHash("0x" + strings.Repeat("bb", 32))
	txHash := common.HexToHash("0x" + strings.Repeat("33", 32))

	block := rawBlock{
		Hash: blockHash,
		Transactions: []rawBlockTransaction{
			{Hash: txHash, To: &contractX, Value: bigHex(0)},
		},
	}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal fixture block: %v", err)
	}

	traces := []txCallTrace{
		{
			TxHash: txHash,
			Result: callFrame{
				Type: "CALL", From: dev, To: &contractX, Value: bigHex(0),
				Calls: []callFrame{
					{Type: "CALL", From: contractX, To: &knownA, Value: bigHex(10)},
					{Type: "CALL", From: contractX, To: &knownB, Value: bigHex(20)},
				},
			},
		},
	}
	traceJSON, err := json.Marshal(traces)
	if err != nil {
		t.Fatalf("marshal fixture traces: %v", err)
	}

	scanner := &Scanner{
		chain:  Chain{Name: "faketrace"},
		client: &fakeTraceClient{blockJSON: blockJSON, traceJSON: traceJSON},
		logger: slog.New(slog.DiscardHandler),
	}

	known := map[common.Address]string{knownA: knownA.Hex(), knownB: knownB.Hex()}
	transfers, err := scanner.scanNativeTransfers(context.Background(), known, 1, 1)
	if err != nil {
		t.Fatalf("scanNativeTransfers() error = %v, want nil", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("transfers = %+v, want exactly 2 (two separate internal transfers in the same tx)", transfers)
	}

	amountByAddress := map[string]int64{}
	logIndexes := map[int]bool{}
	for _, tr := range transfers {
		if tr.TxHash != txHash.Hex() {
			t.Fatalf("transfer %+v has unexpected tx hash, want %s", tr, txHash.Hex())
		}
		amountByAddress[tr.Address] = tr.Amount.Int64()
		logIndexes[tr.LogIndex] = true
	}
	if amountByAddress[knownA.Hex()] != 10 {
		t.Fatalf("knownA amount = %d, want 10 (all transfers: %+v)", amountByAddress[knownA.Hex()], transfers)
	}
	if amountByAddress[knownB.Hex()] != 20 {
		t.Fatalf("knownB amount = %d, want 20 (all transfers: %+v)", amountByAddress[knownB.Hex()], transfers)
	}
	if len(logIndexes) != 2 {
		t.Fatalf("distinct log indexes = %d, want 2 (each internal transfer must get its own synthetic LogIndex): %+v", len(logIndexes), transfers)
	}
	for li := range logIndexes {
		if li >= -1 {
			t.Fatalf("log index %d, want <= -2 (synthetic, never colliding with -1 or a real ERC-20 log index)", li)
		}
	}
}

// TestScanNativeTransfers_TopLevelAndInternalTransferInSameTx_BothRecorded proves the third
// Acceptance Criterion directly: a single transaction that is itself a top-level transfer
// to a known address (tx.To=knownA, value>0) AND, within that same execution, makes a
// further internal CALL to a second known address (e.g. a contract that both keeps part of
// what it receives and relays the rest onward) must produce TWO separate ObservedTransfers
// — the existing top-level scan's entry (LogIndex -1) and the new internal-transfer pass's
// entry (LogIndex <= -2) — never conflated into one, never dropping either.
func TestScanNativeTransfers_TopLevelAndInternalTransferInSameTx_BothRecorded(t *testing.T) {
	t.Parallel()

	knownA := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")
	knownB := common.HexToAddress("0xBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBb")
	dev := common.HexToAddress("0xDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDd")

	blockHash := common.HexToHash("0x" + strings.Repeat("ee", 32))
	txHash := common.HexToHash("0x" + strings.Repeat("66", 32))

	block := rawBlock{
		Hash: blockHash,
		Transactions: []rawBlockTransaction{
			// A top-level transfer straight to knownA — found by the pre-existing
			// tx.To()/tx.Value() scan above, entirely independently of the trace pass.
			{Hash: txHash, To: &knownA, Value: bigHex(100)},
		},
	}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal fixture block: %v", err)
	}

	traces := []txCallTrace{
		{
			TxHash: txHash,
			// The root frame mirrors the same top-level transfer (skipped entirely by the
			// trace pass, per matrix row 2) but ALSO relays part of what it received
			// onward, via one nested CALL to a second known address — a genuinely separate
			// transfer the top-level scan can never see.
			Result: callFrame{
				Type: "CALL", From: dev, To: &knownA, Value: bigHex(100),
				Calls: []callFrame{
					{Type: "CALL", From: knownA, To: &knownB, Value: bigHex(30)},
				},
			},
		},
	}
	traceJSON, err := json.Marshal(traces)
	if err != nil {
		t.Fatalf("marshal fixture traces: %v", err)
	}

	scanner := &Scanner{
		chain:  Chain{Name: "faketrace"},
		client: &fakeTraceClient{blockJSON: blockJSON, traceJSON: traceJSON},
		logger: slog.New(slog.DiscardHandler),
	}

	known := map[common.Address]string{knownA: knownA.Hex(), knownB: knownB.Hex()}
	transfers, err := scanner.scanNativeTransfers(context.Background(), known, 1, 1)
	if err != nil {
		t.Fatalf("scanNativeTransfers() error = %v, want nil", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("transfers = %+v, want exactly 2 (the tx's own top-level transfer to knownA, plus its separate internal transfer to knownB — neither conflated nor dropped)", transfers)
	}

	var gotTopLevel, gotInternal *core.ObservedTransfer
	for i := range transfers {
		switch {
		case transfers[i].Address == knownA.Hex():
			gotTopLevel = &transfers[i]
		case transfers[i].Address == knownB.Hex():
			gotInternal = &transfers[i]
		}
	}

	if gotTopLevel == nil {
		t.Fatalf("no transfer to knownA (the top-level transfer) found among %+v", transfers)
	}
	if gotTopLevel.TxHash != txHash.Hex() {
		t.Fatalf("top-level transfer tx hash = %s, want %s", gotTopLevel.TxHash, txHash.Hex())
	}
	if gotTopLevel.LogIndex != nativeTransferLogIndex {
		t.Fatalf("top-level transfer log index = %d, want %d", gotTopLevel.LogIndex, nativeTransferLogIndex)
	}
	if gotTopLevel.Amount.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("top-level transfer amount = %s, want 100", gotTopLevel.Amount)
	}

	if gotInternal == nil {
		t.Fatalf("no transfer to knownB (the internal transfer) found among %+v", transfers)
	}
	if gotInternal.TxHash != txHash.Hex() {
		t.Fatalf("internal transfer tx hash = %s, want %s (same tx as the top-level transfer)", gotInternal.TxHash, txHash.Hex())
	}
	if gotInternal.Amount.Cmp(big.NewInt(30)) != 0 {
		t.Fatalf("internal transfer amount = %s, want 30", gotInternal.Amount)
	}
	if gotInternal.LogIndex >= -1 {
		t.Fatalf("internal transfer log index = %d, want <= -2 (must never collide with the top-level sentinel -1)", gotInternal.LogIndex)
	}
}

// TestScanDeposits_TraceFailure_StillReturnsTopLevelAndERC20 proves the graceful-degradation
// contract end-to-end, through the full ScanDeposits path (not just scanNativeTransfers in
// isolation, unlike the two tests above): when debug_traceBlockByNumber fails — e.g.
// Arbitrum's configured ZAN endpoint not supporting it — ScanDeposits must still return
// both the top-level native transfer and the ERC-20 transfer normally, with no error, and
// this must hold across MULTIPLE poll cycles on the same Scanner instance while the trace
// RPC is called, and Warn logged, exactly ONCE total — never once per poll cycle (AC2).
func TestScanDeposits_TraceFailure_StillReturnsTopLevelAndERC20(t *testing.T) {
	t.Parallel()

	knownAddr := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")
	tokenAddr := common.HexToAddress("0xEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEe")
	fromAddr := common.HexToAddress("0xFfFfFfFfFfFfFfFfFfFfFfFfFfFfFfFfFfFfFfFf")

	blockHash := common.HexToHash("0x" + strings.Repeat("cc", 32))
	nativeTxHash := common.HexToHash("0x" + strings.Repeat("44", 32))
	erc20TxHash := common.HexToHash("0x" + strings.Repeat("55", 32))

	const blockNum = 7
	nativeAmount := big.NewInt(1234)
	block := rawBlock{
		Hash: blockHash,
		Transactions: []rawBlockTransaction{
			{Hash: nativeTxHash, To: &knownAddr, Value: bigHex(nativeAmount.Int64())},
		},
	}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal fixture block: %v", err)
	}

	erc20Amount := big.NewInt(555)
	erc20Log := types.Log{
		Address:     tokenAddr,
		Topics:      []common.Hash{erc20TransferSignature, common.BytesToHash(fromAddr.Bytes()), common.BytesToHash(knownAddr.Bytes())},
		Data:        common.LeftPadBytes(erc20Amount.Bytes(), 32),
		BlockNumber: blockNum,
		TxHash:      erc20TxHash,
		BlockHash:   blockHash,
		Index:       3,
	}

	warnLogger, warns := newWarnCountingLogger()
	fake := &fakeTraceClient{
		blockJSON: blockJSON,
		traceErr:  errors.New("debug_traceBlockByNumber method not found"),
		erc20Logs: []types.Log{erc20Log},
	}
	scanner := &Scanner{chain: Chain{Name: "faketrace"}, client: fake, logger: warnLogger}

	tokenRegistry := map[string]core.Asset{strings.ToLower(tokenAddr.Hex()): core.AssetUSDC}

	for pollCycle := 1; pollCycle <= 2; pollCycle++ {
		transfers, unsupported, err := scanner.ScanDeposits(context.Background(), []string{knownAddr.Hex()}, tokenRegistry, blockNum, blockNum)
		if err != nil {
			t.Fatalf("poll %d: ScanDeposits() error = %v, want nil (a trace failure must never propagate as a poll error)", pollCycle, err)
		}
		if len(unsupported) != 0 {
			t.Fatalf("poll %d: unsupported = %+v, want none", pollCycle, unsupported)
		}

		var gotNative, gotERC20 *core.ObservedTransfer
		for i := range transfers {
			switch transfers[i].TxHash {
			case nativeTxHash.Hex():
				gotNative = &transfers[i]
			case erc20TxHash.Hex():
				gotERC20 = &transfers[i]
			}
		}
		if gotNative == nil {
			t.Fatalf("poll %d: no native transfer found among %+v — top-level detection must be unaffected by the trace failure", pollCycle, transfers)
		}
		if gotNative.LogIndex != nativeTransferLogIndex {
			t.Fatalf("poll %d: native log index = %d, want %d", pollCycle, gotNative.LogIndex, nativeTransferLogIndex)
		}
		if gotNative.Amount.Cmp(nativeAmount) != 0 {
			t.Fatalf("poll %d: native amount = %s, want %s", pollCycle, gotNative.Amount, nativeAmount)
		}
		if gotERC20 == nil {
			t.Fatalf("poll %d: no ERC-20 transfer found among %+v — ERC-20 detection must be unaffected by the trace failure", pollCycle, transfers)
		}
		if gotERC20.Asset != core.AssetUSDC {
			t.Fatalf("poll %d: ERC-20 asset = %q, want %q", pollCycle, gotERC20.Asset, core.AssetUSDC)
		}
		if gotERC20.Amount.Cmp(erc20Amount) != 0 {
			t.Fatalf("poll %d: ERC-20 amount = %s, want %s", pollCycle, gotERC20.Amount, erc20Amount)
		}
	}

	if fake.traceCalls != 1 {
		t.Fatalf("debug_traceBlockByNumber call count = %d, want exactly 1 (disabled permanently after its first failure, never retried on the second poll cycle)", fake.traceCalls)
	}
	if got := warns.count(); got != 1 {
		t.Fatalf("warning log count = %d, want exactly 1 (one warning total across both poll cycles, not one per poll cycle)", got)
	}
}

// TestScanNativeTransfers_RevertedInternalCall_NotRecorded covers review loop 1's core
// correctness finding (caught independently by both the adversarial and edge-case review
// passes): a matching CALL frame whose value transfer was rolled back by a revert must
// never be recorded — recording it would credit a customer for ETH that was never actually
// transferred. Two sub-cases in one block: a CALL frame that itself reverted (Error set
// directly on the matching frame), and a CALL frame that did NOT itself revert but is nested
// under an ancestor that did (Error set higher up the tree, not on the matching frame) — the
// EVM rolls back the whole reverted subtree's effects regardless of which exact frame
// "caused" it, so both must be excluded.
func TestScanNativeTransfers_RevertedInternalCall_NotRecorded(t *testing.T) {
	t.Parallel()

	knownA := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")
	knownB := common.HexToAddress("0xBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBb")
	contractX := common.HexToAddress("0xCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCc")
	contractY := common.HexToAddress("0xDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDd")
	dev := common.HexToAddress("0xEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEeEe")

	blockHash := common.HexToHash("0x" + strings.Repeat("dd", 32))
	tx1Hash := common.HexToHash("0x" + strings.Repeat("77", 32)) // matching frame itself reverted
	tx2Hash := common.HexToHash("0x" + strings.Repeat("88", 32)) // ancestor reverted, matching frame didn't

	block := rawBlock{
		Hash: blockHash,
		Transactions: []rawBlockTransaction{
			{Hash: tx1Hash, To: &contractX, Value: bigHex(0)},
			{Hash: tx2Hash, To: &contractY, Value: bigHex(0)},
		},
	}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal fixture block: %v", err)
	}

	traces := []txCallTrace{
		{
			TxHash: tx1Hash,
			Result: callFrame{
				Type: "CALL", From: dev, To: &contractX, Value: bigHex(0),
				Calls: []callFrame{
					// This exact frame reverted — its own Error is set, even though it
					// reports type/to/value as if the transfer happened.
					{Type: "CALL", From: contractX, To: &knownA, Value: bigHex(50), Error: "execution reverted"},
				},
			},
		},
		{
			TxHash: tx2Hash,
			Result: callFrame{
				Type: "CALL", From: dev, To: &contractY, Value: bigHex(0),
				// The ancestor (this frame's direct child) reverted; its own nested CALL to
				// knownB reports no error on itself, but it's inside a rolled-back subtree.
				Calls: []callFrame{
					{
						Type: "CALL", From: contractY, To: &contractX, Value: bigHex(0), Error: "out of gas",
						Calls: []callFrame{
							{Type: "CALL", From: contractX, To: &knownB, Value: bigHex(60)},
						},
					},
				},
			},
		},
	}
	traceJSON, err := json.Marshal(traces)
	if err != nil {
		t.Fatalf("marshal fixture traces: %v", err)
	}

	scanner := &Scanner{
		chain:  Chain{Name: "faketrace"},
		client: &fakeTraceClient{blockJSON: blockJSON, traceJSON: traceJSON},
		logger: slog.New(slog.DiscardHandler),
	}

	known := map[common.Address]string{knownA: knownA.Hex(), knownB: knownB.Hex()}
	transfers, err := scanner.scanNativeTransfers(context.Background(), known, 1, 1)
	if err != nil {
		t.Fatalf("scanNativeTransfers() error = %v, want nil", err)
	}
	if len(transfers) != 0 {
		t.Fatalf("transfers = %+v, want none (both knownA's and knownB's value transfers were rolled back by a revert — recording either would be a phantom deposit)", transfers)
	}
}

// TestScanNativeTransfers_CallCode_NotRecorded proves the review-loop-1 correction to the
// spec's frozen "Never" bullet: CALLCODE never actually moves balance to `to` in the EVM (it
// only executes `to`'s code in the caller's own storage context — go-ethereum's
// EVM.CallCode checks CanTransfer for gas-accounting consistency but never calls Transfer),
// so a CALLCODE frame reporting a nonzero "value" to a known address must never produce an
// ObservedTransfer, unlike the initial (incorrect) implementation which treated it exactly
// like CALL.
func TestScanNativeTransfers_CallCode_NotRecorded(t *testing.T) {
	t.Parallel()

	knownA := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")
	contractX := common.HexToAddress("0xCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCc")
	dev := common.HexToAddress("0xDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDd")

	blockHash := common.HexToHash("0x" + strings.Repeat("ff", 32))
	txHash := common.HexToHash("0x" + strings.Repeat("99", 32))

	block := rawBlock{
		Hash: blockHash,
		Transactions: []rawBlockTransaction{
			{Hash: txHash, To: &contractX, Value: bigHex(0)},
		},
	}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal fixture block: %v", err)
	}

	traces := []txCallTrace{
		{
			TxHash: txHash,
			Result: callFrame{
				Type: "CALL", From: dev, To: &contractX, Value: bigHex(0),
				Calls: []callFrame{
					{Type: "CALLCODE", From: contractX, To: &knownA, Value: bigHex(77)},
				},
			},
		},
	}
	traceJSON, err := json.Marshal(traces)
	if err != nil {
		t.Fatalf("marshal fixture traces: %v", err)
	}

	scanner := &Scanner{
		chain:  Chain{Name: "faketrace"},
		client: &fakeTraceClient{blockJSON: blockJSON, traceJSON: traceJSON},
		logger: slog.New(slog.DiscardHandler),
	}

	known := map[common.Address]string{knownA: knownA.Hex()}
	transfers, err := scanner.scanNativeTransfers(context.Background(), known, 1, 1)
	if err != nil {
		t.Fatalf("scanNativeTransfers() error = %v, want nil", err)
	}
	if len(transfers) != 0 {
		t.Fatalf("transfers = %+v, want none (CALLCODE never moves balance to `to`, unlike CALL)", transfers)
	}
}

// TestScanNativeTransfers_InternalTransferLogIndex_DeterministicAcrossRescans proves the
// property (chain, tx_hash, log_index) idempotency (AD-5) actually depends on: re-scanning
// the IDENTICAL block+trace fixture must assign the identical synthetic LogIndex to the
// identical internal transfer every time, since a watcher re-polling an overlapping block
// range (the normal, expected steady-state behavior per track_deposits.go) must produce a
// byte-for-byte-repeatable ObservedTransfer for the DB's ON CONFLICT DO NOTHING to dedupe
// correctly — a non-deterministic index would silently create duplicate deposit rows for
// the same on-chain event instead of being caught by the unique constraint.
func TestScanNativeTransfers_InternalTransferLogIndex_DeterministicAcrossRescans(t *testing.T) {
	t.Parallel()

	knownA := common.HexToAddress("0xAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAaAa")
	knownB := common.HexToAddress("0xBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBbBb")
	contractX := common.HexToAddress("0xCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCcCc")
	dev := common.HexToAddress("0xDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDdDd")

	blockHash := common.HexToHash("0x" + strings.Repeat("11", 32))
	txHash := common.HexToHash("0x" + strings.Repeat("22", 32))

	block := rawBlock{
		Hash: blockHash,
		Transactions: []rawBlockTransaction{
			{Hash: txHash, To: &contractX, Value: bigHex(0)},
		},
	}
	blockJSON, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal fixture block: %v", err)
	}

	// A trace shaped so dfsIndex assignment isn't trivially "the first frame is always 0":
	// a non-matching STATICCALL sibling precedes the two matching CALLs, and one matching
	// CALL is nested one level deeper than the other.
	traces := []txCallTrace{
		{
			TxHash: txHash,
			Result: callFrame{
				Type: "CALL", From: dev, To: &contractX, Value: bigHex(0),
				Calls: []callFrame{
					{Type: "STATICCALL", From: contractX, To: &knownA, Value: bigHex(1)},
					{
						Type: "CALL", From: contractX, To: &contractX, Value: bigHex(0),
						Calls: []callFrame{
							{Type: "CALL", From: contractX, To: &knownA, Value: bigHex(15)},
						},
					},
					{Type: "CALL", From: contractX, To: &knownB, Value: bigHex(25)},
				},
			},
		},
	}
	traceJSON, err := json.Marshal(traces)
	if err != nil {
		t.Fatalf("marshal fixture traces: %v", err)
	}

	known := map[common.Address]string{knownA: knownA.Hex(), knownB: knownB.Hex()}

	scan := func() []core.ObservedTransfer {
		scanner := &Scanner{
			chain:  Chain{Name: "faketrace"},
			client: &fakeTraceClient{blockJSON: blockJSON, traceJSON: traceJSON},
			logger: slog.New(slog.DiscardHandler),
		}
		transfers, err := scanner.scanNativeTransfers(context.Background(), known, 1, 1)
		if err != nil {
			t.Fatalf("scanNativeTransfers() error = %v, want nil", err)
		}
		return transfers
	}

	first := scan()
	second := scan()

	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("transfers = %+v / %+v, want exactly 2 in each scan", first, second)
	}

	logIndexByAddress := func(transfers []core.ObservedTransfer) map[string]int {
		m := make(map[string]int, len(transfers))
		for _, tr := range transfers {
			m[tr.Address] = tr.LogIndex
		}
		return m
	}

	firstIndexes := logIndexByAddress(first)
	secondIndexes := logIndexByAddress(second)
	for addr, idx := range firstIndexes {
		other, ok := secondIndexes[addr]
		if !ok {
			t.Fatalf("address %s present in first scan but missing from second: first=%+v second=%+v", addr, first, second)
		}
		if other != idx {
			t.Fatalf("LogIndex for %s changed across identical re-scans: first=%d second=%d — the DB's (chain, tx_hash, log_index) dedup depends on this being stable", addr, idx, other)
		}
	}
	if firstIndexes[knownA.Hex()] == firstIndexes[knownB.Hex()] {
		t.Fatalf("knownA and knownB got the same LogIndex %d within one scan, want distinct", firstIndexes[knownA.Hex()])
	}
}
