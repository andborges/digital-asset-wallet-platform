---
title: 'Story 2.1: Track Incoming Deposits & Expose Pending Status'
type: 'feature'
created: '2026-07-16'
status: 'done'
review_loop_iteration: 0
followup_review_recommended: true
context: []
warnings: ['oversized']
baseline_revision: 'cdaacd14d585f20c0b567179ecb10b803868b8d7'
final_revision: 'NOT_COMMITTED (user global policy: no auto-commits — see Auto Run Result)'
---

<intent-contract>

## Intent

**Problem:** Deposits sent to a customer's issued address aren't tracked at all today — the platform has no watcher, no deposit record, and no way to answer "did this deposit arrive, and how confirmed is it?"

**Approach:** Add one watcher OS process per chain (`walletd watcher --chain=<base|arbitrum>`) that polls each configured chain, detects ETH/USDC transfers landing on any known deposit address, and persists them through an `observed`→`safe` state machine plus an outbox event — all behind the existing EVM adapter boundary. Expose pending deposits through a new read endpoint.

## Boundaries & Constraints

**Always:**
- All chain-specific RPC, log-filtering, and tier-detection logic lives in `internal/adapter/evm`; `internal/core`, `internal/adapter/postgres`, and `internal/adapter/api` reference only the `Deposit` domain type and its state names — never go-ethereum or a chain ID (AD-1).
- The "observed" transition writes the deposit row and a `deposit.pending` outbox event in one Postgres transaction (AD-4).
- `(chain, tx_hash, log_index)` is a DB unique constraint; re-observing the same event on a repoll is a no-op by construction (AD-5), never an application-level existence check.
- Attribution is only via the persisted `deposit_addresses` table (Story 1.5) — never re-derive an address, never match by `tx.origin`/`EXTCODESIZE` heuristics.
- The watcher is the sole writer of `deposits` rows.
- One OS process per chain (`--chain` flag), not one process looping both chains (AD-2).
- CI gains a `make check-import-boundary` step appended to the existing `build-and-test` job (see `.github/workflows/ci.yml`'s standing placeholder comment) — never a second workflow file.

**Block If:** (none — every open design question below has a reasonable default; see Design Notes.)

**Never:**
- Crediting balance, journal postings, or the `finalized`/`credited` states — Story 2.2.
- Reorg detection or the `orphaned` transition — Story 2.4.
- The full crash/downtime-recovery test suite — Story 2.5 (this story only needs a persisted per-(chain,tier) cursor as AD-5 groundwork).
- A dedicated "unsupported token" observation record — Story 2.3. Any non-ETH/USDC transfer to a deposit address is silently ignored this story (must not crash the watcher).

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| ETH deposit observed | tx with `to` = a known deposit address, value > 0 | New `deposits` row, state `observed`, `deposit.pending` outbox event, one transaction | none |
| USDC deposit observed | ERC-20 `Transfer` log to a known deposit address | Same as above, `asset=usdc` | none |
| Re-poll same event | Same `(chain, tx_hash, log_index)` reprocessed | No duplicate row; existing row untouched | Insert conflict treated as success, not an error |
| Batch posts to L1 | An `observed` deposit's `block_number` &lt;= the chain's current `safe` tag | Deposit transitions `observed` → `safe` | none |
| Unsupported-token transfer | ERC-20 `Transfer` log from a contract not ETH/USDC | Ignored — no row, no crash | none |
| Query pending deposits | `GET /customers/{id}/deposits` | Array of deposits with `status: "pending"` and `tier` (`observed`\|`safe`) | 404 if customer unknown |
| go-ethereum imported outside `internal/adapter/evm` | Any `.go` file, including `_test.go` | `make check-import-boundary` fails | CI red |

</intent-contract>

## Code Map

- `internal/adapter/postgres/migrations/0005_create_deposits.sql` -- `deposits`, `watcher_cursors`, `outbox_events` tables (new — first story needing the outbox)
- `internal/core/deposit.go` -- `Deposit` type, full `DepositState` enum (only `Observed`/`Safe` transitioned here), `ObservedTransfer`, sentinel errors
- `internal/core/ports.go` -- add `ChainScanner`, `DepositAddressLister`, `DepositRepository`, `DepositReader`
- `internal/core/track_deposits.go` -- `TrackDeposits.Execute(ctx, chain)`: one watcher poll cycle
- `internal/core/get_customer_deposits.go` -- `GetCustomerDeposits` use case
- `internal/adapter/evm/scanner.go` -- `ChainScanner` impl: head/safe tags + tx/log scanning
- `internal/adapter/postgres/deposit_repo.go` -- `DepositRepository` impl (`RecordObserved`, `PromoteToSafe`, `Cursor`/`SetCursor`)
- `internal/adapter/postgres/deposit_address_lister.go` -- `DepositAddressLister` impl
- `internal/adapter/postgres/deposit_reader.go` -- `DepositReader` impl
- `api/openapi.yaml` / `internal/adapter/api/server.gen.go` -- `GET /customers/{id}/deposits`, `Deposit`/`DepositsResponse` schemas
- `internal/adapter/api/deposits.go` -- `GetCustomerDeposits` handler
- `cmd/walletd/main.go` -- new `watcher` subcommand (`--chain=base|arbitrum`)
- `Makefile`, `.github/workflows/ci.yml` -- `check-import-boundary` target + CI step

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0005_create_deposits.sql` -- create `deposits` (PK id, customer_id, chain, asset, address, tx_hash, log_index, amount numeric(78,0), block_number, state, observed_at, updated_at, `UNIQUE(chain, tx_hash, log_index)`), `watcher_cursors` (PK `(chain, tier)`, last_block), `outbox_events` (PK id, event_type, payload jsonb, created_at) -- outbox is generic/reusable, not deposit-specific, per AD-4/AD-13
- [x] `internal/core/deposit.go` -- `Deposit` struct; `DepositState` enum `observed|safe|finalized|orphaned|credited` (AD-6's fixed vocabulary, declared now, only observed/safe used); `ObservedTransfer{Chain, Asset, Address, TxHash, LogIndex, Amount, BlockNumber}`; sentinel `ErrCustomerNotFound` reuse
- [x] `internal/core/ports.go` -- `ChainScanner{Head(ctx) (latest, safe uint64, err error); ScanDeposits(ctx, knownAddresses []string, fromBlock, toBlock uint64) ([]ObservedTransfer, error)}`; `DepositAddressLister{ListDepositAddresses(ctx) ([]string, error)}`; `DepositRepository{RecordObserved(ctx, Deposit) (inserted bool, err error); PromoteToSafe(ctx, chain, safeBlock uint64) (int, error); Cursor(ctx, chain, tier string) (uint64, error); SetCursor(ctx, chain, tier string, block uint64) error}`; `DepositReader{ListCustomerDeposits(ctx, customerID string) ([]Deposit, error)}`
- [x] `internal/core/track_deposits.go` -- `TrackDeposits.Execute(ctx, chain)`: list known addresses, `Head()`, scan `(cursor("observed")+1, latest]`, `RecordObserved` each transfer + advance the observed cursor, `PromoteToSafe(chain, safe)`, advance the safe cursor -- all within one transaction opened via `core.TxBeginner`
- [x] `internal/core/get_customer_deposits.go` -- thin wrapper over `DepositReader`, `ErrCustomerNotFound` passthrough
- [x] `internal/adapter/evm/scanner.go` -- `Head`: `eth_blockNumber` + `eth_getBlockByNumber("safe", false)`, error (not a heuristic fallback) if the tag is unsupported; `ScanDeposits`: per-block tx scan for native transfers (`tx.To()` in known-address set, `log_index=-1` sentinel) + `eth_getLogs` ERC-20 `Transfer` topic filter for USDC; any other token's `Transfer` log is skipped, not recorded
- [x] `internal/adapter/postgres/deposit_repo.go` -- `RecordObserved`: `INSERT ... ON CONFLICT (chain, tx_hash, log_index) DO NOTHING` + outbox insert only when a row was actually inserted; `PromoteToSafe`: bulk `UPDATE ... WHERE state='observed' AND chain=$1 AND block_number<=$2`
- [x] `internal/adapter/postgres/deposit_address_lister.go` -- `SELECT address FROM deposit_addresses`
- [x] `internal/adapter/postgres/deposit_reader.go` -- `SELECT ... FROM deposits WHERE customer_id = (via deposit_addresses join)` -- join on address, since `deposits` has no direct `customer_id` FK by design (address is the only attribution key, AD-8); resolve `customer_id` at read time via `deposit_addresses`
- [x] `api/openapi.yaml` -- `GET /customers/{id}/deposits`; `Deposit{id, chain, asset, amount, txHash, status, tier, observedAt}`, `DepositsResponse{deposits: []Deposit}`; regenerate `server.gen.go` (oapi-codegen v2.7.2, confirm empirically as prior stories did)
- [x] `internal/adapter/api/deposits.go` -- `GetCustomerDeposits` handler, same shape as `customers.go`'s `GetCustomer`
- [x] `cmd/walletd/main.go` -- `watcher` subcommand: parse `--chain`, read that chain's `*_RPC_URL`/`*_CHAIN_ID`, `DATABASE_URL`, `WATCHER_POLL_INTERVAL` (default 5s); run `evm.VerifyDeployerPresence` once at startup (reuse Story 1.5's check); loop `TrackDeposits.Execute` on a ticker until SIGINT/SIGTERM
- [x] `Makefile` -- `check-import-boundary` target: fail if any `.go` file outside `internal/adapter/evm/` imports `github.com/ethereum/go-ethereum`
- [x] `.github/workflows/ci.yml` -- replace the "Story 2.1 adds..." comment with a `run: make check-import-boundary` step
- [x] `internal/core/track_deposits_test.go` -- fake `ChainScanner`/`DepositRepository`: new observed row, re-poll no-op, safe promotion, unsupported-token transfer ignored
- [x] `internal/adapter/evm/scanner_test.go` -- real-anvil test (model: `deployer_test.go`) proving native-ETH and ERC-20-Transfer scanning both work over real RPC
- [x] `internal/adapter/api/integration_test.go` -- extend with `TestGetCustomerDeposits_EndToEnd` (seed a `deposits` row directly; no watcher runs in this test)

**Acceptance Criteria:**
- Given a customer's deposit address receives ETH or USDC on Base or Arbitrum and that chain's watcher polls, when processed, then a `deposits` row exists in `observed` state keyed by `(chain, tx_hash, log_index)`, advancing to `safe` once its block is at or below the chain's current `safe` tag.
- Given a deposit in `observed` or `safe` state, when queried via `GET /customers/{id}/deposits`, then it appears with `status: "pending"` and its current `tier`.
- Given the `observed` transition commits, when inspected, then an `outbox_events` row with `event_type: "deposit.pending"` exists in the same transaction.
- Given the codebase, when `make check-import-boundary` runs, then no go-ethereum import or chain-ID reference exists outside `internal/adapter/evm`.
- Given the same on-chain event is observed twice, when reprocessed, then the unique constraint prevents a duplicate row.

## Spec Change Log

## Review Triage Log

### 2026-07-16 — Review pass

- intent_gap: 0
- bad_spec: 0
- patch: 11 (high 3, medium 3, low 5)
- defer: 4 (medium 1, low 3)
- reject: 3
- addressed_findings:
  - `[high]` `[patch]` `DepositReader.ListCustomerDeposits` had no `WHERE state IN (...)` filter — every deposit state (including future finalized/credited/orphaned) would be serialized as `status: "pending"` with a `tier` value outside the OpenAPI enum. Fixed: query now filters to `observed`/`safe` only.
  - `[high]` `[patch]` `*_USDC_ADDRESS` was validated only for non-emptiness — a left-as-placeholder zero address (as shipped in `.env.example`) silently disabled all USDC deposit detection with no startup failure. Fixed: `runWatcher` now validates it's a well-formed, non-zero EIP-55 address, failing loud at startup like every other misconfiguration.
  - `[high]` `[patch]` Unbounded scan range on first run or after any downtime — a large backlog could exceed an RPC provider's `eth_getLogs` range cap, erroring every poll identically and deadlocking the watcher forever (cursor never advances past a failed poll). Fixed: `TrackDeposits.Execute` now caps each poll's scan range to a fixed max-blocks-per-cycle constant, making catch-up incremental across multiple polls.
  - `[medium]` `[patch]` `WATCHER_POLL_INTERVAL=0` or a negative duration parsed successfully then panicked in `time.NewTicker`. Fixed: validated `&gt; 0` at startup with a descriptive error, matching the existing config-validation convention.
  - `[medium]` `[patch]` No mutual exclusion enforcing AD-2's "one watcher process per chain" — an accidental double-start could regress the persisted cursor. Fixed: watcher now holds a Postgres advisory lock (keyed by chain name) for its process lifetime; a second instance for the same chain fails to start rather than racing.
  - `[medium]` `[patch]` `postgres.Migrate` runs unlocked — now called from 3 concurrently-starting processes (api + 2 watchers) instead of 1, racing on `CREATE TABLE`. Fixed: enabled goose's session-locking provider option so concurrent `Migrate` calls serialize.
  - `[low]` `[patch]` `scanUSDCTransfers` had no zero-amount guard (unlike the native path's `Sign() &lt;= 0` check) — a standards-valid zero-value `Transfer` event was recorded as a spurious deposit. Fixed: same guard added.
  - `[low]` `[patch]` No DB-level `CHECK` constraints on `deposits.chain`/`asset`/`state` (unlike `address`, two lines below, which has one). Fixed: added `CHECK` constraints to migration 0005 (not yet run against any persistent environment, safe to edit in place) — defense in depth alongside the reader-query fix above.
  - `[low]` `[patch]` `check-import-boundary` was reachable only via CI, not `make lint` — a local dev following the documented workflow got no AD-1 protection before pushing. Fixed: added as a `lint` prerequisite.
  - `[low]` `[patch]` A persisted cursor ahead of the chain's reported head (chain reset, swapped RPC, misconfiguration) was silently absorbed with no signal. Fixed: `TrackDeposits.Execute` now returns a descriptive error in this case instead of silently skipping forever.
  - `[low]` `[patch]` `GetCustomerDeposits` use case had no unit test, and the integration test never asserted the documented `ORDER BY observed_at DESC`. Fixed: added both.
  - `[medium]` `[defer]` Internal ETH transfers (contract `CALL`/`SELFDESTRUCT` to a deposit address) are invisible to top-level-transaction scanning — a real, currently-reachable gap, but tracing-API coverage is costly/often RPC-provider-restricted and was never in this story's committed design (Design Notes specify top-level `tx.To()` scanning only). Logged for a future hardening story.
  - `[low]` `[defer]` A transaction with nonzero value to a *deployed* forwarder that reverts (e.g., underfunded gas) would be recorded as observed with no receipt-status check. Verified unreachable today: deposit addresses are counterfactual/code-less until Story 3.6 deploys the Forwarder, and a plain value transfer to a no-code address cannot revert post-inclusion; Forwarder's `receive()` is a permissive no-op even after deployment. Revisit only if `receive()`/`fallback` ever gains revert-capable logic.
  - `[low]` `[defer]` Re-running `oapi-codegen` after adding the `Deposit` schema renamed some pre-existing enum constants (e.g. `TransferRequestAssetEth` → `Eth`) because several schemas inline the same `chain`/`asset` enum values instead of `$ref`-ing shared `Chain`/`Asset` component schemas — a pre-existing `openapi.yaml` duplication pattern (since Story 1.3/1.4), not introduced by this story, just tipped over the generator's naming-collision threshold. Worth a global fix (extract shared component schemas) outside this story's scope.
  - `[low]` `[defer]` `NewServerInterface`'s constructor keeps growing as unlabeled positional `*core.X` arguments (6 now) — a pre-existing pattern extended incrementally by every story including this one, not this story's to redesign.
  - `[low]` `[reject]` `check-import-boundary`'s substring grep (vs. an AST-based import check) — matching the literal import path string is robust enough for this repo's scale; a false positive would require someone to type the exact import path in a comment, and a full import-graph checker is disproportionate tooling investment for the risk.
  - `[low]` `[reject]` `common.HexToAddress` silently coercing a malformed `deposit_addresses.address` value — already unreachable: migration 0004's `CHECK (address ~ '^0x[0-9a-fA-F]{40}$')` guarantees well-formed addresses before `DepositAddressLister` ever reads one.
  - `[low]` `[reject]` `outbox_events` lacking a delivery/consumption-tracking column — Epic 4 owns the dispatcher and can add whatever tracking it needs (a column, or its own cursor table) when it's designed; speculatively building for an undesigned future epic is out of scope.

## Design Notes

- Native ETH deposits have no log (a plain top-level value transfer to a still-undeployed, counterfactual address) — detected by scanning each block's transactions for `tx.To()` in the known-address set, not log filtering. USDC uses standard ERC-20 `Transfer(from indexed, to indexed, value)` log filtering. `log_index=-1` (never a real EVM log index) is the native-transfer sentinel so both share one `(chain, tx_hash, log_index)` key.
- `safe` tier read via `eth_getBlockByNumber("safe", false)` — both Base (OP-stack) and Arbitrum support standard safe/finalized block tags. If a provider doesn't, `Head` must return an error (fail loud), never silently approximate with "head minus N blocks."
- `outbox_events` is generic (`event_type` + `jsonb payload`), not deposit-specific: Story 2.2's credit event and Epic 4's dispatcher reuse the same table without a schema change.
- Known-address set reloads from `deposit_addresses` every poll cycle — simple and correct; scaling this is not this story's concern.
- `deposits` has no `customer_id` FK; it's resolved at read time via `deposit_addresses`, keeping the watcher's only attribution key the address itself (AD-8), never a customer id it would have to look up mid-scan.

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green, including new unit tests and the real-anvil scanner test
- `make check-import-boundary` -- expected: passes on the resulting tree; deliberately verify it fails if a go-ethereum import is added outside `internal/adapter/evm` (then revert the probe)
- `make contracts-test` -- expected: unaffected, still 4/4

**Manual checks (if no CLI):**
- Run `walletd watcher --chain=base` against a local `anvil` with a seeded customer deposit address; `cast send` a native ETH transfer to that address; confirm a `deposits` row appears and `GET /customers/{id}/deposits` shows `status: pending`, `tier: observed`; advance anvil's safe block and confirm the tier flips to `safe`.

## Auto Run Result

**Status:** done

**Summary:** Added the platform's first deposit-tracking watcher: one OS process per chain (`walletd watcher --chain=base|arbitrum`) that polls Base/Arbitrum, detects native-ETH and USDC transfers landing on known `deposit_addresses` rows (Story 1.5), and persists them through an `observed`→`safe` state machine plus a `deposit.pending` outbox event, all inside a new `internal/adapter/evm.Scanner` (go-ethereum confined per AD-1) and a new `internal/core.TrackDeposits` use case. Exposed via a new `GET /customers/{id}/deposits` endpoint. Added the AD-1 CI import-boundary check (`make check-import-boundary`).

**Files changed** (implementation pass + patch pass combined):

*New:*
- `internal/adapter/postgres/migrations/0005_create_deposits.sql` — `deposits`, `watcher_cursors`, `outbox_events` tables, with `CHECK` constraints on `chain`/`asset`/`state`/`address`
- `internal/core/deposit.go` — `Deposit`, `DepositState` enum, `ObservedTransfer`
- `internal/core/track_deposits.go` — `TrackDeposits.Execute`: one watcher poll cycle, range-capped and cursor-sanity-checked
- `internal/core/get_customer_deposits.go` + `get_customer_deposits_test.go` — read-side use case + unit test
- `internal/core/track_deposits_test.go` — fake-backed unit tests (new deposit, repoll no-op, safe promotion, empty scan, no-new-blocks skip)
- `internal/adapter/evm/scanner.go` + `scanner_test.go` — `ChainScanner` impl (native tx scan + ERC-20 `Transfer` log filter, both zero-amount-guarded) and a real-anvil test
- `internal/adapter/postgres/deposit_repo.go`, `deposit_address_lister.go`, `deposit_reader.go` (reader now filters to `observed`/`safe` only)
- `internal/adapter/api/deposits.go` — `GetCustomerDeposits` handler
- `contracts/src/TestERC20.sol` — throwaway ERC-20 fixture for the real-anvil scanner test

*Modified:*
- `cmd/walletd/main.go` — new `watcher` subcommand: chain-scoped env config, USDC-address and poll-interval validation, a Postgres advisory lock enforcing one-watcher-per-chain, ticker-driven poll loop
- `internal/adapter/postgres/migrate.go` — enabled goose's session-locking provider option (concurrent `Migrate` callers now serialize instead of racing)
- `internal/adapter/evm/chain.go` — added `USDCAddress` field
- `api/openapi.yaml` / `internal/adapter/api/server.gen.go` — `GET /customers/{id}/deposits`, `Deposit`/`DepositsResponse` schemas (regenerated via `oapi-codegen v2.7.2`)
- `internal/adapter/api/customers.go`, `integration_test.go` — wiring + `TestGetCustomerDeposits_EndToEnd` (including a new ordering subtest)
- `Makefile`, `.github/workflows/ci.yml` — `check-import-boundary` target (now also a `lint` prerequisite) + CI step
- `.env.example` — `BASE_USDC_ADDRESS`/`ARBITRUM_USDC_ADDRESS`, `WATCHER_POLL_INTERVAL`

**Review findings breakdown** (2026-07-16 pass, Blind Hunter + Edge Case Hunter, 23 raw → 18 deduplicated):
- 11 patch (3 high, 3 medium, 5 low) — all applied and verified: `DepositReader` state filter, USDC-address validation, unbounded-scan-range chunking, `WATCHER_POLL_INTERVAL` validation, one-watcher-per-chain advisory lock, goose migration locking, zero-amount USDC transfer guard, DB `CHECK` constraints, `check-import-boundary` wired into `lint`, cursor-ahead-of-head fail-loud, missing `GetCustomerDeposits` test + ordering assertion
- 4 defer (1 medium, 3 low) — logged in `deferred-work.md`: internal-ETH-transfer (CALL/SELFDESTRUCT) blind spot; reverted-forwarder-transaction edge case (verified currently unreachable); `oapi-codegen` enum-rename side effect from pre-existing `openapi.yaml` schema duplication; `NewServerInterface`'s growing positional-arg constructor
- 3 reject (noise): `check-import-boundary`'s substring-grep approach; `HexToAddress` malformed-input coercion (already prevented by migration 0004's `CHECK` on `deposit_addresses.address`); speculative `outbox_events` delivery-tracking column for an undesigned future epic
- 0 intent_gap, 0 bad_spec

**Follow-up review recommended:** `true` — the patch pass touched concurrency/locking behavior across four layers (a new Postgres advisory lock, goose session locking, unbounded-range chunking with cursor accounting, and a fail-loud cursor-sanity check), on top of 3 high-severity fixes. That combination warrants one more independent pass with fresh eyes before this is considered fully settled, even though every verification command is green.

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .` — clean
- `make check-import-boundary` — passes; manually verified it fails when a probe file importing go-ethereum is added under `internal/core` (including as `_test.go`), then removed
- `go test ./...` — all green, including the real-Postgres integration suite (`TestGetCustomerDeposits_EndToEnd`, including the new ordering subtest) and the real-anvil scanner test
- `cd contracts && forge test` — unaffected, 4/4
- **Manual end-to-end smoke test** (beyond the spec's own verification commands, done during the patch-review pass): ran real `anvil` + local Postgres + `walletd api` + `walletd watcher --chain=base`; created a real customer, `cast send` a native ETH transfer to its real deposit address, confirmed the watcher recorded it and `GET /customers/{id}/deposits` returned `status: pending`, `tier: observed`. Also confirmed: `WATCHER_POLL_INTERVAL=0s` fails loud; a placeholder zero `BASE_USDC_ADDRESS` fails loud; a malformed `BASE_USDC_ADDRESS` fails loud; a stale cursor ahead of a fresh anvil's head fails loud (caught for real against genuine leftover local-dev state, not just a contrived test); starting a second `watcher --chain=base` while one already holds the chain's advisory lock fails immediately rather than racing. All manual-test data and containers were cleaned up afterward.

**Residual risks:**
- The 4 deferred items above remain open, tracked in `deferred-work.md`.
- No git commit was created (user's global policy: never commit unless explicitly asked in the moment, which overrides this skill's own "commit, don't push" finalize instruction per this session's instruction-precedence rules). All changes remain uncommitted in the working tree. `final_revision` above reflects this — HEAD has not moved from `baseline_revision`.
- `cmd/walletd`'s `runWatcher`/`main` have no automated Go test coverage (package reports "no test files") — the startup-validation and advisory-lock behavior added in this patch pass were verified manually (see above) rather than by a committed test. Story 2.5 (watcher downtime/recovery) is a natural place to add `cmd/walletd`-level test coverage if that gap should close before then.
