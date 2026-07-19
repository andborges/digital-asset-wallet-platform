package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

// nativeTransferLogIndex is the log_index sentinel for a native ETH deposit — a plain
// top-level value transfer to a still-undeployed, counterfactual address has no log to
// key on. -1 is never a real EVM log index, so it lets native and ERC-20 transfers share
// one (chain, tx_hash, log_index) uniqueness key (AD-5).
const nativeTransferLogIndex = -1

// erc20TransferSignature is keccak256("Transfer(address,address,uint256)") — the ERC-20
// standard Transfer event's topic0, identical across every ERC-20 token including USDC.
var erc20TransferSignature = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// scanClient is the minimal RPC surface Head and ScanDeposits need — small enough to
// fake in unit tests without a real chain, the same shape as chainClient in deployer.go.
//
// CallContext (production bug fix, 2026-07-17), not BlockByNumber: go-ethereum's
// BlockByNumber decodes every transaction in a block into its strict *types.Transaction,
// which rejects any transaction type byte it doesn't recognize. Both Arbitrum (deposit/
// internal tx types 0x64-0x6A — an ArbitrumInternalTx is the first transaction of nearly
// every Nitro block) and Base/OP-stack (deposit tx type 0x7E, emitted whenever a user
// bridges from L1) routinely include such transactions in ordinary blocks, which made
// BlockByNumber fail outright in production ("transaction type not supported") — this
// wasn't caught by any anvil-based test because anvil never emits these chain-specific
// transaction types. scanNativeTransfers instead fetches raw eth_getBlockByNumber JSON via
// CallContext and decodes only to/value/hash, fields present on every EVM transaction type
// regardless of its type byte.
type scanClient interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
	CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error
	Close()
}

// scanClientImpl adapts *ethclient.Client to scanClient, adding the raw CallContext
// escape hatch (via the underlying *rpc.Client) that BlockNumber/HeaderByNumber/
// FilterLogs/Close all already satisfy directly on *ethclient.Client.
type scanClientImpl struct {
	ec *ethclient.Client
}

func (c *scanClientImpl) BlockNumber(ctx context.Context) (uint64, error) {
	return c.ec.BlockNumber(ctx)
}

func (c *scanClientImpl) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	return c.ec.HeaderByNumber(ctx, number)
}

func (c *scanClientImpl) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	return c.ec.FilterLogs(ctx, q)
}

func (c *scanClientImpl) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	return c.ec.Client().CallContext(ctx, result, method, args...)
}

func (c *scanClientImpl) Close() {
	c.ec.Close()
}

// rawBlockTransaction is the minimal per-transaction shape scanNativeTransfers needs,
// decoded directly from raw eth_getBlockByNumber JSON — see scanClient's doc comment for
// why. to/value/hash are present on every EVM transaction type's JSON representation
// regardless of its "type" field, so decoding just these three tolerates any type byte,
// recognized or not.
type rawBlockTransaction struct {
	Hash  common.Hash     `json:"hash"`
	To    *common.Address `json:"to"`
	Value *hexutil.Big    `json:"value"`
}

// rawBlock is the minimal eth_getBlockByNumber response shape scanNativeTransfers needs.
type rawBlock struct {
	Hash         common.Hash           `json:"hash"`
	Transactions []rawBlockTransaction `json:"transactions"`
}

// Scanner implements core.ChainScanner against one configured chain's RPC endpoint via
// go-ethereum's ethclient — the chain adapter's boundary (AD-1): nothing outside
// internal/adapter/evm imports go-ethereum or knows this chain's RPC details.
type Scanner struct {
	chain  Chain
	client scanClient
}

// NewScanner dials chain's RPC endpoint once and returns a core.ChainScanner bound to
// it. One Scanner per configured chain, matching one watcher OS process per chain (AD-2).
// Call Close when the watcher process shuts down.
func NewScanner(ctx context.Context, chain Chain) (*Scanner, error) {
	client, err := ethclient.DialContext(ctx, chain.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("connect to %s RPC %q: %w", chain.Name, chain.RPCURL, err)
	}
	return &Scanner{chain: chain, client: &scanClientImpl{ec: client}}, nil
}

// Close releases the underlying RPC connection.
func (s *Scanner) Close() {
	s.client.Close()
}

// Head returns the chain's current head block (eth_blockNumber), its current "safe"
// block (eth_getBlockByNumber("safe", false)), and its current "finalized" block
// (eth_getBlockByNumber("finalized", false), Story 2.2). Both Base (OP-stack) and
// Arbitrum support the standard safe and finalized tags; if the configured RPC endpoint
// does not support either, this returns an error — never a silent "head minus N blocks"
// approximation.
func (s *Scanner) Head(ctx context.Context) (latest, safe, finalized uint64, err error) {
	latest, err = s.client.BlockNumber(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("query %s for head block: %w", s.chain.Name, err)
	}

	safeHeader, err := s.client.HeaderByNumber(ctx, big.NewInt(int64(rpc.SafeBlockNumber)))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("query %s for safe block (RPC endpoint may not support the safe tag): %w", s.chain.Name, err)
	}
	if safeHeader == nil || safeHeader.Number == nil {
		return 0, 0, 0, fmt.Errorf("query %s for safe block: RPC returned no header", s.chain.Name)
	}

	finalizedHeader, err := s.client.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("query %s for finalized block (RPC endpoint may not support the finalized tag): %w", s.chain.Name, err)
	}
	if finalizedHeader == nil || finalizedHeader.Number == nil {
		return 0, 0, 0, fmt.Errorf("query %s for finalized block: RPC returned no header", s.chain.Name)
	}

	return latest, safeHeader.Number.Uint64(), finalizedHeader.Number.Uint64(), nil
}

// BlockHash returns the chain's CURRENT block hash at blockNumber (Story 2.4) — the value
// TrackDeposits.Execute's reorg-check phase compares against a pending deposit's stored
// block_hash. exists is false, with no error, when the RPC reports no header at that
// height at all (empirically confirmed: go-ethereum's ethclient.HeaderByNumber returns the
// sentinel ethereum.NotFound when eth_getBlockByNumber responds with a null result, which
// is exactly what happens when the requested height exceeds the chain's current head) —
// this is the "chain got shorter than the deposit's height" case (Design Notes' I/O
// matrix), a normal outcome of a reorg, never a failed poll.
func (s *Scanner) BlockHash(ctx context.Context, blockNumber uint64) (string, bool, error) {
	header, err := s.client.HeaderByNumber(ctx, new(big.Int).SetUint64(blockNumber))
	if errors.Is(err, ethereum.NotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query %s for block %d header: %w", s.chain.Name, blockNumber, err)
	}
	return header.Hash().Hex(), true, nil
}

// ScanDeposits scans the inclusive block range [fromBlock, toBlock] for ETH/ERC-20
// transfers landing on any address in knownAddresses. Native ETH transfers have no log
// (a plain top-level value transfer to a still-undeployed, counterfactual address), so
// they are found by scanning every block's transactions for tx.To() in knownAddresses;
// ERC-20 transfers are found via a single eth_getLogs Transfer filter, unfiltered by
// contract address (Story 2.3 — see scanERC20Transfers), and classified per log against
// tokenRegistry into ordinary observed deposits vs. unsupported-token observations.
// Attribution is only via knownAddresses (Story 1.5's deposit_addresses table, AD-8) — an
// address is never re-derived here.
func (s *Scanner) ScanDeposits(ctx context.Context, knownAddresses []string, tokenRegistry map[string]core.Asset, fromBlock, toBlock uint64) ([]core.ObservedTransfer, []core.UnsupportedTokenObservation, error) {
	if fromBlock > toBlock {
		return nil, nil, nil
	}

	known := make(map[common.Address]string, len(knownAddresses))
	for _, a := range knownAddresses {
		known[common.HexToAddress(a)] = a
	}

	var transfers []core.ObservedTransfer

	nativeTransfers, err := s.scanNativeTransfers(ctx, known, fromBlock, toBlock)
	if err != nil {
		return nil, nil, err
	}
	transfers = append(transfers, nativeTransfers...)

	erc20Transfers, unsupported, err := s.scanERC20Transfers(ctx, known, tokenRegistry, fromBlock, toBlock)
	if err != nil {
		return nil, nil, err
	}
	transfers = append(transfers, erc20Transfers...)

	return transfers, unsupported, nil
}

// scanNativeTransfers walks every block in [fromBlock, toBlock], looking for plain value
// transfers whose tx.To() lands on a known deposit address. There is no log to filter on
// for a native transfer, so every transaction in range must be inspected directly.
//
// Fetched via raw eth_getBlockByNumber JSON (CallContext), not go-ethereum's BlockByNumber
// (production bug fix, 2026-07-17 — see scanClient's doc comment): BlockByNumber's typed
// transaction decoding rejects Arbitrum/OP-stack-style transaction types that routinely
// appear in ordinary blocks on both configured chains.
func (s *Scanner) scanNativeTransfers(ctx context.Context, known map[common.Address]string, fromBlock, toBlock uint64) ([]core.ObservedTransfer, error) {
	var transfers []core.ObservedTransfer

	for blockNum := fromBlock; blockNum <= toBlock; blockNum++ {
		var block rawBlock
		if err := s.client.CallContext(ctx, &block, "eth_getBlockByNumber", hexutil.EncodeUint64(blockNum), true); err != nil {
			return nil, fmt.Errorf("query %s for block %d: %w", s.chain.Name, blockNum, err)
		}
		for _, tx := range block.Transactions {
			if tx.To == nil {
				// Contract creation — never a transfer to an already-known address.
				continue
			}
			addr, ok := known[*tx.To]
			if !ok {
				continue
			}
			value := (*big.Int)(tx.Value)
			if value == nil || value.Sign() <= 0 {
				continue
			}
			transfers = append(transfers, core.ObservedTransfer{
				Chain:       core.Chain(s.chain.Name),
				Asset:       core.AssetETH,
				Address:     addr,
				TxHash:      tx.Hash.Hex(),
				LogIndex:    nativeTransferLogIndex,
				Amount:      new(big.Int).Set(value),
				BlockNumber: blockNum,
				// BlockHash comes from the block already fetched above (Story 2.4) — never
				// a second RPC round-trip just to learn a hash this call already has.
				BlockHash: block.Hash.Hex(),
			})
		}
	}

	return transfers, nil
}

// scanERC20Transfers finds every ERC-20 Transfer event landing on a known deposit
// address, from ANY token contract (Story 2.3) — the eth_getLogs query is deliberately
// not scoped to a contract address (Design Notes: classifying "unsupported" requires
// actually seeing those logs; a query still scoped to known contracts would filter
// unsupported transfers out before classification ever ran, making AC1 unimplementable).
// Each returned log's emitting contract address (lowercased) is looked up in
// tokenRegistry: a hit produces an ObservedTransfer using the registry's mapped asset (not
// a hardcoded core.AssetUSDC); a miss produces an UnsupportedTokenObservation. This makes
// registering a SECOND CONTRACT ADDRESS for an asset this system already knows (e.g. a
// bridged/wrapped USDC variant at a different address on the same chain) a registry row
// alone (FR34) — no code change here. Recognizing a genuinely NEW asset TYPE still
// requires extending core.Asset's closed enum (and the deposits/crediting_policy CHECK
// constraints that enumerate it) regardless of this registry; token_registry.asset's own
// CHECK is deliberately tightened to 'usdc' only (re-review 2026-07-17, same reasoning as
// Story 2.2's crediting_policy tightening) so that overclaim can't be made by accident.
func (s *Scanner) scanERC20Transfers(ctx context.Context, known map[common.Address]string, tokenRegistry map[string]core.Asset, fromBlock, toBlock uint64) ([]core.ObservedTransfer, []core.UnsupportedTokenObservation, error) {
	if len(known) == 0 {
		return nil, nil, nil
	}

	toTopics := make([]common.Hash, 0, len(known))
	for addr := range known {
		toTopics = append(toTopics, common.BytesToHash(addr.Bytes()))
	}

	// No Addresses filter (Story 2.3): only the Transfer topic0 and the known-deposit-
	// address topic2 filters remain, so a Transfer log from ANY token contract landing on
	// a known address is returned, not just this chain's registered tokens.
	query := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
		Topics:    [][]common.Hash{{erc20TransferSignature}, nil, toTopics},
	}
	logs, err := s.client.FilterLogs(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("query %s for ERC-20 transfer logs: %w", s.chain.Name, err)
	}

	var transfers []core.ObservedTransfer
	var unsupported []core.UnsupportedTokenObservation
	for _, l := range logs {
		if l.Removed || len(l.Topics) != 3 || len(l.Data) != 32 {
			// A standard Transfer(address,address,uint256) log always has exactly 3
			// topics (signature, from, to) and a 32-byte uint256-encoded Data payload;
			// anything else is not a well-formed Transfer event and is skipped rather
			// than guessed at. The Data-length check matters specifically because this
			// story removes the Addresses filter (re-review 2026-07-17): with no
			// eth_getLogs contract allowlist, a log with this topic shape can originate
			// from ANY contract, including an adversarial one emitting an
			// arbitrary-length Data payload — without this check, big.Int.SetBytes would
			// happily decode it into a value exceeding unsupported_token_observations.
			// amount's NUMERIC(78,0) range, failing every insert (and therefore the whole
			// poll) identically forever on the same block range.
			continue
		}
		toAddr := common.BytesToAddress(l.Topics[2].Bytes())
		addr, ok := known[toAddr]
		if !ok {
			continue
		}
		// SetBytes always produces a non-negative value (it decodes Data as an unsigned
		// magnitude) — Sign() <= 0 here can only ever catch zero, never a negative
		// decode. A standards-valid zero-value Transfer event carries no actual deposit,
		// so it's skipped, mirroring scanNativeTransfers' tx.Value().Sign() <= 0 guard
		// (re-review 2026-07-16). Applies identically whether the contract is registered
		// or not.
		amount := new(big.Int).SetBytes(l.Data)
		if amount.Sign() <= 0 {
			continue
		}

		// The registry lookup is case-normalized (lowercase hex) on both sides: log
		// addresses come back from go-ethereum EIP-55 checksummed, while
		// postgres.TokenRegistry.UpsertToken stores contract_address lowercased — Ethereum
		// addresses are case-insensitive at the byte level, so both sides must agree on
		// one canonical case for the lookup to ever hit.
		asset, ok := tokenRegistry[strings.ToLower(l.Address.Hex())]
		if !ok {
			unsupported = append(unsupported, core.UnsupportedTokenObservation{
				Chain:           core.Chain(s.chain.Name),
				Address:         addr,
				ContractAddress: l.Address.Hex(),
				TxHash:          l.TxHash.Hex(),
				LogIndex:        int(l.Index),
				Amount:          amount,
				BlockNumber:     l.BlockNumber,
			})
			continue
		}

		transfers = append(transfers, core.ObservedTransfer{
			Chain:       core.Chain(s.chain.Name),
			Asset:       asset,
			Address:     addr,
			TxHash:      l.TxHash.Hex(),
			LogIndex:    int(l.Index),
			Amount:      amount,
			BlockNumber: l.BlockNumber,
			// BlockHash comes from the log itself (Story 2.4) — go-ethereum populates
			// types.Log.BlockHash from the same eth_getLogs response, never a second RPC
			// round-trip just to learn a hash this call already has.
			BlockHash: l.BlockHash.Hex(),
		})
	}

	return transfers, unsupported, nil
}

var _ core.ChainScanner = (*Scanner)(nil)
