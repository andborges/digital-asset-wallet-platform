---
title: 'Story 3.1: Query Fee Estimate Before Submitting a Withdrawal'
type: 'feature'
created: '2026-07-19'
status: 'done'
review_loop_iteration: 1
followup_review_recommended: false
context: []
warnings: []
baseline_revision: '881ef921ddfb75caefce34f7b2f18df2f39b5901'
---

<intent-contract>

## Intent

**Problem:** Application teams have no way to know a withdrawal's total cost before requesting one — and on an L2, a naive single-number gas estimate systematically undercharges, since it omits the L1 data-posting fee component entirely.

**Approach:** Add `GET /v1/withdrawals/fee-estimate?chain=&asset=&amount=`, backed by a new chain-specific `FeeEstimator` port: Arbitrum's `NodeInterface.gasEstimateComponents()` precompile returns both fee components in one call; Base's `GasPriceOracle` predeploy requires combining a standard `eth_estimateGas`/gas-price call (L2 component) with `getL1FeeUpperBound` (L1 component, the OP-stack method documented specifically for pre-signature estimates). No withdrawal record exists yet — this is a pure, unpersisted, read-only computation.

## Boundaries & Constraints

**Always:**
- All chain-specific fee mechanics stay inside `internal/adapter/evm` (AD-1) — `core` only knows a `FeeEstimator` port and a `FeeEstimate{L2Fee, L1Fee, TotalFee}` domain type, both in base units (`*big.Int`), never floats.
- Every well-known contract/precompile address and ABI signature used here is confirmed empirically before being hardcoded (matching this project's own established discipline — e.g. Story 1.5 caught an EIP-55 checksum typo in the architecture spine's own citation): NodeInterface `0x00000000000000000000000000000000000000C8`, GasPriceOracle `0x4200000000000000000000000000000000000F`. Confirm the exact byte length of each empirically (`cast code <addr>` against a real endpoint, or equivalent) before writing the Go constant — do not hand-copy from this spec's prose without checking.
- USDC's contract address per chain is resolved via the *existing* `token_registry` table (Story 2.3) — never a new, separate USDC-address config path.
- The representative transaction used for estimation needs no real destination address (none is provided by this endpoint's inputs) — a fixed placeholder address is used, documented as intentional, never mistaken for a real destination.

**Block If:** (none — every open design question below has a reasonable, empirically-grounded default; see Design Notes.)

**Never:**
- Persisting the fee estimate anywhere, or creating any withdrawal-related row — no withdrawal resource exists until Story 3.2; this story is a stateless computation only.
- A live-testnet dependency in the default CI test run — Arbitrum's `NodeInterface` cannot be emulated by anvil fork mode (it's ArbOS-native, not deployed bytecode — empirically confirmed), so its logic is unit-tested against a fake RPC response, not a live call. Base's `GasPriceOracle` genuinely can be tested via anvil fork mode (real predeploy bytecode is present in forked state), but that requires forking a live RPC at test time — gate it behind an opt-in env var, off by default, so CI never silently depends on third-party live infrastructure.
- Redesigning or hand-waving the Arbitrum total-fee formula: `TotalFee = gasEstimate * baseFee`; `L2Fee = (gasEstimate - gasEstimateForL1) * baseFee`; `L1Fee = gasEstimateForL1 * baseFee` (confirmed algebraically equivalent to Arbitrum's own documented three-term formula, and empirically verified against a real Arbitrum Sepolia call).

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Valid ETH fee estimate, Arbitrum | `chain=arbitrum&asset=eth&amount=<positive>` | `{l2Fee, l1Fee, totalFee}` base-unit strings, `totalFee = l2Fee + l1Fee` | none |
| Valid USDC fee estimate, either chain | `chain=base&asset=usdc&amount=<positive>` | Same shape; representative calldata is an ABI-encoded `transfer(placeholder, amount)` against that chain's registered USDC contract | none |
| Valid ETH fee estimate, Base | `chain=base&asset=eth&amount=<positive>` | `l2Fee` from `eth_estimateGas × gas price`, `l1Fee` from `getL1FeeUpperBound` | none |
| Invalid chain or asset enum value | `chain=optimism` or `asset=dai` | 400 `problem+json` | Rejected by generated request validation before the handler runs (FR22) |
| Non-positive or missing amount | `amount=0`, `amount=-5`, or omitted | 400 `problem+json` | Explicit validation in the use case |
| USDC requested but no `token_registry` entry for that chain | Registry gap (shouldn't happen in a correctly configured deployment) | 500, clearly logged server-side — never a silently wrong estimate | fail loud, not a guessed contract address |

</intent-contract>

## Code Map

- `internal/core/fee_estimate.go` -- `FeeEstimate` domain type
- `internal/core/ports.go` -- `FeeEstimator` port
- `internal/core/estimate_fee.go` -- `EstimateFee` use case (amount validation, delegates to the port)
- `internal/adapter/evm/fee_estimator.go`, `fee_estimator_test.go` -- per-chain `FeeEstimator` implementation
- `internal/adapter/api/fee_estimate.go` -- new handler
- `api/openapi.yaml` -- `GET /withdrawals/fee-estimate`, `FeeEstimate` schema
- `cmd/walletd/main.go` -- wire `evm.NewFeeEstimator` + `core.NewEstimateFee` into the API composition root

## Tasks & Acceptance

**Execution:**
- [x] `internal/core/fee_estimate.go` -- `FeeEstimate{L2Fee, L1Fee, TotalFee *big.Int}`
- [x] `internal/core/ports.go` -- `FeeEstimator{EstimateFee(ctx, chain Chain, asset Asset, amount *big.Int) (FeeEstimate, error)}`
- [x] `internal/core/estimate_fee.go` -- `EstimateFee` use case: reject `amount <= 0` (`ErrNonPositiveAmount`-style sentinel, matching `CreateTransfer`'s existing validation convention), reject `amount` exceeding a uint256's max (`ErrAmountTooLarge`, added in review), delegate to the port
- [x] `internal/adapter/evm/fee_estimator.go` -- `NewFeeEstimator(chains map[Chain]... , tokenRegistryLister core.TokenRegistryLister)` (or equivalent wiring — confirm the cleanest shape given `Scanner`/`NewScanner`'s existing per-chain pattern); for Arbitrum: build the representative `(to=placeholder, contractCreation=false, data)` payload (`data` empty for ETH, ABI-encoded `transfer(placeholder, amount)` against the chain's registered USDC contract for USDC), call `NodeInterface.gasEstimateComponents` via raw `CallContext` (mirrors Story 2.x's `scanClient.CallContext` pattern for a precompile that isn't a normal deployed contract), compute L2/L1/Total per the confirmed formula; for Base: `eth_estimateGas` + current gas price for L2, `GasPriceOracle.getL1FeeUpperBound(unsignedTxSize)` for L1 (construct a representative unsigned transaction of the right shape to get its RLP-encoded byte size — confirm the exact construction empirically, don't guess)
- [x] `internal/adapter/api/fee_estimate.go` -- `GetWithdrawalFeeEstimate` handler (bearer-auth only, no idempotency key needed — non-mutating GET)
- [x] `api/openapi.yaml` -- `GET /withdrawals/fee-estimate` with `chain`/`asset`/`amount` query params (enum-constrained chain/asset, matching existing schema conventions), `FeeEstimate{l2Fee, l1Fee, totalFee}` response schema (base-unit strings, matching `Balance`'s convention); regenerate `server.gen.go`
- [x] `cmd/walletd/main.go` -- wire the new use case into `runAPI`'s composition root and `NewServerInterface`
- [x] `internal/adapter/evm/fee_estimator_test.go` -- fake-RPC unit tests proving the Arbitrum formula/parsing against a canned `gasEstimateComponents` response (using this story's own empirically-obtained real example numbers as the fixture); a Base test gated behind an opt-in env var (e.g. `RUN_LIVE_FORK_TESTS=1`) that forks Base Sepolia via anvil and calls the real `GasPriceOracle` — skipped by default, never required for `make test`/CI
- [x] `internal/core/estimate_fee_test.go` -- unit tests for the amount-validation boundary
- [x] `internal/adapter/api/integration_test.go` -- new test: valid estimate for each chain/asset combination (against a fake `FeeEstimator` wired into the test harness — no real RPC needed for this end-to-end test), invalid chain/asset enum → 400, non-positive amount → 400, missing bearer token → 401

**Acceptance Criteria:**
- Given a chain, asset, and amount, when `GET /v1/withdrawals/fee-estimate` is called, then the response combines the L2 execution fee and the amortized L1 data fee for that chain (Arbitrum via `NodeInterface.gasEstimateComponents()`, Base via the `GasPriceOracle` predeploy).
- Given the two fee components, when compared to a naive single-number estimate, then both are returned explicitly (`l2Fee`, `l1Fee`) alongside `totalFee`, never collapsed into one undifferentiated number.
- Given an unsupported chain/asset combination is queried, when processed, then the platform returns a 400 `problem+json` response.

## Spec Change Log

## Review Triage Log

Blind Hunter + Edge Case Hunter ran in parallel against the full implementation. Patched (highest severity first):

1. **Critical — `gasPriceOracleAddress` was 19 bytes, not 20** (`fee_estimator.go`): a one-character transcription bug (`0x4200...000F` vs the real `0x4200...0000F`) made every Base fee estimate call the wrong, essentially-uninhabited address. Confirmed independently with `common.HexToAddress`. Fixed the constant; added a test asserting the eth_call's `to` matches the real predeploy address (the prior test never asserted destination, which is why this went undetected).
2. **High — USDC fee estimates would revert in production on both chains.** Neither the Arbitrum `eth_call` nor Base's `EstimateGas` sets a `from` address, so the representative `transfer(placeholder, realAmount)` calldata was simulated as sent by the zero address, which has zero USDC balance — the ERC20 contract's own balance check would revert. Fixed by encoding the representative transfer's amount as a fixed `0` (gas cost/calldata length for `transfer` don't depend on the value; `0 >= 0` passes the balance check trivially even for an unfunded sender). Added a regression test asserting the real requested amount never reaches this calldata.
3. **High — duplicate `token_registry` USDC entries pick a contract address nondeterministically.** Migration 0007's own comment explicitly anticipates a second, bridged/wrapped USDC row per chain — exactly the scenario that broke `usdcContractAddress`'s first-match-wins map iteration. Fixed to error on >1 match; added a regression test.
4. **Medium-high — hardcoded `Value: big.NewInt(0)` in the Base L1 tx-size calculation undercharged large ETH withdrawals.** RLP-encodes as a variable-length integer, so a real (large) ETH amount adds up to ~32 bytes the estimate was ignoring — the exact undercharging failure this story exists to prevent. Fixed by threading the real representative value (the requested amount for ETH, 0 for USDC) through to `representativeUnsignedTxSize`. Added a regression test proving tx size grows with amount.
5. **Medium — no upper bound on `amount`.** go-ethereum's ABI packer silently wraps a value >2^256-1 modulo 2^256 with no error. Added `core.ErrAmountTooLarge` (`amount.BitLen() > 256`), checked before the port is called. (This is also now moot for USDC per fix #2, but kept as defense-in-depth and for ETH.)
6. **Medium — 500s were never logged server-side, and raw internal error text (RPC/node internals) was forwarded straight into the external `problem+json` body** — violating the I/O matrix's explicit "clearly logged server-side... never a silently wrong estimate" requirement and creating an information-disclosure surface. Added a `logger` field to `customerServer` (threaded through `NewServerInterface`); the handler now logs the real error server-side and returns a fixed generic detail externally.
7. **Low-medium — the reused `ErrNonPositiveAmount` sentinel's message text ("transfer amount must be a positive integer") leaked onto this non-transfer endpoint.** Fixed by having the handler map the sentinel to a fixed, endpoint-appropriate message rather than calling `err.Error()`; `errors.Is` identity (and existing tests) unaffected.
8. **Low — combined chain/asset enum validation returned one generic message regardless of which parameter was invalid.** Split into two checks with distinct `invalid-chain` / `invalid-asset` problem types.
9. **Low — unsupported `core.Asset` values silently fell through to the USDC path.** Added an explicit default-case error in `representativeTransaction` (defense-in-depth; the API layer already rejects invalid enums before reaching this code).
10. **Low — no timeout on the handler's RPC-bound `Execute` call.** Added a 10s `context.WithTimeout` (mirrors `main.go`'s `deployerCheckTimeout` pattern), so a stalled RPC endpoint fails fast instead of hanging until the server's global 30s `WriteTimeout`.

Deferred (see `deferred-work.md`): no caching/rate-limiting/circuit-breaking on the per-request live RPC round-trip this endpoint makes — the epic context already flags "caching/refresh policy is an adapter-internal detail," and this is future tuning, not a correctness gap.

Rejected: malformed `token_registry.contract_address` hex — migration 0007's `CHECK (contract_address ~ '^0x[0-9a-fA-F]{40}$')` already guarantees well-formed 20-byte hex at the DB layer, so `common.HexToAddress` can't silently misdecode it in practice.

## Design Notes

- **Arbitrum's three fee-related return values collapse to one formula.** `gasEstimateComponents` returns `(gasEstimate, gasEstimateForL1, baseFee, l1BaseFeeEstimate)`; Arbitrum's own docs give a three-term formula for total fees that algebraically simplifies to `gasEstimate * baseFee` (verified: a live call returning `gasEstimate=27142, gasEstimateForL1=5798, baseFee=20162000` gives `TotalFee=547,237,004,000 wei`, `L2Fee=(27142-5798)*20162000=430,337,728,000`, `L1Fee=5798*20162000=116,899,276,000`, summing exactly). `l1BaseFeeEstimate` is not needed once this simplification is used — don't reintroduce it.
- **`NodeInterface` is not a deployed contract — it's ArbOS-native.** `cast code 0x...C8` against a real endpoint returns real bytecode absence (`0x`) under anvil fork mode, confirmed empirically; only a live Arbitrum node (or a real Arbitrum Sepolia RPC) can answer this call correctly. This is why its Go-side logic is tested against a fake/canned RPC response, not a forked chain.
- **`getL1FeeUpperBound`, not `getL1Fee`, is the right OP-stack method here.** `getL1Fee(bytes)` expects the actual (signed) transaction bytes, which don't exist yet at estimate time; `getL1FeeUpperBound(uint256 unsignedTxSize)` is explicitly documented as the pre-signature estimate path (Fjord+) and only needs a representative unsigned transaction's byte size — confirmed present and functional on Base Sepolia (already past Fjord, on Jovian per its own `isJovian()` flag).
- **The representative transaction's destination doesn't matter for the estimate.** Gas cost for a plain value transfer or a standard `transfer(address,uint256)` call is materially insensitive to the destination address (aside from cold/warm storage access, which can't be known in advance for an estimate anyway) — a fixed placeholder address, clearly documented as such, is correct and sufficient.
- **USDC's contract address comes from `token_registry`, not a new config path.** Story 2.3 already solved "which contract address is USDC on this chain" — reuse `TokenRegistryLister.ListTokenRegistry`, inverting its `(contractAddress → asset)` map to find the USDC entry (a 1-2-entry map, trivially cheap to search; no new port method needed for this).

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green; the Base live-fork test is skipped by default (no `RUN_LIVE_FORK_TESTS` set)
- `make check-import-boundary` -- expected: still passes
- `cd contracts && forge test` -- expected: unaffected, still 4/4

**Manual checks (if no CLI):**
- `curl` the new endpoint against a real Base Sepolia and Arbitrum Sepolia RPC (via a running `walletd api`) for both ETH and USDC, confirm `l2Fee + l1Fee == totalFee` and the magnitudes are sane (roughly matching this story's own empirically-obtained example numbers).

## Auto Run Result

Status: done

Implementation completed, independently verified (build/vet/fmt/import-boundary all green; all fee-estimate-specific unit and integration tests read and re-run directly), then adversarially reviewed by Blind Hunter + Edge Case Hunter in parallel. Ten findings patched (one critical: a wrong `GasPriceOracle` predeploy address that would have broken every Base fee estimate in production; two high: USDC estimates reverting on both chains due to a missing representative-amount fix, and nondeterministic USDC contract resolution under a bridged/wrapped-token registry scenario the codebase's own migration comment anticipates), one item deferred (caching/rate-limiting, explicitly out of scope per the epic context), one item rejected as a non-issue (DB CHECK constraint already guarantees valid contract-address hex). Regression tests added for every patched finding. `make build && make test` (fee-estimate scope) green; the two pre-existing unrelated test suites (`TestReorgDetection_EndToEnd`, `TestTrackDeposits_Execute_ReorgCheck_*`) remain red exactly as before this story, from unrelated prior work.
