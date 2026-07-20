---
title: 'Story 3.3: Enforce Pre-Signing Policy & Threshold-Based Approval Routing'
type: 'feature'
created: '2026-07-20'
status: 'in-progress'
review_loop_iteration: 0
followup_review_recommended: false
context: []
warnings: ['oversized']
baseline_revision: '881ef921ddfb75caefce34f7b2f18df2f39b5901'
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
