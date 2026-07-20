package evm

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// nodeInterfaceAddress is Arbitrum's ArbOS-native NodeInterface precompile (Story 3.1) —
// confirmed empirically to carry no deployed bytecode at all (cast code against a real
// endpoint returns "0x" absence under anvil fork mode; only a genuine Arbitrum node
// answers this call correctly). This is why calls to it go through a raw eth_call
// (CallContext), never a typed/bound contract, and why its Go-side logic is unit-tested
// against a fake RPC response (fee_estimator_test.go) rather than a live fork.
var nodeInterfaceAddress = common.HexToAddress("0x00000000000000000000000000000000000000C8")

// gasPriceOracleAddress is Base's (OP-stack) GasPriceOracle predeploy (Story 3.1) — unlike
// NodeInterface, this carries real deployed bytecode, confirmed present under anvil fork
// mode. (Re-review: the original constant here was one zero byte short — 19 bytes, not
// 20 — a transcription bug caught by adversarial review; verify byte length with
// common.HexToAddress before ever hand-editing this again.)
var gasPriceOracleAddress = common.HexToAddress("0x420000000000000000000000000000000000000F")

// feeEstimatePlaceholderAddress stands in for a withdrawal's destination in the
// representative transaction used to estimate gas (Story 3.1 Design Notes): this
// endpoint's inputs (chain, asset, amount) carry no real destination address, and gas
// cost for a plain value transfer or a standard transfer(address,uint256) call is
// materially insensitive to the destination (aside from cold/warm storage access, which
// can't be known in advance for an estimate anyway). Never a real address — documented
// as intentional, never mistaken for one.
var feeEstimatePlaceholderAddress = common.HexToAddress("0x000000000000000000000000000000000000dEaD")

// nodeInterfaceABI packs/unpacks NodeInterface.gasEstimateComponents calldata for a raw
// eth_call — NodeInterface has no deployed bytecode (see nodeInterfaceAddress), so this
// exists purely as an encoding/decoding helper, never as a typed/bound contract.
var nodeInterfaceABI = mustParseFeeEstimatorABI(`[{
	"name": "gasEstimateComponents",
	"type": "function",
	"inputs": [
		{"name": "to", "type": "address"},
		{"name": "contractCreation", "type": "bool"},
		{"name": "data", "type": "bytes"}
	],
	"outputs": [
		{"name": "gasEstimate", "type": "uint64"},
		{"name": "gasEstimateForL1", "type": "uint64"},
		{"name": "baseFee", "type": "uint256"},
		{"name": "l1BaseFeeEstimate", "type": "uint256"}
	]
}]`)

// erc20TransferABI packs a representative transfer(address,uint256) calldata payload,
// used for USDC fee estimation on both chains (Story 3.1).
var erc20TransferABI = mustParseFeeEstimatorABI(`[{
	"name": "transfer",
	"type": "function",
	"inputs": [
		{"name": "to", "type": "address"},
		{"name": "amount", "type": "uint256"}
	],
	"outputs": [{"name": "", "type": "bool"}]
}]`)

// getL1FeeUpperBoundABI packs/unpacks Base's GasPriceOracle.getL1FeeUpperBound(uint256)
// calldata — the OP-stack method documented specifically for a pre-signature estimate
// (Fjord+; Base Sepolia is already on Jovian), since no real signed transaction exists yet
// at estimate time (Story 3.1 Design Notes). Unlike NodeInterface, GasPriceOracle is a
// real deployed contract, but this ABI is used the same way (Pack/Unpack around a raw
// eth_call via CallContext) for one consistent code path.
var getL1FeeUpperBoundABI = mustParseFeeEstimatorABI(`[{
	"name": "getL1FeeUpperBound",
	"type": "function",
	"inputs": [{"name": "unsignedTxSize", "type": "uint256"}],
	"outputs": [{"name": "", "type": "uint256"}]
}]`)

// mustParseFeeEstimatorABI parses one of this file's fixed ABI JSON constants, panicking
// on failure — a parse failure here would mean a transcription bug in this file, not bad
// runtime input (mirrors address.go's mustHexToHash32 panic-on-programmer-error pattern).
func mustParseFeeEstimatorABI(jsonABI string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(jsonABI))
	if err != nil {
		panic(fmt.Sprintf("evm: invalid fee-estimator ABI constant: %v", err))
	}
	return parsed
}

// feeClient is the minimal RPC surface FeeEstimator needs — small enough to fake in unit
// tests without a real chain, the same shape as scanClient/chainClient in this package.
// CallContext is used for a raw eth_call (never a typed contract binding, required for
// NodeInterface's no-bytecode precompile and used identically for GasPriceOracle for one
// consistent code path); EstimateGas/SuggestGasPrice back Base's standard L2 gas
// estimate.
type feeClient interface {
	CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error
	EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	Close()
}

// feeClientImpl adapts *ethclient.Client to feeClient, adding the raw CallContext escape
// hatch (via the underlying *rpc.Client) the same way scanClientImpl does for scanClient.
type feeClientImpl struct {
	ec *ethclient.Client
}

func (c *feeClientImpl) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	return c.ec.Client().CallContext(ctx, result, method, args...)
}

func (c *feeClientImpl) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	return c.ec.EstimateGas(ctx, msg)
}

func (c *feeClientImpl) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return c.ec.SuggestGasPrice(ctx)
}

func (c *feeClientImpl) Close() {
	c.ec.Close()
}

// FeeEstimator implements core.FeeEstimator across every configured chain (Story 3.1).
// Unlike Scanner (AD-2: one watcher OS process per chain), the api role's fee-estimate
// endpoint takes chain as an input at request time, so one FeeEstimator instance holds
// one RPC client per configured chain — mirroring how runAPI's VerifyDeployerPresence
// loop already handles both chains from a single api process.
type FeeEstimator struct {
	clients             map[core.Chain]feeClient
	tokenRegistryLister core.TokenRegistryLister
}

// NewFeeEstimator dials every chain's RPC endpoint once and returns a core.FeeEstimator
// bound to all of them. Call Close when the api process shuts down.
func NewFeeEstimator(ctx context.Context, chains []Chain, tokenRegistryLister core.TokenRegistryLister) (*FeeEstimator, error) {
	clients := make(map[core.Chain]feeClient, len(chains))
	for _, chain := range chains {
		client, err := ethclient.DialContext(ctx, chain.RPCURL)
		if err != nil {
			for _, c := range clients {
				c.Close()
			}
			return nil, fmt.Errorf("connect to %s RPC %q: %w", chain.Name, chain.RPCURL, err)
		}
		clients[core.Chain(chain.Name)] = &feeClientImpl{ec: client}
	}
	return &FeeEstimator{clients: clients, tokenRegistryLister: tokenRegistryLister}, nil
}

// Close releases every underlying RPC connection.
func (f *FeeEstimator) Close() {
	for _, c := range f.clients {
		c.Close()
	}
}

// EstimateFee implements core.FeeEstimator. amount is assumed already validated positive
// by core.EstimateFee (this port's sole caller) — this method never re-validates it.
func (f *FeeEstimator) EstimateFee(ctx context.Context, chain core.Chain, asset core.Asset, amount *big.Int) (core.FeeEstimate, error) {
	client, ok := f.clients[chain]
	if !ok {
		return core.FeeEstimate{}, fmt.Errorf("fee estimation: no configured RPC client for chain %q", chain)
	}

	to, data, value, err := representativeTransaction(ctx, f.tokenRegistryLister, chain, asset, amount)
	if err != nil {
		return core.FeeEstimate{}, err
	}

	switch chain {
	case core.ChainArbitrum:
		return estimateArbitrumFee(ctx, client, to, data)
	case core.ChainBase:
		return estimateBaseFee(ctx, client, to, data, value)
	default:
		return core.FeeEstimate{}, fmt.Errorf("fee estimation not supported for chain %q", chain)
	}
}

// representativeTransaction builds the (to, data, value) triple used as this estimate's
// representative transaction (Story 3.1 Design Notes): for ETH, an empty-data transfer of
// the requested amount to the fixed placeholder address; for USDC, an ABI-encoded
// transfer(placeholder, 0) call against chain's registered USDC contract (resolved via the
// existing TokenRegistryLister port, Story 2.3 — never a new, separate USDC-address config
// path).
//
// The USDC calldata deliberately encodes amount 0, never the caller's real requested
// amount (re-review, adversarial review): gas cost and calldata length for
// transfer(address,uint256) are insensitive to the encoded value (a fixed-width uint256
// word either way — the same "insensitive to input" reasoning the Design Notes already
// apply to the destination address), and neither estimateArbitrumFee's eth_call nor
// estimateBaseFee's EstimateGas ever sets a "from" address on the simulated call — so a
// nonzero amount would make the ERC20 contract's own balance check fail against the
// zero/default sender's real (zero) USDC balance, reverting every USDC estimate. amount 0
// always passes that check trivially.
func representativeTransaction(ctx context.Context, tokenRegistryLister core.TokenRegistryLister, chain core.Chain, asset core.Asset, amount *big.Int) (to common.Address, data []byte, value *big.Int, err error) {
	switch asset {
	case core.AssetETH:
		return feeEstimatePlaceholderAddress, nil, amount, nil
	case core.AssetUSDC:
		usdcAddress, err := usdcContractAddress(ctx, tokenRegistryLister, chain)
		if err != nil {
			return common.Address{}, nil, nil, err
		}
		data, err := erc20TransferABI.Pack("transfer", feeEstimatePlaceholderAddress, big.NewInt(0))
		if err != nil {
			return common.Address{}, nil, nil, fmt.Errorf("encode representative transfer calldata: %w", err)
		}
		return usdcAddress, data, big.NewInt(0), nil
	default:
		return common.Address{}, nil, nil, fmt.Errorf("fee estimation not supported for asset %q", asset)
	}
}

// usdcContractAddress resolves chain's registered USDC contract address by inverting
// TokenRegistryLister.ListTokenRegistry's (contractAddress -> asset) map (Story 3.1
// Design Notes: Story 2.3 already solved "which contract address is USDC on this chain" —
// a 1-2-entry map, trivially cheap to search; no new port method needed for this). Returns
// an error — never a guessed contract address — if chain has no USDC entry at all (the
// "registry gap" case the story's I/O matrix requires fail loudly rather than silently),
// or if it has more than one (migration 0007 explicitly anticipates a second, bridged/
// wrapped USDC contract address being registered for the same chain — picking either one
// arbitrarily via map iteration order would make an identical request return different
// fee estimates from call to call; re-review, adversarial review).
func usdcContractAddress(ctx context.Context, tokenRegistryLister core.TokenRegistryLister, chain core.Chain) (common.Address, error) {
	registry, err := tokenRegistryLister.ListTokenRegistry(ctx, chain)
	if err != nil {
		return common.Address{}, fmt.Errorf("list token registry for chain %q: %w", chain, err)
	}
	var found []common.Address
	for contractAddress, registeredAsset := range registry {
		if registeredAsset == core.AssetUSDC {
			found = append(found, common.HexToAddress(contractAddress))
		}
	}
	switch len(found) {
	case 0:
		return common.Address{}, fmt.Errorf("no token_registry entry for USDC on chain %q", chain)
	case 1:
		return found[0], nil
	default:
		return common.Address{}, fmt.Errorf("ambiguous token_registry: %d USDC contract addresses registered on chain %q — fee estimation needs exactly one", len(found), chain)
	}
}

// ethCallParams is the eth_call request object's shape — only "to" and "data" are needed
// for a plain, non-value-transfer call.
type ethCallParams struct {
	To   common.Address `json:"to"`
	Data hexutil.Bytes  `json:"data"`
}

// ethCall issues a raw eth_call via CallContext against to with calldata data, against
// the "latest" block — required for NodeInterface (ArbOS-native, no deployed bytecode)
// and used identically for GasPriceOracle for one consistent code path (Story 3.1
// Boundaries & Constraints, Design Notes).
func ethCall(ctx context.Context, client feeClient, to common.Address, calldata []byte) ([]byte, error) {
	var result hexutil.Bytes
	if err := client.CallContext(ctx, &result, "eth_call", ethCallParams{To: to, Data: calldata}, "latest"); err != nil {
		return nil, err
	}
	return result, nil
}

// estimateArbitrumFee calls NodeInterface.gasEstimateComponents(to, false, data) via a raw
// eth_call and computes the fee split per Arbitrum's own three-term formula, algebraically
// simplified (Story 3.1 Design Notes, empirically verified against a real Arbitrum Sepolia
// call): TotalFee = gasEstimate * baseFee; L2Fee = (gasEstimate - gasEstimateForL1) *
// baseFee; L1Fee = gasEstimateForL1 * baseFee. l1BaseFeeEstimate (the fourth return value)
// is not needed once this simplification is used and is deliberately never read.
func estimateArbitrumFee(ctx context.Context, client feeClient, to common.Address, data []byte) (core.FeeEstimate, error) {
	calldata, err := nodeInterfaceABI.Pack("gasEstimateComponents", to, false, data)
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("encode gasEstimateComponents calldata: %w", err)
	}

	result, err := ethCall(ctx, client, nodeInterfaceAddress, calldata)
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("call NodeInterface.gasEstimateComponents: %w", err)
	}

	outputs, err := nodeInterfaceABI.Unpack("gasEstimateComponents", result)
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("decode gasEstimateComponents result: %w", err)
	}
	if len(outputs) != 4 {
		return core.FeeEstimate{}, fmt.Errorf("gasEstimateComponents returned %d outputs, want 4", len(outputs))
	}
	gasEstimate, ok := outputs[0].(uint64)
	if !ok {
		return core.FeeEstimate{}, fmt.Errorf("gasEstimateComponents: unexpected type %T for gasEstimate", outputs[0])
	}
	gasEstimateForL1, ok := outputs[1].(uint64)
	if !ok {
		return core.FeeEstimate{}, fmt.Errorf("gasEstimateComponents: unexpected type %T for gasEstimateForL1", outputs[1])
	}
	baseFee, ok := outputs[2].(*big.Int)
	if !ok {
		return core.FeeEstimate{}, fmt.Errorf("gasEstimateComponents: unexpected type %T for baseFee", outputs[2])
	}

	gasEstimateBig := new(big.Int).SetUint64(gasEstimate)
	gasEstimateForL1Big := new(big.Int).SetUint64(gasEstimateForL1)
	l2Gas := new(big.Int).Sub(gasEstimateBig, gasEstimateForL1Big)
	if l2Gas.Sign() < 0 {
		return core.FeeEstimate{}, fmt.Errorf("gasEstimateComponents: gasEstimateForL1 (%d) exceeds gasEstimate (%d)", gasEstimateForL1, gasEstimate)
	}

	l2Fee := new(big.Int).Mul(l2Gas, baseFee)
	l1Fee := new(big.Int).Mul(gasEstimateForL1Big, baseFee)
	totalFee := new(big.Int).Mul(gasEstimateBig, baseFee)

	return core.FeeEstimate{L2Fee: l2Fee, L1Fee: l1Fee, TotalFee: totalFee}, nil
}

// estimateBaseFee computes Base's two fee components separately (Story 3.1 Design
// Notes): the L2 component from a standard eth_estimateGas x current eth_gasPrice call on
// the representative transaction, and the L1 component from GasPriceOracle's
// getL1FeeUpperBound(unsignedTxSize) — the OP-stack method documented specifically for a
// pre-signature estimate (Fjord+; Base Sepolia is already on Jovian) — given a
// representative unsigned transaction's RLP/binary-encoded byte size. value is the real
// on-chain native-ETH value the transaction would carry (the requested amount for an ETH
// withdrawal, always 0 for a USDC transfer) — it must flow into the tx-size calculation
// below, since a nonzero Value field changes the transaction's RLP-encoded byte length and
// therefore its L1 data fee (re-review, adversarial review: hardcoding 0 here undercounted
// — and so undercharged — every large ETH withdrawal's L1 fee).
func estimateBaseFee(ctx context.Context, client feeClient, to common.Address, data []byte, value *big.Int) (core.FeeEstimate, error) {
	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{To: &to, Data: data, Value: value})
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("estimate gas: %w", err)
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("suggest gas price: %w", err)
	}
	l2Fee := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), gasPrice)

	txSize := representativeUnsignedTxSize(to, data, value, gasLimit, gasPrice)
	calldata, err := getL1FeeUpperBoundABI.Pack("getL1FeeUpperBound", txSize)
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("encode getL1FeeUpperBound calldata: %w", err)
	}
	result, err := ethCall(ctx, client, gasPriceOracleAddress, calldata)
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("call GasPriceOracle.getL1FeeUpperBound: %w", err)
	}
	outputs, err := getL1FeeUpperBoundABI.Unpack("getL1FeeUpperBound", result)
	if err != nil {
		return core.FeeEstimate{}, fmt.Errorf("decode getL1FeeUpperBound result: %w", err)
	}
	if len(outputs) != 1 {
		return core.FeeEstimate{}, fmt.Errorf("getL1FeeUpperBound returned %d outputs, want 1", len(outputs))
	}
	l1Fee, ok := outputs[0].(*big.Int)
	if !ok {
		return core.FeeEstimate{}, fmt.Errorf("getL1FeeUpperBound: unexpected type %T for result", outputs[0])
	}

	totalFee := new(big.Int).Add(l2Fee, l1Fee)
	return core.FeeEstimate{L2Fee: l2Fee, L1Fee: l1Fee, TotalFee: totalFee}, nil
}

// representativeUnsignedTxSize returns the byte length of a representative EIP-1559
// (dynamic-fee) transaction envelope built from the given fields, encoded WITHOUT a
// signature (zero V/R/S) — literally the "unsigned transaction" getL1FeeUpperBound's
// parameter name calls for (Story 3.1 Design Notes): no real signature exists yet at
// estimate time, and the oracle's own "UpperBound" framing accounts for the signature
// overhead this omits. Nonce is fixed at 0 and ChainID is omitted (zero) — this size
// estimate is insensitive to either field's exact value, only to the envelope's overall
// shape (to/data/gas/value fields present, one dynamic-fee-tx envelope). value must be the
// real representative value (see estimateBaseFee) — RLP-encodes as a variable-length
// integer, so a zero value is 1 byte while a large ETH amount can add ~32 bytes, which
// this size estimate must reflect to avoid undercharging the L1 fee.
func representativeUnsignedTxSize(to common.Address, data []byte, value *big.Int, gasLimit uint64, gasPrice *big.Int) *big.Int {
	tx := types.NewTx(&types.DynamicFeeTx{
		Nonce:     0,
		GasTipCap: gasPrice,
		GasFeeCap: gasPrice,
		Gas:       gasLimit,
		To:        &to,
		Value:     value,
		Data:      data,
	})
	encoded, err := tx.MarshalBinary()
	if err != nil {
		// Every field above is a well-formed Go value this function itself constructed —
		// MarshalBinary failing here would be a bug in this function, never bad external
		// input (mirrors address.go's mustHexToHash32 panic-on-programmer-error pattern).
		panic(fmt.Sprintf("evm: marshal representative unsigned transaction: %v", err))
	}
	return new(big.Int).SetUint64(uint64(len(encoded)))
}

var _ core.FeeEstimator = (*FeeEstimator)(nil)
