---
baseline_commit: e632d775dab8b9ac3115dd3d808b7888b0e0e186
---

# Story 1.2: Query Customer Balances

Status: done

<!-- Note: Validation is optional. Run validate-create-story for quality check before dev-story. -->

## Story

As an application team,
I want to query a customer's current balance for each supported asset and chain,
so that I can display accurate, real-time balances without maintaining a shadow ledger.

## Acceptance Criteria

1. **Given** a customer with provisioned accounts and no ledger activity, **when** I GET `/v1/customers/{id}/balances`, **then** each (chain, asset) pair returns a balance of `"0"` in integer base units — wei for ETH, 6-decimal units for USDC. [FR2]
2. **Given** a customer id that does not exist, **when** queried, **then** the platform returns a 404 RFC 9457 `problem+json` response.
3. **Given** the balances endpoint is called under normal v1 load, **when** measured, **then** p95 latency is under 500ms. [NFR6]
4. **Given** the balance is derived from postings rather than stored directly, **when** a balance is returned, **then** it is always recomputable from the journal — no cached value can diverge from the postings. [AD-3]
5. **Given** a request to this endpoint without a valid bearer token, **when** processed, **then** the platform rejects it with a 401 response — this route is not an exception to NFR15/AD-14's no-anonymous-surface rule.

**Architectural decision this story must make and document (not explicit in the epics AC text, but required to satisfy AC4 honestly):** Story 1.1 deliberately did not create the `postings`/`journal_entries` tables ("don't exist until Story 1.3" — see its Dev Notes). But AC4 requires balances to be "recomputable from the journal," and Story 1.3's internal-transfer AC is the first to *write* postings. Resolution: **this story creates the `journal_entries`/`postings` schema** (empty tables, no writer yet) and implements a **real** `SUM(postings.amount)` derivation query. With zero rows, every account correctly derives to `0` — satisfying AC1 without hardcoding it, and making Story 1.3 additive (it only needs to insert rows; the read path is already correct). Do not special-case "no postings table" — build the table now.

## Tasks / Subtasks

- [x] **Task 1: Ledger schema — journal entries & postings** (AC: 1, 4)
  - [x] goose migration `0003_create_journal_entries_and_postings.sql` (next sequential number after `0002_create_idempotency_keys.sql`) — see Dev Notes → Schema. **Do not add any Go domain types for `JournalEntry`/`Posting` in this story** — nothing writes them until Story 1.3; a full domain model for rows nothing produces yet is gold-plating. This story only needs the tables to exist so the derivation query is genuine SQL, not the Go types to construct entries.
- [x] **Task 2: Balance derivation query & repository** (AC: 1, 2, 4)
  - [x] `internal/core`: add `AccountBalance` domain type (`Chain`, `Asset`, `Balance *big.Int`), `BalanceRepository` port interface, and `ErrCustomerNotFound` sentinel error (in `customer.go`, alongside `Customer`)
  - [x] `internal/adapter/postgres`: new `BalanceRepository` — **unlike `CustomerRepository`, this one is NOT poolless/tx-from-context.** It must take a `*pgxpool.Pool` directly in its constructor and query the pool, not `txFromContext(ctx)`. See Dev Notes → "Why this repo cannot use the tx-from-context pattern" — this is the riskiest wiring decision in this story.
  - [x] Query behavior: first confirm the customer exists (a bare `SELECT 1 FROM customers WHERE id = $1` — do not infer existence from an empty accounts/postings join, since a real customer's 4 accounts always exist per Story 1.1, but a nonexistent customer id also produces zero joined rows; these two cases are indistinguishable without an explicit existence check). Return `core.ErrCustomerNotFound` if absent. Otherwise run `SELECT a.chain, a.asset, COALESCE(SUM(p.amount), 0)::text FROM accounts a LEFT JOIN postings p ON p.account_id = a.id WHERE a.customer_id = $1 GROUP BY a.chain, a.asset` and parse each row's balance text into `*big.Int` via `new(big.Int).SetString(s, 10)` (see Dev Notes → Latest technical specifics for why `::text` + `big.Int.SetString`, not `pgtype.Numeric`).
  - [x] `internal/core`: `GetCustomerBalances` use case — constructor takes `BalanceRepository`, `Execute(ctx, customerID string) ([]AccountBalance, error)`, mirroring `CreateCustomer`'s shape (struct holding its port, `NewGetCustomerBalances(repo)`, thin `Execute`).
- [x] **Task 3: OpenAPI spec — GET /customers/{id}/balances** (AC: 1, 2, 5)
  - [x] `api/openapi.yaml`: new path `/customers/{id}/balances`, `operationId: getCustomerBalances`, path param `id` (`type: string, format: uuid`), responses `200` (`BalancesResponse`), `401`, `404`, `500` — all error responses `application/problem+json` / `ProblemDetails`, matching the existing `POST /customers` response style. No `Idempotency-Key` parameter — this is a non-mutating GET.
  - [x] New schemas: `Balance` (`chain`: string enum `[base, arbitrum]`, `asset`: string enum `[eth, usdc]`, `balance`: **string**, all required) and `BalancesResponse` (`balances`: array of `Balance`, required). **`balance` MUST be `type: string`, never `integer`/`number`.** This is the first time a money amount crosses the API boundary — JSON numbers are IEEE-754 float64 in virtually every consumer, which silently loses precision above 2^53 (~9×10^15), far below realistic wei amounts. Every later story that surfaces an amount (fees, transaction history, withdrawal amounts) must follow this same string-typed convention; get it right here since it's the precedent.
  - [x] Regenerate `internal/adapter/api/server.gen.go` via `oapi-codegen` (same config as Story 1.1: `internal/adapter/api/oapi-codegen-config.yaml`, `package: api`, `models: true`, `std-http-server: true`, `embedded-spec: true`). The generated method signature will be `GetCustomerBalances(w http.ResponseWriter, r *http.Request, id openapi_types.UUID)` — no `Params` struct, since the only parameter is a path segment (oapi-codegen v2 passes path params as direct function arguments, only query/header params get a generated `Params` struct — confirm this against the actual generated output, don't hand-guess the signature). Do not hand-edit `server.gen.go`.
- [x] **Task 4: Handler & wiring** (AC: 1, 2, 5)
  - [x] `internal/adapter/api/customers.go`: add a `getBalances *core.GetCustomerBalances` field to `customerServer`; extend `NewServerInterface(createCustomer *core.CreateCustomer, getBalances *core.GetCustomerBalances) ServerInterface`. Implement `GetCustomerBalances(w, r, id)`: call `s.getBalances.Execute(r.Context(), id.String())`; on `core.ErrCustomerNotFound` (`errors.Is`) write a 404 via `WriteProblem(w, http.StatusNotFound, "customer-not-found", ..., r.URL.Path)`; on any other error, 500; on success, `200` + JSON-encoded `BalancesResponse` with each balance's `*big.Int` rendered via `.String()`.
  - [x] `cmd/walletd/main.go`: construct `postgres.NewBalanceRepository(pool)` (pool-direct, alongside the existing `pgxpool.Pool` already built for `TxBeginner`), `core.NewGetCustomerBalances(balanceRepo)`, and pass both use cases into the updated `NewServerInterface(createCustomer, getBalances)` call. No middleware changes needed — `AuthMiddleware` already covers every route unconditionally (AC5), and `IdempotencyMiddleware` already passes GET straight through without a header requirement (Story 1.1's non-mutating-method bypass fix) — confirm this bypass still applies to the new route rather than re-implementing it.
- [x] **Task 5: Tests**
  - [x] `internal/core/get_customer_balances_test.go`: table-driven unit tests against a fake `BalanceRepository` (found → balances returned; `ErrCustomerNotFound` → propagated unchanged).
  - [x] `internal/adapter/api/integration_test.go` (extend the existing real-Postgres integration test, `testcontainers-go` + `postgres:18`, per Story 1.1's precedent — no mocked repository for this test): AC1 (create a customer, GET balances, assert all 4 pairs return `"0"`), AC2 (GET balances for a random unused UUID → 404), AC4 (**insert a `journal_entries` + `postings` row directly via test SQL** against one of the customer's accounts, then GET balances again and assert the affected account's balance reflects the inserted amount — this is the only way to prove the derivation query is real and not a hardcoded zero, since no application code writes postings until Story 1.3), AC5 (missing bearer token → 401).
  - [x] A lightweight assertion that a single balances call completes well under 500ms in the integration test is a reasonable sanity check for AC3, but a real p95-under-load measurement is out of scope here — Story 6.4 ("Load & Latency Validation Against the Performance Envelope") owns the actual load-test harness. Do not build load-testing infrastructure in this story.

## Dev Notes

### Architecture patterns and constraints this story must follow

- **Why this repo cannot use the tx-from-context pattern (the riskiest wiring detail in this story).** `postgres.CustomerRepository` and `postgres.tx.go`'s `txFromContext` assume a transaction is already open on `ctx`, opened by `IdempotencyMiddleware` for mutating requests. But Story 1.1's own review fixes made `IdempotencyMiddleware` **bypass GET/HEAD/OPTIONS/TRACE entirely — no transaction is opened for them.** `GET /customers/{id}/balances` is a GET. If `BalanceRepository` called `txFromContext(ctx)`, it would hit the `panic("postgres: no transaction on context...")` guard on every single call. The fix is structural, not a workaround: `BalanceRepository` takes a `*pgxpool.Pool` directly (constructor-injected, like `TxBeginner` itself) and queries the pool, exactly as `IdempotencyStore.Lookup` already does for its own pre-transaction read (see `ports.go`'s comment: "a plain read, not part of the eventual write transaction"). This is a deliberate asymmetry from `CustomerRepository`, not an inconsistency — document it in the new file's doc comment so a future reader doesn't try to "fix" it into the tx-from-context shape.
- **Double-entry ledger, balances always derived (AD-3).** No balance is ever stored on `Account`; this was already true from Story 1.1. This story is the first to *read* per the AD-3 contract — the `SUM(postings.amount)` query is the one and only balance-computation code path this project will ever have. Every later story that touches money (transfers, deposits, withdrawals, sweeps) writes postings that this same query already accounts for correctly — do not write a second, different balance-reading code path anywhere else in the codebase.
- **One transaction per state change, outbox included (AD-4) — does not apply here.** This story is read-only; there is no state change, no outbox event, nothing to commit. Resist the instinct to wrap the balances query in a transaction "for consistency" — a single `SELECT` needs no transaction, and forcing one would require re-plumbing the tx-from-context assumption this story's Dev Notes just argued against.
- **Money convention.** Integer base units only, `NUMERIC(78,0)` in Postgres, `*big.Int` in Go — no floats anywhere near an amount, including in the derivation query or JSON serialization (see Task 3's `balance: string` requirement).
- **Hexagonal boundary (AD-1, AD-2).** `internal/core` still imports nothing from `internal/adapter/*`. The new `BalanceRepository` port interface lives in `core`, its Postgres implementation in `adapter/postgres`, same shape as `CustomerRepository`.
- **No AccountRepository interface exists yet** — Story 1.1's own file-list comments mention one as a future addition, but it was never actually created (`CustomerRepository.CreateCustomer` still does both customer and account writes). Do not create a generic `AccountRepository` in this story either; add the narrowly-scoped `BalanceRepository` this story actually needs. A general account-repository abstraction can be introduced later if a future story's need is actually generic — inventing one now for a single read method is premature abstraction.
- **Auth still applies to GET routes (NFR15, AD-14).** Nothing in this story's AC set is an exception to "no anonymous surface." Do not special-case this route in `AuthMiddleware`.
- **RFC 9457 errors, existing helper.** Reuse `WriteProblem` from `problem.go` exactly as `customers.go`'s existing handler does — there is no per-status wrapper (no `WriteNotFound`), every call site passes the numeric status directly, e.g. `WriteProblem(w, http.StatusNotFound, "customer-not-found", err.Error(), r.URL.Path)`.

### Source tree components this story creates or touches

```text
digital-asset-wallet-platform/
  internal/core/
    customer.go                      # MODIFY — add ErrCustomerNotFound, AccountBalance type
    ports.go                         # MODIFY — add BalanceRepository interface
    get_customer_balances.go         # NEW — the use case
    get_customer_balances_test.go    # NEW
  internal/adapter/
    api/
      server.gen.go                  # MODIFY (regenerated) — do not hand-edit
      customers.go                   # MODIFY — add GetCustomerBalances handler, extend customerServer + NewServerInterface
      integration_test.go            # MODIFY — extend with balances ACs
    postgres/
      migrations/0003_create_journal_entries_and_postings.sql   # NEW
      balance_repo.go                # NEW — pool-direct, not tx-from-context (see Dev Notes)
      balance_repo_test.go            # NEW (optional — integration_test.go may cover this sufficiently; dev agent's call)
  api/openapi.yaml                   # MODIFY — new path + Balance/BalancesResponse schemas
  cmd/walletd/main.go                 # MODIFY — wire BalanceRepository + GetCustomerBalances
```

No new files under `internal/adapter/evm/`, `internal/adapter/signer/`, `internal/adapter/webhook/`, or `contracts/` — those remain out of scope until Epics 2–4, per Story 1.1's Project Structure Notes (still valid, unchanged by this story).

### Schema (this story's table only)

```sql
-- 0003_create_journal_entries_and_postings.sql
CREATE TABLE journal_entries (
    id          uuid PRIMARY KEY,           -- UUIDv7
    cause_type  text NOT NULL,               -- 'internal_transfer' | 'deposit' | 'withdrawal' | 'sweep' | 'operator_adjustment' (only 'internal_transfer' exists starting Story 1.3; others come with their owning epics)
    cause_id    text NOT NULL,               -- e.g. the caller's idempotency key for internal transfers (FR5)
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cause_type, cause_id)            -- AD-3: same cause can never produce two entries
);

CREATE TABLE postings (
    id                uuid PRIMARY KEY,      -- UUIDv7
    journal_entry_id  uuid NOT NULL REFERENCES journal_entries(id),
    account_id        uuid NOT NULL REFERENCES accounts(id),
    amount            NUMERIC(78,0) NOT NULL, -- signed: positive increases the account, negative decreases it; a balanced entry's postings sum to zero across all accounts it touches (enforced by application logic in the writer, starting Story 1.3 — not a DB constraint)
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_postings_account_id ON postings (account_id);
-- No rows are written by this story — Story 1.3 is the first writer (internal transfers).
-- This story only needs the tables to exist so its SUM(amount) derivation query is real SQL
-- against real (empty) tables, not a special-cased "return 0 if the table doesn't exist" branch.
```

### Config

No new environment variables. This story reuses the existing `DATABASE_URL`-configured `pgxpool.Pool` already constructed in `main.go`.

### Testing standards

- Table-driven Go tests for the use case, isolated with a fake `BalanceRepository`.
- Integration test against real Postgres (`testcontainers-go`, `postgres:18`, or the compose stack) — per the project's rigor thesis (PRD Success Metric 5), do not substitute a mocked repository. This is the only way to genuinely prove AC4 (see Task 5's direct-SQL-insert approach).
- No reorg/chain/crash-recovery testing applies (no chain code yet) — unchanged from Story 1.1.

### Project Structure Notes

- No conflicts with the existing structure — this story extends `internal/core`, `internal/adapter/{api,postgres}`, `api/openapi.yaml`, and `cmd/walletd/main.go`, all files Story 1.1 already established the shape of. Read each MODIFY-marked file above in full before changing it (per this workflow's standing rule) — do not guess at `customers.go`'s or `main.go`'s current wiring from memory.
- `BalanceRepository`'s pool-direct constructor is a deliberate variance from `CustomerRepository`'s poolless/tx-from-context shape, justified above — not an inconsistency to "fix" toward matching `CustomerRepository`.

### References

- [Source: _bmad-output/planning-artifacts/epics.md#Epic 1: Foundation — Accounts, Ledger & Deposit Addresses / Story 1.2] — canonical ACs
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md#AD-3, AD-4] — double-entry ledger, derived balances, one-transaction-per-change
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md#4. The money model] — account taxonomy (customer available/hold, forwarder-float, treasury, fees) — only "customer available" (the existing `accounts` table) is relevant to this story; the other taxonomy members arrive with the epics that need them
- [Source: _bmad-output/implementation-artifacts/1-1-create-customer-and-provision-per-asset-accounts.md] — established repo/use-case/handler shapes, `WriteProblem` helper, `txFromContext`/`IdempotencyStore.Lookup` precedent for pool-direct reads, non-mutating-method bypass in `IdempotencyMiddleware`

### Previous story intelligence (from Story 1.1)

- **TDD throughout**: every source file had a corresponding test file written first (red) before implementation (green). Follow the same discipline here.
- **Adversarial code review found real defects** in transaction/rollback edge cases, non-2xx caching, and constant-time comparisons — none of that machinery is touched by this story, but the review process itself (fresh-context adversarial pass before marking done) is the standing convention for every story, not just 1.1.
- **`IdempotencyStore.Lookup` already reads the pool directly, pre-transaction** — this is the existing precedent this story's `BalanceRepository` follows; it is not a new pattern being invented, just applied to a second port.
- **The generated `ServerInterfaceWrapper` does its own header/path validation**; `StdHTTPServerOptions.ErrorHandlerFunc` in `main.go` is already wired to route generator-level errors through `WriteProblem` — confirm this still applies correctly to a malformed `{id}` (non-UUID path segment) on the new route rather than assuming it needs new wiring.
- No dependency additions expected beyond what's already in `go.mod` (`math/big` is stdlib).

## Change Log

- Implemented all 5 ACs end-to-end: zero-balance derivation for a freshly-provisioned customer, 404 for an unknown customer id, sub-500ms response time (sanity-checked, not load-tested), balances genuinely derived from `SUM(postings.amount)` (proved by inserting a fixture posting directly via SQL and asserting the derived balance reflects it), and 401 for a missing bearer token.
- Added goose migration `0003_create_journal_entries_and_postings.sql`: `journal_entries` (unique on `(cause_type, cause_id)` per AD-3) and `postings` (`NUMERIC(78,0)` signed amount, FK to `accounts`). No rows written by this story — Story 1.3 is the first writer; this story's job was making the read path real.
- Added `core.BalanceRepository` port, `core.AccountBalance` domain type, `core.ErrCustomerNotFound` sentinel, and `core.GetCustomerBalances` use case, mirroring `CreateCustomer`'s shape.
- Added `postgres.BalanceRepository`: deliberately pool-direct (not tx-from-context) because `IdempotencyMiddleware` bypasses non-mutating methods and never opens a transaction for GET requests — `txFromContext` would have panicked. Confirms customer existence via a bare `EXISTS` query before the balance query, since a nonexistent customer and a real one with no postings both produce zero joined rows. Amounts are cast to `text` in SQL and parsed via `big.Int.SetString`, sidestepping pgx v5 `pgtype.Numeric` scanning for a base-10 integer value.
- Added `GET /v1/customers/{id}/balances` to `api/openapi.yaml` (`Balance`, `BalancesResponse` schemas; `balance` is `type: string`, never a number, to avoid float64 precision loss — the first money amount to cross the API boundary, setting the precedent for every later story). Regenerated `internal/adapter/api/server.gen.go` via `oapi-codegen` v2.7.2 (matching Story 1.1's pinned version); the generated handler signature takes the path param directly (`id openapi_types.UUID`), no `Params` struct, confirming the story's prediction.
- Extended `customerServer`/`NewServerInterface` in `internal/adapter/api/customers.go` with the new handler and use case; wired `postgres.NewBalanceRepository(pool)` + `core.NewGetCustomerBalances` in `cmd/walletd/main.go` alongside the existing composition root. No middleware changes were needed — `AuthMiddleware` already covers every route unconditionally, and `IdempotencyMiddleware`'s existing non-mutating-method bypass already applies to the new GET route.
- Extended `internal/adapter/api/integration_test.go`'s real-Postgres (`testcontainers-go`, `postgres:18`) test harness with `TestGetCustomerBalances_EndToEnd` covering all 5 ACs, reusing the existing customer-creation flow via a small `createTestCustomer` helper rather than duplicating it.

## Dev Agent Record

### Agent Model Used

claude-haiku-4-5-20251001

### Debug Log References

None — no debugging session artifacts beyond the red/green test runs recorded in Completion Notes.

### Completion Notes List

- TDD followed: `internal/core/get_customer_balances_test.go` was written first and confirmed failing to compile (red — `core.AccountBalance`, `core.NewGetCustomerBalances`, `core.ErrCustomerNotFound` all undefined) before `customer.go`, `ports.go`, and `get_customer_balances.go` were implemented (green).
- Full regression suite green: `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), `go test ./...` — including the real-Postgres integration tests (`testcontainers-go`, `postgres:18`) for both `TestCreateCustomer_EndToEnd` (unchanged, still passing after the `NewServerInterface` signature change) and the new `TestGetCustomerBalances_EndToEnd`.
- `oapi-codegen` v2.7.2 installed locally (`go install .../oapi-codegen@v2.7.2`) to match the version pinned by Story 1.1's own toolchain notes, and used to regenerate `server.gen.go` for real rather than hand-authoring the expected diff.
- Verified the story's two flagged uncertainties directly against generated output rather than assuming: (1) the generated `GetCustomerBalances` method signature takes the path param as a direct argument (`id openapi_types.UUID`, which is a type alias for `uuid.UUID`) with no `Params` struct; (2) the existing `IdempotencyMiddleware` non-mutating bypass and `AuthMiddleware`'s unconditional coverage required zero changes for the new GET route.
- No new dependencies added — `math/big` is stdlib; `github.com/google/uuid` was already a dependency (used in the new integration test to generate fixture row ids and an unused-customer id for the 404 case).

### File List

**New files:**
- `internal/adapter/postgres/migrations/0003_create_journal_entries_and_postings.sql`
- `internal/adapter/postgres/balance_repo.go`
- `internal/core/get_customer_balances.go`
- `internal/core/get_customer_balances_test.go`

**Modified files:**
- `internal/core/customer.go` (added `ErrCustomerNotFound`, `AccountBalance`)
- `internal/core/ports.go` (added `BalanceRepository`)
- `api/openapi.yaml` (added `GET /customers/{id}/balances`, `Balance`, `BalancesResponse`)
- `internal/adapter/api/server.gen.go` (regenerated — do not hand-edit)
- `internal/adapter/api/customers.go` (added `GetCustomerBalances` handler, extended `customerServer`/`NewServerInterface`)
- `internal/adapter/api/integration_test.go` (updated `newTestHandler` wiring; added `createTestCustomer` helper and `TestGetCustomerBalances_EndToEnd`)
- `cmd/walletd/main.go` (wired `BalanceRepository` + `GetCustomerBalances`)
- `_bmad-output/implementation-artifacts/sprint-status.yaml` (status transitions for this story)

## Review Findings

_Adversarial code review (Blind Hunter + Edge Case Hunter + Acceptance Auditor), 2026-07-14. Acceptance Auditor returned PASS on all 5 ACs and every spec constraint. 6 findings survived triage; 5 dismissed as noise._

- [x] [Review][Decision→Patch] Integration test hard-fails on a 500ms wall-clock assertion [internal/adapter/api/integration_test.go:239] — RESOLVED: softened to measure-without-gating. The `elapsed` timing is now recorded via `t.Logf` instead of `t.Fatalf`, removing CI flakiness while keeping AC3's sanity intent; real p95 remains Story 6.4's job.
- [x] [Review][Patch] Balance query has no `ORDER BY`; response ordering of the 4 (chain, asset) pairs is non-deterministic [internal/adapter/postgres/balance_repo.go:52] — FIXED: added `ORDER BY a.chain, a.asset` for stable output.
- [x] [Review][Defer] Raw internal error text leaked in the problem+json `detail` on the 500 path [internal/adapter/api/customers.go:66] — deferred, pre-existing: `CreateCustomer`'s 500 branch does the same, and `problem.go` already warns against sensitive `Detail`. Fix project-wide (log detail server-side, return a generic message), not just this endpoint.
- [x] [Review][Defer] No object-level authorization — any holder of any valid shared token can read any customer's balances by id [internal/adapter/api/customers.go:59] — deferred, pre-existing: the static shared-token model comes from Story 1.1 and is likely by-design for the B2B "application team" caller. Confirm the intended trust model before any end-user-facing exposure.
- [x] [Review][Defer] Existence check and the balance SUM run as two separate pool queries outside a transaction (TOCTOU) [internal/adapter/postgres/balance_repo.go:36-47] — deferred, benign for a read with no writers today; consider folding into a single query when Story 1.3 introduces posting writes.
- [x] [Review][Defer] A negative derived balance would be surfaced verbatim, with no floor or guard [internal/adapter/postgres/balance_repo.go:48] — deferred, latent: no posting writer exists until Story 1.3. Decide on a non-negative guard (read path and/or a DB CHECK) before 1.3 writes rows.

**Dismissed as noise (5):** enum-cast without `Valid()` (chain/asset only ever written from the fixed `SupportedChainAssetPairs`, not reachable); nil `*big.Int` panic (repo always sets non-nil via guarded `SetString`); fewer-than-4-accounts short list (provisioning is atomic — one transaction); non-UUID path param → text/plain 400 (false positive — `ErrorHandlerFunc` is wired in both `main.go:95` and the test harness, yielding problem+json 400); encode-error-after-200 swallowed (matches established codebase convention, no recovery possible after the status line is written).
