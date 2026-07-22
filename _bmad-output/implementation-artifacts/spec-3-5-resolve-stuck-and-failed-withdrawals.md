---
title: 'Story 3.5: Resolve Stuck & Failed Withdrawals'
type: 'feature'
created: '2026-07-22'
status: 'done'
review_loop_iteration: 0
followup_review_recommended: true
context: []
warnings: ['oversized']
baseline_revision: '2961320fd05994af6422eed471f5467dd37188a1'
final_revision: '2961320fd05994af6422eed471f5467dd37188a1 (NOT_COMMITTED past this point: user global policy — no auto-commits. Story 3.5 and its review-pass patches remain uncommitted, stacked on Stories 3.3 and 3.4.)'
---

<intent-contract>

## Intent

**Problem:** Story 3.4's broadcaster has no path forward once anything goes wrong mid-flight: a crash between allocating a nonce and confirming the broadcast leaves a withdrawal permanently parked at `signed`; a broadcast the node never accepts is retried forever with no escalation; a broadcast-but-not-yet-confirmed withdrawal has no time-based signal telling an operator it needs attention; and a degraded chain (RPC/watcher unhealthy) burns one nonce-allocated withdrawal per poll tick into the same stranded state (deferred from Story 3.4's own review). No customer's funds may ever be silently lost or frozen (FR19).

**Approach:** Restructure the broadcaster's sign→send step so the signed transaction bytes and its hash are durably persisted to `broadcast_attempts` BEFORE the network send is attempted (not after, as Story 3.4 left it) — this is what makes resuming after ANY interruption (crash, send error, restart) safe: resuming re-sends the byte-identical previously-signed transaction rather than re-signing (AWS KMS's own ECDSA signing is not guaranteed deterministic, so a second signature over the same digest can legitimately differ — re-sending identical bytes is the only way to guarantee "no ambiguous double-broadcast" regardless of signer backend). A withdrawal broadcast longer than a configurable window without confirming gets a one-time `withdrawal.stuck` outbox event (operator-facing, documented resolution path in a new runbook) — a monitoring signal layered on the existing `broadcast` status, never a new terminal state. The broadcaster skips claiming new work for a chain whose watcher cursor hasn't advanced recently (AD-15's liveness signal, reusing the existing `watcher_cursors` table — no new heartbeat mechanism needed), resuming automatically once it does.

## Boundaries & Constraints

**Always:**
- Sign→persist→send ordering: `BuildUnsignedWithdrawal` → `Signer.Sign` → `AssembleSignedTx` → persist `(tx_hash, signed_tx)` to `broadcast_attempts` (status stays `signed`) → THEN attempt `SendRawTransaction` → on success, transition to `broadcast`. On ANY interruption before the transition to `broadcast` commits, the next poll cycle resumes by re-sending the ALREADY-PERSISTED `signed_tx` bytes verbatim — never re-signing (which could legitimately produce different bytes with the same nonce, the exact "ambiguous double-broadcast" risk this story exists to close) and never re-attempting a fresh claim (the nonce is already spent for this withdrawal; claiming again would double-allocate).
- Re-sending an already-persisted `signed_tx` is treated as successful/idempotent on ANY of: the send returning no error, or an error whose text is recognized as "already known"/"nonce too low" (both mean some transaction at this nonce already reached the node or chain — which, under AD-11's single-writer guarantee, can only be THIS withdrawal's own prior attempt, since nothing else ever sends from this hot wallet). Any other send error leaves the withdrawal at `signed` with `signed_tx` already persisted, retried again next poll — never immediately transitioned to `failed` (Boundaries: distinguishing a truly terminal node rejection from a transient one via RPC error-string matching is unreliable across different node implementations; the stuck-detection window below is what escalates a persistently-failing send to operator attention, not automated error classification).
- Liveness gate: before claiming, the broadcaster checks `watcher_cursors`' most recent `updated_at` for this chain's `observed` tier against a configurable staleness threshold (default 3x `WATCHER_POLL_INTERVAL`) — if stale, skip claiming this tick entirely (no new nonce allocated, no new withdrawal stranded) and log the transition into/out of degraded mode at most once, not every tick.
- Stuck detection: a `broadcast` withdrawal whose `broadcast_attempts.created_at` is older than a configurable threshold (`WITHDRAWAL_STUCK_THRESHOLD`, default 30m) AND whose `withdrawals.stuck_alerted_at` is still NULL gets exactly one `withdrawal.stuck` outbox event, then `stuck_alerted_at` is set — never re-alerted every poll for the same withdrawal, and never itself a status transition (the withdrawal can still resolve to `confirmed` or `failed` normally afterward).
- On-chain revert still settles to `failed` exactly as Story 3.4 already built (`SettleFailedWithdrawal`, unchanged) — this story adds NO new automated path to `failed`; every hold-release in this story's own new code is the crash/resume/stuck machinery keeping a withdrawal correctly at `signed`/`broadcast` until the chain itself resolves it, or an operator manually intervenes per the new runbook.
- The operator runbook (new doc, `docs/runbooks/stuck-withdrawals.md`) documents: what `withdrawal.stuck` means, how to inspect a withdrawal's true on-chain state, and the direct-database procedure to manually force a resolution (there is no operator-facing API for this in v1 — consistent with every other story's "no operator-identity system yet" carve-out).

**Block If:** (none — every open question below (send-error classification, stuck-alert idempotency, liveness signal source) has a reasoned, precedent-grounded default; see Design Notes.)

**Never:**
- Automated classification of `SendRawTransaction` error text into "terminal" vs "transient" — unreliable across node implementations (Base op-geth vs. Arbitrum Nitro), and unnecessary given the resume mechanism already treats any send failure as "retry next poll," escalating only via the time-based stuck signal.
- Fee-bump/replacement transactions — still explicitly out of scope (Story 3.4's own boundary, unchanged); resuming means re-sending the SAME signed bytes, never constructing a new one with different gas parameters.
- A new operator-facing API endpoint for manually resolving a stuck/failed withdrawal — the runbook's direct-database procedure is v1's only resolution path, matching this codebase's existing "no operator-identity system" pattern.
- Any change to Story 3.4's `SettleConfirmedWithdrawal`/`SettleFailedWithdrawal`/`GetFinalizedReceipt` logic, or to the confirm/fail settlement posting directions — both already correct and already reviewed.
- Sweep logic (Story 3.6).

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Fresh approved withdrawal, happy path | Claimed, never attempted before | Build → sign → persist `(tx_hash, signed_tx)` → send → `broadcast` | none |
| Crash after persisting `signed_tx`, before the send call returns | Restart finds `signed` + `signed_tx` set, no `broadcast` transition | Next poll resends the identical persisted bytes; success (or "already known") → `broadcast` | none |
| Crash after persisting `signed_tx`, send DID succeed on-chain, but the `broadcast` transition never committed | Same as above from this process's point of view | Resend is byte-identical to what's already on-chain; node reports "already known"/"nonce too low" — treated as success, transitions to `broadcast` | Recognized error text, not a failure |
| `SendRawTransaction` fails for any other reason | Send error, `signed_tx` already persisted | Withdrawal stays at `signed`; retried next poll; no immediate `failed` | Logged, not fatal |
| Chain's watcher cursor stale beyond the liveness threshold | `MAX(watcher_cursors.updated_at)` for this chain's `observed` tier too old | No new withdrawal claimed this tick; already-claimed/broadcast withdrawals still get polled for receipts | none |
| Broadcast withdrawal exceeds the stuck threshold, still unconfirmed, never alerted | `broadcast_attempts.created_at` old, `stuck_alerted_at` NULL | One `withdrawal.stuck` outbox event; `stuck_alerted_at` set | none |
| Same withdrawal, next poll, still unconfirmed | `stuck_alerted_at` already set | No second alert | none |
| Withdrawal confirms or fails AFTER being marked stuck | Receipt found | Settles exactly as Story 3.4 already does — `stuck_alerted_at` is left set (historical fact, never cleared) | none |

</intent-contract>

## Code Map

- `internal/adapter/postgres/migrations/0012_add_withdrawal_resume_and_stuck_detection.sql` -- `broadcast_attempts.signed_tx text` (nullable); `withdrawals.stuck_alerted_at timestamptz` (nullable)
- `internal/core/withdrawal.go` -- `Withdrawal.StuckAlertedAt *time.Time`
- `internal/core/ports.go` -- extend `WithdrawalRepository`: `RecordSignedTx(ctx, withdrawalID, txHash, signedTxHex string) error`, `MarkBroadcast(ctx, withdrawalID string) error`, `ListSignedWithdrawals(ctx, chain) ([]Withdrawal, error)` (returns each withdrawal plus its `broadcast_attempts` nonce/tx_hash/signed_tx), `ListStuckCandidates(ctx, chain, olderThan time.Duration) ([]Withdrawal, error)`, `MarkStuckAlerted(ctx, withdrawalID string) error`; new `WatcherLivenessChecker` port: `IsLive(ctx, chain, staleAfter time.Duration) (bool, error)`
- `internal/core/sign_and_broadcast_withdrawal.go` -- restructure `Execute` to branch on whether a claimed/resumed withdrawal already has a persisted `signed_tx` (resend path) vs. not (fresh build/sign/persist/send path); send errors return without transitioning status
- `internal/core/detect_stuck_withdrawals.go` -- new use case: lists stuck candidates, writes one `withdrawal.stuck` outbox event each, marks alerted
- `internal/adapter/postgres/withdrawal_repo.go` -- the new repository methods above
- `internal/adapter/postgres/watcher_liveness.go` -- new small repo implementing `WatcherLivenessChecker` against `watcher_cursors`
- `cmd/walletd/main.go` -- `runBroadcaster`'s poll loop gains the liveness gate before claiming, and a stuck-detection pass each tick; new `WITHDRAWAL_STUCK_THRESHOLD`/`LIVENESS_STALENESS_THRESHOLD` optional env vars (defaults per Boundaries)
- `docs/runbooks/stuck-withdrawals.md` -- new operator runbook (Code Map's own deliverable, not just a task side-effect)

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0012_add_withdrawal_resume_and_stuck_detection.sql` -- add both nullable columns; confirm no existing CHECK/index needs widening (neither column participates in one)
- [x] `internal/core/withdrawal.go`, `ports.go` -- struct field; five new/changed `WithdrawalRepository` methods; new `WatcherLivenessChecker` port
- [x] `internal/adapter/postgres/withdrawal_repo.go` -- `RecordSignedTx` (updates `broadcast_attempts.tx_hash`/`signed_tx`, `withdrawals` stays `signed`, `RowsAffected` checked); `MarkBroadcast` (transitions `signed`→`broadcast`, copies `tx_hash` to `withdrawals`, `RowsAffected` checked); `ListSignedWithdrawals`; `ListStuckCandidates` (WHERE state=broadcast AND broadcast_attempts.created_at < now()-threshold AND stuck_alerted_at IS NULL); `MarkStuckAlerted`
- [x] `internal/adapter/postgres/watcher_liveness.go` + test -- `IsLive`: `SELECT max(updated_at) FROM watcher_cursors WHERE chain=$1 AND tier='observed'`, compare against `now()-staleAfter`; no row at all counts as NOT live (a chain the watcher has never polled is not "live")
- [x] `internal/core/sign_and_broadcast_withdrawal.go` + test -- restructure per Boundaries; a claimed-fresh withdrawal builds/signs/persists/sends; a resumed withdrawal (already has `signed_tx`) skips straight to resending those exact bytes; "already known"/"nonce too low" error text on resend is treated as success
- [x] `internal/core/detect_stuck_withdrawals.go` + test -- new use case, one outbox event + one `MarkStuckAlerted` call per candidate, never re-alerting an already-alerted withdrawal
- [x] `cmd/walletd/main.go` -- wire the liveness gate before each claim attempt (log the degraded/recovered transition, not every tick); wire `DetectStuckWithdrawals` into the same poll loop; two new optional env vars with documented defaults
- [x] `docs/runbooks/stuck-withdrawals.md` -- what `withdrawal.stuck` means; how to check a withdrawal's real on-chain state (block explorer by `tx_hash`, or `eth_getTransactionByHash`/`eth_getTransactionReceipt` directly); the direct-database procedure for manually forcing `failed` (release the hold) when an operator determines the transaction will truly never confirm
- [x] `internal/adapter/postgres/withdrawal_broadcast_repo_test.go` (or a new file) -- integration coverage for: resume-after-persisted-signed_tx, resend-treated-as-success on "already known", liveness gate skipping a claim, stuck detection firing once and not twice
- [x] `internal/core/sign_and_broadcast_withdrawal_test.go`, `poll_withdrawal_receipts_test.go` -- unit coverage for the restructured Execute's two branches and the new use case, against fakes

**Acceptance Criteria:**
- [x] Given a withdrawal fails terminally via an on-chain revert, when observed, then its hold is released back to available immediately (FR19) — unchanged from Story 3.4, re-confirmed still holds.
- [x] Given a withdrawal is broadcast but not confirmed within the configured window, when detected, then exactly one `withdrawal.stuck` outbox event is written and the runbook documents its resolution path.
- [x] Given the broadcaster crashes after persisting a withdrawal's signed transaction but before recording a successful broadcast, when it restarts, then the withdrawal resumes by re-sending the identical persisted bytes, never re-signing and never double-allocating a nonce (FR16, NFR8).
- [x] Given a chain's watcher cursor is stale beyond the liveness threshold, when the broadcaster would normally claim new work, then it queues (claims nothing new) rather than rejecting or stranding a withdrawal, resuming automatically once the cursor advances (AD-15).

## Spec Change Log

## Review Triage Log

### 2026-07-22 — Review pass
- intent_gap: 0
- bad_spec: 0
- patch: 7 (high 1, medium 4, low 2)
- defer: 2 (medium 2)
- reject: 0
- addressed_findings:
  - `[high]` `[patch]` `ListStuckCandidates` only ever watched `WithdrawalStatusBroadcast`, leaving a withdrawal parked at `WithdrawalStatusSigned` (claimed, nonce allocated, but never successfully broadcast — exactly the state a persistent resend failure, a KMS/RPC outage, or the new liveness gate itself can leave one in) with zero monitoring coverage. Both the adversarial and edge-case review passes independently caught this, and it directly contradicts the story's own intent-contract ("crash/interruption leaves withdrawals permanently at `signed`... no stuck-detection"). Fixed: `ListStuckCandidates` now covers both statuses, using the status-appropriate staleness column for each (`broadcast_attempts.created_at`, the claim moment, for `signed`; `withdrawals.updated_at`, the broadcast moment, for `broadcast`). `MarkStuckAlerted`'s outbox payload now carries the triggering `status` so an operator isn't left inferring it. New tests cover both the positive case (a `signed` withdrawal old enough IS returned) and the negative case (one not yet old enough is not).
  - `[medium]` `[patch]` `MarkStuckAlerted` inserted the `withdrawal.stuck` outbox event BEFORE the guarded, `RowsAffected()`-checked `UPDATE ... WHERE stuck_alerted_at IS NULL`, so a double-invocation (e.g. two overlapping `DetectStuckWithdrawals` poll cycles) would insert a second outbox event before the guard could reject the write, undermining the story's own "exactly one alert" acceptance criterion at the moment of the race, not just eventually. Fixed: reordered to guard-then-write — the locked `SELECT` now fails loud immediately if already alerted, and the outbox insert only happens after the guarded `UPDATE` succeeds.
  - `[medium]` `[patch]` `SignAndBroadcastWithdrawal.Execute`'s liveness gate blocked resuming an already-`signed` withdrawal (persisted bytes, no new nonce, no liveness-related risk) in addition to fresh claims (which do allocate a new nonce and are exactly what the gate exists to protect). This meant a stale watcher cursor would strand every in-flight `signed` withdrawal too, not just pause new work — a bigger blast radius than AD-15 calls for. Fixed: added an `allowClaim bool` parameter; `cmd/walletd/main.go`'s poll loop now passes the liveness result only as `allowClaim`, so resuming always proceeds regardless of liveness and only fresh claiming is gated. Two new tests assert resuming proceeds with `allowClaim=false` while claiming does not.
  - `[medium]` `[patch]` `ListSignedWithdrawals` had no `LIMIT` and no ORDER BY tiebreaker, and a single malformed `signed_tx` hex value (e.g. from a future manual DB edit) would fail the entire batch rather than just that row. Fixed: added `LIMIT 50` (mirroring this story's other list queries' bounded-batch convention) and an `, id` tiebreaker; a hex-decode failure on one row now skips that row and continues rather than failing the whole call.
  - `[medium]` `[patch]` The runbook (`docs/runbooks/stuck-withdrawals.md`) never mentioned the liveness gate at all, despite it being new, load-bearing behavior an on-call operator investigating a `signed`-status stuck alert would need to understand. Fixed: added a dedicated section explaining what it is, how to check `watcher_cursors` directly, and — importantly — that it does NOT explain a `signed`-status alert by itself (it only blocks new claims, never resuming).
  - `[low]` `[patch]` The runbook's "chain nonce moved past this withdrawal's nonce but no receipt found" branch was, by the runbook's own reasoning elsewhere (AD-11 single-writer, byte-identical resends), seemingly unreachable — it never stated what precondition would actually produce it, which the edge-case review flagged as a gap that would leave an operator unsure whether to trust the branch at all. Fixed: clarified the branch's real preconditions (reorg replacing the block with a competing history, a misconfigured second broadcaster process violating AD-11, or an out-of-band send) and reclassified it as an incident to escalate rather than a routine case to resolve via Step 4.
  - `[low]` `[patch]` The runbook's outbox payload example didn't reflect the new `status` field or that `txHash` is now sometimes empty (a `signed`-status alert never reached broadcast). Fixed: updated the example and added a note steering the reader to skip Step 2 (which assumes a `tx_hash`) for that case.
- **Deferred** (`{implementation_artifacts}/deferred-work.md`): (1) `isAlreadyKnownError`'s resend-success recognition is a string match against a project-authored allowlist of provider phrasings, with no `eth_getTransactionReceipt`-based fallback if a provider ever phrases the same condition differently. (2) The liveness gate's default staleness threshold is a tuning question, not a logic bug — it may be tighter than needed to tolerate a merely transient watcher hiccup, but the right value needs real operational data this environment doesn't have yet. Neither is a mechanical fix; both need either a design decision or production data this review pass doesn't have.

## Design Notes

- **Why persist `signed_tx` before sending, not after (the core restructuring this story makes to Story 3.4's flow).** AWS KMS's ECDSA signing is not documented or guaranteed to be deterministic (unlike the software signer's RFC6979 determinism) — a second `Sign` call over the identical digest can legitimately produce a different, equally valid signature. If a crash forced re-signing on resume, the second signature could differ from a first attempt that ALREADY reached the network, producing two different valid transactions for the same nonce — never a double-spend (only one can ever be mined), but a genuinely ambiguous, hard-to-audit outcome the story's own AC3 explicitly forbids ("no ambiguous double-broadcast"). Persisting the exact signed bytes before the send call removes the ambiguity entirely: resuming always re-sends byte-identical data, which is unconditionally safe to repeat.
- **Why "already known"/"nonce too low" on resend is treated as success, not inspected further.** Under AD-11, exactly one process ever sends from the hot wallet, and nonces are allocated strictly sequentially inside one Postgres transaction per withdrawal — the only transaction that could ever occupy this exact nonce is this withdrawal's own (possibly already-sent) attempt. Either the resend is genuinely still needed (node accepts it, or it was never seen and now is) or the original already landed (node recognizes it as already-known/superseded) — both outcomes correctly converge on `broadcast`.
- **Why stuck detection is a one-time outbox event on top of the existing `broadcast` status, not a new terminal status value.** A stuck withdrawal isn't done — it can still confirm or revert normally once the underlying cause (usually a network delay or fee-cap-too-low situation, Story 3.4's own deferred concern) resolves. Modeling it as a status value would mean deciding what happens when a "stuck" withdrawal later confirms, adding a state-machine edge only to immediately special-case it away.
- **Why the liveness signal reuses `watcher_cursors` rather than a new heartbeat table.** The watcher already updates this table's `updated_at` every successful poll cycle (Story 2.1); a stalled watcher (RPC down, crashed, etc.) stops advancing it. This is exactly AD-15's "watcher heartbeat/cursor staleness" signal, already durably persisted — a new table would just be tracking the same fact twice. As a side effect, this also substantially mitigates Story 3.4's own deferred concern (a sustained RPC/signer outage stranding one withdrawal per poll tick): the SAME kind of outage that breaks KMS/RPC calls very plausibly also stalls the watcher's own polling, so the liveness gate would pause new claims in most of those cases too — not a complete fix (the watcher and broadcaster hit different RPC endpoints/services), but a meaningful reduction, achieved for free by reusing existing infrastructure.

## Verification

**Commands:**
- `go build ./... && go vet ./... && gofmt -l .` -- expected: clean
- `go test ./internal/core/... ./internal/adapter/postgres/...` -- expected: all green (pre-existing, unrelated reorg-detection failures excepted)
- `make check-import-boundary` -- expected: still passes

**Manual checks (if no CLI):**
- With `anvil` installed: kill the broadcaster process between persisting `signed_tx` and the send call (a debug breakpoint or artificial delay), restart it, and confirm the withdrawal still reaches `broadcast`/`confirmed` with no duplicate transaction on-chain.
- Set `WITHDRAWAL_STUCK_THRESHOLD` very low in a local run, broadcast a withdrawal against a chain that won't confirm quickly, and confirm exactly one `withdrawal.stuck` outbox row appears, never a second one on subsequent polls.

## Auto Run Result

**Status:** done

**Summary:** Implemented Story 3.5 end to end: signed transaction bytes are now persisted to `broadcast_attempts.signed_tx` BEFORE the send call, so any crash/interruption between signing and sending resumes by re-sending byte-identical bytes rather than re-signing (AWS KMS's ECDSA signing is not guaranteed deterministic — a second signature over the same digest could legitimately differ from one already on the network, which would otherwise create real ambiguity about which of two valid transactions is "the" one). A resend that comes back "already known"/"nonce too low" is treated as success under AD-11's single-writer guarantee. A new `DetectStuckWithdrawals` use case writes exactly one `withdrawal.stuck` outbox event per withdrawal once it's spent too long without progressing. A new liveness gate (`WatcherLiveness`, reusing the existing `watcher_cursors` table per AD-15) pauses claiming brand-new withdrawals when a chain's watcher looks stalled, without ever blocking resumption of already-signed work. The review pass then found and fixed the story's most severe gap: as originally implemented, stuck detection only ever watched `WithdrawalStatusBroadcast`, leaving a withdrawal stranded at `WithdrawalStatusSigned` — precisely the state this story's own problem statement centers on — completely unmonitored. That fix, plus 6 other patches (a monitoring-race in the one-time-alert guarantee, an over-broad liveness gate that blocked resuming in addition to claiming, and several hardening/documentation gaps), are all applied and verified below.

**Files changed:**

*New:*
- `internal/adapter/postgres/migrations/0012_add_withdrawal_resume_and_stuck_detection.sql` — adds `broadcast_attempts.signed_tx` and `withdrawals.stuck_alerted_at`
- `internal/adapter/postgres/watcher_liveness.go` (+ test) — `WatcherLiveness.IsLive`, AD-15's liveness signal derived from `watcher_cursors`
- `internal/core/detect_stuck_withdrawals.go` (+ test) — the new use case: list stuck candidates, alert each exactly once, one failure doesn't block the rest
- `docs/runbooks/stuck-withdrawals.md` — on-call procedure: what the alert means (now covering both triggering statuses), how to check on-chain state, the liveness gate, and the direct-SQL manual `failed`-resolution procedure mirroring `SettleFailedWithdrawal`'s own posting logic

*Modified:*
- `internal/adapter/postgres/withdrawal_repo.go` — `ListStuckCandidates` (widened to both statuses, correct per-status staleness column), `MarkStuckAlerted` (guard-then-write reordering, `Status` added to the outbox payload), `ListSignedWithdrawals` (`LIMIT 50`, `id` tiebreaker, per-row hex-decode skip instead of whole-batch failure), `RecordSignedTx`/`MarkBroadcast` (persist-before-send split out of the old single `RecordBroadcastTxHash`)
- `internal/core/sign_and_broadcast_withdrawal.go` — restructured around persist-signed-bytes-before-send; resume-on-restart from `ListSignedWithdrawals`; new `allowClaim bool` parameter on `Execute` so the liveness gate only blocks fresh claims, never resuming
- `internal/core/ports.go`, `withdrawal.go` — `WithdrawalRepository` port additions (`RecordSignedTx`, `MarkBroadcast`, `ListSignedWithdrawals`, `ListStuckCandidates`, `MarkStuckAlerted`); `SignedTx` field
- `cmd/walletd/main.go` — `runBroadcaster`'s poll loop always attempts resume regardless of liveness, passing the liveness result through as `allowClaim` for the claim step only; wires up `DetectStuckWithdrawals` on its own poll cadence
- `internal/core/approve_withdrawal_test.go`, `create_withdrawal_test.go` — mechanical fake-repository updates for the widened `WithdrawalRepository` port (no behavioral change)
- `_bmad-output/implementation-artifacts/deferred-work.md` — resolution note on Story 3.4's own deferred "stranded at signed, no self-healing" item (partially addressed: now visible via the stuck alert, though the poll-loop amplification itself isn't fixed); two new deferred items from this story's review

**Review findings breakdown** (2026-07-22 pass, Blind Hunter + Edge Case Hunter, run independently without shared context, findings deduplicated):
- 7 patch (1 high, 4 medium, 2 low) — all applied and re-verified; see the Review Triage Log above for the full list. The one high-severity finding: stuck detection never covered `WithdrawalStatusSigned` at all, contradicting this story's own stated problem.
- 2 defer (both medium) — logged in `deferred-work.md`: `isAlreadyKnownError`'s string-based-only resend recognition, and the liveness gate's default staleness threshold being a tuning question without production data yet.
- 0 reject, 0 intent_gap, 0 bad_spec — every finding was a mechanical patch or an appropriately-deferred design/data question; nothing required renegotiating the frozen intent-contract.

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `make check-import-boundary` — all clean, both before and after the review-pass patches
- `go test ./internal/core/... ./internal/adapter/postgres/... ./internal/adapter/evm/... ./internal/adapter/signer/...` — all green except the same 4 pre-existing, unrelated `TestTrackDeposits_*` reorg-detection failures already established as out-of-scope earlier this session
- `internal/adapter/postgres/withdrawal_broadcast_repo_test.go` runs against a real Postgres 18 testcontainer; new/updated coverage includes: `ListStuckCandidates` for both `signed` (new: `TestListStuckCandidates_ReturnsOldEnoughUnalertedSignedWithdrawals`, `TestListStuckCandidates_SignedNotYetOldEnough_Excluded`) and `broadcast` staleness columns, `MarkStuckAlerted`'s once-only guarantee, `WatcherLiveness.IsLive` scoped correctly to chain and tier
- `internal/core/sign_and_broadcast_withdrawal_test.go` — all 13 pre-existing tests updated for the new `allowClaim` parameter plus 2 new tests (`TestSignAndBroadcastWithdrawal_Execute_AllowClaimFalse_NothingToResume_NeverClaims`, `TestSignAndBroadcastWithdrawal_Execute_AllowClaimFalse_StillResumes`) proving the liveness gate only blocks claiming, never resuming
- Manual crash-resume and real-anvil verification (this spec's own "Manual checks" section) were not run in this environment — no `anvil` binary available here, consistent with every other real-anvil test in this repo throughout the project's history; the persist-before-send logic itself is covered by unit tests with a fake broadcaster/signer, not by a real crash-and-restart scenario

**Residual risks:**
- The 2 deferred items above remain open, tracked in `deferred-work.md`, alongside the items deferred from Stories 3.1-3.4's own review passes.
- No git commit was created (user's global no-auto-commit policy). All changes — Stories 3.3, 3.4, and 3.5 alike — remain uncommitted, stacked on top of each other; see `final_revision` frontmatter.
- The crash-resume guarantee (persist-before-send) and the real-anvil broadcast path are both written but unexercised against a real chain/real crash in this environment, mirroring Story 3.4's identical residual risk for its own anvil-dependent paths.
- `followup_review_recommended: true` — this story's review pass found a high-severity gap (stuck detection missing its most literal case, `signed` withdrawals) in a story whose entire purpose is stuck-detection; that combination — a monitoring feature failing to monitor the state its own problem statement centers on — warrants one more independent look before this is considered fully settled, even though every found issue has now been patched and re-verified.
