---
title: 'Story 2.2: Credit Deposits at Finalized Tier via Policy Table'
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

**Problem:** Story 2.1's watcher tracks deposits through `observed`→`safe`, but nothing ever credits a customer's spendable balance — deposits just sit uncredited forever, and the platform has no ledger concept of "money that landed in a forwarder but hasn't been swept yet."

**Approach:** Add a `finalized` promotion (mirroring `safe`'s, against the chain's `finalized` RPC tag) and a policy-table-driven crediting step to the same per-chain watcher poll cycle: when a deposit reaches `finalized` and the `(chain, asset)` crediting policy says `finalized` (the only v1 value), write one balanced journal entry (debit a new platform "forwarder-float" account, credit the customer's existing account), transition the deposit to `credited`, and write a `deposit.credited` outbox event — atomically, extending the same transaction Story 2.1 already opens per poll.

## Boundaries & Constraints

**Always:**
- Crediting is driven by reading the `crediting_policy` table `(chain, asset) → credit_tier`, never a hardcoded Go constant (FR9) — even though v1 seeds every row to `'finalized'`.
- The credit's journal entry (`cause_type='deposit_credit', cause_id=<deposit.id>`) + its two postings (debit forwarder-float, credit customer) + the deposit's `state='credited'` transition + the `deposit.credited` outbox event all commit in one transaction (AD-4), exactly like `RecordObserved` already does for `deposit.pending` in Story 2.1.
- No code path in this story ever transitions a deposit backward, or touches a row already in `finalized` or `credited` state except to move it forward exactly once (`safe`→`finalized`→`credited`) — this is what makes AC2's "credited balance is never reversed" true by construction, not by a runtime check.
- Platform accounts (forwarder-float today) are represented as `accounts` rows with `customer_id IS NULL`, reusing the existing `accounts`/`postings`/`journal_entries` tables and `TransactionRepository`'s already-generic read path — not a parallel ledger.
- `TransactionRepository`'s existing query (`WHERE a.customer_id = $1`) already excludes the platform-side posting automatically (a `NULL` `customer_id` row matches no customer) — do not add a `cause_type` filter anywhere.

**Block If:** (none — every open design question below has a reasonable, narrowly-scoped default; see Design Notes.)

**Never:**
- Generalizing the crediting query to support a `credit_tier` other than `'finalized'` (e.g., crediting at `'safe'` or `'observed'`) — FR9's forward-compatibility claim is that changing a policy *row* is a config change; making the *code* handle a value other than `'finalized'` is explicitly a future story's job, not this one's.
- A "kind"/"account_type" column on `accounts` distinguishing platform account types — only one platform account type (forwarder-float) exists; that generalization is for whichever future story adds treasury/fees.
- Touching `GET /customers/{id}/deposits` (Story 2.1) — it stays scoped to `observed`/`safe` as `"pending"`; credited deposits surface only through the transaction history endpoint (AC4), never through the deposits endpoint.
- Reorg/orphan handling (Story 2.4) or watcher-downtime recovery hardening (Story 2.5).

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Deposit reaches finalized tier, policy says finalized | `safe` deposit's `block_number` &lt;= chain's `finalized` tag | Deposit → `finalized`, then immediately credited → `credited` in the same poll's transaction | none |
| Credit writes | A finalized, policy-eligible deposit | One journal entry (`deposit_credit`), two postings (debit float, credit customer), deposit → `credited`, one `deposit.credited` outbox row — all in one commit | none |
| Deposit already credited | A `credited` deposit re-encountered on a later poll | Never selected again (state guard), never re-credited, never re-appears in the crediting query | none |
| Transaction history after credit | `GET /customers/{id}/transactions` for a customer with a credited deposit | The credit's journal entry appears with `type: "deposit_credit"`, `status: "credited"` | none |
| Policy not `'finalized'` for a `(chain, asset)` pair | A hypothetical future policy row | That pair's finalized deposits are never selected by this story's query — left `finalized`, uncredited (accepted scope boundary, not a bug) | none |
| Concurrent watcher polls for the same chain | Prevented by Story 2.1's advisory lock | N/A (already enforced) | N/A |

</intent-contract>

## Code Map

- `internal/adapter/postgres/migrations/0006_add_platform_accounts_and_crediting_policy.sql` -- `accounts.customer_id` made nullable + partial unique index for platform accounts + 4 seeded forwarder-float rows; new `crediting_policy` table + 4 seeded `'finalized'` rows
- `internal/core/ports.go` -- `ChainScanner.Head` gains a `finalized` return value; `DepositRepository` gains `PromoteToFinalized`, `CreditFinalizedDeposits`
- `internal/core/deposit.go` -- add `CursorTierFinalized` constant (mirrors `CursorTierSafe`)
- `internal/core/track_deposits.go` -- `Execute` extended: promote safe→finalized, then credit eligible finalized deposits
- `internal/adapter/evm/scanner.go` / `scanner_test.go` -- `Head` also queries the `finalized` RPC tag
- `internal/adapter/postgres/deposit_repo.go` -- `PromoteToFinalized` (mirrors `PromoteToSafe`); `CreditFinalizedDeposits` (policy-joined query + per-row journal/posting/outbox/state writes)
- `internal/adapter/postgres/transaction_repo.go` -- `Transaction.Status` computed from `cause_type` (`"deposit_credit"` → `"credited"`, else unchanged `"completed"`) instead of a hardcoded literal
- `internal/core/track_deposits_test.go` -- fakes extended for the 3-value `Head`, `PromoteToFinalized`, `CreditFinalizedDeposits`
- `internal/adapter/api/integration_test.go` -- new test(s) proving the full finalize→credit→transaction-history path against real Postgres
- `api/openapi.yaml` -- `Transaction.status` description updated to mention `"credited"`

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0006_add_platform_accounts_and_crediting_policy.sql` -- `ALTER TABLE accounts ALTER COLUMN customer_id DROP NOT NULL`; `CREATE UNIQUE INDEX ... ON accounts (chain, asset) WHERE customer_id IS NULL`; seed 4 forwarder-float rows (`customer_id=NULL`) for Base/Arbitrum × ETH/USDC; `CREATE TABLE crediting_policy (chain text, asset text, credit_tier text CHECK (credit_tier IN ('observed','safe','finalized')), updated_at timestamptz, PRIMARY KEY (chain, asset))`; seed 4 rows all `credit_tier='finalized'`
- [x] `internal/core/ports.go` -- `ChainScanner.Head(ctx) (latest, safe, finalized uint64, err error)`; `DepositRepository.PromoteToFinalized(ctx, chain, finalizedBlock uint64) (int, error)`; `DepositRepository.CreditFinalizedDeposits(ctx, chain Chain) (int, error)`
- [x] `internal/core/deposit.go` -- `CursorTierFinalized = "finalized"`
- [x] `internal/core/track_deposits.go` -- after the existing safe-promotion block: `PromoteToFinalized(chain, finalized)` + `SetCursor(chain, CursorTierFinalized, finalized)` + `CreditFinalizedDeposits(chain)`, all before commit
- [x] `internal/adapter/evm/scanner.go` -- `Head` additionally queries `HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))`, same pattern/error-wrapping as the existing `safe` query
- [x] `internal/adapter/postgres/deposit_repo.go` -- `PromoteToFinalized`: `UPDATE deposits SET state='finalized', updated_at=now() WHERE state='safe' AND chain=$1 AND block_number<=$2`, mirrors `PromoteToSafe` exactly; `CreditFinalizedDeposits`: query `deposits d JOIN deposit_addresses da ON da.address=d.address JOIN crediting_policy cp ON cp.chain=d.chain AND cp.asset=d.asset JOIN accounts cust ON cust.customer_id=da.customer_id AND cust.chain=d.chain AND cust.asset=d.asset JOIN accounts float ON float.customer_id IS NULL AND float.chain=d.chain AND float.asset=d.asset WHERE d.chain=$1 AND d.state='finalized' AND cp.credit_tier='finalized' ORDER BY d.block_number`; for each row: insert `journal_entries(cause_type='deposit_credit', cause_id=d.id)`, insert 2 postings (`-amount` on float account, `+amount` on customer account), `UPDATE deposits SET state='credited' WHERE id=d.id`, insert `outbox_events(event_type='deposit.credited', payload={depositId, journalEntryId, chain, asset, amount, customerId})`; return rows-processed count
- [x] `internal/adapter/postgres/transaction_repo.go` -- replace the hardcoded `Status: "completed"` with a small mapping: `causeType == "deposit_credit"` → `"credited"`, else `"completed"`
- [x] `api/openapi.yaml` -- update `Transaction.status`'s description to mention `"credited"` as a possible value (no schema/type change — `status` is already a bare `type: string`)
- [x] `internal/core/track_deposits_test.go` -- update `fakeScanner.Head` to return `(latest, safe, finalized uint64, error)`; extend `fakeDepositRepo` with `PromoteToFinalized`/`CreditFinalizedDeposits`; new test cases: a `safe` deposit at/below the finalized tag promotes and gets "credited" (fake tracks a credited count), a deposit above the finalized tag is untouched
- [x] `internal/adapter/api/integration_test.go` -- new test seeding a `finalized`-tier deposit for a real customer, directly invoking `postgres.NewDepositRepository().CreditFinalizedDeposits` against a transaction from the test env's `TxBeginner`, then asserting: the deposit row is now `credited`; exactly one `deposit_credit` journal entry + 2 postings + 1 `deposit.credited` outbox row exist; `GET /customers/{id}/transactions` shows it with `status: "credited"`, `type: "deposit_credit"`; the customer's `GET /customers/{id}/balances` reflects the credited amount

**Acceptance Criteria:**
- Given a deposit reaches `finalized` tier and the `(chain, asset)` crediting policy is `finalized`, when the watcher processes it, then a `deposit_credit` journal entry (debit forwarder-float, credit customer) is written, the deposit transitions to `credited`, and a `deposit.credited` outbox event is written — all in the same transaction.
- Given a deposit has been credited, when any subsequent poll runs, then it is never re-selected, re-credited, or transitioned backward.
- Given the crediting policy is a `(chain, asset)` table row, when a future row is changed to a different value, then no code change is required for the *data* to reflect the new policy (this story's code only acts on `'finalized'`; generalizing execution to act on other values is explicitly out of scope).
- Given a credited deposit, when queried through `GET /customers/{id}/transactions`, then it appears with `type: "deposit_credit"` and `status: "credited"`.

## Spec Change Log

## Review Triage Log

### 2026-07-17 — Review pass

- intent_gap: 0
- bad_spec: 0
- patch: 7 (high 2, medium 3, low 2)
- defer: 3 (medium 2, low 1)
- reject: 1
- addressed_findings:
  - `[high]` `[patch]` No structural sanity check on the `finalized`/`safe` RPC tags before they drive a real ledger credit — a misconfigured RPC or reset chain (the exact class of issue the existing `observedCursor > latest` check already guards against for the scan phase) had no equivalent guard here, despite now driving actual money movement. Fixed: `Head()`'s three returned values are validated as `finalized <= safe <= latest` before anything is promoted or credited; violation fails the poll loudly.
  - `[high]` `[patch]` `CreditFinalizedDeposits` never validated the deposit amount was strictly positive before crediting — a zero or negative `deposits.amount` (upstream bug or hand-edited data) would silently produce a no-op-but-"credited" deposit or a reversed debit/credit direction. Fixed: an explicit `amount.Sign() <= 0` guard (fails loud, mirrors `CreateTransfer`'s validation) plus a DB-level `CHECK (amount > 0)` on `deposits` as defense in depth.
  - `[medium]` `[patch]` `crediting_policy.credit_tier`'s CHECK permitted `'observed'`/`'safe'` values no code path can act on — an operator setting either (the schema itself invites it) would silently strand every deposit for that `(chain, asset)` at `finalized` forever, with zero signal. Fixed: tightened the CHECK to `credit_tier = 'finalized'` only, until a future story actually implements support for another tier (at which point that story's own migration widens the constraint).
  - `[low]` `[patch]` One bound parameter (`$2`) was reused across two unrelated columns (`d.state` and `cp.credit_tier`) — correct today only because `DepositState`'s and `credit_tier`'s `"finalized"` string literals coincide, with nothing tying the two vocabularies together. Fixed: split into two separate parameters.
  - `[medium]` `[patch]` FR9's "policy-table-driven, not hardcoded" claim had no test proving the `crediting_policy` join is actually load-bearing. Fixed: added a test that deletes a `(chain, asset)` pair's policy row and asserts its finalized deposit is never credited — this is the achievable version of that test given the CHECK-tightening fix above (there's no other *allowed* policy value left to test against).
  - `[low]` `[patch]` The migration's down-path (`DELETE FROM accounts WHERE customer_id IS NULL`) hits a foreign-key violation from `postings.account_id` as soon as any deposit has actually been credited, making the down-migration broken in exactly the environment where it would be used. Fixed: delete dependent `postings`/`journal_entries` rows first.
  - `[medium]` `[patch]` `CreditFinalizedDeposits` had no batch cap, unlike the scan phase's `maxBlocksPerScan` — a large backlog (extended downtime, a burst of deposits) would process entirely within one long-held transaction instead of incrementally across polls. Fixed: added a `LIMIT`-bounded batch size, mirroring the scan phase's incremental-catch-up philosophy.
  - `[medium]` `[defer]` `CreditFinalizedDeposits` has no per-row failure isolation — one hard DB error mid-batch aborts the whole poll's credit phase (and, via the shared transaction, that poll's scan/promotion work too). The most likely realistic trigger (a bad amount) is closed by the validation fix above; genuine per-row isolation (savepoints, or a redesign away from one-transaction-per-poll) is a bigger architectural change appropriate for Epic 6's consolidated fault-injection work, not a quick patch here.
  - `[medium]` `[defer]` A missing `accounts`/`crediting_policy` row for a `(chain, asset)` pair silently excludes any matching deposit from ever being credited (INNER JOINs, no error surfaced) — including the symptom that such a deposit becomes invisible on both the pending-deposits and transaction-history endpoints simultaneously. Unreachable in greenfield: `CreateCustomer` always creates all 4 per-customer accounts, and migration 0006 seeds all 4 platform accounts + all 4 policy rows for the current fixed `SupportedChainAssetPairs`. Becomes relevant only if a future chain/asset is added without matching seed rows.
  - `[low]` `[defer]` No fault-injection test forces a mid-batch failure to verify multi-deposit atomicity (the code's own comment claims a partial failure rolls back cleanly, but nothing proves it). Deferred to Epic 6's consolidated fault-injection suite, matching the identical precedent set for Story 1.3's lock-ordering deadlock test.
  - `[low]` `[reject]` `transactionStatus`'s two-way branch (`deposit_credit` → `credited`, else `completed`) has no exhaustive-switch/lookup-table forcing function to catch a future cause type needing a different status. Only two cause types exist today and the fallback is correct for the one not explicitly branched; building a forcing mechanism against a hypothetical future mistake is speculative hardening, not a real issue in this diff.

## Design Notes

- **Platform accounts reuse the existing `accounts` table** (`customer_id IS NULL`) rather than a parallel table: `postings.account_id` already references `accounts(id)` generically, and `TransactionRepository`'s customer-scoped query already excludes non-customer rows for free — no new join logic needed anywhere that reads the ledger.
- **Only forwarder-float exists as a platform account today.** The architecture spine's "fixed account taxonomy" also names treasury and fees, but those are Epic 3's concern; adding a `kind` column now for account types that don't exist yet would be speculative. `customer_id IS NULL` is a sufficient discriminator for exactly one platform account type.
- **`CreditFinalizedDeposits` does the policy join and all per-row writes in the adapter**, not in `core` — mirrors how `DepositReader` already resolves `customer_id` via a join at read time (Story 2.1) rather than pushing multi-table resolution logic into `core`. `core.TrackDeposits.Execute` just calls the port once per poll, the same shape as its existing `PromoteToSafe` call.
- **`cause_id = deposit.id`** (not an idempotency key) is deposit-crediting's natural dedup key — globally unique, one-to-one with exactly one credit event ever, backed by the same `journal_entries UNIQUE(cause_type, cause_id)` constraint Story 1.3's transfers rely on. Unlike transfers, there's no legitimate client retry scenario here, so a unique-violation is left to fail loudly (a real hit would mean a genuine double-credit bug), not translated to a special sentinel error.
- **No new cursor is needed for correctness of the finalized/credit phases** — both operate as idempotent bulk `WHERE state=...` operations over already-persisted rows (re-running is naturally a no-op once a row has moved past the matched state), unlike the observed-scan phase which genuinely needs a cursor to know which new blocks to scan. `SetCursor(chain, CursorTierFinalized, finalized)` is still written, purely for the same observability-bookkeeping reason Story 2.1 already writes the safe cursor.

## Verification

**Commands:**
- `make build && make lint && make test` -- expected: all green, including the extended integration test and updated unit tests
- `make check-import-boundary` -- expected: still passes (no new go-ethereum usage outside `internal/adapter/evm`)
- `cd contracts && forge test` -- expected: unaffected, still 4/4

**Manual checks (if no CLI):**
- Run the full local stack (`api` + `watcher --chain=base` against anvil), create a customer, send an ETH deposit, and mine enough blocks that anvil's `finalized` tag covers it; confirm the deposit reaches `credited` and `GET /customers/{id}/balances` reflects the credited amount.

## Auto Run Result

**Status:** done

**Summary:** Added policy-driven crediting at the finalized tier: `TrackDeposits.Execute` now promotes `safe`→`finalized` (mirroring `safe`'s own promotion against the chain's `finalized` RPC tag) and, in the same transaction, credits every finalized deposit whose `(chain, asset)` `crediting_policy` row says `'finalized'` — writing one balanced journal entry (debit a new platform forwarder-float account, credit the customer's existing account), transitioning the deposit to `credited`, and writing a `deposit.credited` outbox event. Platform accounts reuse the existing `accounts` table (`customer_id IS NULL`). `GET /customers/{id}/transactions` now shows credited deposits with `status: "credited"`.

**Files changed:**

*New:*
- `internal/adapter/postgres/migrations/0006_add_platform_accounts_and_crediting_policy.sql` — nullable `accounts.customer_id` + partial unique index + 4 seeded forwarder-float rows; `crediting_policy` table (CHECK-tightened to `'finalized'` only) + 4 seeded rows; down-migration ordered to avoid an FK violation once credited rows exist

*Modified:*
- `internal/core/ports.go` — `ChainScanner.Head` gains `finalized`; `DepositRepository` gains `PromoteToFinalized`, `CreditFinalizedDeposits`
- `internal/core/deposit.go` — `CursorTierFinalized` constant
- `internal/core/track_deposits.go` — `Execute` extended with finalized-promotion + crediting phases, plus a `finalized <= safe <= latest` sanity check before either runs
- `internal/adapter/evm/scanner.go`, `scanner_test.go` — `Head` also queries the `finalized` RPC tag
- `internal/adapter/postgres/deposit_repo.go` — `PromoteToFinalized`, `CreditFinalizedDeposits` (policy-joined query, per-row journal/postings/outbox/state writes, amount-sign guard, batch-size cap, two independently-bound query parameters)
- `internal/adapter/postgres/migrations/0005_create_deposits.sql` — added `CHECK (amount > 0)` on `deposits.amount` (not-yet-run migration, safe to edit in place per the 1-5/2-1 precedent)
- `internal/adapter/postgres/transaction_repo.go` — `Transaction.Status` computed from `cause_type` instead of a hardcoded literal
- `internal/core/track_deposits_test.go` — 3-value `fakeScanner.Head`, `PromoteToFinalized`/`CreditFinalizedDeposits` fakes, new test cases
- `internal/adapter/api/integration_test.go` — `TestCreditFinalizedDeposits_EndToEnd` (AC1/AC4, AC2 never-re-credited, and the new FR9 no-policy-row test)
- `api/openapi.yaml` — `Transaction.status` description mentions `"credited"`

**Review findings breakdown** (2026-07-17 pass, Blind Hunter + Edge Case Hunter, 17 raw → 12 deduplicated):
- 7 patch (2 high, 3 medium, 2 low) — all applied and verified: `finalized <= safe <= latest` sanity check, amount-sign validation (+ DB `CHECK`), tightened `credit_tier` CHECK, split reused query parameter, no-crediting-policy-row test, fixed down-migration FK ordering, batch-size cap on `CreditFinalizedDeposits`
- 3 defer (2 medium, 1 low) — logged in `deferred-work.md`: no per-row failure isolation in the credit batch; missing `accounts`/`crediting_policy` join rows silently stranding a deposit (unreachable in greenfield); no fault-injection test for mid-batch atomicity (Epic 6 territory)
- 1 reject (noise): `transactionStatus`'s two-way branch lacking a forcing function for hypothetical future cause types
- 0 intent_gap, 0 bad_spec

**Follow-up review recommended:** `true` — this story introduced the platform's first non-customer ledger account and its first data-driven (not hardcoded) policy table, both money-moving changes reviewed and patched in the same pass; a fresh independent look is warranted before considering the crediting path fully settled, consistent with the same judgment made for Story 1.5's rework.

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `make check-import-boundary` — all clean
- `go test ./...` — all green, including the real-Postgres integration suite (`TestCreditFinalizedDeposits_EndToEnd`, all 3 subtests) and the real-anvil scanner test (confirmed the 3-value `Head()` call works against anvil)
- `cd contracts && forge test` — unaffected, 4/4
- Review diff was scoped to exactly this story's changes (a pre-implementation snapshot of the 11 files this story would touch, diffed against their post-implementation state) rather than the full cumulative uncommitted working tree, so Blind Hunter/Edge Case Hunter reviewed only Story 2.2's actual changes, not Story 2.1's already-reviewed code.

**Residual risks:**
- The 3 deferred items above remain open, tracked in `deferred-work.md`.
- No git commit was created (user's global no-auto-commit policy, which overrides this skill's own "commit, don't push" finalize instruction). All changes remain uncommitted in the working tree, stacked on top of Story 2.1's own uncommitted changes. `final_revision` reflects this — HEAD has not moved from `baseline_revision`.
