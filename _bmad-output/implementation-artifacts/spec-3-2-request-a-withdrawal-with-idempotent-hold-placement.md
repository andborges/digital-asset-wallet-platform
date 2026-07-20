---
title: 'Story 3.2: Request a Withdrawal With Idempotent Hold Placement'
type: 'feature'
created: '2026-07-20'
status: 'done'
review_loop_iteration: 1
followup_review_recommended: false
context: []
warnings: ['oversized']
baseline_revision: '881ef921ddfb75caefce34f7b2f18df2f39b5901'
---

<intent-contract>

## Intent

**Problem:** A customer's withdrawal request must reserve funds the instant it's accepted — otherwise two concurrent requests (or a retried one) could both draw against the same available balance, or a request could be accepted and then silently lost with no record and no reserved funds.

**Approach:** Add `POST /v1/withdrawals`, backed by a new `Withdrawal` domain type, a `WithdrawalRepository` port, and a new `withdrawals` table. Placing a hold is a ledger-only reclassification exactly like Story 1.3's transfer: debit the customer's existing (chain, asset) "available" account, credit a new sibling "hold" account for the same (chain, asset) pair, in one journal entry — no money leaves the customer, no chain interaction yet. The existing `IdempotencyMiddleware` (unchanged, already wraps every mutating route) gives HTTP-level exactly-once for free; the journal entry's `(cause_type='withdrawal_hold', cause_id=<Idempotency-Key>)` unique constraint is the same belt-and-suspenders ledger-level guarantee `CreateTransfer` already relies on for its own narrow pre-commit race window.

## Boundaries & Constraints

**Always:**
- The hold is placed in the SAME transaction as the `withdrawals` row insert (AD-4) — both commit together or neither does.
- A "hold" account is a new sibling of the existing "available" account for the same (customer, chain, asset): add an `account_type` column to `accounts` (values `'available'`, `'hold'`), defaulting existing rows to `'available'`, and widen the unique constraint from `(customer_id, chain, asset)` to `(customer_id, chain, asset, account_type)`. `CreateCustomer` (Story 1.1) is updated to provision both types atomically for every new customer, exactly like it already does for the 4 `SupportedChainAssetPairs`; the migration backfills a hold account for every existing customer row.
- Balance is still never a stored column — `available` and `hold` are each independently derived via `SUM(postings.amount)` (AD-3), same as every other account.
- Lock the customer's `available` and `hold` accounts for the requested (chain, asset) in ONE `ORDER BY id FOR UPDATE` statement (mirrors `CreateTransfer`'s exact deadlock-avoidance pattern) before checking `available`'s balance.
- `withdrawals.status` starts and (for this story) stays `'created'` — `CHECK (status = 'created')`, tightened exactly like migration 0006's `crediting_policy` precedent; Stories 3.3–3.5 each extend this CHECK in their own migration as they add the status value their transition needs, never pre-added here.
- Every withdrawal write is a paired outbox event (`withdrawal.created`), matching every prior story's AD-4 pattern (`deposit.pending`, `deposit.credited`, etc.).
- `destinationAddress` must be structurally well-formed (`^0x[0-9a-fA-F]{40}$`, matching `unsupported_token_observations.address`'s existing CHECK convention) — a 400 if not.

**Block If:** (none — every open design question below has a reasonable, precedent-grounded default.)

**Never:**
- Checking `destinationAddress` against a known-invalid-target denylist (e.g. the zero address) or running any pre-signing policy/threshold check — that is explicitly Story 3.3's job per the epic's own cross-story dependency ordering ("Before signing... destination is well-formed and not a known-invalid target"; "3.3's policy check... gates entry into 3.4's signing path"). This story only validates address *shape*.
- Holding `amount + estimated fee` — the epic separates "the amount is placed on hold" (this story) from "available balance covers amount + estimated fee" (an explicit pre-signing check, Story 3.3). Calling `FeeEstimator`/`EstimateFee` from this use case at all is scope creep: the client already queried Story 3.1's endpoint separately before submitting.
- Any chain interaction, signing, or broadcasting — this story never touches `internal/adapter/evm`'s RPC clients.
- A second, parallel idempotency mechanism — reuse `IdempotencyMiddleware` and the `journal_entries` unique-cause-id pattern exactly as `CreateTransfer` does; do not invent a new dedup table.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Valid withdrawal request | Existing customer, sufficient available balance, well-formed destination | `201` withdrawal resource, status `created`; available debited, hold credited by `amount` | none |
| Replayed Idempotency-Key, same body | Second identical request | `IdempotencyMiddleware` returns the original `201` byte-for-byte; use case never re-invoked | none |
| Replayed Idempotency-Key, different body | Second request, different `amount`/etc. | `409` | `IdempotencyMiddleware`'s existing conflict path |
| Insufficient available balance | `amount` exceeds derived `available` balance | `422` | `core.ErrInsufficientBalance` (reused from `CreateTransfer`) |
| Non-positive or missing amount | `amount <= 0` or omitted | `400` | Explicit validation in the use case |
| Malformed destination address | Not `^0x[0-9a-fA-F]{40}$` | `400` | Explicit validation in the use case |
| Unknown customer | `customerId` has no matching row | `404` | `core.ErrCustomerNotFound` (reused) |
| Invalid chain/asset enum | e.g. `chain=optimism` | `400` | Explicit `.Valid()` checks in the handler, before the use case runs (re-review: no request-validation middleware is wired anywhere in this service; corrected from this row's original wording) |
| Concurrent requests, same customer, same (chain, asset), different keys | Two distinct valid requests racing | Both succeed serially (row-locked), balances stay consistent, no lost update | none |

</intent-contract>

## Code Map

- `internal/core/withdrawal.go` -- `Withdrawal` domain type
- `internal/core/ports.go` -- `WithdrawalRepository` port
- `internal/core/create_withdrawal.go` -- `CreateWithdrawal` use case (amount + address-shape validation, delegates to the port)
- `internal/adapter/postgres/migrations/0009_add_withdrawal_holds_and_withdrawals.sql` -- `accounts.account_type` column + widened unique constraint + hold-account backfill; new `withdrawals` table
- `internal/adapter/postgres/withdrawal_repo.go` -- `WithdrawalRepository` implementation (lock both accounts, balance check, journal entry + postings, `withdrawals` insert, outbox event)
- `internal/core/create_customer.go`, `internal/adapter/postgres/customer_repo.go` -- provision both `available` and `hold` accounts per (chain, asset) for new customers
- `internal/adapter/api/withdrawals.go` -- `CreateWithdrawal` handler
- `api/openapi.yaml` -- `POST /withdrawals`, `Withdrawal` schema; regenerate `server.gen.go`
- `cmd/walletd/main.go` -- wire the new use case into `runAPI`'s composition root and `NewServerInterface`

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0009_add_withdrawal_holds_and_withdrawals.sql` -- add `accounts.account_type text NOT NULL DEFAULT 'available' CHECK (account_type IN ('available','hold'))`; drop and recreate the customer-scoped unique constraint as `(customer_id, chain, asset, account_type)`; backfill one `'hold'` row per existing customer × `SupportedChainAssetPairs` entry; create `withdrawals(id uuid PK, customer_id uuid NOT NULL REFERENCES customers, chain text NOT NULL CHECK, asset text NOT NULL CHECK, amount NUMERIC(78,0) NOT NULL CHECK (amount > 0), destination_address text NOT NULL CHECK (~ '^0x[0-9a-fA-F]{40}$'), status text NOT NULL CHECK (status = 'created'), hold_journal_entry_id uuid NOT NULL REFERENCES journal_entries, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now())`
- [x] `internal/core/withdrawal.go` -- `Withdrawal{ID, CustomerID, Chain, Asset, Amount, DestinationAddress, Status, CreatedAt}`; `ErrMalformedDestinationAddress` sentinel
- [x] `internal/core/ports.go` -- `WithdrawalRepository{CreateWithdrawal(ctx, req WithdrawalRequest) (Withdrawal, error)}`
- [x] `internal/core/create_withdrawal.go` -- `CreateWithdrawal` use case: reject `amount <= 0` (`ErrNonPositiveAmount`, reusing the existing sentinel per `CreateTransfer`/`EstimateFee` convention), reject a malformed `destinationAddress` (`ErrMalformedDestinationAddress`), delegate to the port
- [x] `internal/adapter/postgres/withdrawal_repo.go` -- lock the customer's `available`+`hold` accounts for (chain, asset) in one `ORDER BY id FOR UPDATE` statement; verify `available`'s derived balance covers `amount` (`core.ErrInsufficientBalance`); insert `journal_entries(cause_type='withdrawal_hold', cause_id=<idempotency key>)` (unique-violation -> a sentinel mirroring `ErrDuplicateTransferCause`), two postings (debit available, credit hold); insert the `withdrawals` row; insert a paired `withdrawal.created` outbox event — all in the transaction already open on ctx
- [x] `internal/core/create_customer.go`, `internal/adapter/postgres/customer_repo.go` -- provision `available` AND `hold` accounts (8 rows total) for every new customer
- [x] `internal/adapter/api/withdrawals.go` -- `CreateWithdrawal` handler (bearer-auth + `Idempotency-Key`, both already enforced by existing middleware)
- [x] `api/openapi.yaml` -- `POST /withdrawals` request/response schemas; regenerate `server.gen.go`
- [x] `cmd/walletd/main.go` -- wire `postgres.NewWithdrawalRepository` + `core.NewCreateWithdrawal` into `runAPI` and `NewServerInterface`
- [x] `internal/core/create_withdrawal_test.go` -- unit tests for the amount/address validation boundary
- [x] `internal/adapter/api/integration_test.go` -- new tests: valid withdrawal (hold reflected in balances), idempotent replay (same body -> identical response, no double hold), idempotency conflict (409), insufficient balance (422), malformed address (400), unknown customer (404), invalid chain/asset (400)
- [x] `internal/adapter/postgres/withdrawal_repo_test.go` or integration coverage -- concurrent same-customer withdrawal requests never lose an update (mirrors `CreateTransfer`'s existing lock-ordering assurance)

**Acceptance Criteria:**
- Given a customer with sufficient available balance, when `POST /v1/withdrawals` is called with a valid body and a fresh `Idempotency-Key`, then a `withdrawals` row is created with status `created`, the requested amount moves from that customer's available account to their hold account in one journal entry, and a `withdrawal.created` outbox event is written — all atomically.
- Given the same `Idempotency-Key` is replayed with an identical body, when the request is repeated any number of times, then exactly one hold is ever placed and the original response is returned byte-for-byte.
- Given insufficient available balance, when a withdrawal is requested, then the request is rejected with `422` and no hold is placed.

## Spec Change Log

## Review Triage Log

Blind Hunter + Edge Case Hunter ran in parallel against the full implementation. Two of the four highest-severity findings were independently surfaced by BOTH reviewers (strong cross-validation). Patched (highest severity first):

1. **High (both reviewers) — a withdrawal hold's own journal entry appeared TWICE in the customer's transaction history.** `transaction_repo.go`'s `ListCustomerTransactions` query joined postings→accounts filtered only by `a.customer_id`, with no `account_type` filter — fine for `internal_transfer` (whose two postings land on two DIFFERENT customers, so only one side ever matches), but a withdrawal hold posts to this SAME customer's own available AND hold accounts in one entry, so both legs passed the filter and surfaced as two rows sharing one `id` with opposite-signed amounts. This directly contradicted this story's own boundary ("the hold account itself is not exposed by any endpoint"). Confirmed independently by re-reading the query before patching. Fixed by adding `AND a.account_type = 'available'`, mirroring the identical fix already applied to `balance_repo.go`/`transfer_repo.go`/`deposit_repo.go`. Added a regression test (`TestCreateWithdrawal_EndToEnd/a_withdrawal_hold_appears_exactly_once...`).
2. **High (both reviewers) — no upper-bound check on `WithdrawalRequest.Amount`.** Unlike Story 3.1's `EstimateFee`, which added `ErrAmountTooLarge` for exactly this reason, `CreateWithdrawal.Execute` only rejected `Sign() <= 0` — an amount beyond a uint256's max could be accepted (`withdrawals.amount NUMERIC(78,0)` permits values far beyond uint256's range) only to become unbroadcastable once Story 3.4 tries to encode it into a real transaction. Fixed by reusing `core.ErrAmountTooLarge` (the same sentinel Story 3.1 defined) rather than a parallel one, checked before the port is called; mapped to 400 in the handler. Regression tests added at both the core and HTTP-integration layers.
3. **High (Blind Hunter) — migration 0009's down-script would fail with a foreign-key violation once any withdrawal hold exists.** `DELETE FROM postings WHERE account_id IN (SELECT id FROM accounts WHERE account_type = 'hold')` only removed the hold-side leg of a `withdrawal_hold` journal entry; the paired available-account debit posting survived, so the next statement (`DELETE FROM journal_entries WHERE cause_type = 'withdrawal_hold'`) would violate `postings.journal_entry_id`'s FK — the exact failure mode the migration's own comment claimed to guard against. Fixed by deleting postings via `journal_entry_id IN (SELECT id FROM journal_entries WHERE cause_type = 'withdrawal_hold')` instead of hold-account membership. Verified by manually running the fix against a real throwaway Postgres container: seeded a customer + full withdrawal-hold fixture (accounts, journal entry, two postings, withdrawals row) after migrating up to 0009, then ran `goose down` — succeeded cleanly (confirmed zero hold accounts / zero withdrawal_hold journal entries remained, and the accounts table's schema/constraints were restored to their pre-0009 shape).
4. **Medium (Blind Hunter) — `withdrawals.go`'s 500 path forwarded raw internal error text to the client and never used the `logger` field added specifically for this purpose (Story 3.1).** Same information-disclosure class Story 3.1's own review caught and fixed for `fee_estimate.go`. Fixed: all three 500-producing branches (the generic use-case error, and the two `uuid.Parse` post-processing branches) now log server-side via `s.logger.Error` and return a fixed generic detail externally.
5. **Medium (Blind Hunter) — `deposit_repo.go`'s `CreditFinalizedDeposits` platform float-account join had no explicit `account_type` filter**, unlike the customer-side join in the same query (which the implementer did filter). It only worked because no code path creates a `'hold'`-type platform account today — an implicit, unenforced cross-migration assumption. Fixed by adding the same explicit `account_type = 'available'` filter for consistency and defense-in-depth.
6. **Low (Blind Hunter) — no defensive check on the account-locking query's row count** in `withdrawal_repo.go`. The `UNIQUE(customer_id, chain, asset, account_type)` constraint should make more than one row per account_type unreachable, but the code silently kept whichever row a map assignment scanned last rather than erroring loudly if it ever happened. Added an explicit row-count check.
7. **Low — spec documentation inaccuracy**: the I/O matrix's "Invalid chain/asset enum" row claimed enforcement via "generated request validation, before the handler runs" — no such middleware exists anywhere in this service; enforcement is the handler's own explicit `.Valid()` checks. Corrected the spec's wording (functionally correct behavior, only the documentation was wrong).

Deferred (see `deferred-work.md`):
- Migration 0009's backfill logic (minting one hold account per pre-existing customer row) has no automated test — every test in this diff runs migrations against an empty database, so the backfill INSERT never executes against real pre-existing data in CI. Manually verified the down-migration fix (see #3 above) but not the backfill path specifically. This mirrors a project-wide gap: no migration's down-path or backfill logic has automated coverage anywhere in this codebase (0001-0008 included), not something introduced by this story.
- `TestCreateWithdrawal_ConcurrentRequestsNeverLoseAnUpdate`'s actual lock-contention level is bounded by the test's default (unconfigured) `pgxpool.Pool` size, not strictly by its 20 launched goroutines — it still proves correctness (no lost update), just not necessarily under maximal contention.
- Duplicated inline `chain`/`asset` enum definitions in `api/openapi.yaml` (this story adds 2 more instances) — a pre-existing anti-pattern first logged in the Story 2.1 review, not newly introduced.

## Design Notes

- **Why a new `account_type` column instead of a separate `hold_accounts` table.** `accounts` already carries every column a hold account needs (customer_id, chain, asset); postings/balance derivation is entirely generic over `account_id`. A parallel table would duplicate the unique-constraint and balance-derivation logic for no benefit — one column plus a wider composite unique index is the minimal change, and mirrors how migration 0006 added platform accounts as more rows in the same table rather than a new one.
- **Why the hold is a plain ledger reclassification, not a new posting "kind."** `postings.amount` is already signed and summed per-account (AD-3); moving `amount` from available to hold via a debit+credit pair needs no new columns or semantics — it's structurally identical to `CreateTransfer`'s two-posting pattern, just within one customer's own two accounts instead of across two customers.
- **Why `hold_journal_entry_id` is stored directly on `withdrawals`.** Every later story that needs to reference "the journal entry that placed this withdrawal's hold" (e.g. releasing it on failure, Story 3.5) can join directly rather than re-deriving it from `(cause_type, cause_id)` string matching.

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green (pre-existing reorg-detection test failures are unrelated and untouched by this story)
- `make check-import-boundary` -- expected: still passes

**Manual checks (if no CLI):**
- After a successful withdrawal request, `GET /v1/customers/{id}/balances` should show the available balance decreased by `amount` — Story 3.2 introduces no new balances endpoint output for the hold account itself (out of scope; a future story may expose it).

## Auto Run Result

Status: done

Implementation completed by a subagent, independently verified (build/vet/fmt/import-boundary all green; all withdrawal/customer/transaction-specific tests read and re-run directly, including the concurrency test), then adversarially reviewed by Blind Hunter + Edge Case Hunter in parallel. Seven findings patched — three high severity, two of which (the transaction-history double-report and the missing amount upper bound) were independently surfaced by BOTH reviewers, a strong cross-validation signal. The down-migration fix was manually verified end-to-end against a real throwaway Postgres container (seeded a full withdrawal-hold fixture, then ran `goose down` and confirmed a clean rollback) since this project has no automated down-migration test infrastructure. Three items deferred (migration backfill test coverage — a project-wide pre-existing gap; concurrency test's pool-size-bounded contention level; pre-existing openapi.yaml enum duplication). Regression tests added for every patched finding, including a new integration test proving a withdrawal hold surfaces exactly once in `GET /v1/customers/{id}/transactions`. `make build && make test` (withdrawal/customer/transaction scope) green; the same pre-existing unrelated reorg-detection test failures (5 tests, `TestReorgDetection_EndToEnd` + `TestTrackDeposits_Execute_ReorgCheck_*`) remain red exactly as before this story.
