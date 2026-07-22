---
title: 'Story 3.3: Enforce Pre-Signing Policy & Threshold-Based Approval Routing'
type: 'feature'
created: '2026-07-20'
status: 'done'
review_loop_iteration: 0
followup_review_recommended: true
context: []
warnings: ['oversized']
baseline_revision: '881ef921ddfb75caefce34f7b2f18df2f39b5901'
final_revision: '0c3f61b3624e67525f249a59731b14cca82a35a2 (NOT_COMMITTED past this point: user global policy — no auto-commits. All review-pass patches remain uncommitted on top of this revision.)'
---

<intent-contract>

## Intent

**Problem:** A large or invalid withdrawal must never execute automatically — every withdrawal needs an explicit pre-signing policy check (destination sanity, fee-inclusive balance coverage), and anything above a configurable per-asset amount must wait for an operator's explicit sign-off before it can ever reach Story 3.4's signing/broadcast path.

**Approach:** Immediately after Story 3.2 places a withdrawal's hold (same request, same open transaction — AD-6's "api-through-core, single writer" applies to this transition too, not a separate poller), `CreateWithdrawal` evaluates FR18's policy set and FR17's threshold routing, then advances `withdrawals.status` from `created` to either `awaiting-approval` (writing an `approval.required` outbox event) or directly to `approved` (writing a `withdrawal.approved` outbox event) — all in the transaction Story 3.2 already opens. A new `POST /v1/withdrawals/{id}/approve` endpoint lets an operator move an `awaiting-approval` withdrawal to `approved`, logging actor/timestamp/reason (NFR11) directly on the row.

## Boundaries & Constraints

**Always:**
- The policy-check-and-route step commits in the SAME transaction as Story 3.2's hold placement (this codebase's `IdempotencyMiddleware` opens exactly one transaction per HTTP request, committed once at the end — mirrors how Story 2.2's `CreditFinalizedDeposits` already treats multiple logical transitions inside one physical commit; there is no separate poller subcommand for withdrawals, matching `ARCHITECTURE-SPINE.md`'s AD-6 "api-through-core" writer for this state machine).
- Threshold VALUES live in a new `withdrawal_approval_thresholds` data table (chain, asset, threshold_amount), never a Go constant — mirrors migration 0006's `crediting_policy` precedent exactly (FR9-style: "a policy table, not hard-coded"). Seed one row per `core.SupportedChainAssetPairs` entry with a placeholder value, explicitly documented as an ops/pre-launch setting to be revised before production (PRD open question 3), never treated as a final number.
- The fee-inclusive balance check calls the existing `core.FeeEstimator` port (Story 3.1) to get `EstimateFee(chain, asset, amount).TotalFee`, then verifies the customer's `available` account balance (which, after Story 3.2's hold, already excludes `amount`) covers that fee. This is arithmetically identical to requiring pre-hold `available >= amount + fee`, evaluated post-hold as `available >= fee` — no separate pre-hold balance re-read is needed.
- The destination-address check adds exactly one denylist entry to Story 3.2's existing shape-only validation: the zero address (`0x` + 40 zero hex chars, 20 bytes — confirmed via `python3` before writing the Go constant, mirroring Story 3.1's own "verify byte length empirically" discipline after its address-transcription bug). No other denylist entries in v1 (FR18 says "e.g. the zero address," not an exhaustive list).
- `withdrawals.status`'s CHECK widens from `= 'created'` to `IN ('created', 'awaiting-approval', 'approved')` — exactly the three values this story's own transitions produce; Stories 3.4/3.5 each widen it further in their own migration as they add the value their own transition needs (mirrors Story 3.2's own migration 0009 precedent).
- The approval action's actor and reason are logged directly on the `withdrawals` row (`approved_by`, `approval_reason`, `approved_at`) — stored, not re-derived (mirrors Story 3.2's own `hold_journal_entry_id` design note: "every later story that needs this can join directly").
- `POST /v1/withdrawals/{id}/approve` requires a non-empty `actor` and `reason` in its request body (NFR11: "operator actions... logged with actor, timestamp, and reason") — this system has no separate operator-identity/auth tier yet (a pre-existing, already-logged limitation of the shared-bearer-token model), so actor is caller-supplied, not derived from the request's auth context. This is this story's only new mutating route; it reuses the same bearer-token + `IdempotencyMiddleware` stack as every other mutating route.
- Approving an already-`approved` or non-existent withdrawal is rejected loudly (404 / 409), never silently accepted or silently a no-op.

**Block If:** (none — every open design question below has a reasonable, precedent-grounded default; see Design Notes.)

**Never:**
- A new poller/background process/subcommand for withdrawal state advancement — this story's only two write paths are `POST /withdrawals` (extended) and the new `POST /withdrawals/{id}/approve`, both synchronous HTTP handlers through core, matching the architecture's explicit "api-through-core" decision for this state machine (broadcaster-style single-writer-with-advisory-lock is Story 3.4's pattern for `approved→signed→broadcast→confirmed`, not this story's).
- Introducing `signed`/`broadcast`/`confirmed`/`failed` as CHECK-allowed status values, or any chain interaction, signing, or broadcasting logic — those remain exactly Story 3.4/3.5's job.
- A generic/extensible policy engine — FR18 explicitly calls the v1 policy set "deliberately minimal"; the two checks here (fee-inclusive balance, zero-address denylist) are the entire v1 scope, hardcoded as two straight-line checks, not a rule-engine abstraction.
- Re-validating or re-placing the hold itself, or touching `journal_entries`/`postings` in this story — the ledger movement is already complete once Story 3.2's hold commits; this story only reads the resulting balance and writes `withdrawals.status` + its own outbox event(s).

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Valid withdrawal, amount at or below threshold | Sufficient available balance for amount+fee, well-formed non-zero destination | `201`, `status: "approved"`; `withdrawal.approved` outbox event written | none |
| Valid withdrawal, amount exceeds threshold | Same as above but `amount > threshold` | `201`, `status: "awaiting-approval"`; `approval.required` outbox event written | none |
| Destination is the zero address | `destinationAddress = "0x000...000"` | `400` | `core.ErrInvalidDestinationAddress`, rejected before any hold is placed (same pre-repo validation step as Story 3.2's shape check) |
| Available balance covers amount but not amount+fee | Hold succeeds (Story 3.2 logic unchanged), but post-hold available < estimated fee | `422`, hold is NOT committed (whole request transaction rolls back) | `core.ErrInsufficientBalanceForFee` |
| FeeEstimator call fails (RPC error, e.g.) | Transient adapter failure | `500`, logged server-side, generic detail externally (mirrors Story 3.1/3.2's own fix for this) | wrapped error, no hold committed |
| No `withdrawal_approval_thresholds` row for the requested (chain, asset) | Registry gap (shouldn't happen in a correctly configured deployment) | `500`, clearly logged server-side — never a silently-approved or silently-blocked withdrawal | fail loud, mirrors Story 3.1's identical "registry gap" principle |
| Operator approves an `awaiting-approval` withdrawal with actor+reason | Valid withdrawal id, non-empty `actor`/`reason` | `200`, `status: "approved"`, `approvedAt`/`approvedBy`/`approvalReason` populated; `withdrawal.approved` outbox event written | none |
| Operator approves a withdrawal not in `awaiting-approval` (e.g. already `approved`, or still `created`) | Status mismatch | `409` | `core.ErrWithdrawalNotAwaitingApproval` |
| Operator approves an unknown withdrawal id | No matching row | `404` | `core.ErrWithdrawalNotFound` |
| Missing `actor` or `reason` in the approve request body | Empty/omitted field | `400` | Explicit validation in the use case |
| Concurrent duplicate approve requests for the same withdrawal | Two requests racing | Exactly one succeeds and transitions the row; the other sees the now-`approved` status and returns `409` (row-level lock makes this deterministic, not a race) | `core.ErrWithdrawalNotAwaitingApproval` on the loser |

</intent-contract>

## Code Map

- `internal/adapter/postgres/migrations/0010_add_withdrawal_approval_thresholds_and_approval_states.sql` -- new `withdrawal_approval_thresholds` table (mirrors `crediting_policy`'s shape) seeded with one placeholder row per `SupportedChainAssetPairs` entry; widen `withdrawals.status` CHECK; add nullable `approved_at timestamptz`, `approved_by text`, `approval_reason text` columns to `withdrawals`
- `internal/core/withdrawal.go` -- add `WithdrawalStatusAwaitingApproval`, `WithdrawalStatusApproved` constants; add `ErrInvalidDestinationAddress`, `ErrInsufficientBalanceForFee`, `ErrWithdrawalNotAwaitingApproval`, `ErrWithdrawalNotFound` sentinels; extend `Withdrawal` with `ApprovedAt *time.Time`, `ApprovedBy`, `ApprovalReason string`
- `internal/core/ports.go` -- add `WithdrawalThresholdLister{GetApprovalThreshold(ctx, chain, asset) (*big.Int, error)}` port; extend `WithdrawalRepository.CreateWithdrawal`'s signature with two additional parameters, `feeEstimate *big.Int` and `targetStatus string` (the fee estimate and threshold decision are computed in core, before the repository call — the repository only executes what core already decided, never calling `FeeEstimator`/`WithdrawalThresholdLister` itself, per AD-1's adapters-don't-call-adapters rule); add `ApproveWithdrawal(ctx, id, actor, reason string) (Withdrawal, error)`
- `internal/core/create_withdrawal.go` -- `CreateWithdrawal` gains `FeeEstimator` and `WithdrawalThresholdLister` dependencies; adds the zero-address check; computes fee estimate and target status (`amount > threshold` → awaiting-approval, else approved) before delegating to the repository
- `internal/core/approve_withdrawal.go` -- new `ApproveWithdrawal` use case: validates non-empty actor/reason, delegates to the repository
- `internal/adapter/postgres/withdrawal_repo.go` -- `CreateWithdrawal` extended: after the existing hold-placement logic (unchanged), checks post-hold available balance against the given fee estimate, writes `withdrawals.status` as the given target status (no longer hardcoded `created`), writes `approval.required` or `withdrawal.approved` outbox event depending on target status; new `ApproveWithdrawal` method: locks the withdrawal row, verifies `status = 'awaiting-approval'`, updates to `approved` + audit columns, writes `withdrawal.approved` outbox event
- `internal/adapter/postgres/withdrawal_threshold_lister.go` -- new small repo implementing `WithdrawalThresholdLister` against the new table (same shape as `postgres.NewTokenRegistry`)
- `internal/adapter/api/withdrawals.go` -- extend `CreateWithdrawal`'s error mapping (`ErrInvalidDestinationAddress` → 400, `ErrInsufficientBalanceForFee` → 422); new `ApproveWithdrawal` handler for `POST /v1/withdrawals/{id}/approve`
- `api/openapi.yaml` -- widen `Withdrawal.status` enum; add `approvedAt`/`approvedBy`/`approvalReason` (nullable) to the `Withdrawal` schema; add `POST /withdrawals/{id}/approve` with an `{actor, reason}` request body; regenerate `server.gen.go`
- `cmd/walletd/main.go` -- wire `postgres.NewWithdrawalThresholdLister`, the extended `CreateWithdrawal`, and the new `ApproveWithdrawal` use case into `runAPI`'s composition root and `NewServerInterface`

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0010_add_withdrawal_approval_thresholds_and_approval_states.sql` -- `CREATE TABLE withdrawal_approval_thresholds (chain text NOT NULL CHECK (chain IN ('base','arbitrum')), asset text NOT NULL CHECK (asset IN ('eth','usdc')), threshold_amount NUMERIC(78,0) NOT NULL CHECK (threshold_amount > 0), updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (chain, asset))`; seed 4 rows (one per `SupportedChainAssetPairs` entry) with a clearly-commented placeholder value; `ALTER TABLE withdrawals DROP CONSTRAINT <name found empirically>` and re-add widened to `CHECK (status IN ('created', 'awaiting-approval', 'approved'))`; `ALTER TABLE withdrawals ADD COLUMN approved_at timestamptz, ADD COLUMN approved_by text, ADD COLUMN approval_reason text`
- [x] `internal/core/withdrawal.go` -- add the two new status constants; add the four new error sentinels; extend `Withdrawal` struct
- [x] `internal/core/ports.go` -- add `WithdrawalThresholdLister`; extend `WithdrawalRepository`'s interface (both `CreateWithdrawal`'s parameters to carry fee estimate + target status, and the new `ApproveWithdrawal` method)
- [x] `internal/core/create_withdrawal.go` -- constructor gains `feeEstimator FeeEstimator, thresholds WithdrawalThresholdLister`; `Execute` adds the zero-address check (after shape validation, before repo call), calls `feeEstimator.EstimateFee` and `thresholds.GetApprovalThreshold`, computes target status, passes everything to the repository
- [x] `internal/core/approve_withdrawal.go` + test -- `ApproveWithdrawal` use case rejecting empty actor/reason before delegating
- [x] `internal/adapter/postgres/withdrawal_repo.go` -- extend `CreateWithdrawal`'s balance check (existing `available >= amount` check unchanged; add a second check for `available >= feeEstimate` using the SAME already-locked available account balance read, no second lock needed); write the given target status instead of the hardcoded constant; write `approval.required` (target = awaiting-approval) or `withdrawal.approved` (target = approved) outbox event; new `ApproveWithdrawal` method locking the withdrawal row `FOR UPDATE`, checking `status = 'awaiting-approval'`, updating columns, writing the outbox event
- [x] `internal/adapter/postgres/withdrawal_threshold_lister.go` + test -- `GetApprovalThreshold` reading the new table, returning a "no threshold configured" error if missing (never a guessed default)
- [x] `internal/adapter/api/withdrawals.go` -- extend error `switch` in `CreateWithdrawal`; new `ApproveWithdrawal(w, r, id, params)` handler validating the body then delegating
- [x] `api/openapi.yaml` -- schema/path additions; regenerate `server.gen.go`
- [x] `cmd/walletd/main.go` -- composition-root wiring
- [x] `internal/core/create_withdrawal_test.go` -- new cases for the zero-address rejection, fee-based threshold routing (fake `FeeEstimator`/`WithdrawalThresholdLister`), insufficient-balance-for-fee propagation
- [x] `internal/core/approve_withdrawal_test.go` -- unit tests for actor/reason validation boundary
- [x] `internal/adapter/postgres/withdrawal_repo_test.go` -- integration coverage for: auto-approval path, awaiting-approval routing, insufficient-balance-for-fee rejection (with rollback, no partial write), approve transition, double-approve rejection, concurrent approve requests racing
- [x] `internal/adapter/api/integration_test.go` -- end-to-end cases for both routing outcomes, the zero-address 400, the approve endpoint's full status matrix (200/404/409/400), and missing bearer/idempotency-key on the new route

**Acceptance Criteria:**
- Given a withdrawal amount exceeds the configured per-asset threshold, when it advances from "created", then it enters "awaiting-approval" state, writes an `approval.required` outbox event, and does not proceed until an operator explicitly approves it (FR17, FR29).
- Given a withdrawal is at or below threshold, when it advances, then it proceeds automatically toward "approved" without operator intervention.
- Given a withdrawal is about to advance via either path, when policy is checked, then available balance covers amount plus estimated fee, the destination address is well-formed and not a known-invalid target, and threshold routing has already been applied — any failure blocks the advance (FR18).
- Given an operator approves an awaiting-approval withdrawal, when approved, then it transitions to "approved" and the approval is logged with actor, timestamp, and reason (NFR11).

## Spec Change Log

## Review Triage Log

### 2026-07-21 — Review pass
- intent_gap: 0
- bad_spec: 0
- patch: 10 (high 2, medium 6, low 2)
- defer: 1 (medium 1)
- reject: 2
- addressed_findings:
  - `[high]` `[patch]` A matching internal-transfer-class bug's sibling here: the `"withdrawal.approved"` outbox event had two structurally incompatible payload shapes depending on which write path produced it (`CreateWithdrawal`'s auto-approval branch: chain/asset/amount/destinationAddress/customerId, no approvedBy/reason; `ApproveWithdrawal`'s operator path: only approvedBy/reason) — a downstream consumer (Story 3.4's future broadcaster) could not decode one fixed shape. Fixed: unified into one `withdrawalRoutedPayload` struct with `approvedBy`/`approvalReason` as `omitempty`, used by both write paths; added payload-content assertions (previously only presence/count was checked) to both `TestCreateWithdrawal_AutoApprovalRouting` and `TestApproveWithdrawal_TransitionsToApproved`.
  - `[high]` `[patch]` `actor`/`reason` on `POST /withdrawals/{id}/approve` were checked only for exact emptiness (`== ""`), so a whitespace-only value (`" "`) passed validation and was persisted verbatim into the audit trail (NFR11). Fixed: `ApproveWithdrawal.Execute` now trims both before validating and before passing them on, so the trimmed value — not the raw one — reaches the repository and the audit columns.
  - `[medium]` `[patch]` The zero-address denylist error (`ErrInvalidDestinationAddress`) and the shape-check error (`ErrMalformedDestinationAddress`) both mapped to the identical RFC 9457 problem `"title"` (`"invalid-destination-address"`), contradicting the adjacent comment's claim that a distinct title keeps the two failure reasons distinguishable. Fixed: the denylist case now maps to its own title, `"denylisted-destination-address"`.
  - `[medium]` `[patch]` `ApproveWithdrawal`'s repository `UPDATE` discarded its `RowsAffected()`, unlike the codebase's own established convention for this exact class of risk (Story 2.4's `OrphanDeposit` fix). Currently unreachable given the preceding `SELECT ... FOR UPDATE` re-check, but fixed for defense-in-depth against a future refactor that removes that check while leaving this `UPDATE`'s `WHERE` clause as the only remaining guard.
  - `[medium]` `[patch]` `feeEstimate.TotalFee`/`threshold` flowed straight into `big.Int.Cmp` with no nil-guard; a future adapter bug returning a nil value with no error would panic the request handler. Fixed: added explicit nil-checks in `CreateWithdrawal.Execute` with new unit tests for both cases.
  - `[medium]` `[patch]` Migration 0010's down-migration deleted `withdrawals` rows past `'created'` without cleaning up their `hold_journal_entry_id`-referenced `journal_entries`/postings (both legs), unlike migration 0009's own down-migration precedent for the identical class of orphaning risk. Fixed: scoped `DELETE`s against `journal_entries`/`postings` added before the `withdrawals` delete, keyed by `hold_journal_entry_id` so a `'created'` withdrawal's own still-valid hold is never touched.
  - `[medium]` `[patch]` The account-lock query's `rows.Err()` error branch in `CreateWithdrawal` returned without calling `rows.Close()`, unlike the sibling scan-error branch three lines above it. Fixed for consistency.
  - `[low]` `[patch]` The OpenAPI `ApproveWithdrawalRequest` schema's `actor`/`reason` were `required` but had no `minLength`, so the schema alone (unlike the enforcing Go-level check) would accept an empty string. Fixed: added `minLength: 1` to both; regenerated `server.gen.go` (embedded-spec bytes only — no generated-code logic change, since this codebase has no request-body JSON-Schema-validation middleware wired against the embedded spec).
  - `[low]` `[patch]` No test exercised the exact threshold boundary combined with a nonzero fee estimate — existing boundary tests all used a zero fee. Added a case proving the threshold comparison and the fee estimate are independent inputs that don't interact unexpectedly at the boundary.
- **Deferred** (`{implementation_artifacts}/deferred-work.md`): `CreateWithdrawal.Execute` calls the chain-RPC-backed `FeeEstimator` and `WithdrawalThresholdLister` before any check that the customer even exists — a syntactically valid but nonexistent `customerId` now burns a live outbound RPC call plus a DB round trip before returning 404, a cost/latency amplification surface this story's new external dependencies introduced. Not patched now: fixing it requires deciding where an early, cheap customer-existence check should live (today it's discovered implicitly, deep in the repository's account lookup), a design decision beyond a mechanical one-line fix.
- **Rejected** (noise): `WithdrawalThresholdLister.GetApprovalThreshold` reads outside the request's open transaction — mirrors the existing `TokenRegistryLister` precedent exactly, not a new inconsistency, and not a correctness bug under read-committed isolation for a rarely-changing ops-config value; outbox_events rows are never cleaned up by any migration's down-path anywhere in this codebase (pre-existing, codebase-wide characteristic, not newly introduced by this story).

## Design Notes

- **Why the policy-check-and-route step shares Story 3.2's transaction rather than a separate poller.** `ARCHITECTURE-SPINE.md`'s AD-6 designates withdrawals as "api-through-core, single writer" (unlike Epic 2's watcher-poller pattern for deposits) — the solution design's own withdrawal sequence diagram shows the API role writing directly to `awaiting-approval` + outbox in the same step as creation. This codebase's `IdempotencyMiddleware` already opens exactly one transaction per HTTP request and commits once at the end, so "two transitions" (hold placement, then policy-route) unavoidably share one physical commit here — the same pattern Story 2.2's `CreditFinalizedDeposits` already established (multiple logical transitions, one poll-cycle transaction).
- **Why the fee-inclusive balance check is `available >= fee`, not `available >= amount + fee`.** Story 3.2's hold already moved `amount` out of `available` into `hold` before this check runs. FR18's "available balance covers amount plus estimated fee" is evaluated against the PRE-hold available balance; since `available_post_hold = available_pre_hold - amount`, the two checks are arithmetically identical: `available_pre_hold >= amount + fee` ⟺ `available_post_hold >= fee`. Reading the already-current (post-hold) balance avoids a redundant pre-hold snapshot.
- **Why threshold values are a data table, not a Go constant.** PRD's own open question 3 flags per-asset thresholds as "ops/pre-launch settings" to be fixed before launch — exactly the same "policy is data, not code" reasoning migration 0006's `crediting_policy` table already established for crediting tiers (FR9). Seeding a placeholder value here (rather than leaving the table empty) lets every other check in this story be exercised by tests without a separate seeding step; the placeholder is explicitly commented as provisional.
- **Why `approved_by`/`approval_reason`/`approved_at` are stored directly on `withdrawals`, not derived from an audit log table.** Mirrors Story 3.2's own `hold_journal_entry_id` design note: the one thing every later consumer needs ("who approved this, when, why") is cheapest to have as plain columns on the row itself; NFR11's append-only audit trail is satisfied by the outbox event this same transition writes, which downstream (Epic 4) can persist immutably once dispatched.
- **Why the approve endpoint accepts caller-supplied `actor`, not an authenticated identity.** This system's auth model is a static shared-bearer-token allowlist with no per-caller identity (a pre-existing limitation, first logged in the Story 1.2 review and repeated since) — there is no operator-identity system to derive `actor` from. Requiring it in the request body is the only way to satisfy NFR11's "logged with actor" today; tightening this once a real operator-identity system exists is out of this story's scope.

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green (the pre-existing reorg-detection test failures remain unrelated and untouched)
- `make check-import-boundary` -- expected: still passes

**Manual checks (if no CLI):**
- Create a withdrawal below threshold: `GET` its status via a future story's endpoint (none yet in v1) or query Postgres directly — confirm `status = 'approved'`. Create one above threshold, confirm `status = 'awaiting-approval'`, then call the approve endpoint and confirm the transition plus the audit columns.

## Auto Run Result

**Status:** done

**Summary:** Implementation (migration 0010, threshold-routing in `CreateWithdrawal`, the new `ApproveWithdrawal` use case/endpoint, all tests) was already complete and passing when this auto-dev run picked up the story — it had been implemented in an earlier session but never carried through review. This run verified every task and acceptance criterion against real Postgres/HTTP-stack tests, then ran a full Blind Hunter + Edge Case Hunter adversarial pass, which found 10 real, fixable issues (2 high) and 1 real-but-deferred design concern. All 10 patches were applied and re-verified; nothing required reopening the frozen intent-contract.

**Files changed (this review pass, on top of the already-complete implementation):**
- `internal/adapter/api/withdrawals.go` — the zero-address denylist error now maps to its own RFC 9457 problem title (`denylisted-destination-address`), no longer identical to the shape-check error's title
- `internal/core/approve_withdrawal.go` — `actor`/`reason` are trimmed before validation and before reaching the repository, rejecting whitespace-only values
- `internal/adapter/postgres/withdrawal_repo.go` — unified the `withdrawal.approved` outbox payload into one shape (`withdrawalRoutedPayload`, with `approvedBy`/`approvalReason` as `omitempty`) written by both `CreateWithdrawal`'s auto-approval branch and `ApproveWithdrawal`'s operator path; `ApproveWithdrawal`'s `UPDATE` now checks `RowsAffected()`; the account-lock query's `rows.Err()` branch now closes `rows` before returning
- `internal/core/create_withdrawal.go` — nil-guards on `feeEstimate.TotalFee` and `threshold` before the `big.Int.Cmp` calls that use them
- `internal/adapter/postgres/migrations/0010_add_withdrawal_approval_thresholds_and_approval_states.sql` — down-migration now deletes the `journal_entries`/`postings` for each deleted withdrawal's hold (scoped by `hold_journal_entry_id`) before deleting the withdrawal row itself, mirroring migration 0009's own precedent
- `api/openapi.yaml`, `internal/adapter/api/server.gen.go` — `ApproveWithdrawalRequest.actor`/`.reason` gain `minLength: 1` (spec-accuracy only; this codebase has no runtime JSON-Schema validation middleware, so Go-level enforcement is unchanged and remains the real guard)
- `internal/adapter/postgres/withdrawal_repo_test.go` — new `outboxEventPayload` helper decoding actual payload contents (previously only presence/count was checked); extended `TestCreateWithdrawal_AutoApprovalRouting` and `TestApproveWithdrawal_TransitionsToApproved` to assert on it
- `internal/core/create_withdrawal_test.go` — new cases: threshold boundary combined with a nonzero fee estimate; nil `TotalFee`/nil `threshold` fail loud without calling the repository
- `_bmad-output/implementation-artifacts/deferred-work.md` — new entry for the pre-customer-existence-check RPC/DB cost concern
- `_bmad-output/implementation-artifacts/sprint-status.yaml` — `3-3-enforce-pre-signing-policy-and-threshold-based-approval-routing` marked `done`

**Review findings breakdown** (2026-07-21 pass, Blind Hunter + Edge Case Hunter, run independently without shared context, findings deduplicated):
- 10 patch (2 high, 6 medium, 2 low) — all applied and re-verified: outbox payload schema unification (high, both reviewers found this independently), actor/reason whitespace trimming (high, both reviewers), destination-address problem-title collision, `ApproveWithdrawal`'s missing `RowsAffected` check, nil-guards before `big.Int.Cmp`, migration 0010's down-migration orphaning `journal_entries`/`postings`, a resource-leak on an error path in the account-lock query, OpenAPI `minLength`, and a missing threshold-boundary-with-nonzero-fee test
- 1 defer (medium) — logged in `deferred-work.md`: `FeeEstimator`/`WithdrawalThresholdLister` calls happen before customer existence is verified, a cost/latency amplification surface; fixing it is a design decision (where to add an early check), not a mechanical patch
- 2 reject (noise): `WithdrawalThresholdLister` reading outside the tx (mirrors existing `TokenRegistryLister` precedent); outbox_events never cleaned up on any down-migration (pre-existing, codebase-wide, not new here)
- 0 intent_gap, 0 bad_spec — every finding was either a mechanical patch or out of this story's scope; nothing required renegotiating the frozen intent-contract

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `make check-import-boundary` — all clean
- `go test ./internal/core/... ./internal/adapter/postgres/... ./internal/adapter/api/...` — all withdrawal/approve-related tests pass (core unit tests, real-Postgres-18-testcontainer integration tests, full HTTP end-to-end tests), including every newly added case exercising this pass's fixes
- The only test failures observed (`TestTrackDeposits_Execute_ReorgCheck_*` in `internal/core`, `TestReorgDetection_EndToEnd` in `internal/adapter/api`) are pre-existing and unrelated — Story 2.4's `checkForReorgs` call site is commented out in `track_deposits.go`, untouched by this story; already called out as known/unrelated in this spec's own Verification notes before this run started

**Residual risks:**
- The 1 deferred item above remains open, tracked in `deferred-work.md`.
- No git commit was created (user's global no-auto-commit policy). All changes — the original implementation and this review pass's patches alike — remain uncommitted/committed-as-found; see `final_revision` frontmatter.
- `followup_review_recommended: true` — 2 high-severity findings (an outbox schema inconsistency affecting a future Epic 4 consumer, and an audit-trail integrity gap) plus a data-integrity fix to a migration's down-path, spanning API/core/repository/migration layers, is enough breadth and consequence to warrant one more independent look before this story is fully trusted, even though every individual fix here was small and mechanical.
