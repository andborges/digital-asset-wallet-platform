---
baseline_commit: b81a67897efc158c16eecdbe551f39b3293e4942
---

# Story 1.3: Ledger-Only Internal Transfer Between Customers

Status: done

<!-- Note: Validation is optional. Run validate-create-story for quality check before dev-story. -->

## Story

As an application team,
I want to move balance from one customer to another without any on-chain transaction,
so that I can support customer-to-customer transfers instantly and at zero gas cost.

## Acceptance Criteria

1. **Given** two customers each with a provisioned account for the same (chain, asset) pair, and the source account's derived balance covers the amount, **when** I POST `/v1/transfers` with an `Idempotency-Key`, `sourceCustomerId`, `destinationCustomerId`, `chain`, `asset`, and `amount`, **then** a balanced journal entry (debit source, credit destination) commits atomically and both balances update immediately. [FR4, AD-3, AD-4]
2. **Given** the source account's derived balance is less than the requested amount, **when** the transfer is submitted, **then** it is rejected with a 422 `problem+json` response and no postings are written.
3. **Given** the same `Idempotency-Key` is replayed with the same request body, **when** processed again, **then** the original response is returned and the balances are not moved a second time. [FR24]
4. **Given** a transfer completes, **when** the journal is inspected, **then** the entry's `cause_type` is exactly `internal_transfer` and its `cause_id` is the caller's idempotency key — no other code path can produce a second entry for the same cause. [FR5]
5. **Given** a mutating request without an `Idempotency-Key` header, **when** processed, **then** it is rejected with a 400 `problem+json` response and no side effects occur (standing IdempotencyMiddleware behavior, unchanged by this story).
6. **Given** a request to this endpoint without a valid bearer token, **when** processed, **then** the platform rejects it with a 401 response — this route is not an exception to NFR15/AD-14's no-anonymous-surface rule.
7. **Given** a `sourceCustomerId` or `destinationCustomerId` that has no account for the requested (chain, asset), **when** submitted, **then** the platform returns a 404 `problem+json` response and no postings are written.
8. **Given** `sourceCustomerId` equals `destinationCustomerId`, or `amount` is zero/negative, **when** submitted, **then** the platform rejects it with a 400 `problem+json` response before any account lookup or lock is attempted.
9. **Given** a `chain` or `asset` value that is not one of the supported enum members, **when** submitted, **then** the platform rejects it with a 400 `problem+json` response — not a 404, which would otherwise be indistinguishable from a genuinely unknown customer.

**Architectural decisions this story must make (not explicit in the epics AC text, but required to satisfy AC1 and the existing schema honestly):**

- **Transfers are scoped to a single (chain, asset) pair, on both sides — `chain` is a required request field even though epics.md's literal AC text lists only "source, destination, asset, and amount."** Story 1.1 provisions four accounts per customer, one per (chain, asset) pair (`UNIQUE (customer_id, chain, asset)` in `0001_create_customers_and_accounts.sql`) — there is no single "the ETH account," only "the Base ETH account" and "the Arbitrum ETH account." The architecture's account-taxonomy table describes "customer available" balance as "per customer × asset," but that prose predates and does not override the schema Stories 1.1/1.2 actually shipped, which is chain-scoped. `0001_create_customers_and_accounts.sql` itself flags this exact question as "a known decision point in Story 1.3, not a surprise" — this is that resolution: **do not attempt cross-chain balance aggregation.** A transfer debits the source's (chain, asset) account and credits the destination's (chain, asset) account — the same pair on both sides. This keeps the transfer a strict two-row operation against the existing schema, requires no migration, and stays consistent with forwarder-float being explicitly per-chain (AD-3) — merging customer balances across chains would be a much larger, unrequested redesign.
- **Two-account locking must happen in one deterministic-order statement, or opposite-direction concurrent transfers can deadlock.** A transfer from customer A to B and a concurrent transfer from B to A, if each locked "its own source first," would deadlock (classic lock-ordering hazard). The fix: lock both accounts in a single `SELECT ... FOR UPDATE ORDER BY id` statement keyed on the same `(chain, asset, customer_id IN (...))` shape for every transfer — see Dev Notes → Locking. This single-statement approach also closes the balance-check race: holding the source row's lock for the remainder of the transaction means a concurrent transfer against the same source account blocks until this one commits or rolls back, so two concurrent transfers can never both read a stale balance and jointly overdraw the account. **This resolves the deferred-work.md item "Negative derived balance surfaced with no guard... decide on a non-negative floor... before rows are written"**: the guard is this story's FOR-UPDATE-locked pre-check, not a database `CHECK` constraint (a per-row `CHECK` can't express "the derived SUM across all this account's postings stays ≥ 0" — only a trigger could, and that's disproportionate machinery for a single-writer-per-story invariant). Any future story that also debits "customer available" (e.g. Epic 3's withdrawal hold) must take the same `FOR UPDATE` lock on the account row before checking sufficiency — that discipline, not a DB constraint, is what keeps balances non-negative as more writers are added.
- **A rare duplicate-cause race is handled explicitly, not left to crash as a raw 500.** `IdempotencyMiddleware`'s own dedup (the `idempotency_keys` table) only resolves *after* the wrapped handler returns — two requests with an identical `Idempotency-Key` arriving close enough together both pass `store.Lookup` as "not found" and both begin their own transaction, both calling `CreateTransfer` inside their own uncommitted transaction. The `journal_entries` `UNIQUE (cause_type, cause_id)` constraint (AD-3, AD-5) is what actually prevents the double-write here: the second transaction's `INSERT` blocks until the first commits, then fails with a unique-violation. Map that specific failure to a 409 `problem+json` response (do not attempt to replay the winner's exact response — that would require this repository to read the `idempotency_keys` store, a different port, and is unwarranted machinery for a sub-millisecond race). The caller can safely retry; a retry lands after the winner's commit, hits `IdempotencyMiddleware`'s own `store.Lookup` path, and gets the original result. No balances are ever moved twice either way (FR24 holds), which is the actual invariant that matters.
- **No new migration.** `0003_create_journal_entries_and_postings.sql` (Story 1.2) already created `journal_entries` and `postings`, empty, exactly for this story to be the first writer. Do not add a new migration for these tables.

## Tasks / Subtasks

- [x] **Task 1: Ledger domain types & port** (AC: 1, 2, 4, 7, 8)
  - [x] `internal/core/customer.go`: add `ErrInsufficientBalance` and `ErrDuplicateTransferCause` sentinel errors (alongside the existing `ErrCustomerNotFound`); add `TransferRequest` (`SourceCustomerID`, `DestinationCustomerID string`; `Chain Chain`; `Asset Asset`; `Amount *big.Int`; `IdempotencyKey string` — becomes the journal entry's `cause_id`, FR5) and `Transfer` (`ID string` — the journal entry id; `SourceCustomerID`, `DestinationCustomerID string`; `Chain Chain`; `Asset Asset`; `Amount *big.Int`; `CreatedAt time.Time`) domain types.
  - [x] `internal/core/ports.go`: add `TransferRepository` interface with one method, `CreateTransfer(ctx context.Context, req TransferRequest) (Transfer, error)` — doc-comment it exactly like `CustomerRepository`: implementations run inside the transaction already open on `ctx` (opened by `IdempotencyMiddleware`), never their own.
  - [x] `internal/core/create_transfer.go`: `CreateTransfer` use case — constructor `NewCreateTransfer(repo TransferRepository)`, `Execute(ctx, req TransferRequest) (Transfer, error)`. Validate `ErrSelfTransfer` (source == destination) and `ErrNonPositiveAmount` (`req.Amount == nil || req.Amount.Sign() <= 0`) **before** calling the repository (AC8) — these are ledger-domain invariants, not persistence concerns, and checking them here means `TransferRepository` never has to handle "both ids are actually the same account" as a locking edge case. Define `ErrSelfTransfer` and `ErrNonPositiveAmount` in this same file.
- [x] **Task 2: Postgres TransferRepository — deadlock-safe locking + balance guard** (AC: 1, 2, 4, 7)
  - [x] `internal/adapter/postgres/transfer_repo.go`: `TransferRepository` (no pool field — tx-from-context, like `CustomerRepository`, since this serves a mutating POST). Constructor `NewTransferRepository()`.
  - [x] `CreateTransfer` implementation, in order, all inside `txFromContext(ctx)`:
    1. Lock + resolve both accounts in one statement: `SELECT id, customer_id FROM accounts WHERE chain = $1 AND asset = $2 AND customer_id = ANY($3::uuid[]) ORDER BY id FOR UPDATE`, with `$3` = `[]string{req.SourceCustomerID, req.DestinationCustomerID}`. Build a `map[customerID]accountID` from the result. Missing source or destination in the map → `fmt.Errorf("%w: ...", core.ErrCustomerNotFound)`.
    2. Sum the source account's postings **within this same transaction** (`tx.QueryRow`, not the pool): `SELECT COALESCE(SUM(amount), 0)::text FROM postings WHERE account_id = $1`, parse via `big.Int.SetString` exactly like `BalanceRepository` (Story 1.2 precedent). If derived balance < `req.Amount` → `core.ErrInsufficientBalance`, no writes.
    3. Insert the journal entry: `INSERT INTO journal_entries (id, cause_type, cause_id, created_at) VALUES ($1, 'internal_transfer', $2, $3)` with a fresh UUIDv7 id and `cause_id = req.IdempotencyKey`. On a unique-violation (`pgconn.PgError.Code == pgUniqueViolation`, the existing `pgUniqueViolation` constant in `idempotency_store.go`), return `core.ErrDuplicateTransferCause` wrapped with the key.
    4. Insert two postings in one `pgx.Batch` (mirroring `CustomerRepository.CreateCustomer`'s batch pattern): source account `amount = -req.Amount`, destination account `amount = +req.Amount`, both referencing the new journal entry id. **Bind the amount parameter with an explicit `::numeric` cast in the SQL text** (`VALUES ($1, $2, $3, $4::numeric, $5)`), passing `amount.String()` as a plain Go string — this sidesteps pgx v5 guessing the wrong wire format for a `NUMERIC(78,0)` column from a bare string parameter, the mirror image of this codebase's existing `::text` cast trick used on the `SELECT` side (`balance_repo.go`). **Confirm this against a real test insert before relying on it** — Story 1.2 only ever *read* this column, so there is no existing precedent for the *write* direction; if `::numeric` binding doesn't work as expected with the installed pgx v5 version, the fallback is `pgtype.Numeric`, not floats or `int64` (money convention is non-negotiable regardless of which binding mechanism wins).
    5. Return the constructed `core.Transfer`.
- [x] **Task 3: OpenAPI spec — POST /transfers** (AC: 1, 2, 3, 5, 6, 7, 8)
  - [x] `api/openapi.yaml`: new path `/transfers`, `operationId: createTransfer`, `Idempotency-Key` parameter (existing `#/components/parameters/IdempotencyKey` ref), JSON request body `TransferRequest`, responses `201` (`Transfer`), `400`, `401`, `404`, `409`, `422`, `500` — all error responses `application/problem+json` / `ProblemDetails`.
  - [x] New schemas: `TransferRequest` (`sourceCustomerId`, `destinationCustomerId`: uuid strings; `chain`: enum `[base, arbitrum]`; `asset`: enum `[eth, usdc]`; `amount`: string, all required) and `Transfer` (`id`, `sourceCustomerId`, `destinationCustomerId`, `chain`, `asset`, `amount`, `createdAt`, all required). `amount` **must** be `type: string`, matching `Balance`'s existing precedent (Story 1.2) — never a JSON number.
  - [x] Regenerate `internal/adapter/api/server.gen.go` via `oapi-codegen` v2.7.2 (same version pinned by Stories 1.1/1.2). This is `std-http-server` (non-strict) mode: unlike path/header params, the request body is **not** auto-decoded into the handler signature — expect `CreateTransfer(w http.ResponseWriter, r *http.Request, params CreateTransferParams)` with only `IdempotencyKey` in `params`, and decode the JSON body yourself in the handler using the generated `TransferRequest` model type. **Confirm the exact generated type/enum names** (e.g. whether the generator names the new enums `TransferRequestChain`/`TransferRequestAsset` and `TransferChain`/`TransferAsset`, independent of the existing `BalanceChain`/`BalanceAsset`) against the real generated output — do not hand-guess, per Story 1.2's own established practice. Do not hand-edit `server.gen.go`.
- [x] **Task 4: Handler & wiring** (AC: 1, 2, 3, 4, 5, 6, 7, 8, 9)
  - [x] `internal/adapter/api/transfers.go` (new file, following `customers.go`'s shape): implement `CreateTransfer(w, r, params CreateTransferParams)` on `customerServer` (extend the existing struct — it already documents "later stories add their own use cases here as this service grows"). Steps, in order: decode `r.Body` into the generated `TransferRequest` model (JSON decode failure → 400 `invalid-transfer-request`); **call `.Valid()` on the decoded `Chain` and `Asset` enum values and reject with 400 `invalid-chain-or-asset` if either is false (AC9)** — this is the first story where these enums are externally supplied rather than internally generated from `SupportedChainAssetPairs` (unlike Stories 1.1/1.2, where a `.Valid()` check would have been unreachable dead code), so skipping it here would let a bogus value silently fall through to a misleading 404 instead of a clear 400; parse `amount` via `big.Int.SetString(body.Amount, 10)` (parse failure → 400 `invalid-amount`); build `core.TransferRequest` with `IdempotencyKey: r.Header.Get("Idempotency-Key")` (the header is still present and unmodified on `r` when this handler runs — `IdempotencyMiddleware` reads it but never strips it); call `s.createTransfer.Execute(r.Context(), req)`; map errors: `errors.Is(err, core.ErrSelfTransfer)` or `core.ErrNonPositiveAmount` → 400, `core.ErrCustomerNotFound` → 404, `core.ErrInsufficientBalance` → 422, `core.ErrDuplicateTransferCause` → 409, anything else → 500; on success, 201 + JSON-encoded `Transfer` with `Amount` rendered via `.String()`.
  - [x] Extend `customerServer` struct and `NewServerInterface` constructor to also take `createTransfer *core.CreateTransfer`, mirroring how `getBalances` was added in Story 1.2.
  - [x] `cmd/walletd/main.go`: construct `postgres.NewTransferRepository()`, `core.NewCreateTransfer(transferRepo)`, and pass into the updated `NewServerInterface(createCustomer, getBalances, createTransfer)` call. No middleware changes needed — `AuthMiddleware` and `IdempotencyMiddleware` already cover every route and every mutating method unconditionally.
- [x] **Task 5: Tests**
  - [x] `internal/core/create_transfer_test.go`: table-driven unit tests against a fake `TransferRepository` — self-transfer rejected before the repo is ever called (assert the fake's `CreateTransfer` was not invoked), non-positive amount (zero and negative) rejected before the repo is called, and successful/`ErrCustomerNotFound`/`ErrInsufficientBalance`/`ErrDuplicateTransferCause` all pass through from the fake unchanged.
  - [x] `internal/adapter/api/integration_test.go` (extend, real Postgres via `testcontainers-go` + `postgres:18`, no mocked repository — same rigor as Stories 1.1/1.2): create two customers via the existing `createTestCustomer` helper, credit the source's chosen (chain, asset) account by inserting a fixture journal entry + posting directly via test SQL (same technique Story 1.2 used to prove `AC4`'s derivation query was real). Cover: AC1 (successful transfer, both balances update, verified via the balances endpoint), AC2 (amount exceeding balance → 422, and assert the `postings` row count is unchanged afterward), AC3 (replay same key + body → same result, balances move only once), AC4 (query `journal_entries` directly: exactly one row with `cause_type = 'internal_transfer'` and `cause_id` = the idempotency key used), AC5 (missing `Idempotency-Key` → 400, already generic but worth one assertion on this route), AC6 (missing bearer token → 401), AC7 (unknown `sourceCustomerId` and, separately, unknown `destinationCustomerId` → 404), AC8 (`sourceCustomerId == destinationCustomerId` → 400; zero and negative `amount` → 400), AC9 (an unsupported `chain` or `asset` string → 400, not 404).
  - [ ] `internal/adapter/postgres/transfer_repo_test.go` — optional (per Story 1.2's precedent; `integration_test.go` may cover this sufficiently, dev agent's call). **Skipped**: the `integration_test.go` suite exercises `TransferRepository` end-to-end (including the locking and balance-guard paths) against real Postgres, satisfying this option's own "may cover this sufficiently" clause. A dedicated concurrency test proving the `FOR UPDATE` lock serializes under real goroutine contention was considered but left to Story 6.3's consolidated fault-injection suite, which is where this codebase's concurrency/crash-recovery claims are systematically exercised rather than piecemeal per story.

## Review Findings

Adversarial code review (2026-07-15) — Blind Hunter + Edge Case Hunter + Acceptance Auditor, triaged. 1 decision-needed, 2 patch, 3 defer, 12 dismissed as noise/by-design.

- [x] [Review][Decision→Defer] Concurrency (deadlock) regression test for the two-account lock — RESOLVED (2026-07-15): accept the spec's deferral to Story 6.3's consolidated fault-injection suite. The `SELECT … ORDER BY id FOR UPDATE` follows the correct, standard deadlock-avoidance pattern (assessed correct); a live opposite-direction regression test belongs to 6.3 where concurrency is exercised systematically. The comment reword is retained as a patch (below). [internal/adapter/postgres/transfer_repo.go:34-45]
- [x] [Review][Patch] Reword the locking comment to cite the plan structure, not scan order [internal/adapter/postgres/transfer_repo.go:34-40] — APPLIED: comment now explains that FOR UPDATE row locks are acquired by the `LockRows` node above the `Sort`, so `ORDER BY id` governs lock order, and calls out that dropping the `ORDER BY` reintroduces deadlock risk.
- [x] [Review][Patch] Missing required body-field validation yields a misleading 404 instead of 400 [internal/adapter/api/transfers.go:22] — APPLIED: the handler now rejects a `uuid.Nil` sourceCustomerId/destinationCustomerId with a 400 `invalid-transfer-request` before the account lookup.
- [x] [Review][Patch] AC7 integration subtest omits the AC-required "no postings are written" assertion [internal/adapter/api/integration_test.go:375] — APPLIED: the AC7 subtest now snapshots `postingsCount` before/after and asserts it is unchanged after both 404s.
- [x] [Review][Defer] Error responses echo `err.Error()` into problem `detail` [internal/adapter/api/transfers.go:55-70] — deferred, pre-existing platform-wide convention (identical in `CreateCustomer`/`GetCustomerBalances`); already tracked in deferred-work.md from the 1-2 review. 500s leak wrapped SQL context; not a 1.3 regression.
- [x] [Review][Defer] Amount exceeding NUMERIC(78,0) surfaces as a 500, not a clean 400 [internal/adapter/postgres/transfer_repo.go:118] — deferred, practically unreachable (the balance check returns 422 first unless the source is over-funded); no upper bound is enforced anywhere.
- [x] [Review][Defer] journal_entries unique-violation is mapped to ErrDuplicateTransferCause without checking the constraint name [internal/adapter/postgres/transfer_repo.go:99-104] — deferred, only the `cause_id` constraint is realistically hit (the PK is a fresh UUIDv7); a future added unique constraint would be misreported as a 409.

## Dev Notes

### Architecture patterns and constraints this story must follow

- **Chain-scoped transfers, no migration** — see "Architectural decisions" above; this is the load-bearing resolution of a gap Story 1.2 deliberately left open.
- **Locking (the riskiest wiring decision in this story).** Both accounts must be locked in one `SELECT ... FOR UPDATE ORDER BY id` statement, never as two separate `FOR UPDATE` calls — two separate calls can deadlock under opposite-direction concurrent transfers (A→B racing B→A), while one statement locking rows in a fixed order (by `id`) can't, because every transfer's lock statement has the same shape and Postgres locks the rows it scans in that same order regardless of which transfer issued it. This single lock, held for the rest of the transaction, is also what makes the balance check race-free: a second transfer against the same source account cannot even read the balance until the first transaction commits or rolls back.
- **Double-entry ledger, one transaction per state change (AD-3, AD-4).** The journal entry and both its postings commit in the exact same Postgres transaction as everything else in this request — no separate "read balance" step outside the transaction, unlike `BalanceRepository`'s deliberately poolless read path (Story 1.2). This repository is mutating, so — like `CustomerRepository` — it must use `txFromContext(ctx)`, never its own pool.
- **Idempotency by unique constraint, twice over (AD-5).** `IdempotencyMiddleware`'s `idempotency_keys` table is the primary, HTTP-level dedup (unchanged, reused as-is). `journal_entries`' own `UNIQUE (cause_type, cause_id)` is a second, deeper guarantee that this story's AC4 explicitly requires being tested, and the two can race against each other in the way "Architectural decisions" describes — handle the resulting unique-violation explicitly (409), do not let it surface as an unhandled 500.
- **Money convention.** Integer base units only, `NUMERIC(78,0)` in Postgres, `*big.Int` in Go, `string` (never a JSON number) at the API boundary — unchanged from Story 1.2, now exercised on the write side for the first time.
- **Hexagonal boundary (AD-1, AD-2).** `internal/core` still imports nothing from `internal/adapter/*`. `TransferRepository`'s port lives in `core`; its Postgres implementation lives in `adapter/postgres`, same shape as `CustomerRepository`.
- **No `AccountRepository` — still not needed.** Story 1.2 explicitly declined to generalize a standalone account repository for a single read method; this story's account-locking query is similarly narrow (transfer-specific `FOR UPDATE` + customer-id lookup) and does not warrant inventing one either.
- **Auth applies unconditionally (NFR15, AD-14).** No exception for this route.
- **`BalanceRepository`'s existence-check-then-SUM TOCTOU (flagged in `deferred-work.md` against this story) stays deferred, explicitly.** Now that this story adds a concurrent writer, a `GetCustomerBalances` read could observe the customer's accounts mid-transfer, with its two internal queries reflecting slightly different instants. This is ordinary read skew, not a correctness bug — the ledger itself never derives from that read path, only the HTTP response does, and no financial decision downstream depends on that response being from a single atomic instant. Folding it into one round-trip (CTE or `RIGHT JOIN`) remains a nice-to-have polish item, not something this story needs to implement.

### Source tree components this story creates or touches

```text
digital-asset-wallet-platform/
  internal/core/
    customer.go                # MODIFY — add ErrInsufficientBalance, ErrDuplicateTransferCause, TransferRequest, Transfer
    ports.go                    # MODIFY — add TransferRepository interface
    create_transfer.go          # NEW — the use case, ErrSelfTransfer, ErrNonPositiveAmount
    create_transfer_test.go     # NEW
  internal/adapter/
    api/
      server.gen.go             # MODIFY (regenerated) — do not hand-edit
      transfers.go               # NEW — CreateTransfer handler
      customers.go               # MODIFY — extend customerServer + NewServerInterface with createTransfer
      integration_test.go        # MODIFY — extend with transfer ACs
    postgres/
      transfer_repo.go           # NEW — tx-from-context, deadlock-safe two-account lock + balance guard
      transfer_repo_test.go      # NEW (optional — see Task 5)
  api/openapi.yaml               # MODIFY — new path + TransferRequest/Transfer schemas
  cmd/walletd/main.go              # MODIFY — wire TransferRepository + CreateTransfer
```

No new migration file, no new files under `internal/adapter/evm/`, `internal/adapter/signer/`, `internal/adapter/webhook/`, or `contracts/` — those remain out of scope until Epics 2–4.

### Schema (reused, not created)

This story writes to the tables Story 1.2 already created in `0003_create_journal_entries_and_postings.sql` — `journal_entries` (`UNIQUE (cause_type, cause_id)`) and `postings` (`NUMERIC(78,0)`, FK to `accounts`). Story 1.3 is their first writer, exactly as that migration's comments anticipated. Do not add a new migration.

### Config

No new environment variables.

### Testing standards

- Table-driven Go tests for the use case, isolated with a fake `TransferRepository` (unchanged pattern from Stories 1.1/1.2).
- Integration test against real Postgres (`testcontainers-go`, `postgres:18`) — no mocked repository, per the project's rigor thesis (PRD Success Metric 5) and Story 1.2's precedent. This is the only way to genuinely prove the `FOR UPDATE` locking and unique-constraint behavior.
- A true concurrent-overdraw regression test is optional here (Task 5) — the consolidated fault-injection suite (Story 6.3) is where NFR1 gets its full, systematic treatment across every mutating flow, not just this one.

### Project Structure Notes

- No conflicts with the existing structure — this story extends `internal/core`, `internal/adapter/{api,postgres}`, `api/openapi.yaml`, and `cmd/walletd/main.go`, all files Stories 1.1/1.2 already established the shape of. Read each MODIFY-marked file above in full before changing it — do not guess at `customers.go`'s, `main.go`'s, or `ports.go`'s current contents from memory.
- `TransferRepository` follows `CustomerRepository`'s tx-from-context shape (mutating), not `BalanceRepository`'s pool-direct shape (non-mutating read) — pick the pattern that matches this port's actual read/write nature, per Story 1.2's own explicit guidance on this choice.

### References

- [Source: _bmad-output/planning-artifacts/epics.md#Epic 1: Foundation — Accounts, Ledger & Deposit Addresses / Story 1.3] — canonical ACs
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md#AD-3, AD-4, AD-5] — double-entry ledger, one-transaction-per-change, idempotency by constraint
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md#4. The money model] — account taxonomy, "internal transfers are the degenerate case: a single balanced journal entry between two customers' available accounts, idempotent on the caller's key, no chain involvement at all"
- [Source: internal/adapter/postgres/migrations/0001_create_customers_and_accounts.sql] — the per-(chain, asset) account schema and its own flagged note that Story 1.3 must reconcile the chain-scoping question
- [Source: _bmad-output/implementation-artifacts/deferred-work.md#Deferred from: code review of 1-2-query-customer-balances] — the negative-balance-guard item this story's locking design resolves
- [Source: _bmad-output/implementation-artifacts/1-2-query-customer-balances.md] — established repo/use-case/handler shapes, `WriteProblem` helper, tx-from-context vs pool-direct distinction, `::text`/`big.Int.SetString` amount-parsing precedent

### Previous story intelligence (from Story 1.2)

- **TDD throughout**: every source file had a corresponding test file written first (red) before implementation (green). Follow the same discipline here.
- **Adversarial code review is a standing convention**, not a one-off — expect a fresh-context review pass (Blind Hunter + Edge Case Hunter + Acceptance Auditor) before this story is marked done, same as 1.1 and 1.2.
- **Repository shape is a deliberate choice, not a default**: `CustomerRepository` (mutating, tx-from-context) vs. `BalanceRepository` (non-mutating, pool-direct) diverge on purpose. `TransferRepository` is mutating, so it follows `CustomerRepository`'s shape.
- **`::text` cast + `big.Int.SetString` is the established read-side amount pattern** (`balance_repo.go`); this story introduces the write-side counterpart (`::numeric` cast + `.String()`) — confirm it works the same way empirically rather than assuming symmetry.
- **`ErrorHandlerFunc` in `main.go` already routes generator-level errors (malformed path/header params) through `WriteProblem`** — confirmed for `id` path params in 1.2; the same wiring should apply to `CreateTransfer`'s `Idempotency-Key` header param without new work, but a decode failure on the *JSON body* is this handler's own responsibility (the generator never touches the body in `std-http-server` mode), unlike header/path params.
- No dependency additions expected — `math/big`, `github.com/google/uuid`, `github.com/jackc/pgx/v5` are all already in use.

## Change Log

- Implemented all 9 ACs end-to-end: successful (chain, asset)-scoped internal transfer with atomic balance movement, 422 for insufficient balance with no postings written, byte-for-byte idempotent replay with balances moved exactly once, a verifiable `internal_transfer`/idempotency-key journal cause, 400 for a missing `Idempotency-Key` (generic, unchanged), 401 for a missing bearer token (generic, unchanged), 404 for an unknown source or destination customer, 400 for self-transfer and non-positive amounts, and 400 (not 404) for an unsupported `chain`/`asset` value.
- Resolved the chain-scoping ambiguity `0001_create_customers_and_accounts.sql` flagged for this story: transfers require an explicit `chain` field and move balance within the same (chain, asset) pair on both sides — no cross-chain aggregation, no new migration (reused Story 1.2's empty `journal_entries`/`postings` tables as their first writer).
- Added `core.TransferRepository` port, `core.TransferRequest`/`core.Transfer` domain types, and `core.ErrInsufficientBalance`/`core.ErrDuplicateTransferCause` sentinels alongside the existing `core.ErrCustomerNotFound`.
- Added `core.CreateTransfer` use case: validates `ErrSelfTransfer` and `ErrNonPositiveAmount` before ever touching the repository (AC8), then delegates.
- Added `postgres.TransferRepository`: locks both accounts in one `SELECT ... FOR UPDATE ORDER BY id` statement (deadlock-safe by construction — every transfer locks in the same relative order regardless of direction), sums the source account's postings inside the same held lock (race-free balance check), writes the journal entry (mapping a `journal_entries` unique-violation to `ErrDuplicateTransferCause` for the narrow concurrent-same-key race), then writes both postings in one `pgx.Batch` with an explicit `::numeric` cast on the amount parameter — confirmed empirically against real Postgres on the first integration-test run, no `pgtype.Numeric` fallback needed.
- Added `POST /v1/transfers` to `api/openapi.yaml` (`TransferRequest`, `Transfer` schemas; `amount` is `type: string`, matching `Balance`'s established convention). Regenerating via `oapi-codegen` v2.7.2 surfaced a real naming interaction: adding a second `chain`/`asset` enum pair caused the generator to rename `BalanceAsset`'s constants from bare `Eth`/`Usdc` to `BalanceAssetEth`/`BalanceAssetUsdc` (collision avoidance), while the new `TransferRequestAsset`/`TransferRequestChain` enums kept the bare `Eth`/`Usdc`/`Arbitrum`/`Base` names. No production code referenced the old bare constants directly (existing code always converts via `BalanceChain(...)`/`BalanceAsset(...)`), so nothing broke — but this confirms the story's own instruction to verify generated names empirically rather than hand-guess.
- Added `internal/adapter/api/transfers.go`: decodes the JSON body (this is the first mutating endpoint with one — `std-http-server` mode does not auto-decode it), validates `.Valid()` on the decoded `chain`/`asset` enums before any account lookup (AC9 — the first story where these enums are externally supplied rather than internally generated, so an invalid value must not silently fall through to a misleading 404), parses `amount` via `big.Int.SetString`, and maps every use-case error to its documented HTTP status.
- Extended `customerServer`/`NewServerInterface` with `createTransfer`, and wired `postgres.NewTransferRepository()` + `core.NewCreateTransfer` into `cmd/walletd/main.go`'s composition root alongside the existing use cases.
- Extended `internal/adapter/api/integration_test.go` with `TestCreateTransfer_EndToEnd` (real Postgres via `testcontainers-go`) covering all 9 ACs, plus `creditAccount`, `postTransfer`, `assertBalance`, and `postingsCount` test helpers reused across subtests.
- Skipped the optional `transfer_repo_test.go` (Task 5) — the integration suite already exercises the repository's locking and balance-guard paths against real Postgres; a dedicated concurrency/race test is left to Story 6.3's consolidated fault-injection suite.

## Dev Agent Record

### Agent Model Used

claude-sonnet-5

### Debug Log References

None — no debugging session artifacts beyond the red/green test runs recorded in Completion Notes. The `::numeric` cast and `ANY($3::uuid[])` bindings the story flagged as unverified assumptions both worked correctly on the first real-Postgres test run.

### Completion Notes List

- TDD followed for the use-case layer: `internal/core/create_transfer_test.go` was written first and confirmed failing to compile (red — `core.Transfer`, `core.TransferRequest`, `core.NewCreateTransfer`, and the new sentinel errors all undefined) before `customer.go`, `ports.go`, and `create_transfer.go` were implemented (green). The Postgres repository and HTTP handler layers were then implemented directly and validated via the real-Postgres integration suite (`TestCreateTransfer_EndToEnd`), consistent with how Story 1.2's own repository work was verified — these layers require a live transaction/database to exercise meaningfully, so isolated Go unit tests with fakes would not have caught the real risks (lock ordering, unique-constraint mapping, numeric binding).
- Full regression suite green: `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), `go test ./...` — including the real-Postgres integration tests (`testcontainers-go`, `postgres:18`) for `TestCreateCustomer_EndToEnd` and `TestGetCustomerBalances_EndToEnd` (both unchanged, still passing after the `NewServerInterface` signature change) and the new `TestCreateTransfer_EndToEnd` (all 9 AC subtests passed on the first run).
- `oapi-codegen` v2.7.2 installed locally (`go install .../oapi-codegen@v2.7.2`, matching Stories 1.1/1.2's pinned version) and used to regenerate `server.gen.go` for real.
- Verified the story's two flagged uncertainties directly against real behavior rather than assuming: (1) the `::numeric`-cast string-parameter binding for the `NUMERIC(78,0)` `postings.amount` column worked as designed on the first test run — no `pgtype.Numeric` fallback was needed; (2) the generated enum names for the new schemas are `TransferRequestChain`/`TransferRequestAsset` (bare `Base`/`Arbitrum`/`Eth`/`Usdc` constants) and `TransferChain`/`TransferAsset` (for the response), and — unexpectedly — regenerating also renamed `BalanceAsset`'s constants to `BalanceAssetEth`/`BalanceAssetUsdc` to avoid a naming collision; confirmed no existing code referenced the old bare names directly.
- No new dependencies added — `math/big`, `github.com/google/uuid`, `github.com/jackc/pgx/v5` were all already in use.

### File List

**New files:**
- `internal/core/create_transfer.go`
- `internal/core/create_transfer_test.go`
- `internal/adapter/postgres/transfer_repo.go`
- `internal/adapter/api/transfers.go`

**Modified files:**
- `internal/core/customer.go` (added `ErrInsufficientBalance`, `ErrDuplicateTransferCause`, `TransferRequest`, `Transfer`)
- `internal/core/ports.go` (added `TransferRepository`)
- `api/openapi.yaml` (added `POST /transfers`, `TransferRequest`, `Transfer`)
- `internal/adapter/api/server.gen.go` (regenerated — do not hand-edit)
- `internal/adapter/api/customers.go` (extended `customerServer`/`NewServerInterface` with `createTransfer`)
- `internal/adapter/api/integration_test.go` (updated `newTestHandler` wiring; added `TestCreateTransfer_EndToEnd` and its helpers)
- `cmd/walletd/main.go` (wired `TransferRepository` + `CreateTransfer`)
- `_bmad-output/implementation-artifacts/sprint-status.yaml` (status transitions for this story)
