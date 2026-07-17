---
title: 'Story 2.4: Detect & Safely Reverse Reorged Pre-Finality Deposits'
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

**Problem:** Deposits in `observed`/`safe` state today are never re-checked against the chain — if a reorg replaces the block a pending deposit was seen in, the platform keeps treating it as valid, with no signal to the customer and no way to distinguish "still pending" from "chain history moved and this may no longer be real."

**Approach:** Store each deposit's `block_hash` at observation time; every poll, before any promotion, re-check every `observed`/`safe` deposit's stored hash against the chain's current hash at that height — a mismatch (or a height that no longer exists) transitions the deposit to `orphaned`, writes a `deposit.orphaned` outbox event, and leaves the ledger untouched (orphaning only ever touches pre-finality rows). If the same transaction later reappears on-chain, it is tracked as a brand-new `observed` record, never conflated with the orphaned one.

## Boundaries & Constraints

**Always:**
- Reorg-checking runs first in `TrackDeposits.Execute`, before the existing observed-scan/safe-promotion/finalized-promotion/crediting phases, all still inside the one transaction the poll cycle already opens (AD-4) — an orphaned deposit must never be promoted or credited in the same cycle it's discovered to be orphaned.
- Only `observed`/`safe` deposits are ever candidates for orphaning — `finalized`/`credited` rows are never touched by this story's code, which is what makes AC1's "no balance ever affected" and FR13's "credited balance never reversed" true by construction, not by a runtime check.
- The `deposit.orphaned` outbox event is written in the same transaction as the state transition (AD-4), the same pattern as `deposit.pending`/`deposit.credited`.
- A deposit's `(chain, tx_hash, log_index)` uniqueness must allow a *second* row once the first is `orphaned` — a re-broadcast of the exact same signed transaction has the same hash, so the constraint that dedupes repeated observation of one still-valid event must not also block that event's legitimate re-observation after a reorg. Confirm the exact name Postgres assigned to the existing `deposits` `UNIQUE(chain, tx_hash, log_index)` constraint empirically (e.g. via `\d deposits` or `pg_constraint`) before dropping it — never guess it.
- `GET /customers/{id}/deposits` gains visibility into `orphaned` deposits (AC1's "provisional visibility reflects this") — a customer must be able to see a deposit was reorged away, not have it silently vanish.

**Block If:** (none — every open design question below has a reasonable, narrowly-scoped default; see Design Notes.)

**Never:**
- Reprocessing/backfilling deposits that were already `finalized`/`credited` before this story existed — reorg-checking only ever looks at `observed`/`safe` rows.
- A "re-org depth" or confirmation-count heuristic — detection is purely "does the chain's current block hash at this height still match what we stored," not a manually-tuned depth threshold.
- Any change to `unsupported_token_observations` — it has no state machine and no orphan concept; this story is entirely about the `deposits` table.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Block hash still matches | An `observed`/`safe` deposit's stored `block_hash` equals the chain's current hash at that height | No change | none |
| Block replaced by reorg | Stored `block_hash` differs from the chain's current hash at that height | Deposit → `orphaned`, `deposit.orphaned` outbox event, same transaction | none |
| Chain got shorter than the deposit's height | `block_number` exceeds the chain's current `latest` | Deposit → `orphaned` (the block plainly no longer exists) | none |
| Transaction reappears after being orphaned | Same `(chain, tx_hash, log_index)` observed again, prior row is `orphaned` | A brand-new `deposits` row, `observed` state — never conflated with the orphaned one | none |
| Re-poll of the same still-valid event | Same `(chain, tx_hash, log_index)`, prior row is `observed`/`safe`/`finalized`/`credited` (not orphaned) | No duplicate row (existing constraint behavior, unchanged) | none |
| Orphan check vs. finalized/credited deposits | A `finalized` or `credited` deposit's original block is later replaced (should never happen post-finality) | Never checked, never touched — out of scope by construction | none |
| Deterministic reorg test | `anvil_reorg` forces a real reorg in a test | The watcher detects it and orphans the affected deposit over real RPC | none |

</intent-contract>

## Code Map

- `internal/adapter/postgres/migrations/0008_add_deposit_reorg_detection.sql` -- `deposits.block_hash` column; replace the table's plain `UNIQUE(chain, tx_hash, log_index)` with a partial unique index scoped to non-orphaned rows
- `internal/core/deposit.go` -- `Deposit`/`ObservedTransfer` gain `BlockHash`
- `internal/core/ports.go` -- `ChainScanner.BlockHash`; `DepositRepository.ListPendingDeposits`, `.OrphanDeposit`
- `internal/core/track_deposits.go` -- new reorg-check phase, first in `Execute`
- `internal/adapter/evm/scanner.go`, `scanner_test.go` -- `BlockHash` impl; `scanNativeTransfers`/`scanERC20Transfers` populate `BlockHash` on every `ObservedTransfer`
- `internal/adapter/postgres/deposit_repo.go` -- `RecordObserved` persists `block_hash` and its `ON CONFLICT` clause targets the new partial index; `ListPendingDeposits`, `OrphanDeposit`
- `internal/adapter/postgres/deposit_reader.go` -- widen the `WHERE state IN (...)` filter to include `orphaned`
- `internal/adapter/api/deposits.go` -- surface `status: "orphaned"` for orphaned-tier deposits
- `api/openapi.yaml` -- widen `Deposit.status`/`Deposit.tier` enums
- `internal/core/track_deposits_test.go`, `internal/adapter/evm/scanner_test.go`, `internal/adapter/api/integration_test.go` -- new/updated tests, including a real-`anvil_reorg` test

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0008_add_deposit_reorg_detection.sql` -- `ALTER TABLE deposits ADD COLUMN block_hash text NOT NULL CHECK (block_hash ~ '^0x[0-9a-fA-F]{64}$')`; confirm and drop the existing `UNIQUE(chain, tx_hash, log_index)` constraint by its actual name; `CREATE UNIQUE INDEX idx_deposits_active_chain_tx_hash_log_index ON deposits (chain, tx_hash, log_index) WHERE state != 'orphaned'`
- [x] `internal/core/deposit.go` -- `Deposit.BlockHash string`; `ObservedTransfer.BlockHash string`
- [x] `internal/core/ports.go` -- `ChainScanner.BlockHash(ctx, blockNumber uint64) (hash string, exists bool, err error)` (`exists=false` when the chain no longer has a block at that height at all); `DepositRepository.ListPendingDeposits(ctx, chain Chain) ([]Deposit, error)` (state `observed`/`safe` only); `DepositRepository.OrphanDeposit(ctx, depositID string) error` (transitions to `orphaned` + writes `deposit.orphaned` outbox event, same transaction)
- [x] `internal/core/track_deposits.go` -- first phase of `Execute` (before the observed-scan/promotion phases, inside the same transaction): `ListPendingDeposits(chain)`, dedupe by `block_number`, call `BlockHash` per unique height, `OrphanDeposit` any whose stored hash doesn't match (or whose height no longer exists)
- [x] `internal/adapter/evm/scanner.go` -- `BlockHash`: `HeaderByNumber(ctx, big.NewInt(int64(blockNumber)))`, `exists=false` if the RPC reports no header at that height (chain shorter than expected), else return its hash; `scanNativeTransfers` populates `BlockHash` from the already-fetched `block.Hash()`; `scanERC20Transfers` populates it from `l.BlockHash`
- [x] `internal/adapter/postgres/deposit_repo.go` -- `RecordObserved`'s `INSERT` includes `block_hash`; its `ON CONFLICT` clause targets the new partial index (`ON CONFLICT (chain, tx_hash, log_index) WHERE state != 'orphaned' DO NOTHING`); `ListPendingDeposits` (`SELECT ... WHERE chain=$1 AND state IN ('observed','safe')`); `OrphanDeposit` (`UPDATE deposits SET state='orphaned' WHERE id=$1` + outbox insert, mirroring `RecordObserved`'s paired-write pattern)
- [x] `internal/adapter/postgres/deposit_reader.go` -- widen `WHERE da.customer_id = $1 AND d.state IN (...)` to include `'orphaned'`
- [x] `internal/adapter/api/deposits.go` -- `status` is `"pending"` for observed/safe, `"orphaned"` for orphaned; `tier` includes `"orphaned"`
- [x] `api/openapi.yaml` -- `Deposit.status` enum `[pending, orphaned]`; `Deposit.tier` enum `[observed, safe, orphaned]`; regenerate `server.gen.go`
- [x] `internal/core/track_deposits_test.go` -- fake extensions for `BlockHash`/`ListPendingDeposits`/`OrphanDeposit`; new cases: matching hash → no change, mismatched hash → orphaned + outbox event, height beyond chain head → orphaned, an orphaned deposit is never re-selected by later polls' reorg-check
- [x] `internal/adapter/evm/scanner_test.go` -- new real-anvil test using `anvil_reorg` (confirm the exact RPC call shape against the installed anvil version rather than assuming it) to force a real reorg and prove `BlockHash` reflects the new canonical history
- [x] `internal/adapter/api/integration_test.go` -- new test: seed an `observed` deposit with a stale `block_hash`, directly invoke the reorg-check path (or `TrackDeposits.Execute` against a fake/real scanner) to confirm it orphans, then seed a fresh `observed` row for the identical `(chain, tx_hash, log_index)` and confirm both rows coexist (old orphaned, new observed); confirm `GET /customers/{id}/deposits` shows the orphaned row with `status: "orphaned"`

**Acceptance Criteria:**
- Given an `observed`/`safe` deposit whose block has been replaced by a competing history, when the watcher polls, then it transitions to `orphaned`, its `GET /customers/{id}/deposits` visibility reflects this, and no balance is ever affected.
- Given a reorg is followed by the same transaction reappearing in the canonical chain, when the watcher re-observes it, then a fresh `observed` deposit record exists alongside the (untouched) orphaned one — never double-counted.
- Given this behavior, when tested against real `anvil_reorg`, then it is reproduced deterministically, not merely asserted against fakes.

## Spec Change Log

## Review Triage Log

### 2026-07-17 — Review pass

- intent_gap: 0
- bad_spec: 0
- patch: 6 (high 3, medium 2, low 1)
- defer: 1 (medium 1)
- reject: 3
- addressed_findings:
  - `[high]` `[patch]` `ALTER TABLE deposits ADD COLUMN block_hash text NOT NULL` had no `DEFAULT` — deploy-blocking against any `deposits` table that already has rows (any real environment where Stories 2.1-2.3's watcher has run), since Postgres can't satisfy `NOT NULL` for pre-existing rows without one. Fixed: `block_hash` is nullable at the DB level (format CHECK only applies when non-null); `RecordObserved` always populates it for every newly-recorded deposit going forward; `checkForReorgs`/`ListPendingDeposits` skip reorg-checking for any legacy row where it's `NULL` (their historical hash was never captured, so there's nothing to compare) rather than guessing or forcing a value.
  - `[high]` `[patch]` The down-migration re-added a plain, non-partial `UNIQUE(chain, tx_hash, log_index)` constraint — but this story's entire point (AC2) is letting an orphaned row and its re-observed successor share that key, so rolling back after even one reorg-and-reappear (exactly what `TestReorgDetection_EndToEnd` exercises) would fail the `ADD CONSTRAINT` with a duplicate-key violation, leaving a rollback stuck. Fixed: the down-migration now deletes the orphaned half of any colliding pair before re-adding the plain constraint (down-migrations are dev/rollback tooling, not a production path — sacrificing an already-superseded orphaned audit row on rollback is the right tradeoff).
  - `[medium]` `[patch]` `checkForReorgs` makes its `BlockHash` RPC calls after `Begin()`, holding the DB transaction open for the duration of those network round-trips. Re-examined against the existing codebase rather than fixed by restructuring: `ScanDeposits` (Story 2.1) already does the identical thing — up to `maxBlocksPerScan` RPC calls per poll, also passed `txCtx`, also after `Begin()` — so this is not a new inconsistency Story 2.4 introduces, it's an established, accepted characteristic of this watcher's transaction model. Downgraded from the "unbounded" framing to a bounded one by the batch-cap patch below (mirroring how `ScanDeposits`' own RPC-during-tx exposure is bounded by `maxBlocksPerScan`), rather than restructuring `DepositRepository` into an awkward dual pool/transaction mode purely to move one read earlier.
  - `[medium]` `[patch]` `ListPendingDeposits`/`checkForReorgs` had no batch cap, unlike `CreditFinalizedDeposits`' deliberate `maxCreditsPerPoll` guard against exactly this class of risk in the same file. Fixed: added an analogous cap, mirroring the established pattern.
  - `[medium]` `[patch]` `OrphanDeposit`'s `UPDATE` didn't check `RowsAffected` — a non-matching `depositID` would still "succeed" and write a `deposit.orphaned` outbox event for a transition that never happened; the test fake was already stricter than the real implementation (returning an error in this case), meaning no test could have caught the gap. Fixed: the real implementation now checks `RowsAffected` and returns an error on zero, matching the fake.
  - `[low]` `[patch]` `GetCustomerDeposits`' doc comment was left stale (`"both observed and safe tiers exposed as pending"`), not updated to mention the new orphaned tier this same diff routes through it — unlike the parallel comment in `deposit_reader.go`, which was correctly updated. Fixed.
  - `[medium]` `[defer]` `BlockHash`'s not-found/mismatch signal is a single RPC call per poll with no retry — a transient provider hiccup could in principle be misread as "chain got shorter." Not patched now: the current architecture assumes one dedicated RPC endpoint per chain (no load-balanced/multi-replica setup exists anywhere in this codebase's config model — that style of multi-provider concern is explicitly AD-12/Epic-5's territory, for reconciliation only), under which a spurious not-found/mismatch essentially requires an actual reorg to occur. Hardening against an unreliable or multi-replica RPC setup is a larger, more speculative undertaking better suited to Epic 6's fault-injection work or whenever this system's RPC-provider model is generalized.
  - `[low]` `[reject]` Building `idx_deposits_active_chain_tx_hash_log_index` with a plain (non-`CONCURRENTLY`) `CREATE UNIQUE INDEX` takes a blocking lock for the build's duration. This is not a regression this story introduces — every prior migration in this codebase (0001 through 0007) builds its indexes the same way; this system's migrations have always been simple blocking `goose Up` calls at startup, never a zero-downtime rolling-migration design.
  - `[low]` `[reject]` A TOCTOU gap exists between `Head()`'s tags and `checkForReorgs`' own independent `BlockHash` calls (not drawn from one atomic chain snapshot). Self-correcting by design: `checkForReorgs` re-verifies every pending deposit's hash on every single poll, a few seconds apart, so any inconsistency from this gap is caught and corrected on the very next cycle — building cross-call snapshot consistency for a self-healing, low-consequence timing gap is disproportionate.
  - `[low]` `[reject]` The API handler has no explicit guard against an unrecognized `DepositState` reaching it (would silently default to `status: "pending"`). `DepositReader`'s `WHERE state IN (...)` filter is already the established, enforced boundary for this exact concern (the same reasoning applied to and accepted for `transactionStatus`'s two-way branch in the 2.2 review) — a state that can't reach the handler doesn't need a second guard at the handler layer too.

## Design Notes

- **Detection is a stored-hash comparison, not a depth heuristic.** Every `observed`/`safe` deposit's `block_hash` (captured at observation time) is compared against the chain's *current* hash at that same height every poll. A mismatch — or the height no longer existing at all — is unambiguous proof the block was replaced; no confirmation-count tuning is needed or wanted.
- **The partial unique index is the crux of AC2.** A re-broadcast of the exact same signed transaction (same nonce, same signature) after a reorg has the identical `tx_hash` — Ethereum transaction hashes are a function of signed content, not block context. The old plain `UNIQUE(chain, tx_hash, log_index)` would silently block a legitimate re-observation via `ON CONFLICT DO NOTHING`, treating "reappeared for real" as "already recorded." Scoping the uniqueness to `WHERE state != 'orphaned'` fixes this precisely: at most one *active* record per event, but an orphaned record no longer counts against a fresh one.
- **Reorg-checking runs before every other phase in `Execute`**, so a deposit orphaned this cycle can never also be promoted or credited this same cycle — ordering is what makes AC1's "no balance ever affected" hold without needing a special-case guard in `PromoteToSafe`/`PromoteToFinalized`/`CreditFinalizedDeposits`.
- **Pending deposits are deduped by `block_number` before calling `BlockHash`** — a straightforward, cheap optimization (not a scale requirement) avoiding redundant RPC calls when multiple deposits share a block.

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green, including the real-anvil `anvil_reorg` test and the extended integration test
- `make check-import-boundary` -- expected: still passes
- `cd contracts && forge test` -- expected: unaffected, still 4/4

**Manual checks (if no CLI):**
- Run the full local stack against anvil; observe a deposit; force a reorg via `cast rpc anvil_reorg ...` (or the anvil CLI equivalent) that replaces its block; confirm the next poll orphans it and `GET /customers/{id}/deposits` shows `status: "orphaned"`; re-send the same transaction and confirm a fresh `observed` deposit appears.

## Auto Run Result

**Status:** done

**Summary:** Added reorg detection for pre-finality deposits: every `observed`/`safe` deposit now stores the hash of the block it was seen in, and the watcher's poll cycle re-verifies that hash against the chain's current history before any promotion or crediting runs — a mismatch (or a height that no longer exists) orphans the deposit and writes a `deposit.orphaned` outbox event, with the ledger untouched (only pre-finality rows are ever candidates). A partial unique index (scoped to non-orphaned rows) lets the same transaction reappear as a brand-new `observed` record after being orphaned, never conflated with the stale one. Verified against a real `anvil_reorg`-forced reorg, not just fakes. `GET /customers/{id}/deposits` now surfaces orphaned deposits with `status: "orphaned"`.

**Files changed:**

*New:*
- `internal/adapter/postgres/migrations/0008_add_deposit_reorg_detection.sql` — nullable `block_hash` column (deploy-safe against a populated table), dropped the plain `UNIQUE(chain, tx_hash, log_index)` constraint (confirmed its real name empirically) in favor of a partial unique index scoped to non-orphaned rows, with a down-migration that reconciles any orphaned/active duplicate pair before restoring the old constraint

*Modified:*
- `internal/core/deposit.go`, `ports.go` — `Deposit`/`ObservedTransfer` gain `BlockHash`; `ChainScanner.BlockHash`; `DepositRepository.ListPendingDeposits`, `.OrphanDeposit`
- `internal/core/track_deposits.go`, `track_deposits_test.go`, `get_customer_deposits.go` — reorg-check phase runs first in `Execute` (before scan/promotion/crediting), skips legacy rows with no stored hash, doc comment corrected
- `internal/adapter/evm/scanner.go`, `scanner_test.go` — `BlockHash` impl (confirmed `ethereum.NotFound` semantics and the real `anvil_reorg` RPC shape empirically); both scan paths populate `BlockHash` on every `ObservedTransfer`
- `internal/adapter/postgres/deposit_repo.go` — `RecordObserved` persists `block_hash` and its `ON CONFLICT` now targets the partial index; `ListPendingDeposits` (LIMIT-bounded, mirrors `CreditFinalizedDeposits`' cap), `OrphanDeposit` (now checks `RowsAffected`)
- `internal/adapter/postgres/deposit_reader.go`, `internal/adapter/api/deposits.go` — widen the state filter and API response to include `orphaned`
- `api/openapi.yaml`, `server.gen.go` — widened `Deposit.status`/`Deposit.tier` enums (regenerated)
- `internal/adapter/api/integration_test.go` — `TestReorgDetection_EndToEnd`

**Review findings breakdown** (2026-07-17 pass, Blind Hunter + Edge Case Hunter, 15 raw → 10 deduplicated):
- 6 patch (3 high, 2 medium, 1 low) — all applied and independently re-verified, including a manual `goose up`/`down` cycle against a real Postgres seeded with the exact orphaned-plus-fresh coexistence scenario this story creates: made `block_hash` deploy-safe (nullable, no backfill assumption); fixed the down-migration to reconcile duplicates before restoring the old constraint (confirmed working against real Postgres); added a batch cap to the reorg-check phase; fixed `OrphanDeposit` to check `RowsAffected`; fixed a stale doc comment
- 1 defer (medium) — logged in `deferred-work.md`: `BlockHash`'s single-RPC-call detection has no retry against a hypothetically unreliable/multi-replica provider (not applicable to this system's current single-endpoint-per-chain architecture)
- 3 reject (noise): non-concurrent index build during migration (pre-existing project-wide pattern, not a regression); a self-correcting TOCTOU gap between `Head()` and the reorg-check's own RPC calls; the API handler's lack of a second defensive guard beyond the reader's already-enforced state filter
- 0 intent_gap, 0 bad_spec

**Follow-up review recommended:** `true` — this story changed a live production constraint (dropping and replacing `deposits`' uniqueness guarantee with a partial index) and introduced the platform's first state-reversal transition; both are exactly the kind of change that warrants one more independent look, especially given the review pass already caught a deploy-blocking migration bug and a rollback-breaking down-migration in this same diff.

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `make check-import-boundary` — all clean
- `go test ./...` — all green, including the real-Postgres integration suite (`TestReorgDetection_EndToEnd`) and all three real-anvil scanner tests (including the new `anvil_reorg`-forced test)
- `cd contracts && forge test` — unaffected, 4/4
- **Manual verification beyond the spec's own commands**: ran `goose up` through migration 0008 against a real throwaway Postgres container, seeded an orphaned deposit and a fresh `observed` deposit sharing the identical `(chain, tx_hash, log_index)` (the exact AC2 coexistence scenario), then ran `goose down` — confirmed it succeeds cleanly, correctly deletes the orphaned duplicate, and leaves the fresh row intact. This is the specific failure mode Blind Hunter and Edge Case Hunter both flagged before the patch.
- Review diff was scoped to exactly this story's changes (pre-implementation snapshots diffed against post-implementation state), so both reviewers reviewed only Story 2.4's actual changes, not Stories 2.1–2.3's already-reviewed code.

**Residual risks:**
- The 1 deferred item above remains open, tracked in `deferred-work.md`.
- No git commit was created (user's global no-auto-commit policy). All changes remain uncommitted, stacked on top of Stories 2.1–2.3's own uncommitted changes. `final_revision` reflects this — HEAD has not moved from `baseline_revision`.
- Legacy deposit rows from before this migration (`block_hash IS NULL`) are permanently exempt from reorg-checking — an accepted, documented tradeoff given a historical hash can't be retroactively reconstructed, not something a future story needs to "fix" so much as be aware of.
