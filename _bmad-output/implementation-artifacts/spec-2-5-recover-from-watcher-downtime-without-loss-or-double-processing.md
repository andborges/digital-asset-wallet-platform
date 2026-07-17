---
title: 'Story 2.5: Recover From Watcher Downtime Without Loss or Double-Processing'
type: 'feature'
created: '2026-07-17'
status: 'done'
review_loop_iteration: 0
followup_review_recommended: true
context: []
warnings: []
baseline_revision: 'cdaacd14d585f20c0b567179ecb10b803868b8d7'
final_revision: 'NOT_COMMITTED (user global policy: no auto-commits — see Auto Run Result)'
---

<intent-contract>

## Intent

**Problem:** Stories 2.1–2.4 already built cursor-based, one-transaction-per-poll recovery as an architectural property (every poll resumes from the last *committed* cursor; a failed/interrupted poll rolls back entirely; re-observing an already-recorded event is a no-op) — but nothing explicitly proves it under the exact conditions this story's ACs describe: an extended backlog after real downtime, and a genuine mid-batch failure.

**Approach:** Add targeted tests that exercise the existing design under those conditions rather than build new recovery machinery: a multi-poll catch-up test proving a backlog spanning more than one `maxBlocksPerScan`-sized range is fully recovered with no gaps; a mid-batch-failure test proving a poll that fails partway through recording several deposits leaves nothing committed and a subsequent clean poll recovers everything exactly once. Add one small operator-facing improvement: log each tier's resumed cursor position at watcher startup, so a restart's recovery is visible, not silent.

## Boundaries & Constraints

**Always:**
- No change to the transaction/cursor architecture itself (Stories 2.1–2.4's design already satisfies these ACs by construction) — this story proves it, it doesn't rebuild it.
- Every new test must exercise the *real* mechanism (real fakes driving `TrackDeposits.Execute`, or real Postgres/anvil where already established) — never assert against a re-description of the code's own logic.
- The mid-batch-failure test's scope is the observed-scan/`RecordObserved` loop specifically (AC3's literal "processing of a batch of blocks") — not `CreditFinalizedDeposits`' per-deposit credit loop, whose own mid-batch atomicity is already deferred to Story 6.3's consolidated fault-injection suite (2.2 review) and stays there.

**Block If:** (none.)

**Never:**
- A real OS-process-kill test (spawning the compiled `walletd watcher` binary and sending it `SIGKILL` mid-poll) — Postgres's own crash-safety guarantee (an uncommitted transaction from a dropped connection is rolled back automatically) is the database engine's responsibility, not something this codebase re-implements or needs to prove via a timing-dependent, flaky subprocess test. Proving the application-level transaction/cursor logic recovers correctly (this story's actual job) is the right level of rigor.
- Reopening or duplicating Story 6.3's deferred consolidated fault-injection scope (the credit-batch atomicity test, the transfer lock-ordering deadlock test) — this story adds its own narrower, AC3-scoped test, not a general fault-injection framework.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Backlog spans more than one `maxBlocksPerScan` range | Cursor far behind `latest` (simulated extended downtime) | Full catch-up across multiple `Execute()` calls, no gap, no duplicate, cursor advances by the capped amount each call | none |
| Rescan re-observes an already-recorded deposit | Same `(chain, tx_hash, log_index)` reprocessed after "downtime" | No duplicate row (existing behavior, explicitly asserted under this scenario) | none |
| Poll fails partway through recording a batch of deposits | `RecordObserved` errors on the Nth of several transfers in one `ScanDeposits` result | Whole transaction rolls back: no deposit rows, no cursor advance, no outbox events — from that poll | Poll returns an error; caller (the watcher loop) logs it and retries next tick |
| Poll retried after a mid-batch failure | Same range re-scanned on the next `Execute()` call | Every deposit from that range is recorded exactly once — none lost, none duplicated | none |
| Watcher restarts | Process starts fresh | Startup log line reports each tier's resumed cursor position | none |

</intent-contract>

## Code Map

- `internal/core/track_deposits_test.go` -- new tests: multi-poll catch-up across a `maxBlocksPerScan`-spanning backlog; mid-batch `RecordObserved` failure → full rollback → clean retry recovers everything
- `cmd/walletd/main.go` -- log each tier's resumed cursor at watcher startup, before the poll loop begins

## Tasks & Acceptance

**Execution:**
- [x] `internal/core/track_deposits_test.go` -- `TestTrackDeposits_Execute_MultiPollCatchUpAfterDowntime`: a `fakeScanner` reporting `latest` far beyond the persisted cursor (more than `maxBlocksPerScan` blocks of "missed" backlog) with transfers spread across that whole range; call `Execute` repeatedly until the cursor reaches `latest`; assert every transfer was recorded exactly once and the cursor advanced by `maxBlocksPerScan` (or less, on the final call) each time
- [x] `internal/core/track_deposits_test.go` -- `TestTrackDeposits_Execute_MidBatchScanFailureRollsBackCleanly`: a `fakeDepositRepo.RecordObserved` that errors on the Nth call within a single `Execute` invocation (simulating a poll that fails partway through recording a multi-deposit scan result); assert `Execute` returns an error, the fake transaction was rolled back (not committed), and neither the cursor nor any deposit advanced
- [x] `internal/core/track_deposits_test.go` -- `TestTrackDeposits_Execute_RetryAfterMidBatchFailureRecoversFully`: following the failure above, call `Execute` again against the same (unadvanced) state; assert every deposit from that range is now recorded exactly once — proving AC3's "never skipping or reprocessing ambiguously" concretely, not just "it returned no error"
- [x] `cmd/walletd/main.go` -- in `runWatcher`, after acquiring the advisory lock and before the poll loop starts, read and log the current `CursorTierObserved`/`CursorTierSafe`/`CursorTierFinalized` cursor values for this chain (via the existing `DepositRepository.Cursor` method, no new port needed) so a restart's resumption point is operator-visible, not silent

**Acceptance Criteria:**
- Given the watcher was down for a period spanning more than one poll's worth of blocks, when it restarts and polls repeatedly, then it fully catches up from its last persisted cursor with no missed or duplicated deposit.
- Given a rescan re-observes an already-recorded deposit, when reprocessed, then the existing `(chain, tx_hash, log_index)` constraint makes it a no-op — proven explicitly under a downtime/rescan scenario, not just a same-poll repoll.
- Given the watcher fails partway through recording a batch of scanned transfers, when it restarts (or the next tick runs), then it resumes from the last *committed* cursor and recovers every deposit from that range exactly once — never skipping, never double-processing.

## Spec Change Log

## Review Triage Log

### 2026-07-17 — Review pass

- intent_gap: 0
- bad_spec: 0
- patch: 5 (medium 3, low 2)
- defer: 1 (low 1)
- reject: 4
- addressed_findings:
  - `[medium]` `[patch]` The startup cursor-logging block (added during this same review cycle to fix an earlier panic) turned a pure observability nicety into a fatal startup dependency two ways: (1) a shutdown signal landing in the narrow window right after "watcher starting" cancels its context and the block returns an error instead of the clean shutdown path the poll loop uses for the identical signal moments later; (2) any transient DB hiccup here kills the whole process, unlike `Execute`'s own established "log and retry" philosophy for the exact same class of error one loop iteration later. Fixed: the block now logs a warning and continues to the poll loop on any error (including cancellation) rather than returning one — it can no longer prevent the watcher from starting.
  - `[medium]` `[patch]` The new startup transaction's `Rollback` used the raw, cancelable `ctx` — every other rollback site in this codebase (`track_deposits.go`, `middleware_idempotency.go`) deliberately uses `context.WithoutCancel` so a canceled parent can't interfere with cleanup. Fixed: matched the established pattern.
  - `[medium]` `[patch]` Nothing guards against the exact class of bug that already caused one real panic (a transaction-required repository method called outside a transaction) ever recurring — the only defense was a code comment. Building a full regression-test harness for `cmd/walletd` (which has never had any test infrastructure) was judged disproportionate to a small logging nicety; instead, added a `recover()` around the cursor-logging block so any future reintroduction of this bug degrades to a logged warning instead of a fatal crash, consistent with treating this block as best-effort (same philosophy as the fix above).
  - `[low]` `[patch]` The throwaway startup transaction's `Rollback` error was silently discarded, unlike every other error in this same block, which is carefully wrapped and surfaced. Fixed: logged as a warning if it fails (non-fatal, matching the block's now-best-effort nature).
  - `[low]` `[patch]` `TestTrackDeposits_Execute_MidBatchScanFailureRollsBackCleanly`'s comment implied its cursor-unchanged assertions prove the rollback mechanism works, but `SetCursor` for any tier is only ever reached after the `RecordObserved` loop completes — the injected failure happens inside that loop, so those specific assertions would read the same regardless of whether rollback/restore worked at all. Fixed: corrected the comment to attribute the real proof to the assertions that do depend on `restore()` (`repo.inserted`/`repo.seen` being empty, given the first of three transfers did succeed before the injected failure).
  - `[low]` `[reject]` This diff's fix opens and immediately discards a real Postgres transaction to satisfy `DepositRepository.Cursor`'s `txFromContext` requirement, rather than following the established "read-only repos hold their own pool" pattern (`DepositReader`, `BalanceRepository`, etc.). Considered and rejected: `DepositRepository` is fundamentally a write-path, transaction-only repository by design (its `Cursor` method is also called mid-transaction from `TrackDeposits.Execute`) — giving it a second, pool-backed mode for just this one read would trade one inconsistency for a different one (why does only *this* method on the struct bypass `txFromContext`?).
  - `[low]` `[reject]` The test hardcodes `maxBlocksPerScan = 2000` with a comment about not being able to reference the unexported production constant; a future production value that happens to still produce the same call count over this test's fixed 5000-block backlog wouldn't be caught by every assertion as cleanly as the comment implies. Rejected as excessive hardening against a low-probability, low-consequence divergence (a wrong test assertion, not a production bug) — the test already fails loudly if the call count changes at all.
  - `[low]` `[reject]` Three sequential `Cursor` round-trips at startup (one per tier) could be one query. Rejected: this runs once per process lifetime (not per poll), the overhead is milliseconds, and adding a new repository method purely to save two round trips in a logging nicety is disproportionate.
  - `[low]` `[reject]` The new startup transaction holds a second pool connection alongside the advisory-lock connection already held for the process's lifetime. Rejected: the connection is released within milliseconds (unlike the lock connection), and two simultaneous connections at startup is comfortably within any reasonable pool default — not worth a comment or a fix.
  - Startup log ordering being momentarily misleading if the cursor-read block fails (a finding related to the two above) is resolved as a side effect of the first patch above: since the block can no longer fail fatally, "starting" followed by either the cursor line or a warning is now always coherent.
  - `[low]` `[defer]` The test harness's mid-batch-failure rollback simulation (`snapshot`/`restore`) only covers `fakeDepositRepo`, not `fakeUnsupportedTokenRepo` — a future test combining an injected failure with non-empty unsupported-token transfers would get a false pass on rollback correctness for that path. Not fixed now: this story's mid-batch test is explicitly scoped to the observed-scan/`RecordObserved` loop per its own `<intent-contract>` boundary; extending the harness for a path no current test exercises is out of scope here.

## Design Notes

- **This story is validation-first, not a new-capability story.** Stories 2.1's "one transaction per poll, cursor advances only on commit" design was already built with NFR8/FR14 in mind — its own doc comments already say "so the next poll simply retries the same block range." The gap this story closes is that nothing had explicitly proven it under the specific conditions these ACs describe (a backlog spanning multiple polls; a genuine mid-batch failure), which is exactly the kind of gap the project's "prove it, don't just assert it" discipline (already applied via `anvil_reorg`, real Postgres integration tests, etc.) exists to close.
- **Why not a real process-kill test:** killing the actual `walletd watcher` binary and restarting it would prove the same thing the fake-based test proves, but non-deterministically (OS scheduling/signal timing) and at much higher implementation cost, while ALSO silently re-testing Postgres's own crash-safety guarantee (dropped connections roll back) rather than this codebase's logic. The fake-based test isolates exactly the property this codebase is responsible for.
- **Scope boundary with the 2.2-deferred credit-batch atomicity test:** AC3 is about "a batch of blocks" — the observed-scan phase. `CreditFinalizedDeposits`' own per-row credit loop has a structurally identical (but distinct) atomicity question already deferred to Story 6.3; this story doesn't reopen or duplicate that scope.

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green, including the three new unit tests
- `make check-import-boundary` -- expected: still passes
- `cd contracts && forge test` -- expected: unaffected, still 4/4

**Manual checks (if no CLI):**
- Restart `walletd watcher --chain=base` against a chain with a pre-existing cursor; confirm the startup log line reports the resumed cursor positions.

## Auto Run Result

**Status:** done

**Summary:** Added three tests proving Stories 2.1-2.4's cursor-based, one-transaction-per-poll design actually delivers this story's acceptance criteria: full multi-poll catch-up across a backlog spanning more than one `maxBlocksPerScan` range (no gaps, no duplicates, correct cursor advancement each call); a mid-batch `RecordObserved` failure rolling back cleanly (nothing committed, transaction rolled back, no partial cursor/deposit state); and a subsequent retry recovering every deposit from the same range exactly once. Added one operator-facing improvement: the watcher now logs each tier's resumed cursor position at startup. No new recovery machinery, tables, or ports were added — the existing architecture already satisfied these ACs by construction; this story proves it.

**A real bug was caught and fixed during implementation verification, independent of the formal review pass**: the first version of the startup cursor-logging code called a transaction-required repository method (`DepositRepository.Cursor`) outside of any transaction, which panics by design (`txFromContext`'s explicit AD-4 guard) — since `cmd/walletd` has zero automated test coverage, `go test ./...` never caught this; only actually running the compiled binary did. Confirmed via a real run against Postgres + anvil that every watcher startup panicked immediately. Fixed by wrapping the three cursor reads in a short-lived, rolled-back-immediately transaction, then re-confirmed via another real run that the watcher now starts and logs correctly.

**Files changed:**

*Modified:*
- `internal/core/track_deposits_test.go` — three new tests (`TestTrackDeposits_Execute_MultiPollCatchUpAfterDowntime`, `TestTrackDeposits_Execute_MidBatchScanFailureRollsBackCleanly`, `TestTrackDeposits_Execute_RetryAfterMidBatchFailureRecoversFully`); extended `fakeScanner` (block-range filtering + range history), `fakeDepositRepo` (failure injection + snapshot/restore), `fakeTx`/`fakeTxBeginner` (rollback-restore wiring) to support them
- `cmd/walletd/main.go` — startup cursor-logging block (fixed from its initial panicking version, then further hardened by the review pass below to be fully non-fatal)

**Review findings breakdown** (2026-07-17 pass, Blind Hunter + Edge Case Hunter, 14 raw → 12 deduplicated):
- 5 patch (3 medium, 2 low) — all applied and independently re-verified with real runs (normal startup, and a `kill`-during-startup check): made the startup cursor-logging block fully non-fatal (any error, including a shutdown-signal race, is now logged as a warning rather than aborting the whole watcher — closing both a fatal-on-transient-error regression and a shutdown-signal race the initial fix introduced); matched the codebase's established `context.WithoutCancel` rollback pattern; added a `recover()` safety net so any future reintroduction of the original panic degrades to a warning instead of a crash; logged (instead of silently discarding) a rollback failure; corrected a misleading test comment about which assertions actually depend on rollback working
- 1 defer (low) — logged in `deferred-work.md`: the mid-batch-failure test harness's rollback simulation doesn't cover `fakeUnsupportedTokenRepo`, out of this story's explicitly observed-scan-scoped boundary
- 4 reject (noise): an alternative "pool-backed read" design for the cursor fix (has its own consistency tradeoffs); a hypothetical future test-divergence scenario judged excessive to guard against; combining 3 startup round-trips into 1 (disproportionate for a once-per-process-lifetime log line); an extra pool-connection dependency at startup (well within any reasonable pool default)
- 0 intent_gap, 0 bad_spec

**Follow-up review recommended:** `true` — this small diff already produced one real panic (caught only by manual execution, not `go test`) and the review pass caught two further fatal-startup regressions in the fix for it; `cmd/walletd`'s complete lack of automated test coverage means this class of bug has no automated safety net going forward, which is worth a fresh independent look given the pattern has now repeated within a single story.

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `make check-import-boundary` — all clean
- `go test ./...` — all green, including every pre-existing test (unaffected) and all three new tests
- `cd contracts && forge test` — unaffected, 4/4
- **Manual verification beyond the spec's own commands** (necessary given `cmd/walletd` has no automated tests): ran the compiled `walletd watcher` binary against real Postgres + anvil twice — once to discover the original panic (confirmed via the actual panic trace), once after the fix to confirm clean startup, correct cursor logging, and clean shutdown on a signal
- Review diff was scoped to exactly this story's two changed files, so both reviewers reviewed only Story 2.5's actual changes

**Residual risks:**
- The 1 deferred item above remains open, tracked in `deferred-work.md`.
- No git commit was created (user's global no-auto-commit policy). All changes remain uncommitted, stacked on top of Stories 2.1–2.4's own uncommitted changes. `final_revision` reflects this — HEAD has not moved from `baseline_revision`.
- `cmd/walletd` still has zero automated test coverage — this story's own experience (one real panic, caught only by manual execution) is itself evidence this gap is worth closing at some point, though building that harness was judged out of proportion for this specific story's scope.
