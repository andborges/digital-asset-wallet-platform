package evm

import (
	"context"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// fakeFeeClient is a minimal feeClient test double — small enough to fake without a real
// chain, the same shape as fakeChainClient in deployer_test.go.
type fakeFeeClient struct {
	callContextFunc func(ctx context.Context, result interface{}, method string, args ...interface{}) error
	estimateGas     uint64
	estimateGasErr  error
	gasPrice        *big.Int
	gasPriceErr     error
}

func (f *fakeFeeClient) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	return f.callContextFunc(ctx, result, method, args...)
}

func (f *fakeFeeClient) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	return f.estimateGas, f.estimateGasErr
}

func (f *fakeFeeClient) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return f.gasPrice, f.gasPriceErr
}

func (f *fakeFeeClient) Close() {}

// fakeTokenRegistryLister is a test double for core.TokenRegistryLister, standing in for
// the real Postgres-backed token_registry lookup (Story 2.3) that usdcContractAddress
// inverts.
type fakeTokenRegistryLister struct {
	registries map[core.Chain]map[string]core.Asset
	err        error
}

func (f *fakeTokenRegistryLister) ListTokenRegistry(ctx context.Context, chain core.Chain) (map[string]core.Asset, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.registries[chain], nil
}

// nodeInterfaceCannedResponse builds a fake eth_call response for
// NodeInterface.gasEstimateComponents, ABI-encoded the same way a real response would be
// — this exercises the real Unpack logic against a real encoding, not a hand-rolled
// stand-in. l1BaseFeeEstimate is set to an arbitrary non-zero value: the formula this
// story uses deliberately never reads it (Design Notes: "l1BaseFeeEstimate is not needed
// once this simplification is used — don't reintroduce it"), so its exact value never
// matters here.
func nodeInterfaceCannedResponse(t *testing.T, gasEstimate, gasEstimateForL1 uint64, baseFee *big.Int) []byte {
	t.Helper()
	encoded, err := nodeInterfaceABI.Methods["gasEstimateComponents"].Outputs.Pack(
		gasEstimate, gasEstimateForL1, baseFee, big.NewInt(999),
	)
	if err != nil {
		t.Fatalf("pack canned gasEstimateComponents response: %v", err)
	}
	return encoded
}

// ethCallFakeClient returns a fakeFeeClient whose CallContext validates it was asked for
// an eth_call and writes canned into the *hexutil.Bytes result pointer — the shape every
// Arbitrum-path test below needs.
func ethCallFakeClient(t *testing.T, canned []byte) *fakeFeeClient {
	t.Helper()
	return &fakeFeeClient{
		callContextFunc: func(ctx context.Context, result interface{}, method string, args ...interface{}) error {
			if method != "eth_call" {
				t.Fatalf("CallContext method = %q, want %q", method, "eth_call")
			}
			ptr, ok := result.(*hexutil.Bytes)
			if !ok {
				t.Fatalf("CallContext result type = %T, want *hexutil.Bytes", result)
			}
			*ptr = canned
			return nil
		},
	}
}

// TestFeeEstimator_Arbitrum_ComputesFeeFromRealExampleNumbers proves the Arbitrum formula
// against this story's own empirically-obtained real Arbitrum Sepolia example (Design
// Notes): a live call returning gasEstimate=27142, gasEstimateForL1=5798,
// baseFee=20162000 must produce L2Fee=430337728000, L1Fee=116899276000,
// TotalFee=547237004000 (all wei), summing exactly. NodeInterface has no deployed
// bytecode (ArbOS-native), so this is tested against a fake/canned RPC response, never a
// live call (Story 3.1 Boundaries & Constraints).
func TestFeeEstimator_Arbitrum_ComputesFeeFromRealExampleNumbers(t *testing.T) {
	const gasEstimate = uint64(27142)
	const gasEstimateForL1 = uint64(5798)
	baseFee := big.NewInt(20162000)
	canned := nodeInterfaceCannedResponse(t, gasEstimate, gasEstimateForL1, baseFee)

	client := ethCallFakeClient(t, canned)
	estimator := &FeeEstimator{clients: map[core.Chain]feeClient{core.ChainArbitrum: client}}

	got, err := estimator.EstimateFee(context.Background(), core.ChainArbitrum, core.AssetETH, big.NewInt(1))
	if err != nil {
		t.Fatalf("EstimateFee() error = %v, want nil", err)
	}

	wantL2 := big.NewInt(430337728000)
	wantL1 := big.NewInt(116899276000)
	wantTotal := big.NewInt(547237004000)
	if got.L2Fee.Cmp(wantL2) != 0 {
		t.Errorf("L2Fee = %s, want %s", got.L2Fee, wantL2)
	}
	if got.L1Fee.Cmp(wantL1) != 0 {
		t.Errorf("L1Fee = %s, want %s", got.L1Fee, wantL1)
	}
	if got.TotalFee.Cmp(wantTotal) != 0 {
		t.Errorf("TotalFee = %s, want %s", got.TotalFee, wantTotal)
	}
	if sum := new(big.Int).Add(got.L2Fee, got.L1Fee); sum.Cmp(got.TotalFee) != 0 {
		t.Fatalf("L2Fee + L1Fee = %s, want equal to TotalFee %s", sum, got.TotalFee)
	}
}

// TestFeeEstimator_Arbitrum_USDC_ResolvesContractAddressFromTokenRegistry proves the USDC
// path resolves its representative transaction's destination from the existing
// TokenRegistryLister port (Story 2.3), never a new config path (Story 3.1 Boundaries &
// Constraints).
func TestFeeEstimator_Arbitrum_USDC_ResolvesContractAddressFromTokenRegistry(t *testing.T) {
	canned := nodeInterfaceCannedResponse(t, 27142, 5798, big.NewInt(20162000))
	// capturedTo is the "to" ARGUMENT encoded inside gasEstimateComponents' own calldata
	// (the representative transaction's destination) — not the eth_call's own "to" field,
	// which is always nodeInterfaceAddress itself (the precompile being called).
	var capturedTo string
	client := &fakeFeeClient{
		callContextFunc: func(ctx context.Context, result interface{}, method string, args ...interface{}) error {
			params, ok := args[0].(ethCallParams)
			if !ok {
				t.Fatalf("eth_call first arg type = %T, want ethCallParams", args[0])
			}
			if params.To != nodeInterfaceAddress {
				t.Fatalf("eth_call 'to' = %s, want the NodeInterface precompile address %s", params.To.Hex(), nodeInterfaceAddress.Hex())
			}
			decoded, err := nodeInterfaceABI.Methods["gasEstimateComponents"].Inputs.Unpack(params.Data[4:])
			if err != nil {
				t.Fatalf("decode gasEstimateComponents calldata: %v", err)
			}
			capturedTo = decoded[0].(common.Address).Hex()
			ptr := result.(*hexutil.Bytes)
			*ptr = canned
			return nil
		},
	}
	registryLister := &fakeTokenRegistryLister{
		registries: map[core.Chain]map[string]core.Asset{
			core.ChainArbitrum: {"0x0000000000000000000000000000000000abcd": core.AssetUSDC},
		},
	}
	estimator := &FeeEstimator{
		clients:             map[core.Chain]feeClient{core.ChainArbitrum: client},
		tokenRegistryLister: registryLister,
	}

	if _, err := estimator.EstimateFee(context.Background(), core.ChainArbitrum, core.AssetUSDC, big.NewInt(100)); err != nil {
		t.Fatalf("EstimateFee() error = %v, want nil", err)
	}

	// EIP-55 checksummed form of the registry's lowercase-stored address — computed, not
	// hand-typed, so this assertion can't itself carry a transcription-error checksum typo.
	wantTo := common.HexToAddress("0x0000000000000000000000000000000000abcd").Hex()
	if capturedTo != wantTo {
		t.Fatalf("gasEstimateComponents 'to' argument = %s, want the registered USDC contract address %s", capturedTo, wantTo)
	}
}

// TestFeeEstimator_Arbitrum_USDC_RepresentativeCalldataEncodesZeroAmount proves the
// representative transfer() call encodes amount 0, never the caller's real requested
// amount (re-review, adversarial review): neither this eth_call nor Base's EstimateGas
// path ever sets a "from" address, so a nonzero amount would make the ERC20 contract's own
// balance check revert against the zero/default sender's real (zero) USDC balance —
// breaking every live USDC fee estimate. amount 0 always passes that check trivially, and
// (being a fixed-width uint256 word either way) doesn't change the calldata's byte length.
func TestFeeEstimator_Arbitrum_USDC_RepresentativeCalldataEncodesZeroAmount(t *testing.T) {
	canned := nodeInterfaceCannedResponse(t, 27142, 5798, big.NewInt(20162000))
	var capturedAmount *big.Int
	client := &fakeFeeClient{
		callContextFunc: func(ctx context.Context, result interface{}, method string, args ...interface{}) error {
			params := args[0].(ethCallParams)
			decoded, err := nodeInterfaceABI.Methods["gasEstimateComponents"].Inputs.Unpack(params.Data[4:])
			if err != nil {
				t.Fatalf("decode gasEstimateComponents calldata: %v", err)
			}
			innerCalldata := decoded[2].([]byte)
			transferArgs, err := erc20TransferABI.Methods["transfer"].Inputs.Unpack(innerCalldata[4:])
			if err != nil {
				t.Fatalf("decode inner transfer calldata: %v", err)
			}
			capturedAmount = transferArgs[1].(*big.Int)
			ptr := result.(*hexutil.Bytes)
			*ptr = canned
			return nil
		},
	}
	registryLister := &fakeTokenRegistryLister{
		registries: map[core.Chain]map[string]core.Asset{
			core.ChainArbitrum: {"0x0000000000000000000000000000000000abcd": core.AssetUSDC},
		},
	}
	estimator := &FeeEstimator{
		clients:             map[core.Chain]feeClient{core.ChainArbitrum: client},
		tokenRegistryLister: registryLister,
	}

	// A large real requested amount — if it leaked into the representative calldata, this
	// would be immediately visible below.
	realAmount := new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(1_000_000))
	if _, err := estimator.EstimateFee(context.Background(), core.ChainArbitrum, core.AssetUSDC, realAmount); err != nil {
		t.Fatalf("EstimateFee() error = %v, want nil", err)
	}

	if capturedAmount == nil || capturedAmount.Sign() != 0 {
		t.Fatalf("representative transfer() amount = %v, want 0 (must never be the real requested amount %s)", capturedAmount, realAmount)
	}
}

// TestFeeEstimator_USDC_AmbiguousTokenRegistry_FailsLoud proves that more than one
// token_registry entry mapped to USDC on the same chain (migration 0007 explicitly
// anticipates this for a bridged/wrapped USDC variant) errors rather than picking one
// arbitrarily via Go's randomized map iteration order (re-review, adversarial review).
func TestFeeEstimator_USDC_AmbiguousTokenRegistry_FailsLoud(t *testing.T) {
	registryLister := &fakeTokenRegistryLister{registries: map[core.Chain]map[string]core.Asset{
		core.ChainBase: {
			"0x0000000000000000000000000000000000aaaa": core.AssetUSDC,
			"0x0000000000000000000000000000000000bbbb": core.AssetUSDC,
		},
	}}
	estimator := &FeeEstimator{
		clients:             map[core.Chain]feeClient{core.ChainBase: &fakeFeeClient{}},
		tokenRegistryLister: registryLister,
	}

	_, err := estimator.EstimateFee(context.Background(), core.ChainBase, core.AssetUSDC, big.NewInt(100))
	if err == nil {
		t.Fatal("EstimateFee() error = nil, want a non-nil error (ambiguous token_registry: 2 USDC entries)")
	}
}

// TestFeeEstimator_UnsupportedAsset_FailsLoud proves an asset value that is neither ETH
// nor USDC errors rather than silently falling through to the USDC path (re-review,
// adversarial review).
func TestFeeEstimator_UnsupportedAsset_FailsLoud(t *testing.T) {
	estimator := &FeeEstimator{
		clients:             map[core.Chain]feeClient{core.ChainBase: &fakeFeeClient{}},
		tokenRegistryLister: &fakeTokenRegistryLister{},
	}

	_, err := estimator.EstimateFee(context.Background(), core.ChainBase, core.Asset("dai"), big.NewInt(100))
	if err == nil {
		t.Fatal("EstimateFee() error = nil, want a non-nil error (unsupported asset)")
	}
}

// TestFeeEstimator_Base_ETH_TxSizeReflectsRealValue proves the representative unsigned
// transaction used for GasPriceOracle.getL1FeeUpperBound carries the real requested ETH
// amount as its Value field, not a hardcoded 0 (re-review, adversarial review): Value
// RLP-encodes as a variable-length integer, so hardcoding 0 undercounts (and therefore
// undercharges) the L1 fee for any large ETH withdrawal.
func TestFeeEstimator_Base_ETH_TxSizeReflectsRealValue(t *testing.T) {
	sizeFor := func(t *testing.T, amount *big.Int) *big.Int {
		t.Helper()
		var capturedSize *big.Int
		wantL1 := big.NewInt(1)
		cannedL1, err := getL1FeeUpperBoundABI.Methods["getL1FeeUpperBound"].Outputs.Pack(wantL1)
		if err != nil {
			t.Fatalf("pack canned getL1FeeUpperBound response: %v", err)
		}
		client := &fakeFeeClient{
			estimateGas: 21000,
			gasPrice:    big.NewInt(1000000000),
			callContextFunc: func(ctx context.Context, result interface{}, method string, args ...interface{}) error {
				params := args[0].(ethCallParams)
				decoded, err := getL1FeeUpperBoundABI.Methods["getL1FeeUpperBound"].Inputs.Unpack(params.Data[4:])
				if err != nil {
					t.Fatalf("decode getL1FeeUpperBound calldata: %v", err)
				}
				capturedSize = decoded[0].(*big.Int)
				ptr := result.(*hexutil.Bytes)
				*ptr = cannedL1
				return nil
			},
		}
		estimator := &FeeEstimator{clients: map[core.Chain]feeClient{core.ChainBase: client}}
		if _, err := estimator.EstimateFee(context.Background(), core.ChainBase, core.AssetETH, amount); err != nil {
			t.Fatalf("EstimateFee() error = %v, want nil", err)
		}
		return capturedSize
	}

	smallSize := sizeFor(t, big.NewInt(1))
	// A value large enough that its RLP encoding needs the full 32 bytes, versus a 1-wei
	// transfer's single-byte encoding.
	largeAmount := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	largeSize := sizeFor(t, largeAmount)

	if largeSize.Cmp(smallSize) <= 0 {
		t.Fatalf("tx size for a large ETH amount = %s, want strictly greater than the size for a 1-wei amount (%s) — Value must vary the encoded size", largeSize, smallSize)
	}
}

// TestFeeEstimator_USDC_MissingTokenRegistryEntry_FailsLoud proves a missing
// token_registry entry for USDC surfaces as an error, never a guessed contract address
// (Story 3.1 I/O matrix: "fail loud, not a guessed contract address").
func TestFeeEstimator_USDC_MissingTokenRegistryEntry_FailsLoud(t *testing.T) {
	registryLister := &fakeTokenRegistryLister{registries: map[core.Chain]map[string]core.Asset{
		core.ChainBase: {},
	}}
	estimator := &FeeEstimator{
		clients:             map[core.Chain]feeClient{core.ChainBase: &fakeFeeClient{}},
		tokenRegistryLister: registryLister,
	}

	_, err := estimator.EstimateFee(context.Background(), core.ChainBase, core.AssetUSDC, big.NewInt(100))
	if err == nil {
		t.Fatal("EstimateFee() error = nil, want a non-nil error (no token_registry entry for USDC)")
	}
}

// TestFeeEstimator_TokenRegistryError_Propagates proves a token_registry read failure
// propagates rather than being swallowed.
func TestFeeEstimator_TokenRegistryError_Propagates(t *testing.T) {
	wantErr := errors.New("query token registry: connection refused")
	registryLister := &fakeTokenRegistryLister{err: wantErr}
	estimator := &FeeEstimator{
		clients:             map[core.Chain]feeClient{core.ChainBase: &fakeFeeClient{}},
		tokenRegistryLister: registryLister,
	}

	_, err := estimator.EstimateFee(context.Background(), core.ChainBase, core.AssetUSDC, big.NewInt(100))
	if !errors.Is(err, wantErr) {
		t.Fatalf("EstimateFee() error = %v, want wrapping %v", err, wantErr)
	}
}

// TestFeeEstimator_UnconfiguredChain_FailsLoud proves a chain with no dialed RPC client
// returns an error rather than a nil-pointer panic or a silent zero estimate.
func TestFeeEstimator_UnconfiguredChain_FailsLoud(t *testing.T) {
	estimator := &FeeEstimator{clients: map[core.Chain]feeClient{}}

	_, err := estimator.EstimateFee(context.Background(), core.ChainBase, core.AssetETH, big.NewInt(1))
	if err == nil {
		t.Fatal("EstimateFee() error = nil, want a non-nil error (no configured RPC client)")
	}
}

// TestFeeEstimator_Base_EstimatesL2AndL1FeeSeparately exercises Base's fee path against a
// fake feeClient: EstimateGas/SuggestGasPrice back the L2 component, and a canned eth_call
// response backs GasPriceOracle.getL1FeeUpperBound's L1 component (real deployed
// bytecode, but faked here the same way as NodeInterface for a fast, deterministic unit
// test — the real predeploy is exercised by the opt-in live-fork test below).
func TestFeeEstimator_Base_EstimatesL2AndL1FeeSeparately(t *testing.T) {
	wantL1 := big.NewInt(123456)
	cannedL1, err := getL1FeeUpperBoundABI.Methods["getL1FeeUpperBound"].Outputs.Pack(wantL1)
	if err != nil {
		t.Fatalf("pack canned getL1FeeUpperBound response: %v", err)
	}
	client := &fakeFeeClient{
		estimateGas: 21000,
		gasPrice:    big.NewInt(1000000000),
		callContextFunc: func(ctx context.Context, result interface{}, method string, args ...interface{}) error {
			if method != "eth_call" {
				t.Fatalf("CallContext method = %q, want %q", method, "eth_call")
			}
			// Pins the real GasPriceOracle predeploy address (re-review, adversarial
			// review): a prior transcription bug shortened this constant to 19 bytes, and
			// no test caught it because nothing here asserted the eth_call's destination.
			params, ok := args[0].(ethCallParams)
			if !ok {
				t.Fatalf("eth_call first arg type = %T, want ethCallParams", args[0])
			}
			if params.To != gasPriceOracleAddress {
				t.Fatalf("eth_call 'to' = %s, want the GasPriceOracle predeploy address %s", params.To.Hex(), gasPriceOracleAddress.Hex())
			}
			ptr := result.(*hexutil.Bytes)
			*ptr = cannedL1
			return nil
		},
	}
	estimator := &FeeEstimator{clients: map[core.Chain]feeClient{core.ChainBase: client}}

	got, err := estimator.EstimateFee(context.Background(), core.ChainBase, core.AssetETH, big.NewInt(1))
	if err != nil {
		t.Fatalf("EstimateFee() error = %v, want nil", err)
	}

	wantL2 := new(big.Int).Mul(big.NewInt(21000), big.NewInt(1000000000))
	if got.L2Fee.Cmp(wantL2) != 0 {
		t.Fatalf("L2Fee = %s, want %s (eth_estimateGas x eth_gasPrice)", got.L2Fee, wantL2)
	}
	if got.L1Fee.Cmp(wantL1) != 0 {
		t.Fatalf("L1Fee = %s, want %s (from GasPriceOracle.getL1FeeUpperBound)", got.L1Fee, wantL1)
	}
	wantTotal := new(big.Int).Add(wantL2, wantL1)
	if got.TotalFee.Cmp(wantTotal) != 0 {
		t.Fatalf("TotalFee = %s, want %s", got.TotalFee, wantTotal)
	}
}

// TestFeeEstimator_Base_RealAnvilFork_GasPriceOracle exercises the real
// GasPriceOracle predeploy against an anvil fork of a live Base RPC endpoint — genuinely
// possible (unlike NodeInterface) because GasPriceOracle has real deployed bytecode that
// survives an anvil fork (Story 3.1 Design Notes). This depends on live, third-party
// infrastructure, so it is gated behind RUN_LIVE_FORK_TESTS=1 and skipped by default:
// never required for `make test` or CI.
func TestFeeEstimator_Base_RealAnvilFork_GasPriceOracle(t *testing.T) {
	if os.Getenv("RUN_LIVE_FORK_TESTS") != "1" {
		t.Skip("set RUN_LIVE_FORK_TESTS=1 to run this opt-in test — it forks a live Base RPC via anvil (never required for make test/CI, Story 3.1 Design Notes)")
	}
	forkURL := os.Getenv("BASE_SEPOLIA_FORK_RPC_URL")
	if forkURL == "" {
		t.Skip("BASE_SEPOLIA_FORK_RPC_URL is not set — required to fork a live Base Sepolia endpoint for this test")
	}

	anvilPath, err := exec.LookPath("anvil")
	if err != nil {
		t.Skip("anvil not found on PATH — install Foundry (foundryup) to run this test")
	}

	port := freeLocalPort(t)
	cmd := exec.Command(anvilPath, "--port", port, "--silent", "--fork-url", forkURL)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start anvil fork: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	rpcURL := "http://127.0.0.1:" + port
	waitForAnvilReady(t, rpcURL)

	ctx := context.Background()
	estimator, err := NewFeeEstimator(ctx, []Chain{{Name: "base", RPCURL: rpcURL, ChainID: anvilChainID}}, &fakeTokenRegistryLister{})
	if err != nil {
		t.Fatalf("NewFeeEstimator() error = %v, want nil", err)
	}
	t.Cleanup(estimator.Close)

	got, err := estimator.EstimateFee(ctx, core.ChainBase, core.AssetETH, big.NewInt(1_000_000_000_000_000))
	if err != nil {
		t.Fatalf("EstimateFee() error = %v, want nil", err)
	}
	if got.L1Fee == nil || got.L1Fee.Sign() <= 0 {
		t.Fatalf("L1Fee = %v, want a positive value from the real GasPriceOracle predeploy", got.L1Fee)
	}
	if got.L2Fee == nil || got.L2Fee.Sign() <= 0 {
		t.Fatalf("L2Fee = %v, want a positive value", got.L2Fee)
	}
	if sum := new(big.Int).Add(got.L2Fee, got.L1Fee); sum.Cmp(got.TotalFee) != 0 {
		t.Fatalf("L2Fee + L1Fee = %s, want equal to TotalFee %s", sum, got.TotalFee)
	}
}
