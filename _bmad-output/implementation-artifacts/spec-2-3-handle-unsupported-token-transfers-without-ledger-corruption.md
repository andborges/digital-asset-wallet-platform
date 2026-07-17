---
title: 'Story 2.3: Handle Unsupported Token Transfers Without Ledger Corruption'
type: 'feature'
created: '2026-07-17'
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

**Problem:** Today the scanner's `eth_getLogs` query is hardcoded to only the configured USDC contract address — any other ERC-20 sent to a customer's deposit address is invisible to the platform entirely, and "USDC is the only ERC-20" is baked into `scanUSDCTransfers`'s name and logic, not data.

**Approach:** Replace the single hardcoded USDC-address filter with a `token_registry` table (`chain, contract_address → asset`) that the scanner's broadened, unfiltered-by-contract `eth_getLogs` query classifies against per log: a match is an ordinary observed deposit (unchanged behavior); no match is recorded as a new `unsupported_token_observations` row — visible, never credited, never touching the ledger. Watcher startup upserts the configured USDC address into the registry from the same env vars used today.

## Boundaries & Constraints

**Always:**
- All chain-specific scanning logic stays inside `internal/adapter/evm` (AD-1) — the registry classification happens in the scanner, using a registry snapshot `core.TrackDeposits` loads once per poll and passes in, the same shape as the known-deposit-address set.
- An unsupported-token observation is recorded in the SAME transaction as everything else that poll cycle (AD-4), keyed `(chain, tx_hash, log_index)` with the identical idempotency guarantee deposits already have (AD-5) — a repoll never double-records it.
- Unsupported-token observations never produce a `journal_entries`/`postings` row, never transition any `deposits` row, and are never credited — this is the whole point of the story (FR11).
- The registry is genuinely data, not code (FR34): classification reads the DB table at scan time; adding a second ERC-20 to an already-supported chain is a registry row, never a new `scanXTransfers` function or a new hardcoded asset comparison.
- `deposits`'s existing `UNIQUE(chain, tx_hash, log_index)` and `unsupported_token_observations`'s own identical constraint are two independent guarantees — the same `(chain, tx_hash, log_index)` can appear in at most one of the two tables for a given poll's classification, never both (a log is classified exactly once, by construction of a single scan pass).

**Block If:** (none — every open design question below has a reasonable, narrowly-scoped default; see Design Notes.)

**Never:**
- A UI/dashboard for operator triage — AC3 only requires the data be queryable; a bare `GET` endpoint returning a flat list satisfies it.
- Automatic reclassification of historical `unsupported_token_observations` rows if a token is later added to the registry — a row documents what was unsupported *at the time*; it is a historical record, never retroactively touched.
- Any change to `scanNativeTransfers` — native ETH is never "unsupported"; this story is entirely about the ERC-20/log-scanning path.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Registered token (USDC) transfer | ERC-20 Transfer log from a `token_registry`-listed contract, to a known deposit address | Recorded as an ordinary observed deposit (unchanged from Story 2.1/2.2) | none |
| Unregistered token transfer | ERC-20 Transfer log from a contract NOT in `token_registry`, to a known deposit address | New `unsupported_token_observations` row: chain, deposit address, contract address, tx hash, amount, block number — no deposit row, no journal posting | none |
| Same unsupported event re-polled | Same `(chain, tx_hash, log_index)` reprocessed | No duplicate row (unique constraint no-op), mirrors `deposits`' own repoll behavior | none |
| Operator queries observations | `GET /v1/unsupported-token-observations` | Flat list with contract address and amount for manual triage | none |
| New ERC-20 later added to registry | A new `token_registry` row for an already-supported chain | Its transfers become ordinary observed deposits on the next poll; no code change | none |
| Zero/negative-amount unsupported transfer | Standards-valid zero-value Transfer log from an unregistered contract | Skipped, not recorded (mirrors the existing zero-amount guard already applied to registered-token transfers) | none |

</intent-contract>

## Code Map

- `internal/adapter/postgres/migrations/0007_add_token_registry_and_unsupported_token_observations.sql` -- `token_registry (chain, contract_address, asset)`, PK `(chain, contract_address)`; `unsupported_token_observations` table, `UNIQUE(chain, tx_hash, log_index)`
- `internal/core/deposit.go` -- `UnsupportedTokenObservation` type
- `internal/core/ports.go` -- `TokenRegistryLister`, `UnsupportedTokenRepository`; `ChainScanner.ScanDeposits` gains a `tokenRegistry` parameter and a second return value
- `internal/core/track_deposits.go` -- `Execute` loads the token registry alongside known addresses, records unsupported observations returned by the scan
- `internal/core/list_unsupported_token_observations.go` -- new operator-facing read use case
- `internal/adapter/evm/scanner.go`, `scanner_test.go` -- `scanUSDCTransfers` → `scanERC20Transfers`: unfiltered-by-contract `eth_getLogs`, classifies each log against the registry snapshot into supported/unsupported
- `internal/adapter/evm/chain.go` -- remove `USDCAddress` (superseded by the DB-backed registry; the env var that populated it moves to a startup upsert)
- `internal/adapter/postgres/token_registry.go` -- `TokenRegistryLister` impl + an `UpsertToken` method used once at watcher startup
- `internal/adapter/postgres/unsupported_token_repo.go` -- `UnsupportedTokenRepository` impl (record + list)
- `internal/adapter/api/unsupported_token_observations.go` -- new handler
- `cmd/walletd/main.go` -- `runWatcher` upserts the configured USDC address into `token_registry` at startup instead of building `evm.Chain{USDCAddress: ...}`
- `api/openapi.yaml` -- `GET /unsupported-token-observations`, `UnsupportedTokenObservation`/`UnsupportedTokenObservationsResponse` schemas
- `internal/core/track_deposits_test.go`, `internal/adapter/api/integration_test.go` -- updated/new tests

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0007_add_token_registry_and_unsupported_token_observations.sql` -- `CREATE TABLE token_registry (chain text CHECK (chain IN ('base','arbitrum')), contract_address text CHECK (contract_address ~ '^0x[0-9a-fA-F]{40}$'), asset text CHECK (asset = 'usdc'), created_at timestamptz DEFAULT now(), PRIMARY KEY (chain, contract_address))` -- CHECK tightened to the one ERC-20 asset the codebase can currently interpret, matching the 2.2 patch precedent; `CREATE TABLE unsupported_token_observations (id uuid PK, chain text CHECK(...), address text CHECK(address ~ regex), contract_address text CHECK(same regex), tx_hash text, log_index integer, amount NUMERIC(78,0) CHECK (amount > 0), block_number bigint, observed_at timestamptz DEFAULT now(), UNIQUE(chain, tx_hash, log_index))`
- [x] `internal/core/deposit.go` -- `UnsupportedTokenObservation{ID, Chain, Address, ContractAddress, TxHash, LogIndex, Amount, BlockNumber, ObservedAt}`
- [x] `internal/core/ports.go` -- `TokenRegistryLister{ListTokenRegistry(ctx) (map[string]Asset, error)}` (keyed by contract address, lowercase-normalized); `UnsupportedTokenRepository{RecordObservation(ctx, UnsupportedTokenObservation) (inserted bool, err error); ListObservations(ctx) ([]UnsupportedTokenObservation, error)}`; `ChainScanner.ScanDeposits(ctx, knownAddresses []string, tokenRegistry map[string]Asset, fromBlock, toBlock uint64) ([]ObservedTransfer, []UnsupportedTokenObservation, error)`
- [x] `internal/core/track_deposits.go` -- load `tokenRegistry` via the new lister alongside `addresses`; pass to `ScanDeposits`; after the existing `RecordObserved` loop, loop the returned unsupported observations calling `RecordUnsupportedTokenObservation` (repoll-safe, mirrors `RecordObserved`'s no-op-on-conflict pattern) — same transaction
- [x] `internal/core/list_unsupported_token_observations.go` -- thin wrapper over `UnsupportedTokenRepository.ListObservations`
- [x] `internal/adapter/evm/scanner.go` -- rename `scanUSDCTransfers` → `scanERC20Transfers`; drop the `Addresses: []common.Address{...}` restriction from the `eth_getLogs` query (keep the `Transfer` topic0 + known-deposit-address topic2 filters); for each returned log, look up `l.Address` (lowercased hex) in `tokenRegistry` — a hit produces an `ObservedTransfer` exactly as today (asset = the registry's mapped asset, not a hardcoded `AssetUSDC`); a miss produces an `UnsupportedTokenObservation`; the existing zero-amount guard applies to both branches
- [x] `internal/adapter/evm/chain.go` -- remove `USDCAddress` field (superseded by the registry)
- [x] `internal/adapter/postgres/token_registry.go` -- `ListTokenRegistry` (`SELECT contract_address, asset FROM token_registry WHERE chain = $1`); `UpsertToken(ctx, chain, contractAddress, asset string) error` (`INSERT ... ON CONFLICT (chain, contract_address) DO UPDATE SET asset = EXCLUDED.asset`) — called once at watcher startup, not part of the per-poll transaction
- [x] `internal/adapter/postgres/unsupported_token_repo.go` -- `RecordObservation` (`INSERT ... ON CONFLICT (chain, tx_hash, log_index) DO NOTHING`, mirrors `RecordObserved`); `ListObservations` (plain `SELECT ... ORDER BY observed_at DESC`, no customer scoping — this is operator-facing, platform-wide)
- [x] `internal/adapter/api/unsupported_token_observations.go` -- `GetUnsupportedTokenObservations` handler (bearer-auth only, no customer id — matches this codebase's existing "single internal consumer" auth model, no new role concept)
- [x] `cmd/walletd/main.go` -- `runWatcher`: after validating `*_USDC_ADDRESS` (unchanged validation), call `postgres.NewTokenRegistry(pool).UpsertToken(ctx, chainName, usdcAddress, "usdc")` once at startup, before the poll loop; build `evm.Chain{Name, RPCURL, ChainID}` without `USDCAddress`; wire `TokenRegistryLister`/`UnsupportedTokenRepository` into `core.NewTrackDeposits`
- [x] `api/openapi.yaml` -- `GET /unsupported-token-observations`; `UnsupportedTokenObservation{id, chain, depositAddress, contractAddress, txHash, amount, blockNumber, observedAt}`, `UnsupportedTokenObservationsResponse{observations: []UnsupportedTokenObservation}`; regenerate `server.gen.go`
- [x] `internal/core/track_deposits_test.go` -- update `fakeScanner.ScanDeposits` to the new signature/return shape; new test: an unsupported-token observation returned by the scanner is recorded via the repo, a supported one is recorded as a deposit as before
- [x] `internal/adapter/evm/scanner_test.go` -- extend the real-anvil test (or add a sibling test) proving an ERC-20 Transfer from a contract NOT in the passed-in registry map is classified as unsupported, not as a deposit
- [x] `internal/adapter/api/integration_test.go` -- new `TestGetUnsupportedTokenObservations_EndToEnd`: seed a row directly, confirm it's returned with contract address + amount; confirm auth is still required

**Acceptance Criteria:**
- Given a transfer of a token not in `token_registry` arrives at a known deposit address, when the watcher processes it, then an `unsupported_token_observations` row is written — never a `deposits` row, never a journal posting.
- Given the token registry is a `(chain, contract_address)` table, when a new ERC-20 is added to an already-supported chain, then only a registry row is needed — no `scanner.go` change, no new asset comparison.
- Given an unsupported-token observation exists, when queried via `GET /unsupported-token-observations`, then it is visible with its contract address and amount.

## Spec Change Log

## Review Triage Log

### 2026-07-17 — Review pass

- intent_gap: 0
- bad_spec: 0
- patch: 8 (high 2, medium 2, low 4)
- defer: 2 (medium 1, low 1)
- reject: 3
- addressed_findings:
  - `[high]` `[patch]` This story's own Design Notes overclaimed: comments across `ports.go`/`scanner.go`/the migration said adding a genuinely new ERC-20 asset is "a registry row, never a code change" — but the migration's own `CHECK (asset = 'usdc')` (deliberately tightened, matching the 2.2 precedent) makes that literally false; a new *asset type* still requires extending `core.Asset`'s closed enum. Fixed: corrected the comments to accurately scope what the registry actually delivers zero-code-change — a second *contract address* for an *already-supported* asset (e.g. a bridged/wrapped USDC variant at a different address on the same chain), not a brand-new asset type.
  - `[high]` `[patch]` Removing the `Addresses` filter from `eth_getLogs` (this story's whole point) means an adversarial contract's Transfer-shaped log can carry an arbitrary-length `Data` payload — `amount.Sign() <= 0` only rejected zero/negative, not oversized. An amount exceeding `NUMERIC(78,0)`'s range would fail every insert identically, and because that error rolls back the whole poll's transaction, the same block range retries and fails forever — a chain-wide watcher DoS from a single malicious log. Fixed: reject any Transfer log whose `Data` isn't exactly 32 bytes (the standard `uint256` encoding) before either classification branch runs.
  - `[medium]` `[patch]` The new platform-wide, unpaginated `GET /unsupported-token-observations` combined with unfiltered-by-contract scanning is a self-inflicted amplifier: an attacker can cheaply generate many distinct `tx_hash`es (the uniqueness constraint doesn't dedupe across transactions) to grow the table unbounded, with no index supporting its own `ORDER BY observed_at DESC` read pattern. Fixed: added a bounded `LIMIT` to the read query and a supporting index. Full cursor-based pagination (Story 1.4's pattern) is a larger follow-up if volume ever actually warrants it — not applied here given AC3 only requires visibility, not scale.
  - `[medium]` `[patch]` `token_registry`'s primary key isn't case-normalized: `UpsertToken` always lowercases, but a manually-inserted operator row (explicitly the advertised extension path) could use checksummed case, producing two rows for the same real contract with nondeterministic lookup behavior. Fixed: added `CHECK (contract_address = lower(contract_address))`, enforcing the invariant at the DB level rather than by convention alone.
  - `[low]` `[patch]` A comment claimed the amount-sign guard defended against "a negative-looking decode," but `big.Int.SetBytes` only ever produces non-negative values — the guard can only ever catch zero. Fixed the comment to describe what it actually catches.
  - `[low]` `[patch]` The literal `"usdc"` string passed to `UpsertToken` at watcher startup wasn't tied to `core.AssetUSDC` at compile time. Fixed: reference the constant instead of a bare literal.
  - `[low]` `[patch]` `ListTokenRegistry` cast the DB's `asset` column straight to `core.Asset` with no validation against known enum members — currently unreachable given the DB `CHECK`, but cheap defense in depth consistent with this codebase's established pattern. Fixed: added a validation check when scanning registry rows.
  - `[low]` `[patch]` No test proved the `token_registry.asset` CHECK constraint actually rejects an unsupported asset value — the exact gap that let the overclaiming comments (first finding above) go uncaught. Fixed: added a test asserting the CHECK fires for a non-`'usdc'` value.
  - `[medium]` `[defer]` The new platform-wide `GET /unsupported-token-observations` endpoint widens the blast radius of this system's existing shared-bearer-token auth model: any valid token can now enumerate every customer's deposit address in one call, not just look one up at a time. This is the same "no object-level authorization" limitation first logged in the 1-2 review and repeatedly deferred since — fixing the auth model is a cross-cutting change well beyond this story's scope, but this endpoint's aggregation angle is a real escalation worth flagging distinctly.
  - `[low]` `[defer]` The two real-anvil scanner tests (`TestScanner_RealAnvil_FindsNativeAndUSDCTransfers`, `TestScanner_RealAnvil_ClassifiesUnregisteredTokenAsUnsupported`) duplicate nearly all of their anvil-startup/contract-deployment boilerplate. A shared test helper would reduce CI overhead and keep future plumbing changes from needing to be applied twice by hand — a maintainability nice-to-have, not a correctness issue.
  - `[low]` `[reject]` No mechanism exists to reclassify/backfill `unsupported_token_observations` rows once a token is later added to the registry. This is an explicit, already-documented scope boundary in this spec's own `<intent-contract>` ("Never... automatic reclassification... a row documents what was unsupported *at the time*"), not an oversight.
  - `[low]` `[reject]` `TokenRegistry.UpsertToken` writes directly via the pool rather than through `txFromContext`, unlike every other repository write in this codebase. This is intentional and already clearly documented (it runs once at watcher startup, never as part of a per-poll transaction) — correctly designed, not a bug.
  - `[low]` `[reject]` `BlockNumber`'s `uint64`→`int64` conversion in the new API response could theoretically wrap negative. Requires a block number beyond `math.MaxInt64` — unreachable at any timescale relevant to this system's operational lifetime.

## Design Notes

- **`Chain.USDCAddress` is removed, not deprecated in place** — Story 2.1 added it as a scanner-owned field; Story 2.2 didn't touch it; this story replaces its *role* (telling the scanner which contract is USDC) with the registry, so the field becomes genuinely dead weight, not backward-compatible surface worth keeping. The env var it read (`*_USDC_ADDRESS`) is unchanged — only *where* that value ends up (a DB upsert at startup, not a struct field the scanner reads) changes.
- **The registry is populated by the watcher process itself at startup, not by a migration.** Contract addresses are environment-specific (mainnet vs. testnet vs. local anvil) — a migration can't read `BASE_USDC_ADDRESS` at apply time the way `runWatcher` already does. `UpsertToken` keeps the registry in sync with configuration on every restart while remaining the actual mechanism scanning reads from — an operator manually inserting a third `token_registry` row (a genuinely new ERC-20) is picked up on the very next poll with zero code change, which is the literal content of FR34.
- **Broadening the `eth_getLogs` query to drop the contract-address filter is deliberate, not incidental.** Classifying "unsupported" requires actually seeing those logs — a query still scoped to known contracts would filter unsupported transfers out before the classification step ever ran, making AC1 unimplementable as stated.
- **`token_registry.asset` is CHECK-constrained to `'usdc'` only**, not `('eth','usdc')` or left open — mirrors the exact reasoning behind Story 2.2's `crediting_policy` CHECK tightening: no code path exists yet for a hypothetical third asset, and native ETH has no contract address to register in the first place.

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green, including the extended real-anvil scanner test and the new integration test
- `make check-import-boundary` -- expected: still passes
- `cd contracts && forge test` -- expected: unaffected, still 4/4

**Manual checks (if no CLI):**
- Run the full local stack against anvil; send an ERC-20 transfer from a token contract NOT registered in `token_registry` to a customer's deposit address; confirm no `deposits` row is created and `GET /unsupported-token-observations` shows it with the correct contract address and amount.

## Auto Run Result

**Status:** done

**Summary:** Replaced the hardcoded single-USDC-contract filter with a data-driven `token_registry` table: the scanner's `eth_getLogs` query now drops its contract-address restriction (seeing every ERC-20 Transfer landing on a known deposit address), then classifies each log against a per-chain registry snapshot — a hit is an ordinary observed deposit (unchanged behavior), a miss is recorded as a new `unsupported_token_observations` row, visible via a new operator-facing `GET /unsupported-token-observations` endpoint, never credited, never touching the ledger. The watcher upserts its configured USDC address into the registry at startup, replacing `evm.Chain.USDCAddress`.

**Files changed:**

*New:*
- `internal/adapter/postgres/migrations/0007_add_token_registry_and_unsupported_token_observations.sql` — `token_registry` (CHECK-tightened to `asset='usdc'`, lowercase-enforced `contract_address`), `unsupported_token_observations` (CHECK `amount>0`, supporting index for its read pattern)
- `internal/core/list_unsupported_token_observations.go` — operator-facing read use case
- `internal/adapter/postgres/token_registry.go` — `ListTokenRegistry` (with defense-in-depth asset validation) + `UpsertToken`
- `internal/adapter/postgres/unsupported_token_repo.go` — `RecordObservation` (mirrors `RecordObserved`'s idempotency pattern) + `ListObservations` (LIMIT-bounded)
- `internal/adapter/api/unsupported_token_observations.go` — new handler

*Modified:*
- `internal/core/deposit.go`, `ports.go` — `UnsupportedTokenObservation` type; `TokenRegistryLister`, `UnsupportedTokenRepository` ports; `ChainScanner.ScanDeposits` gains a registry parameter and a second return value
- `internal/core/track_deposits.go`, `track_deposits_test.go` — loads the registry each poll, records unsupported observations in the same transaction
- `internal/adapter/evm/scanner.go`, `scanner_test.go` — `scanUSDCTransfers` → `scanERC20Transfers`: unfiltered-by-contract query, per-log classification, 32-byte Data-length rejection (closes a DoS vector), corrected comments scoping what the registry actually delivers
- `internal/adapter/evm/chain.go` — removed `USDCAddress` (superseded by the registry)
- `cmd/walletd/main.go` — `runWatcher` upserts the configured USDC address into the registry at startup (via `core.AssetUSDC`, not a bare string literal)
- `internal/adapter/api/customers.go`, `integration_test.go` — wiring + `TestGetUnsupportedTokenObservations_EndToEnd`
- `api/openapi.yaml`, `server.gen.go` — `GET /unsupported-token-observations` + schemas (regenerated)
- `.env.example` — comment updated to describe the registry-upsert flow

**Review findings breakdown** (2026-07-17 pass, Blind Hunter + Edge Case Hunter, 14 raw → 13 deduplicated):
- 8 patch (2 high, 2 medium, 4 low) — all applied and verified: corrected an overclaiming comment this spec itself introduced (registry enables new contract addresses for known assets, not new asset types — the CHECK was already correctly tight, the comments weren't accurate); rejected non-32-byte Transfer log `Data` (closes a real DoS: an adversarial contract could otherwise stall the whole poll forever via a numeric-overflow insert failure); LIMIT + supporting index on the new unpaginated read endpoint; case-normalization CHECK on `token_registry`'s primary key; corrected a misleading comment about `SetBytes` producing negative values; tied a hardcoded `"usdc"` literal to `core.AssetUSDC`; defense-in-depth asset validation in `ListTokenRegistry`; a test proving the asset CHECK constraint is real
- 2 defer (1 medium, 1 low) — logged in `deferred-work.md`: the new platform-wide endpoint's aggregation angle on this system's existing shared-bearer-token auth limitation; duplicated real-anvil test boilerplate
- 3 reject (noise): no retroactive-reclassification mechanism (already an explicit `<intent-contract>` boundary); `UpsertToken`'s intentional non-transactional write; theoretical `int64` block-number overflow
- 0 intent_gap, 0 bad_spec

**Follow-up review recommended:** `true` — this story removed the Addresses filter from a live `eth_getLogs` query (a genuine expansion of what untrusted on-chain data reaches this codebase) and introduced the platform's first fully unauthenticated-by-tenant, cross-customer-aggregating read endpoint; both warrant a fresh independent look given the DoS-shaped findings this pass already caught once.

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `make check-import-boundary` — all clean
- `go test ./...` — all green, including the real-Postgres integration suite (`TestGetUnsupportedTokenObservations_EndToEnd`, including the new CHECK-constraint test) and both real-anvil scanner tests (confirmed the 32-byte Data-length guard doesn't reject real ERC-20 logs)
- `cd contracts && forge test` — unaffected, 4/4
- Review diff was scoped to exactly this story's changes (pre-implementation snapshots diffed against post-implementation state), so Blind Hunter/Edge Case Hunter reviewed only Story 2.3's actual changes, not Stories 2.1/2.2's already-reviewed code.

**Residual risks:**
- The 2 deferred items above remain open, tracked in `deferred-work.md`.
- No git commit was created (user's global no-auto-commit policy). All changes remain uncommitted, stacked on top of Stories 2.1/2.2's own uncommitted changes. `final_revision` reflects this — HEAD has not moved from `baseline_revision`.
- The unpaginated read endpoint's `LIMIT` bounds response size but not underlying table growth from spam — full pagination or rate-limiting is a larger follow-up if real-world volume ever warrants it (noted, not blocking, per AC3's actual scope).
