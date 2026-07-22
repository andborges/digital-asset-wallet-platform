package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// broadcastClient is the minimal RPC surface Broadcaster needs — small enough to fake in
// unit tests without a real chain, the same shape as scanClient/feeClient in this package.
type broadcastClient interface {
	EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	Close()
}

// broadcastClientImpl adapts *ethclient.Client to broadcastClient.
type broadcastClientImpl struct {
	ec *ethclient.Client
}

func (c *broadcastClientImpl) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	return c.ec.EstimateGas(ctx, msg)
}

func (c *broadcastClientImpl) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return c.ec.SuggestGasPrice(ctx)
}

func (c *broadcastClientImpl) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	// ethclient.Client.SendTransaction RLP-encodes tx and issues eth_sendRawTransaction —
	// exactly the wire call the Code Map calls for, via the standard library rather than a
	// hand-rolled raw CallContext (Story 3.1's fee estimator already establishes that raw
	// CallContext is reserved for endpoints with no typed client method, e.g. NodeInterface;
	// eth_sendRawTransaction has one here, so it is used).
	return c.ec.SendTransaction(ctx, tx)
}

func (c *broadcastClientImpl) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	return c.ec.TransactionReceipt(ctx, txHash)
}

func (c *broadcastClientImpl) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	return c.ec.HeaderByNumber(ctx, number)
}

func (c *broadcastClientImpl) Close() {
	c.ec.Close()
}

// Broadcaster implements core.TransactionBroadcaster against one configured chain's RPC
// endpoint (Story 3.4) — the chain adapter's boundary (AD-1): nothing outside
// internal/adapter/evm imports go-ethereum, RLP, or raw-transaction code. One Broadcaster
// per configured chain, matching one broadcaster OS process per chain (AD-11, mirroring
// Scanner's identical one-instance-per-chain shape).
type Broadcaster struct {
	chain               Chain
	chainIDBig          *big.Int
	client              broadcastClient
	tokenRegistryLister core.TokenRegistryLister
}

// NewBroadcaster dials chain's RPC endpoint once and returns a core.TransactionBroadcaster
// bound to it. tokenRegistryLister resolves chain's registered USDC contract address for
// asset == core.AssetUSDC withdrawals (Story 3.1's fee_estimator.go already established this
// exact "invert the (contractAddress -> asset) registry" resolution — reused here via the
// package-level usdcContractAddress helper, not duplicated). Call Close when the
// broadcaster process shuts down.
func NewBroadcaster(ctx context.Context, chain Chain, tokenRegistryLister core.TokenRegistryLister) (*Broadcaster, error) {
	client, err := ethclient.DialContext(ctx, chain.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("connect to %s RPC %q: %w", chain.Name, chain.RPCURL, err)
	}
	return &Broadcaster{
		chain:               chain,
		chainIDBig:          new(big.Int).SetUint64(chain.ChainID),
		client:              &broadcastClientImpl{ec: client},
		tokenRegistryLister: tokenRegistryLister,
	}, nil
}

// Close releases the underlying RPC connection.
func (b *Broadcaster) Close() {
	b.client.Close()
}

// checkChain returns an error if chain does not match this Broadcaster's own configured
// chain — defensive: a Broadcaster instance is bound to exactly one chain at construction
// (mirroring Scanner), so a mismatched chain argument is a caller bug, never a runtime
// condition to route around.
func (b *Broadcaster) checkChain(chain core.Chain) error {
	if core.Chain(b.chain.Name) != chain {
		return fmt.Errorf("broadcaster configured for chain %q, got %q", b.chain.Name, chain)
	}
	return nil
}

// BuildUnsignedWithdrawal implements core.TransactionBroadcaster: constructs an unsigned
// EIP-1559 dynamic-fee transaction (Design Notes: go-ethereum's standard
// types.NewTx(&types.DynamicFeeTx{...})) for a plain ETH value transfer (asset ==
// core.AssetETH) or an ABI-encoded ERC-20 transfer(to, amount) call against chain's
// registered USDC contract (asset == core.AssetUSDC), using nonce as already allocated by
// WithdrawalRepository.ClaimApprovedWithdrawal. Gas parameters reuse the same
// EstimateGas/SuggestGasPrice shape Story 3.1's fee_estimator.go already established for
// Base's L2 gas estimate — Story 3.1's own FeeEstimator port is deliberately NOT wired in
// here (Design Notes: its shape doesn't line up cleanly with "the real gas this specific
// transaction needs to actually broadcast," as opposed to a representative estimate), a
// known, accepted gap rather than an awkward integration.
func (b *Broadcaster) BuildUnsignedWithdrawal(ctx context.Context, chain core.Chain, asset core.Asset, nonce int64, to string, amount *big.Int) (digest [32]byte, unsignedTx []byte, err error) {
	if err := b.checkChain(chain); err != nil {
		return [32]byte{}, nil, err
	}
	if nonce < 0 {
		return [32]byte{}, nil, fmt.Errorf("nonce must be non-negative, got %d", nonce)
	}
	if amount == nil || amount.Sign() <= 0 {
		return [32]byte{}, nil, fmt.Errorf("amount must be positive, got %v", amount)
	}

	destination := common.HexToAddress(to)

	var (
		txTo  common.Address
		data  []byte
		value *big.Int
		// estimateData is the calldata used ONLY for the EstimateGas simulation below —
		// identical to data for ETH (a plain value transfer has no contract code to
		// revert), but for USDC it deliberately encodes amount 0 instead of the real
		// amount (re-review 2026-07-21, mirrors fee_estimator.go's representativeTransaction
		// doc comment exactly): neither this call nor Arbitrum's analogous eth_call ever
		// sets a "from" address, so simulating transfer(to, <the real amount>) would fail
		// the ERC-20 contract's OWN require(balance[from] >= amount) check against the
		// zero/default sender's real (zero) USDC balance on any real chain/contract —
		// reverting gas estimation for every real USDC withdrawal. Gas cost for
		// transfer(address,uint256) is insensitive to the encoded value (a fixed-width
		// uint256 word either way), so amount 0 estimates identically to the real amount
		// while never tripping that guard. The REAL amount still reaches the chain: data
		// (not estimateData) is what's actually signed and broadcast below.
		estimateData []byte
	)
	switch asset {
	case core.AssetETH:
		txTo, data, value = destination, nil, amount
		estimateData = data
	case core.AssetUSDC:
		usdcAddress, err := usdcContractAddress(ctx, b.tokenRegistryLister, chain)
		if err != nil {
			return [32]byte{}, nil, err
		}
		packed, err := erc20TransferABI.Pack("transfer", destination, amount)
		if err != nil {
			return [32]byte{}, nil, fmt.Errorf("encode withdrawal transfer calldata: %w", err)
		}
		estimatePacked, err := erc20TransferABI.Pack("transfer", destination, big.NewInt(0))
		if err != nil {
			return [32]byte{}, nil, fmt.Errorf("encode representative withdrawal transfer calldata: %w", err)
		}
		txTo, data, value = usdcAddress, packed, big.NewInt(0)
		estimateData = estimatePacked
	default:
		return [32]byte{}, nil, fmt.Errorf("broadcasting not supported for asset %q", asset)
	}

	gasLimit, err := b.client.EstimateGas(ctx, ethereum.CallMsg{To: &txTo, Data: estimateData, Value: value})
	if err != nil {
		return [32]byte{}, nil, fmt.Errorf("estimate gas: %w", err)
	}
	gasPrice, err := b.client.SuggestGasPrice(ctx)
	if err != nil {
		return [32]byte{}, nil, fmt.Errorf("suggest gas price: %w", err)
	}

	// GasFeeCap gets a fixed 20% headroom over the current suggested price (re-review
	// 2026-07-21) — fee-bump/replacement is explicitly out of this story's scope (Boundaries
	// & Constraints), so a withdrawal whose cap is exactly today's price has zero margin
	// against even a small base-fee increase before inclusion and would simply never mine,
	// with no remediation path until a future story adds one. This does not eliminate that
	// risk (a large or sustained base-fee spike can still exceed the buffer) — it only
	// reduces how often the no-headroom case is hit, which is the full extent of what a
	// mechanical fix can responsibly do without building replacement logic. GasTipCap stays
	// at the raw suggested price — it is not the field that determines whether the network
	// will include the transaction at all (GasFeeCap is), so it gets no artificial inflation.
	gasFeeCap := new(big.Int).Div(new(big.Int).Mul(gasPrice, big.NewInt(120)), big.NewInt(100))

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   b.chainIDBig,
		Nonce:     uint64(nonce),
		GasTipCap: gasPrice,
		GasFeeCap: gasFeeCap,
		Gas:       gasLimit,
		To:        &txTo,
		Value:     value,
		Data:      data,
	})

	signer := types.LatestSignerForChainID(b.chainIDBig)
	hash := signer.Hash(tx)

	encoded, err := tx.MarshalBinary()
	if err != nil {
		return [32]byte{}, nil, fmt.Errorf("marshal unsigned withdrawal tx: %w", err)
	}

	return [32]byte(hash), encoded, nil
}

// AssembleSignedTx implements core.TransactionBroadcaster: decodes unsignedTx back into a
// *types.Transaction, attaches signature (Signer's own 65-byte r||s||v output — exactly the
// shape types.Transaction.WithSignature expects), and re-encodes the now-signed
// transaction. Pure and deterministic given its inputs, so it performs no chain I/O and
// takes no context or chain parameter — the transaction's own ChainID (already embedded in
// unsignedTx by BuildUnsignedWithdrawal) determines which EIP-1559 signer to reconstruct.
func (b *Broadcaster) AssembleSignedTx(unsignedTx []byte, signature [65]byte) (signedTx []byte, txHash string, err error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(unsignedTx); err != nil {
		return nil, "", fmt.Errorf("unmarshal unsigned withdrawal tx: %w", err)
	}

	signer := types.LatestSignerForChainID(tx.ChainId())
	signed, err := tx.WithSignature(signer, signature[:])
	if err != nil {
		return nil, "", fmt.Errorf("attach signature to withdrawal tx: %w", err)
	}

	encoded, err := signed.MarshalBinary()
	if err != nil {
		return nil, "", fmt.Errorf("marshal signed withdrawal tx: %w", err)
	}

	return encoded, signed.Hash().Hex(), nil
}

// SendRawTransaction implements core.TransactionBroadcaster: decodes signedTx and sends it
// via eth_sendRawTransaction (see broadcastClientImpl.SendTransaction's doc comment).
func (b *Broadcaster) SendRawTransaction(ctx context.Context, chain core.Chain, signedTx []byte) error {
	if err := b.checkChain(chain); err != nil {
		return err
	}

	var tx types.Transaction
	if err := tx.UnmarshalBinary(signedTx); err != nil {
		return fmt.Errorf("unmarshal signed withdrawal tx: %w", err)
	}
	if err := b.client.SendTransaction(ctx, &tx); err != nil {
		return fmt.Errorf("send raw withdrawal transaction: %w", err)
	}
	return nil
}

// GetFinalizedReceipt implements core.TransactionBroadcaster: mirrors Scanner.Head's own
// "finalized" tag query (AD-7) to determine whether txHash's receipt has landed at or below
// the chain's current finalized block. found is false, with no error, both when no receipt
// exists yet (ethereum.NotFound) and when a receipt exists but its block hasn't reached
// "finalized" yet — both mean "keep polling," never an error.
func (b *Broadcaster) GetFinalizedReceipt(ctx context.Context, chain core.Chain, txHash string) (found, success bool, err error) {
	if err := b.checkChain(chain); err != nil {
		return false, false, err
	}

	receipt, err := b.client.TransactionReceipt(ctx, common.HexToHash(txHash))
	if errors.Is(err, ethereum.NotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("query %s for withdrawal tx %s receipt: %w", b.chain.Name, txHash, err)
	}

	finalizedHeader, err := b.client.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return false, false, fmt.Errorf("query %s for finalized block (RPC endpoint may not support the finalized tag): %w", b.chain.Name, err)
	}
	if finalizedHeader == nil || finalizedHeader.Number == nil {
		return false, false, fmt.Errorf("query %s for finalized block: RPC returned no header", b.chain.Name)
	}

	if receipt.BlockNumber == nil || receipt.BlockNumber.Cmp(finalizedHeader.Number) > 0 {
		// The receipt exists but its block hasn't reached the finalized tag yet — an
		// ordinary "keep polling" outcome, never an error (mirrors ethereum.NotFound's own
		// treatment above).
		return false, false, nil
	}

	return true, receipt.Status == types.ReceiptStatusSuccessful, nil
}

var _ core.TransactionBroadcaster = (*Broadcaster)(nil)
