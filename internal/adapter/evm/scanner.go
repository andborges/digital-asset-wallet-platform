package evm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// callFrame is the callTracer JSON shape debug_traceBlockByNumber returns for one call (or
// subcall) — matching go-ethereum's eth/tracers/native.callFrame's own JSON marshaling
// (Type marshals as a plain string like "CALL"/"STATICCALL"/"CREATE"; Value may be omitted
// or present as zero when the call carries no value — either way the != nil and Sign() > 0
// guards in scanInternalTransfers filter it out safely). Only the fields
// scanInternalTransfers actually needs are decoded — the same "decode just what's needed"
// discipline as rawBlockTransaction above. Error is non-empty when this specific call
// reverted, ran out of gas, or hit an invalid opcode (re-review 2026-07-20): the EVM rolls
// back every state change made during such a call's execution, including any value it
// transferred, so a frame with a non-empty Error — or a descendant of one — never actually
// moved value and must never be recorded as a transfer.
type callFrame struct {
	Type  string          `json:"type"`
	From  common.Address  `json:"from"`
	To    *common.Address `json:"to"`
	Value *hexutil.Big    `json:"value"`
	Error string          `json:"error"`
	Calls []callFrame     `json:"calls"`
}

// txCallTrace is one element of debug_traceBlockByNumber's callTracer response array — one
// per transaction in the block. TxHash (go-ethereum's eth/tracers.txTraceResult, "txHash")
// is what lets an internal transfer be attributed to the correct (chain, tx_hash) key
// directly, without relying on the trace array's ordering matching rawBlock.Transactions'.
// Error (re-review 2026-07-20) is non-empty when tracing THIS transaction itself failed
// (independently of the rest of the block's trace succeeding) — Result is a zero value in
// that case, not a real (empty) call tree, so it must be skipped rather than walked.
type txCallTrace struct {
	TxHash common.Hash `json:"txHash"`
	Result callFrame   `json:"result"`
	Error  string      `json:"error"`
}

// Scanner implements core.ChainScanner against one configured chain's RPC endpoint via
// go-ethereum's ethclient — the chain adapter's boundary (AD-1): nothing outside
// internal/adapter/evm imports go-ethereum or knows this chain's RPC details.
type Scanner struct {
	chain  Chain
	client scanClient
	logger *slog.Logger

	// internalTraceDisabled is set once, permanently, the first time
	// debug_traceBlockByNumber fails for any reason (unsupported method, quota error,
	// malformed response) — and never cleared or retried for the rest of this Scanner
	// instance's lifetime. Both observed failure modes (method not found; quota/tier
	// gating) are permanent for the life of a given RPC configuration (Design Notes), so a
	// watcher process restart — already the natural re-probe point per AD-2's one-OS-
	// process-per-chain model — is what re-enables this pass, never an in-process retry.
	// Unsynchronized: safe only because, like every other Scanner field, it is only ever
	// read/written from the single goroutine that drives one Scanner instance's sequential
	// poll loop (AD-2) — never call ScanDeposits on the same Scanner from two goroutines.
	internalTraceDisabled bool
}

// NewScanner dials chain's RPC endpoint once and returns a core.ChainScanner bound to
// it. One Scanner per configured chain, matching one watcher OS process per chain (AD-2).
// logger is used exclusively to log the one-time Warn when debug_traceBlockByNumber turns
// out to be unsupported (see scanInternalTransfers) — every other Scanner method returns
// its own errors to the caller instead of logging. Call Close when the watcher process
// shuts down.
func NewScanner(ctx context.Context, chain Chain, logger *slog.Logger) (*Scanner, error) {
	client, err := ethclient.DialContext(ctx, chain.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("connect to %s RPC %q: %w", chain.Name, chain.RPCURL, err)
	}
	return &Scanner{chain: chain, client: &scanClientImpl{ec: client}, logger: logger}, nil
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
//
// In this same per-block loop, after the top-level tx.To()/tx.Value() pass below, this also
// runs scanInternalTransfers — a second, best-effort pass over the SAME already-fetched
// block for ETH that reached a known address via an internal CALL rather than a top-level
// transfer (see scanInternalTransfers' doc comment). One debug_traceBlockByNumber call per
// block, not per transaction, mirroring this function's own one eth_getBlockByNumber call
// per block.
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

		transfers = append(transfers, s.scanInternalTransfers(ctx, known, block, blockNum)...)
	}

	return transfers, nil
}

// scanInternalTransfers is the second, best-effort ETH-transfer detection pass this spec
// adds: it walks each transaction's internal call tree (via debug_traceBlockByNumber's
// callTracer) for CALL frames carrying value > 0 to a known deposit address — the case the
// top-level tx.To()/tx.Value() scan above can never see, because the top-level tx itself has
// to=<intermediary contract> and value=0 (e.g. an EIP-7702 smart-account redeemDelegations,
// a multisig relay, any other contract-mediated send). The root frame of each tx's trace is
// deliberately skipped: it is exactly what the top-level scan above already covers, so
// walking it here too would double-count the same transfer. Only CALL is matched — not
// CALLCODE (re-review 2026-07-20, Boundaries & Constraints): CALLCODE never actually moves
// balance to `to`, it only executes `to`'s code in the caller's own storage context.
//
// A frame — or any of its ancestors — whose Error is non-empty reverted, ran out of gas, or
// hit an invalid opcode; the EVM rolls back every state change from such a call, including
// any value it appeared to transfer, so it must never be recorded (re-review 2026-07-20:
// both the adversarial and edge-case review passes independently caught the initial
// implementation crediting phantom deposits for reverted transfers). A tx whose own trace
// itself failed (txCallTrace.Error non-empty, Result a zero value rather than a real empty
// call tree) is skipped entirely rather than walked.
//
// This is intentionally best-effort and degrades gracefully: trace support varies by RPC
// provider/chain (confirmed empirically — Base's public RPC supports
// debug_traceBlockByNumber, Arbitrum's configured public RPC does not) and was never part
// of Story 2.1's committed design. A failing/unsupported call is caught here — never
// propagated to ScanDeposits' caller — logged exactly once via s.logger at Warn, and
// permanently disables this pass for the rest of this Scanner instance's lifetime (no
// retries; see internalTraceDisabled's doc comment). Top-level and ERC-20 detection are
// completely unaffected by this failure, on both chains, regardless of trace support.
//
// Each matching frame gets a synthetic LogIndex of -2-dfsIndex, where dfsIndex is that
// frame's pre-order depth-first index within its transaction's trace (starting at 0,
// counting every non-root frame visited, not just matching ones) — deterministic and
// unique within one (chain, tx_hash) on every re-scan, distinct from both
// nativeTransferLogIndex (-1) and any real (always >= 0) ERC-20 log index.
func (s *Scanner) scanInternalTransfers(ctx context.Context, known map[common.Address]string, block rawBlock, blockNum uint64) []core.ObservedTransfer {
	if s.internalTraceDisabled || len(known) == 0 {
		return nil
	}

	var traces []txCallTrace
	// An explicit timeout (re-review 2026-07-20) bounds how long an unusually deep/expensive
	// block's trace can run before the RPC node gives up, rather than relying entirely on
	// whatever default that node happens to be configured with — reduces, though does not
	// eliminate, the chance a single expensive block trips the permanent sticky-disable below
	// for a reason that is not really "this endpoint can never trace" (deferred-work.md).
	if err := s.client.CallContext(ctx, &traces, "debug_traceBlockByNumber", hexutil.EncodeUint64(blockNum), map[string]any{"tracer": "callTracer", "timeout": "10s"}); err != nil {
		s.internalTraceDisabled = true
		if s.logger != nil {
			s.logger.Warn("debug_traceBlockByNumber failed — permanently disabling internal-transfer detection for this watcher process (top-level and ERC-20 detection are unaffected); restart the watcher to re-probe",
				"chain", s.chain.Name, "block", blockNum, "error", err)
		}
		return nil
	}

	var transfers []core.ObservedTransfer
	for _, trace := range traces {
		if trace.Error != "" {
			// Tracing this specific transaction failed independently of the rest of the
			// block's trace succeeding — Result is a zero value, not a real empty call
			// tree, so walking it would find nothing but for the wrong reason. Silently
			// missing this tx's internal transfers (if any) is an accepted, narrow gap:
			// escalating to Warn here, on every occurrence, on a call that already fires
			// once per block, risks the exact log-spam this pass otherwise avoids.
			continue
		}

		dfsIndex := 0
		var walk func(frame callFrame, reverted bool)
		walk = func(frame callFrame, reverted bool) {
			for _, child := range frame.Calls {
				idx := dfsIndex
				dfsIndex++
				childReverted := reverted || child.Error != ""

				if !childReverted && child.Type == "CALL" && child.To != nil && child.Value != nil {
					if addr, ok := known[*child.To]; ok {
						value := (*big.Int)(child.Value)
						if value.Sign() > 0 {
							transfers = append(transfers, core.ObservedTransfer{
								Chain:       core.Chain(s.chain.Name),
								Asset:       core.AssetETH,
								Address:     addr,
								TxHash:      trace.TxHash.Hex(),
								LogIndex:    -2 - idx,
								Amount:      new(big.Int).Set(value),
								BlockNumber: blockNum,
								// BlockHash comes from the block already fetched by
								// scanNativeTransfers above (Story 2.4) — never a second
								// RPC round-trip just to learn a hash that call already has.
								BlockHash: block.Hash.Hex(),
							})
						}
					}
				}

				// Recurse regardless of this frame's own type, match outcome, or revert
				// status: dfsIndex must stay identical across re-scans of the same trace
				// (idempotency, AD-5) whether or not a given branch reverted or matched,
				// and a STATICCALL/DELEGATECALL frame never itself carries a value
				// transfer (Boundaries & Constraints) but code it executes can still make
				// further CALL frames that do (e.g. a proxy delegating into an
				// implementation that forwards ETH onward) — those must still be found,
				// UNLESS this branch is already known-reverted, in which case nothing
				// beneath it ever took effect either.
				walk(child, childReverted)
			}
		}
		walk(trace.Result, trace.Result.Error != "")
	}

	return transfers
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
