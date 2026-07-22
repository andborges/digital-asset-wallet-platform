package evm

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"os/exec"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/andborges/digital-asset-wallet-platform/internal/adapter/signer/software"
	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeBroadcastClient is a broadcastClient test double — the same shape as
// fakeFeeClient/fakeRawBlockClient elsewhere in this package, small enough to fake in unit
// tests without a real chain.
type fakeBroadcastClient struct {
	gasLimit    uint64
	gasLimitErr error
	gasPrice    *big.Int
	gasPriceErr error

	// gotEstimateGasMsg captures the ethereum.CallMsg EstimateGas was actually invoked
	// with — re-review 2026-07-21: needed to prove BuildUnsignedWithdrawal's USDC path
	// simulates gas with amount 0 (never the real withdrawal amount), never observable via
	// the returned gasLimit alone.
	gotEstimateGasMsg *ethereum.CallMsg

	sendErr error
	sentTx  *types.Transaction
	sentTxs []*types.Transaction

	receipt    *types.Receipt
	receiptErr error

	finalizedHeader    *types.Header
	finalizedHeaderErr error
}

func (f *fakeBroadcastClient) EstimateGas(_ context.Context, msg ethereum.CallMsg) (uint64, error) {
	f.gotEstimateGasMsg = &msg
	return f.gasLimit, f.gasLimitErr
}

func (f *fakeBroadcastClient) SuggestGasPrice(context.Context) (*big.Int, error) {
	return f.gasPrice, f.gasPriceErr
}

func (f *fakeBroadcastClient) SendTransaction(_ context.Context, tx *types.Transaction) error {
	f.sentTx = tx
	f.sentTxs = append(f.sentTxs, tx)
	return f.sendErr
}

func (f *fakeBroadcastClient) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return f.receipt, f.receiptErr
}

func (f *fakeBroadcastClient) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return f.finalizedHeader, f.finalizedHeaderErr
}

func (f *fakeBroadcastClient) Close() {}

func newTestBroadcaster(chainID uint64, client broadcastClient, tokenRegistryLister core.TokenRegistryLister) *Broadcaster {
	return &Broadcaster{
		chain:               Chain{Name: "base", ChainID: chainID},
		chainIDBig:          new(big.Int).SetUint64(chainID),
		client:              client,
		tokenRegistryLister: tokenRegistryLister,
	}
}

func TestBroadcaster_BuildUnsignedWithdrawal_ETH_Deterministic(t *testing.T) {
	client := &fakeBroadcastClient{gasLimit: 21_000, gasPrice: big.NewInt(1_000_000_000)}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	ctx := context.Background()
	digest1, unsigned1, err := b.BuildUnsignedWithdrawal(ctx, core.ChainBase, core.AssetETH, 5, "0x00000000000000000000000000000000000000AA", big.NewInt(1_000))
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}
	digest2, unsigned2, err := b.BuildUnsignedWithdrawal(ctx, core.ChainBase, core.AssetETH, 5, "0x00000000000000000000000000000000000000AA", big.NewInt(1_000))
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}

	if digest1 != digest2 {
		t.Fatal("BuildUnsignedWithdrawal is not deterministic — same inputs produced different digests")
	}
	if string(unsigned1) != string(unsigned2) {
		t.Fatal("BuildUnsignedWithdrawal is not deterministic — same inputs produced different unsigned tx bytes")
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(unsigned1); err != nil {
		t.Fatalf("unmarshal unsigned tx: %v", err)
	}
	if tx.Nonce() != 5 {
		t.Fatalf("tx.Nonce() = %d, want 5", tx.Nonce())
	}
	if tx.To() == nil || strings.ToLower(tx.To().Hex()) != "0x00000000000000000000000000000000000000aa" {
		t.Fatalf("tx.To() = %v, want the ETH withdrawal's destination address", tx.To())
	}
	if tx.Value().Cmp(big.NewInt(1_000)) != 0 {
		t.Fatalf("tx.Value() = %s, want 1000 (the ETH amount)", tx.Value())
	}
	if len(tx.Data()) != 0 {
		t.Fatal("a plain ETH transfer must carry no calldata")
	}
}

func TestBroadcaster_BuildUnsignedWithdrawal_USDC_UsesRegisteredContractAndEncodesTransfer(t *testing.T) {
	client := &fakeBroadcastClient{gasLimit: 65_000, gasPrice: big.NewInt(1_000_000_000)}
	usdcAddress := common.HexToAddress("0x00000000000000000000000000000000000badC0")
	destination := common.HexToAddress("0x00000000000000000000000000000000000000AA")
	tokenRegistryLister := &fakeTokenRegistryLister{registries: map[core.Chain]map[string]core.Asset{
		core.ChainBase: {strings.ToLower(usdcAddress.Hex()): core.AssetUSDC},
	}}
	b := newTestBroadcaster(anvilChainID, client, tokenRegistryLister)

	_, unsigned, err := b.BuildUnsignedWithdrawal(context.Background(), core.ChainBase, core.AssetUSDC, 0, destination.Hex(), big.NewInt(42_000))
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(unsigned); err != nil {
		t.Fatalf("unmarshal unsigned tx: %v", err)
	}
	if tx.To() == nil || *tx.To() != usdcAddress {
		t.Fatalf("tx.To() = %v, want the registered USDC contract address %v (never the destination directly)", tx.To(), usdcAddress)
	}
	if tx.Value().Sign() != 0 {
		t.Fatalf("tx.Value() = %s, want 0 (value moves via the ERC-20 call, not the tx's native value)", tx.Value())
	}

	outputs, err := erc20TransferABI.Methods["transfer"].Inputs.Unpack(tx.Data()[4:])
	if err != nil {
		t.Fatalf("decode transfer() calldata: %v", err)
	}
	gotTo, ok := outputs[0].(common.Address)
	if !ok || gotTo != destination {
		t.Fatalf("decoded transfer() to = %v, want %v", outputs[0], destination)
	}
	gotAmount, ok := outputs[1].(*big.Int)
	if !ok || gotAmount.Cmp(big.NewInt(42_000)) != 0 {
		t.Fatalf("decoded transfer() amount = %v, want 42000", outputs[1])
	}
}

// TestBroadcaster_BuildUnsignedWithdrawal_USDC_EstimatesGasWithZeroAmount is this file's
// regression test for the adversarial review's core finding (2026-07-21): EstimateGas must
// be simulated with a transfer(to, 0) call, never transfer(to, <the real amount>) — on a
// real chain/contract, neither this call nor Arbitrum's analogous eth_call ever sets a
// "from" address, so simulating the real amount would fail the ERC-20 contract's own
// require(balance[from] >= amount) check against the zero/default sender's real (zero)
// USDC balance, reverting gas estimation for every real USDC withdrawal (mirrors
// fee_estimator.go's representativeTransaction, which already established this exact
// mitigation for Story 3.1's own USDC fee estimate). The REAL amount must still reach the
// actual unsigned transaction returned to the caller — this test checks both halves.
func TestBroadcaster_BuildUnsignedWithdrawal_USDC_EstimatesGasWithZeroAmount(t *testing.T) {
	client := &fakeBroadcastClient{gasLimit: 65_000, gasPrice: big.NewInt(1_000_000_000)}
	usdcAddress := common.HexToAddress("0x00000000000000000000000000000000000badC0")
	destination := common.HexToAddress("0x00000000000000000000000000000000000000AA")
	tokenRegistryLister := &fakeTokenRegistryLister{registries: map[core.Chain]map[string]core.Asset{
		core.ChainBase: {strings.ToLower(usdcAddress.Hex()): core.AssetUSDC},
	}}
	b := newTestBroadcaster(anvilChainID, client, tokenRegistryLister)

	realAmount := big.NewInt(42_000)
	_, unsigned, err := b.BuildUnsignedWithdrawal(context.Background(), core.ChainBase, core.AssetUSDC, 0, destination.Hex(), realAmount)
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}

	if client.gotEstimateGasMsg == nil {
		t.Fatal("EstimateGas was never called")
	}
	estimateOutputs, err := erc20TransferABI.Methods["transfer"].Inputs.Unpack(client.gotEstimateGasMsg.Data[4:])
	if err != nil {
		t.Fatalf("decode EstimateGas's simulated transfer() calldata: %v", err)
	}
	estimateAmount, ok := estimateOutputs[1].(*big.Int)
	if !ok || estimateAmount.Sign() != 0 {
		t.Fatalf("EstimateGas simulated transfer() amount = %v, want exactly 0 (never the real withdrawal amount)", estimateOutputs[1])
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(unsigned); err != nil {
		t.Fatalf("unmarshal unsigned tx: %v", err)
	}
	realOutputs, err := erc20TransferABI.Methods["transfer"].Inputs.Unpack(tx.Data()[4:])
	if err != nil {
		t.Fatalf("decode the real unsigned tx's transfer() calldata: %v", err)
	}
	realTxAmount, ok := realOutputs[1].(*big.Int)
	if !ok || realTxAmount.Cmp(realAmount) != 0 {
		t.Fatalf("real unsigned tx transfer() amount = %v, want %s (the actual withdrawal amount must still reach the chain)", realOutputs[1], realAmount)
	}
}

// TestBroadcaster_BuildUnsignedWithdrawal_GasFeeCapHasHeadroomOverGasTipCap proves the
// adversarial review's fee-headroom finding (2026-07-21): GasFeeCap must exceed GasTipCap
// (the raw suggested price) by the documented buffer, since fee-bump/replacement is out of
// this story's scope and a zero-headroom cap has no margin against even a small base-fee
// increase before inclusion.
func TestBroadcaster_BuildUnsignedWithdrawal_GasFeeCapHasHeadroomOverGasTipCap(t *testing.T) {
	client := &fakeBroadcastClient{gasLimit: 21_000, gasPrice: big.NewInt(1_000_000_000)}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	_, unsigned, err := b.BuildUnsignedWithdrawal(context.Background(), core.ChainBase, core.AssetETH, 0, "0x00000000000000000000000000000000000000AA", big.NewInt(1))
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(unsigned); err != nil {
		t.Fatalf("unmarshal unsigned tx: %v", err)
	}
	if tx.GasTipCap().Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Fatalf("GasTipCap = %s, want the raw suggested price 1000000000 unchanged", tx.GasTipCap())
	}
	if tx.GasFeeCap().Cmp(tx.GasTipCap()) <= 0 {
		t.Fatalf("GasFeeCap = %s, want strictly greater than GasTipCap = %s (headroom)", tx.GasFeeCap(), tx.GasTipCap())
	}
}

func TestBroadcaster_BuildUnsignedWithdrawal_USDC_NoRegisteredContract_FailsLoud(t *testing.T) {
	client := &fakeBroadcastClient{}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	_, _, err := b.BuildUnsignedWithdrawal(context.Background(), core.ChainBase, core.AssetUSDC, 0, "0x00000000000000000000000000000000000000AA", big.NewInt(1))
	if err == nil {
		t.Fatal("err = nil, want an error — no USDC contract is registered for this chain")
	}
}

func TestBroadcaster_BuildUnsignedWithdrawal_WrongChain_ReturnsError(t *testing.T) {
	b := newTestBroadcaster(anvilChainID, &fakeBroadcastClient{}, &fakeTokenRegistryLister{})

	_, _, err := b.BuildUnsignedWithdrawal(context.Background(), core.ChainArbitrum, core.AssetETH, 0, "0x00000000000000000000000000000000000000AA", big.NewInt(1))
	if err == nil {
		t.Fatal("err = nil, want an error — this Broadcaster is configured for base, not arbitrum")
	}
}

// TestBroadcaster_AssembleSignedTx_ProducesRecoverableSignature signs a real digest with a
// real ECDSA key (crypto.Sign — allowed here, this file lives in internal/adapter/evm,
// AD-1's own designated home for go-ethereum imports) and proves AssembleSignedTx attaches
// that signature correctly: the resulting signed transaction's recovered sender matches the
// signing key's own address, and its hash matches go-ethereum's own tx.Hash().
func TestBroadcaster_AssembleSignedTx_ProducesRecoverableSignature(t *testing.T) {
	client := &fakeBroadcastClient{gasLimit: 21_000, gasPrice: big.NewInt(1_000_000_000)}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	_, unsignedTx, err := b.BuildUnsignedWithdrawal(context.Background(), core.ChainBase, core.AssetETH, 3, "0x00000000000000000000000000000000000000AA", big.NewInt(500))
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}

	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	wantAddress := crypto.PubkeyToAddress(privateKey.PublicKey)

	var unsigned types.Transaction
	if err := unsigned.UnmarshalBinary(unsignedTx); err != nil {
		t.Fatalf("unmarshal unsigned tx: %v", err)
	}
	signer := types.LatestSignerForChainID(unsigned.ChainId())
	digest := signer.Hash(&unsigned)

	rawSig, err := crypto.Sign(digest[:], privateKey)
	if err != nil {
		t.Fatalf("sign digest: %v", err)
	}
	var signature [65]byte
	copy(signature[:], rawSig)

	signedTx, txHash, err := b.AssembleSignedTx(unsignedTx, signature)
	if err != nil {
		t.Fatalf("AssembleSignedTx() error = %v, want nil", err)
	}

	var got types.Transaction
	if err := got.UnmarshalBinary(signedTx); err != nil {
		t.Fatalf("unmarshal signed tx: %v", err)
	}
	gotSender, err := types.Sender(signer, &got)
	if err != nil {
		t.Fatalf("recover sender: %v", err)
	}
	if gotSender != wantAddress {
		t.Fatalf("recovered sender = %v, want %v", gotSender, wantAddress)
	}
	if txHash != got.Hash().Hex() {
		t.Fatalf("txHash = %q, want %q", txHash, got.Hash().Hex())
	}
	if got.Nonce() != 3 {
		t.Fatalf("signed tx nonce = %d, want 3 (unchanged from the unsigned tx)", got.Nonce())
	}
}

func TestBroadcaster_SendRawTransaction_DecodesAndSends(t *testing.T) {
	client := &fakeBroadcastClient{gasLimit: 21_000, gasPrice: big.NewInt(1_000_000_000)}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	_, unsignedTx, err := b.BuildUnsignedWithdrawal(context.Background(), core.ChainBase, core.AssetETH, 0, "0x00000000000000000000000000000000000000AA", big.NewInt(1))
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var unsigned types.Transaction
	if err := unsigned.UnmarshalBinary(unsignedTx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	signer := types.LatestSignerForChainID(unsigned.ChainId())
	digest := signer.Hash(&unsigned)
	rawSig, err := crypto.Sign(digest[:], privateKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	var signature [65]byte
	copy(signature[:], rawSig)
	signedTx, _, err := b.AssembleSignedTx(unsignedTx, signature)
	if err != nil {
		t.Fatalf("AssembleSignedTx() error = %v, want nil", err)
	}

	if err := b.SendRawTransaction(context.Background(), core.ChainBase, signedTx); err != nil {
		t.Fatalf("SendRawTransaction() error = %v, want nil", err)
	}
	if client.sentTx == nil {
		t.Fatal("SendTransaction was never called on the underlying client")
	}
	if client.sentTx.Nonce() != 0 {
		t.Fatalf("sent tx nonce = %d, want 0", client.sentTx.Nonce())
	}
}

func TestBroadcaster_SendRawTransaction_WrongChain_ReturnsError(t *testing.T) {
	b := newTestBroadcaster(anvilChainID, &fakeBroadcastClient{}, &fakeTokenRegistryLister{})
	if err := b.SendRawTransaction(context.Background(), core.ChainArbitrum, []byte{}); err == nil {
		t.Fatal("err = nil, want an error — this Broadcaster is configured for base, not arbitrum")
	}
}

func TestBroadcaster_GetFinalizedReceipt_NotFound(t *testing.T) {
	client := &fakeBroadcastClient{receiptErr: ethereum.NotFound}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	found, success, err := b.GetFinalizedReceipt(context.Background(), core.ChainBase, "0xabc")
	if err != nil {
		t.Fatalf("err = %v, want nil (not-found is an ordinary keep-polling outcome)", err)
	}
	if found || success {
		t.Fatalf("(found, success) = (%v, %v), want (false, false)", found, success)
	}
}

func TestBroadcaster_GetFinalizedReceipt_ReceiptNotYetFinalized(t *testing.T) {
	client := &fakeBroadcastClient{
		receipt:         &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(100)},
		finalizedHeader: &types.Header{Number: big.NewInt(90)},
	}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	found, success, err := b.GetFinalizedReceipt(context.Background(), core.ChainBase, "0xabc")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if found || success {
		t.Fatalf("(found, success) = (%v, %v), want (false, false) — the receipt's block (100) is past the finalized tag (90)", found, success)
	}
}

func TestBroadcaster_GetFinalizedReceipt_FinalizedAndSuccessful(t *testing.T) {
	client := &fakeBroadcastClient{
		receipt:         &types.Receipt{Status: types.ReceiptStatusSuccessful, BlockNumber: big.NewInt(90)},
		finalizedHeader: &types.Header{Number: big.NewInt(100)},
	}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	found, success, err := b.GetFinalizedReceipt(context.Background(), core.ChainBase, "0xabc")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !found || !success {
		t.Fatalf("(found, success) = (%v, %v), want (true, true)", found, success)
	}
}

func TestBroadcaster_GetFinalizedReceipt_FinalizedAndReverted(t *testing.T) {
	client := &fakeBroadcastClient{
		receipt:         &types.Receipt{Status: types.ReceiptStatusFailed, BlockNumber: big.NewInt(90)},
		finalizedHeader: &types.Header{Number: big.NewInt(100)},
	}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	found, success, err := b.GetFinalizedReceipt(context.Background(), core.ChainBase, "0xabc")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !found || success {
		t.Fatalf("(found, success) = (%v, %v), want (true, false) — the receipt reports a reverted transaction", found, success)
	}
}

func TestBroadcaster_GetFinalizedReceipt_PropagatesOtherErrors(t *testing.T) {
	wantErr := errors.New("rpc endpoint unreachable")
	client := &fakeBroadcastClient{receiptErr: wantErr}
	b := newTestBroadcaster(anvilChainID, client, &fakeTokenRegistryLister{})

	_, _, err := b.GetFinalizedReceipt(context.Background(), core.ChainBase, "0xabc")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
}

// TestBroadcaster_RealAnvil_SendAndConfirm exercises the full build -> sign -> assemble ->
// send -> confirm path against a locally-installed `anvil` instance — skipped gracefully if
// anvil isn't installed (mirrors scanner_test.go's real-anvil tests exactly). Signing here
// goes through the real internal/adapter/signer/software.Signer (re-review 2026-07-21,
// adversarial review: an earlier version signed via crypto.Sign directly, so no test, run
// or unrun, ever exercised a real core.Signer implementation together with the real
// TransactionBroadcaster the way cmd/walletd's composition root actually wires them) — a
// test-only cross-adapter import, the same already-endorsed pattern
// internal/adapter/api/integration_test.go's own import of internal/adapter/evm uses
// (deferred-work.md).
func TestBroadcaster_RealAnvil_SendAndConfirm(t *testing.T) {
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
	dialClient, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatalf("dial anvil: %v", err)
	}
	defer dialClient.Close()

	// Fund a fresh throwaway account (the hot wallet stand-in) from anvil's dev account
	// #0, so the withdrawal transaction below has real ETH to send.
	devKey, err := crypto.HexToECDSA(anvilDefaultPrivateKeyHex)
	if err != nil {
		t.Fatalf("parse anvil dev private key: %v", err)
	}
	devAddr := crypto.PubkeyToAddress(devKey.PublicKey)
	signerKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate hot-wallet key: %v", err)
	}
	hotWalletAddr := crypto.PubkeyToAddress(signerKey.PublicKey)
	withdrawalSigner, err := software.NewSigner(hex.EncodeToString(crypto.FromECDSA(signerKey)))
	if err != nil {
		t.Fatalf("construct software signer: %v", err)
	}
	if !strings.EqualFold(withdrawalSigner.Address(), hotWalletAddr.Hex()) {
		t.Fatalf("software.Signer.Address() = %s, want %s (must derive the same address as the funded hot wallet key)", withdrawalSigner.Address(), hotWalletAddr.Hex())
	}

	devNonce, err := dialClient.PendingNonceAt(ctx, devAddr)
	if err != nil {
		t.Fatalf("get dev nonce: %v", err)
	}
	gasPrice, err := dialClient.SuggestGasPrice(ctx)
	if err != nil {
		t.Fatalf("suggest gas price: %v", err)
	}
	fundTx := types.NewTransaction(devNonce, hotWalletAddr, big.NewInt(1_000_000_000_000_000_000), 21_000, gasPrice, nil)
	signedFundTx, err := types.SignTx(fundTx, types.NewEIP155Signer(big.NewInt(anvilChainID)), devKey)
	if err != nil {
		t.Fatalf("sign fund tx: %v", err)
	}
	if err := dialClient.SendTransaction(ctx, signedFundTx); err != nil {
		t.Fatalf("send fund tx: %v", err)
	}
	if _, err := waitForReceipt(ctx, dialClient, signedFundTx.Hash()); err != nil {
		t.Fatalf("wait for fund tx receipt: %v", err)
	}

	chain := Chain{Name: "anvil", RPCURL: rpcURL, ChainID: anvilChainID}
	broadcaster, err := NewBroadcaster(ctx, chain, &fakeTokenRegistryLister{})
	if err != nil {
		t.Fatalf("NewBroadcaster() error = %v, want nil", err)
	}
	defer broadcaster.Close()

	recipient := "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	withdrawAmount := big.NewInt(1_000_000_000_000_000) // 0.001 ETH

	digest, unsignedTx, err := broadcaster.BuildUnsignedWithdrawal(ctx, core.Chain("anvil"), core.AssetETH, 0, recipient, withdrawAmount)
	if err != nil {
		t.Fatalf("BuildUnsignedWithdrawal() error = %v, want nil", err)
	}
	signature, err := withdrawalSigner.Sign(ctx, core.Chain("anvil"), digest)
	if err != nil {
		t.Fatalf("Sign() error = %v, want nil", err)
	}

	signedTx, txHash, err := broadcaster.AssembleSignedTx(unsignedTx, signature)
	if err != nil {
		t.Fatalf("AssembleSignedTx() error = %v, want nil", err)
	}

	if err := broadcaster.SendRawTransaction(ctx, core.Chain("anvil"), signedTx); err != nil {
		t.Fatalf("SendRawTransaction() error = %v, want nil", err)
	}

	if _, err := waitForReceipt(ctx, dialClient, common.HexToHash(txHash)); err != nil {
		t.Fatalf("wait for withdrawal tx receipt: %v", err)
	}

	// anvil's default dev-mode chain finalizes instantly (no separate consensus depth),
	// so the freshly-mined block should already be reported as finalized.
	found, success, err := broadcaster.GetFinalizedReceipt(ctx, core.Chain("anvil"), txHash)
	if err != nil {
		t.Fatalf("GetFinalizedReceipt() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("found = false, want true — the withdrawal tx has a receipt and anvil finalizes instantly")
	}
	if !success {
		t.Fatal("success = false, want true — the withdrawal tx should have succeeded")
	}

	recipientBalance, err := dialClient.BalanceAt(ctx, common.HexToAddress(recipient), nil)
	if err != nil {
		t.Fatalf("query recipient balance: %v", err)
	}
	if recipientBalance.Cmp(withdrawAmount) < 0 {
		t.Fatalf("recipient balance = %s, want at least %s", recipientBalance, withdrawAmount)
	}
}
