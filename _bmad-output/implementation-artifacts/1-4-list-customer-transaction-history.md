---
baseline_commit: 045ca784fb4e239793f0a3a6548740ccf8d6a022
---

# Story 1.4: List Customer Transaction History

Status: done

<!-- Note: Validation is optional. Run validate-create-story for quality check before dev-story. -->

## Story

As an application team,
I want to list a customer's transaction history with status,
so that I can show customers an accurate record without querying the chain myself.

## Acceptance Criteria

1. **Given** a customer with one completed internal transfer, **when** I GET `/v1/customers/{id}/transactions`, **then** the transfer appears twice — once for each side — with its `type` (`internal_transfer`), `amount` (signed, from that customer's own perspective: negative for the debited/source side, positive for the credited/destination side), `asset`, `chain`, `status` (`completed`), and `createdAt` timestamp. [FR3]
2. **Given** a customer with no transactions, **when** queried, **then** an empty paginated list (`"transactions": []`) is returned, not an error.
3. **Given** more transactions exist than the page size, **when** queried, **then** the response is paginated with stable ordering (newest first) and a `nextCursor` for the next page — `null`/omitted once the last page is reached. Paging through every page must yield every transaction exactly once, in order, no duplicates and no gaps.
4. **Given** the query reads generically from the cause-tagged journal (`journal_entries` joined to `postings` restricted to this customer's own accounts, with no `cause_type` filter anywhere in the query), **when** future cause types (deposit, withdrawal) are added in later epics, **then** they appear in this endpoint automatically, with no endpoint rewrite required.
5. **Given** a `customerId` that has no customer record, **when** queried, **then** the platform returns a 404 `problem+json` response — inferred from Stories 1.2/1.3's own precedent for unknown-customer handling; epics.md's literal Story 1.4 AC text doesn't restate it, but every other per-customer GET in this API 404s on an unknown id and this one must stay consistent.
6. **Given** a request to this endpoint without a valid bearer token, **when** processed, **then** the platform rejects it with a 401 response — this route is not an exception to NFR15/AD-14's no-anonymous-surface rule (inferred, standing behavior already tested per-route in Stories 1.1–1.3).
7. **Given** a `cursor` query parameter that does not decode to a valid page marker (tampered, truncated, or from a different customer), **when** submitted, **then** the platform rejects it with a 400 `problem+json` response — never a 500 and never silently treated as "first page."
8. **Given** a `pageSize` query parameter that is present but not a positive integer (zero, negative, or non-numeric), **when** submitted, **then** the platform rejects it with a 400 `problem+json` response. A `pageSize` above the maximum is not an error — it is silently clamped to the maximum (see Dev Notes → Pagination).

**Architectural decisions this story must make (not explicit in the epics AC text, but required to satisfy AC1–AC4 and the existing schema honestly):**

- **`status` is a fixed constant, `"completed"`, for every row this story's query can ever produce — and that is deliberately, not lazily, forward-compatible.** A row only exists in `journal_entries`/`postings` once AD-4's single commit has happened; every writer that has shipped so far (Story 1.3's `TransferRepository`) only ever writes a journal entry once its postings are final; the balanced-postings query has no notion of "half-written." So for internal transfers, `"completed"` is unconditionally correct, not a placeholder. **Caveat for future story authors, stated here so it isn't mistaken for an oversight later:** this reasoning stops being sufficient the moment a future cause type writes a journal entry for a *non-terminal* step of its own state machine — the Solution Design's withdrawal sequence diagram shows the **hold placement** (state `created`, long before `confirmed`) already carries a journal entry (available → hold posting). When Epic 3 reaches that story, a naive reuse of "row exists in the journal ⇒ status completed" would mislabel an in-flight withdrawal as finished. That reconciliation is explicitly **that** epic's job, not this one's — Story 1.4 only has to be honest about the one cause type that exists today, and it is.
- **The generic query never inspects `cause_type` for filtering or joins — only for display.** `cause_type` is selected as a plain column and passed through as the `type` field; it is never used in a `WHERE`, `CASE`, or lookup table. This is what makes AC4's "no endpoint rewrite required" literally true for any future cause type that follows the existing pattern (one journal entry, N postings, written atomically) — the query doesn't know or care what `internal_transfer` means, and neither will it need to know what a future `deposit_credit` or `withdrawal_settle` means, for the basic case of "does this row show up."
- **`amount` is the customer's own posting amount, signed — not the transfer's unsigned magnitude.** `Transfer.amount` (the create-transfer response body, Story 1.3) is unsigned because it isn't attached to either party's perspective. This endpoint *is* per-customer, so it follows the conventional ledger/statement idiom (debits negative, credits positive) instead: the row's `amount` is exactly `postings.amount` for the posting on *this* customer's account, which is already signed correctly by construction (Story 1.3 writes `-amount` for the source posting, `+amount` for the destination posting) — no sign-flipping logic needed, just select the column as-is.
- **Keyset pagination on `(journal_entries.created_at, journal_entries.id)`, not on `id` alone.** `journal_entries.id` is a UUIDv7 (broadly time-ordered) but relying on its byte order as the sole sort/cursor key would be an extra, unstated assumption about UUIDv7 layout holding forever; `created_at` is the column every other part of this codebase already treats as the authoritative ordering field. Order by `created_at DESC, id DESC` (the `id` tiebreak only matters for same-millisecond ties and guarantees a strict total order — pagination cannot skip or repeat a row even under ties). The opaque cursor encodes both fields (see Dev Notes → Pagination for the exact scheme); a row-wise comparison (`WHERE (created_at, id) < ($cursor_created_at, $cursor_id)`) makes the query itself trivial once the cursor is decoded.
- **No new migration.** `journal_entries` and `postings` (Story 1.2's `0003_...sql`) already carry everything this read needs — `cause_type`, `created_at`, `amount`, and the `account_id` FK back to `accounts` (which carries `chain`/`asset`/`customer_id`). This is a pure read against existing tables, exactly like `BalanceRepository`.
- **This story does not surface the counterparty's customer id.** AC1 asks for `type`, `amount`, `asset`, `chain`, `status`, `createdAt` — not "who was on the other side." Adding that would require a self-join to find the journal entry's other posting(s), which is real added complexity, generalizes worse once a cause type ever has more than 2 postings, and isn't asked for. Left as a candidate future enhancement, not built speculatively here.

## Tasks / Subtasks

- [x] **Task 1: Ledger domain types & port** (AC: 1, 2, 3, 4, 5, 7, 8)
  - [x] `internal/core/customer.go`: add `Transaction` (`ID string` — the journal entry id; `Type string` — the `cause_type` column, verbatim; `Amount *big.Int` — signed; `Chain Chain`; `Asset Asset`; `Status string`; `CreatedAt time.Time`) and `TransactionPage` (`Transactions []Transaction`; `NextCursor string` — empty string means "no next page") domain types. Add `ErrInvalidCursor` and `ErrInvalidPageSize` sentinel errors alongside the existing `ErrCustomerNotFound`/`ErrInsufficientBalance`/`ErrDuplicateTransferCause`.
  - [x] `internal/core/ports.go`: add `TransactionRepository` interface with one method, `ListCustomerTransactions(ctx context.Context, customerID string, pageSize int, cursor string) (TransactionPage, error)` — doc-comment it like `BalanceRepository`: this is a non-mutating read, implementations query independently of any transaction on ctx (no `txFromContext`), because `IdempotencyMiddleware` never opens a transaction for this route's GET method. `cursor == ""` means "first page." Returns `ErrCustomerNotFound` if no customer with that id exists, `ErrInvalidCursor` if `cursor` is non-empty but doesn't decode, `ErrInvalidPageSize` if `pageSize <= 0`.
  - [x] `internal/core/list_customer_transactions.go`: `ListCustomerTransactions` use case — constructor `NewListCustomerTransactions(repo TransactionRepository)`, `Execute(ctx, customerID string, pageSize int, cursor string) (TransactionPage, error)`. Apply the `pageSize` policy here, before calling the repository: `pageSize == 0` (caller omitted it) → substitute the default (20); `pageSize < 0` → `ErrInvalidPageSize` (AC8); `pageSize > 100` → clamp to 100 (AC8, no error). Then delegate to the repository unchanged.
- [x] **Task 2: Postgres TransactionRepository — generic cause-tagged journal read** (AC: 1, 2, 3, 4, 5, 7)
  - [x] `internal/adapter/postgres/transaction_repo.go`: `TransactionRepository` holds its own `*pgxpool.Pool` (like `BalanceRepository`, not `CustomerRepository`/`TransferRepository`) — this route is non-mutating. Constructor `NewTransactionRepository(pool *pgxpool.Pool)`.
  - [x] Cursor codec (private helpers in this file, not exported — this is a persistence detail, not a domain concern): encode `created_at` (RFC 3339 nanosecond) + `id` into an opaque base64 string; decode reverses it, returning `core.ErrInvalidCursor` on any malformed input (wrong shape, bad base64, unparseable timestamp, non-UUID id).
  - [x] `ListCustomerTransactions` implementation, in order:
    1. Existence check exactly like `BalanceRepository.CustomerBalances`: `SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)` → `core.ErrCustomerNotFound` if false. (Same accepted TOCTOU as `BalanceRepository`'s own existence-check-then-query pattern — see `deferred-work.md`; not a new problem introduced here.)
    2. If `cursor != ""`, decode it; on decode failure return `core.ErrInvalidCursor` before running any query.
    3. Main query (pool-direct, no transaction):
       ```sql
       SELECT je.id, je.cause_type, je.created_at, p.amount, a.chain, a.asset
       FROM journal_entries je
       JOIN postings p ON p.journal_entry_id = je.id
       JOIN accounts a ON a.id = p.account_id
       WHERE a.customer_id = $1
         AND ($2::boolean IS FALSE OR (je.created_at, je.id) < ($3, $4))
       ORDER BY je.created_at DESC, je.id DESC
       LIMIT $5
       ```
       Bind `$2` = "cursor present" boolean, `$3`/`$4` = decoded cursor fields (ignored by Postgres when `$2` is false — pass zero values), `$5` = `pageSize + 1` (fetch one extra row to detect a next page without a second round-trip).
    4. If more than `pageSize` rows came back, there is a next page: build `NextCursor` from the `pageSize`-th row (0-indexed: `rows[pageSize-1]`) and truncate the slice to `pageSize`. Otherwise `NextCursor` stays `""`.
    5. Map each remaining row to `core.Transaction`, parsing `amount` via `big.Int.SetString` exactly like `BalanceRepository`/`TransferRepository` (established `::numeric`-column-as-text read pattern — cast `p.amount::text` in the `SELECT`, same as the other two repositories, rather than binding a `pgtype.Numeric` on the read side). Set `Status: "completed"` unconditionally (see the story's "Architectural decisions" for why that is safe today and explicitly not this story's problem to generalize).
- [x] **Task 3: OpenAPI spec — GET /customers/{id}/transactions** (AC: 1, 2, 3, 5, 6, 7, 8)
  - [x] `api/openapi.yaml`: new path `/customers/{id}/transactions`, `operationId: listCustomerTransactions`, path param `id` (uuid, like the existing balances route), query params `cursor` (string, optional) and `pageSize` (integer, optional), responses `200` (`TransactionsResponse`), `400`, `401`, `404`, `500` — error responses `application/problem+json` / `ProblemDetails`, matching every other route.
  - [x] New schemas: `Transaction` (`id` uuid, `type` string, `amount` string, `chain` enum `[base, arbitrum]`, `asset` enum `[eth, usdc]`, `status` string, `createdAt` date-time — all required) and `TransactionsResponse` (`transactions`: array of `Transaction`, required; `nextCursor`: string, **not** required/nullable-omit-when-absent — matches this API's existing style of omitting rather than null-ing absent optional fields). `amount` **must** be `type: string` (money-as-string convention, unchanged since Story 1.2).
  - [x] Regenerate `internal/adapter/api/server.gen.go` via `oapi-codegen` v2.7.2 (same pinned version as Stories 1.1–1.3). Confirmed the exact generated parameter-binding type: `ListCustomerTransactionsParams{Cursor *string; PageSize *int}` — matched the prediction exactly.
- [x] **Task 4: Handler & wiring** (AC: 1, 2, 3, 4, 5, 6, 7, 8)
  - [x] `internal/adapter/api/transactions.go` (new file, following `customers.go`'s non-mutating `GetCustomerBalances` shape — no idempotency/body concerns, this is a GET): implement `ListCustomerTransactions(w, r, id uuid.UUID, params ListCustomerTransactionsParams)` on `customerServer`. Steps: read `params.PageSize` (nil → pass `0` through to the use case, which substitutes the default; a decode-level generator error for a non-numeric `pageSize` is already routed through `main.go`'s `ErrorHandlerFunc` → 400, same as today's `id` path param handling); read `params.Cursor` (nil → pass `""`); call `s.listTransactions.Execute(r.Context(), id.String(), pageSize, cursor)`; map errors: `errors.Is(err, core.ErrCustomerNotFound)` → 404, `errors.Is(err, core.ErrInvalidCursor)` or `core.ErrInvalidPageSize` → 400, anything else → 500; on success, 200 + JSON-encoded `TransactionsResponse` with each `Transaction.Amount` rendered via `.String()` and `NextCursor` omitted (zero value) when the use case returned `""`.
  - [x] Extend `customerServer` struct and `NewServerInterface` constructor to also take `listTransactions *core.ListCustomerTransactions`, mirroring how `createTransfer` was added in Story 1.3.
  - [x] `cmd/walletd/main.go`: construct `postgres.NewTransactionRepository(pool)`, `core.NewListCustomerTransactions(transactionRepo)`, and pass into the updated `NewServerInterface(createCustomer, getBalances, createTransfer, listTransactions)` call. No middleware changes — `AuthMiddleware` already covers this route unconditionally, and it's a GET so `IdempotencyMiddleware` passes it straight through untouched, exactly like the existing balances route.
- [x] **Task 5: Tests**
  - [x] `internal/core/list_customer_transactions_test.go`: table-driven unit tests against a fake `TransactionRepository` — `pageSize == 0` substitutes 20 before the repo is called (assert what the fake actually received), `pageSize < 0` rejected as `ErrInvalidPageSize` before the repo is ever called (assert the fake's method was not invoked), `pageSize > 100` clamped to 100 before the repo is called, and successful/`ErrCustomerNotFound`/`ErrInvalidCursor` all pass through from the fake unchanged.
  - [x] `internal/adapter/api/integration_test.go` (extend, real Postgres via `testcontainers-go` + `postgres:18`, no mocked repository — same rigor as Stories 1.1–1.3): reuse `createTestCustomer`/`postTransfer`/`creditAccount` helpers already in this file. Cover: AC1 (one internal transfer between two customers; query both customers' history; assert the source sees a negative `amount` and the destination sees the same magnitude positive, both with `type: "internal_transfer"`, `status: "completed"`, matching `chain`/`asset`, and a `createdAt` timestamp), AC2 (a freshly created customer with no transfers → `200` with `"transactions": []`, not an error), AC3 (create enough transfers between two customers to span at least 3 pages at a small test `pageSize`; page through using `nextCursor` until it's empty; assert the concatenated result is every transaction exactly once, in strictly descending `createdAt` order, no duplicates, no gaps), AC5 (a random unused uuid as `id` → 404), AC6 (missing bearer token → 401), AC7 (a garbage `cursor` value, e.g. `"not-a-real-cursor"` → 400, not 500), AC8 (`pageSize=0` explicit query value is indistinguishable from omitted at the wire level — the generator maps an absent param to `nil`, not to `0`, so this AC is really about `pageSize=-1` and `pageSize=abc` → 400; also assert `pageSize=1000` succeeds and returns at most 100 rows, proving the clamp). **Found and fixed a real bug during this test's first real-Postgres run**: binding an empty string for the cursor's `uuid`-typed SQL parameter failed with "invalid input syntax for type uuid" even when the cursor branch was logically unreached (`$2::boolean IS FALSE OR ...`) — Postgres validates a parameter's text form against its inferred type at bind time, before the `OR` can short-circuit at execution time. Fixed by always binding a syntactically valid placeholder (`uuid.Nil`) when no cursor is present.
  - [x] `internal/adapter/postgres/transaction_repo_test.go` — **Skipped**, same as Story 1.3's precedent for `transfer_repo_test.go`: the integration suite already exercises this repository's query (including cursor encode/decode, the existence check, and pagination) against real Postgres, and is sufficient on its own.

## Dev Notes

### Architecture patterns and constraints this story must follow

- **Non-mutating read, pool-direct — not tx-from-context.** `TransactionRepository` follows `BalanceRepository`'s shape (holds its own `*pgxpool.Pool`), not `CustomerRepository`'s/`TransferRepository`'s (tx-from-context) — this is a GET route, and `IdempotencyMiddleware` never opens a transaction for non-mutating methods. Calling `txFromContext(ctx)` here would panic (see `internal/adapter/postgres/tx.go`'s own doc comment on that function). This is Story 1.2's own explicit guidance for making this choice, restated because it's this story's turn to apply it.
- **Generic-by-construction, not generic-by-accident.** The whole point of AC4 is that this query must never grow a `WHERE cause_type = '...'` or a `switch cause_type { ... }` for anything that affects *whether a row appears* or *what its `type`/`amount`/`chain`/`asset` are* — those columns are selected as-is from `journal_entries`/`postings`/`accounts`, which is exactly what makes a future writer's rows "just show up." `status` is the one field that isn't a passthrough (see "Architectural decisions" above for why a constant is safe today and explicitly bounded to today).
- **Pagination (the riskiest wiring decision in this story).** Order by `(created_at DESC, id DESC)`; encode both fields into the opaque cursor; compare with a Postgres row-wise `(created_at, id) < (cursor_created_at, cursor_id)` predicate. Fetch `pageSize + 1` rows to detect a next page without a second query. A cursor is only ever produced by this endpoint itself and only ever consumed by it — treat any cursor that doesn't decode as attacker/bug input, not as "must mean first page" (AC7). Do not use `OFFSET`-based pagination (it re-scans skipped rows on every page and its correctness silently degrades under concurrent inserts — the exact newest-first stability AC3 requires would not hold).
- **Money convention.** Integer base units only, `NUMERIC(78,0)` in Postgres, `*big.Int` in Go, `string` (never a JSON number) at the API boundary — unchanged since Story 1.2, `amount` here is signed (see "Architectural decisions").
- **Hexagonal boundary (AD-1, AD-2).** `internal/core` still imports nothing from `internal/adapter/*`. `TransactionRepository`'s port lives in `core`; its Postgres implementation lives in `adapter/postgres`, same shape as `BalanceRepository`.
- **Auth applies unconditionally (NFR15, AD-14).** No exception for this route, same as every other route so far.
- **The existence-check-then-query TOCTOU is an accepted, already-deferred trade-off, not a new bug.** `deferred-work.md`'s entry from the 1-2 review ("Existence check + balance SUM run as two separate pool queries outside a transaction") already covers exactly this shape of read; `TransactionRepository` inherits the same trade-off for the same reason (benign for a read-only route with no delete path). Do not attempt to fix it as part of this story — it's tracked, not forgotten.

### Source tree components this story creates or touches

```text
digital-asset-wallet-platform/
  internal/core/
    customer.go                     # MODIFY — add Transaction, TransactionPage, ErrInvalidCursor, ErrInvalidPageSize
    ports.go                        # MODIFY — add TransactionRepository interface
    list_customer_transactions.go   # NEW — the use case
    list_customer_transactions_test.go  # NEW
  internal/adapter/
    api/
      server.gen.go                 # MODIFY (regenerated) — do not hand-edit
      transactions.go               # NEW — ListCustomerTransactions handler
      customers.go                  # MODIFY — extend customerServer + NewServerInterface with listTransactions
      integration_test.go           # MODIFY — extend with transaction-history ACs
    postgres/
      transaction_repo.go           # NEW — pool-direct, generic cause-tagged journal read + cursor codec
      transaction_repo_test.go      # NEW (optional — see Task 5)
  api/openapi.yaml                  # MODIFY — new path + Transaction/TransactionsResponse schemas
  cmd/walletd/main.go                 # MODIFY — wire TransactionRepository + ListCustomerTransactions
```

No new migration file, no new files under `internal/adapter/evm/`, `internal/adapter/signer/`, `internal/adapter/webhook/`, or `contracts/` — those remain out of scope until Epics 2–4.

### Schema (reused, not created)

This story only reads `journal_entries`, `postings`, and `accounts` — all created by Stories 1.1/1.2 (`0001_...sql`, `0003_...sql`) and already written to by Story 1.3. No new migration.

### Config

No new environment variables.

### Testing standards

- Table-driven Go tests for the use case, isolated with a fake `TransactionRepository` (unchanged pattern from Stories 1.1–1.3).
- Integration test against real Postgres (`testcontainers-go`, `postgres:18`) — no mocked repository, per the project's rigor thesis (PRD Success Metric 5) and Stories 1.1–1.3's precedent. This is the only realistic way to prove keyset pagination is actually gap-free and duplicate-free across pages, not just correct-looking in isolation.
- AC4 (forward-compatibility for future cause types) cannot be meaningfully automated in this story — no second cause type exists yet to exercise it against. Satisfy it by construction (no `cause_type` filtering anywhere in the query, verified by code review) rather than by a test that would otherwise have to fabricate a fake future cause type.

### Project Structure Notes

- No conflicts with the existing structure — this story extends `internal/core`, `internal/adapter/{api,postgres}`, `api/openapi.yaml`, and `cmd/walletd/main.go`, all files Stories 1.1–1.3 already established the shape of. Read each MODIFY-marked file above in full before changing it — do not guess at `customers.go`'s, `main.go`'s, or `ports.go`'s current contents from memory.
- `TransactionRepository` follows `BalanceRepository`'s pool-direct shape (non-mutating read), not `CustomerRepository`'s/`TransferRepository`'s tx-from-context shape (mutating) — pick the pattern that matches this port's actual read/write nature, per Story 1.2's own explicit guidance on this choice, restated in Story 1.3's Dev Notes and now here for the third time because it's a recurring fork every new port must resolve deliberately, not by copy-paste of whichever file was open last.

### References

- [Source: _bmad-output/planning-artifacts/epics.md#Epic 1: Foundation — Accounts, Ledger & Deposit Addresses / Story 1.4] — canonical ACs
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md#AD-1, AD-3, AD-4] — hexagonal boundary, double-entry ledger, one-transaction-per-change
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md#4. The money model, §5] — account taxonomy; the withdrawal hold-placement sequence diagram that grounds this story's "status is a constant today, not forever" caveat
- [Source: internal/adapter/postgres/migrations/0003_create_journal_entries_and_postings.sql] — `journal_entries`/`postings` shape this story reads, unchanged
- [Source: internal/adapter/postgres/balance_repo.go] — the pool-direct, existence-check-then-query pattern this story's repository follows
- [Source: _bmad-output/implementation-artifacts/deferred-work.md#Deferred from: code review of 1-2-query-customer-balances] — the existence-check TOCTOU this story's repository inherits, not reopens
- [Source: _bmad-output/implementation-artifacts/1-3-ledger-only-internal-transfer-between-customers.md] — established repo/use-case/handler shapes, `WriteProblem` helper, `::text`/`big.Int.SetString` amount-parsing precedent, the "resolve an unstated architectural gap explicitly, in-story" convention this story's own "Architectural decisions" section follows

### Previous story intelligence (from Story 1.3)

- **TDD throughout**: every source file had a corresponding test file written first (red) before implementation (green). Follow the same discipline here.
- **Adversarial code review is a standing convention**, not a one-off — expect a fresh-context review pass (Blind Hunter + Edge Case Hunter + Acceptance Auditor) before this story is marked done, same as 1.1–1.3. Story 1.3's own review surfaced (and this story should pre-empt): missing required-field validation on decode, an under-tested edge-case AC, and an imprecise code comment about *why* a concurrency-safety mechanism works — write comments about pagination correctness with the same care (don't just assert it's correct, say why the row-wise comparison and the `pageSize+1` over-fetch are each individually necessary).
- **Repository shape is a deliberate choice, not a default**: mutating ports use tx-from-context (`CustomerRepository`, `TransferRepository`); non-mutating reads use pool-direct (`BalanceRepository`, and now `TransactionRepository`).
- **`::text` cast + `big.Int.SetString` is the established read-side amount pattern** (`balance_repo.go`, `transfer_repo.go`'s own balance-check read) — reuse it verbatim here; there is no write-side risk in this story (nothing here writes `postings`), so Story 1.3's `::numeric`-cast verification work doesn't need re-doing.
- **Story 1.3's code review resolved a similar "riskiest wiring decision" pattern** (deadlock-safe locking) by writing the reasoning directly into the code comment, verified against how Postgres actually acquires locks (not assumed). Apply the same discipline to this story's cursor/keyset-pagination correctness — it is the analogous "this one thing must actually be right" risk here.
- No dependency additions expected — `math/big`, `github.com/google/uuid`, `github.com/jackc/pgx/v5`, and Go's own `encoding/base64`/`time` packages are either already in use or standard library.

## Change Log

- Implemented all 8 ACs end-to-end: a customer's transaction history read generically from the cause-tagged journal (no `cause_type` filter anywhere), signed per-customer amounts (debit negative, credit positive), keyset pagination on `(created_at, id)` with an opaque cursor, empty-list (not error) for a customer with no transactions, 404 for an unknown customer, 401 for a missing bearer token, 400 for an undecodable cursor, and 400 for an invalid `pageSize` with silent clamping above the maximum.
- Added `core.Transaction`/`core.TransactionPage` domain types and `core.ErrInvalidCursor`/`core.ErrInvalidPageSize` sentinels alongside the existing ledger errors.
- Added `core.TransactionRepository` port (pool-direct, non-mutating — like `BalanceRepository`, not `CustomerRepository`/`TransferRepository`) and `core.ListCustomerTransactions` use case, which owns the `pageSize` policy (default 20, clamp to 100, reject negative) before delegating to the repository.
- Added `postgres.TransactionRepository`: existence check (same accepted TOCTOU as `BalanceRepository`), then a single generic query joining `journal_entries`/`postings`/`accounts` restricted to the customer's own accounts, ordered `(created_at DESC, id DESC)`, over-fetching `pageSize + 1` rows to detect a next page. `status` is a fixed `"completed"` constant, deliberately scoped to today's only cause type (see the story's "Architectural decisions").
- **Found and fixed a real bug on the first real-Postgres integration-test run**: binding an empty string for the cursor's `uuid`-typed SQL parameter (`je.id`) failed with "invalid input syntax for type uuid" even when the cursor branch of the `OR` was never logically reached — Postgres validates a parameter's text form against its inferred type at bind time, before execution can short-circuit the `OR`. Fixed by always binding a syntactically valid placeholder (`uuid.Nil`) when no cursor is present, regardless of whether it's ever compared against.
- Added `POST`-sibling `GET /v1/customers/{id}/transactions` to `api/openapi.yaml` (`Transaction`, `TransactionsResponse` schemas; `amount` is `type: string`, signed). Regenerating via `oapi-codegen` v2.7.2 produced the predicted `ListCustomerTransactionsParams{Cursor *string; PageSize *int}` and `TransactionChain`/`TransactionAsset` enum names (collision-avoided, matching the `TransferChain`/`TransferAsset` precedent from Story 1.3) — confirmed empirically, not hand-guessed.
- Added `internal/adapter/api/transactions.go`: reads `params.PageSize`/`params.Cursor` (nil → zero value, letting the use case's own policy apply), maps every use-case error to its documented HTTP status, and omits `nextCursor` from the response when there is no further page.
- Extended `customerServer`/`NewServerInterface` with `listTransactions`, and wired `postgres.NewTransactionRepository(pool)` + `core.NewListCustomerTransactions` into `cmd/walletd/main.go`'s composition root.
- Extended `internal/adapter/api/integration_test.go` with `TestListCustomerTransactions_EndToEnd` (real Postgres via `testcontainers-go`) covering all 8 ACs, plus `getTransactions` and `transactionsResponseBody` test helpers. The pagination test (AC3) deliberately reads `dest`'s history rather than `source`'s, since `source` also carries the `creditAccount` funding fixture (itself a live, incidental demonstration that AC4's genericity holds for a cause type — `test_fixture` — the endpoint has never heard of).
- Skipped the optional `transaction_repo_test.go` (Task 5) — the integration suite already exercises the repository's existence check, cursor codec, and pagination against real Postgres.

## Dev Agent Record

### Agent Model Used

claude-opus-4-8

### Debug Log References

None beyond the red/green test runs recorded in Completion Notes. The cursor-parameter bind failure (empty string bound as `uuid`) was caught on the first real-Postgres run of `TestListCustomerTransactions_EndToEnd` and fixed immediately — see Change Log.

### Completion Notes List

- TDD followed for the use-case layer: `internal/core/list_customer_transactions_test.go` was written first and confirmed failing to compile (red — `core.TransactionPage`, `core.Transaction`, `core.NewListCustomerTransactions`, `core.ErrInvalidPageSize` all undefined) before `customer.go`, `ports.go`, and `list_customer_transactions.go` were implemented (green). The Postgres repository, OpenAPI regeneration, and HTTP handler layers were then implemented directly and validated via the real-Postgres integration suite (`TestListCustomerTransactions_EndToEnd`), consistent with Stories 1.2/1.3's own precedent — these layers require a live database/generator run to exercise meaningfully.
- Full regression suite green: `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), `go test ./...` — including the real-Postgres integration tests (`testcontainers-go`, `postgres:18`) for `TestCreateCustomer_EndToEnd`, `TestGetCustomerBalances_EndToEnd`, and `TestCreateTransfer_EndToEnd` (all unchanged, still passing after the `NewServerInterface` signature change) and the new `TestListCustomerTransactions_EndToEnd` (all 7 AC subtests passed after one fix — see below).
- `oapi-codegen` v2.7.2 installed locally (`go install .../oapi-codegen@v2.7.2`, matching Stories 1.1–1.3's pinned version) and used to regenerate `server.gen.go` for real; confirmed the generated `ListCustomerTransactionsParams` and `TransactionChain`/`TransactionAsset` names empirically rather than assuming them.
- The AC3 pagination test's first run surfaced a real bug (a `uuid`-typed bind parameter receiving an empty string when no cursor was supplied) — fixed by binding `uuid.Nil` as the placeholder instead of `""` when `hasCursor` is false. The AC1/AC3 tests' first runs also surfaced a test-design issue, not an implementation bug: the `creditAccount` funding fixture (`cause_type = 'test_fixture'`) legitimately appears in `source`'s own history (correctly proving AC4's genericity), so AC1 was adjusted to find the specific `internal_transfer` entry rather than assume the list has exactly one item, and AC3 was adjusted to paginate over `dest` (never touched by the funding fixture) for a clean, exact count.
- No new dependencies added — `math/big`, `github.com/google/uuid`, `github.com/jackc/pgx/v5`, `encoding/base64`, and `time` were all already in use or standard library.

### File List

**New files:**
- `internal/core/list_customer_transactions.go`
- `internal/core/list_customer_transactions_test.go`
- `internal/adapter/postgres/transaction_repo.go`
- `internal/adapter/api/transactions.go`

**Modified files:**
- `internal/core/customer.go` (added `Transaction`, `TransactionPage`, `ErrInvalidCursor`, `ErrInvalidPageSize`)
- `internal/core/ports.go` (added `TransactionRepository`)
- `api/openapi.yaml` (added `GET /customers/{id}/transactions`, `Transaction`, `TransactionsResponse`)
- `internal/adapter/api/server.gen.go` (regenerated — do not hand-edit)
- `internal/adapter/api/customers.go` (extended `customerServer`/`NewServerInterface` with `listTransactions`)
- `internal/adapter/api/integration_test.go` (updated `newTestHandler` wiring; added `TestListCustomerTransactions_EndToEnd` and its helpers)
- `cmd/walletd/main.go` (wired `TransactionRepository` + `ListCustomerTransactions`)
- `_bmad-output/implementation-artifacts/sprint-status.yaml` (status transitions for this story)

## Review Findings

Adversarial code review (Blind Hunter + Edge Case Hunter + Acceptance Auditor), 2026-07-15. Scope: story 1.4 file list only (Makefile / docker-compose swagger-ui removal excluded as out-of-scope).

- [x] [Review][Patch] AC7 — a well-formed but tampered / cross-customer cursor is accepted, not rejected — `decodeCursor` (`transaction_repo.go:46-67`) validates only base64/shape/timestamp/uuid form. A forged cursor `base64(<any valid RFC3339 time>|<any valid uuid>)`, or a genuine cursor minted for customer B replayed against customer A, decodes cleanly and is used as the keyset origin (`:109`), returning `200` with a shifted page instead of the `400` AC7 demands for "tampered … or from a different customer." No data leak (query stays scoped by `a.customer_id = $1`), but it contradicts AC7's literal text. **Resolved (decision → patch): harden the cursor** — add an HMAC signature over the payload and bind the customer id into it; `decodeCursor` verifies the MAC + customer match and returns `ErrInvalidCursor` otherwise.
- [x] [Review][Patch] Keyset key `(created_at, je.id)` is not unique per result row — latent AC3/AC4 gap — the query emits one row per `(journal_entry, posting)` but the cursor/order key is only `(je.created_at, je.id)` (`transaction_repo.go:104-153`). A future cause type that writes ≥2 postings on one customer's own accounts within a single journal entry produces rows sharing an identical `(created_at, je.id)`; across a page boundary the resume predicate `(je.created_at, je.id) < ($3,$4)` drops the sibling (gap) and/or repeats an id (dup), breaking AC3 exactly when AC4's "future cause types appear automatically" is exercised. Not triggerable today (`internal_transfer` = one posting per customer). **Resolved (decision → patch): harden the key now** — include the posting id in the `ORDER BY` and cursor so the keyset is unique per row regardless of postings-per-entry.
- [x] [Review][Patch] AC8 — explicit `?pageSize=0` returns `200` + default 20 instead of `400` [internal/core/list_customer_transactions.go:30] — AC8 explicitly lists "zero" as a reject-with-400 case. An *absent* param binds to `nil`, but an *explicit* `?pageSize=0` binds to `&0` (confirmed against oapi-codegen runtime `BindQueryParameterWithOptions`), which the handler collapses to `0` (`transactions.go:19-22`) and the use case treats as "omitted → default 20." The Dev-Note claim that "0 is indistinguishable from omitted at the wire level" is factually incorrect. Fix: distinguish omitted from explicit-0 (e.g. thread `*int` through `Execute`) so explicit `0` → `ErrInvalidPageSize`.
- [x] [Review][Patch] AC3 — pagination test under-asserts ordering [internal/adapter/api/integration_test.go] — the AC3 subtest asserts only non-ascending `createdAt` (ties allowed) and does not assert the strict `(created_at, id)` total order the architectural decision names as the anti-skip/repeat guarantee. Gap/dup coverage (exact count + uniqueness map) is solid; strengthen the ordering assertion to include the `id` tiebreak.
- [x] [Review][Defer] Raw internal error text leaked in problem+json `detail` on 500 [internal/adapter/api/transactions.go:40] — deferred, pre-existing (same platform-wide item already logged in the 1-2 and 1-3 reviews).
- [x] [Review][Defer] No composite index on `journal_entries(created_at, id)` backing the pagination sort [internal/adapter/postgres/transaction_repo.go:110] — deferred, pre-existing (story deliberately adds no migration; only `idx_postings_account_id` exists, so sort cost grows with total matched history, not page size).

Dismissed as noise (2): `Status: "completed"` constant — by design, extensively documented in the story's "Architectural decisions" as correct for today's only cause type and explicitly bounded to a future epic; shared static-token auth / no per-customer authorization — systemic across all routes, not introduced by this change (already tracked in deferred-work from the 1-2 review).
