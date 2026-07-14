---
baseline_commit: 799e07f56d1ac26b28808aad1167c072a8677f86
---

# Story 1.1: Create Customer & Provision Per-Asset Accounts

Status: done

<!-- Note: Validation is optional. Run validate-create-story for quality check before dev-story. -->

## Story

As an application team,
I want to create a customer record via the API and have per-asset accounts provisioned automatically,
so that I can begin tracking a customer's balances from day one.

## Acceptance Criteria

1. **Given** a POST `/v1/customers` request with a valid `Idempotency-Key` header, **when** the platform processes it, **then** a customer record is created and one account per supported (chain, asset) pair — (Base, ETH), (Base, USDC), (Arbitrum, ETH), (Arbitrum, USDC) — is provisioned with a zero balance, and the customer id is returned. [FR1]
2. **Given** the same `Idempotency-Key` is replayed with the same request body, **when** processed again, **then** the original response is returned byte-for-byte and no second customer or account rows are created. [FR23]
3. **Given** a mutating request without an `Idempotency-Key` header, **when** processed, **then** the platform rejects it with a 400 RFC 9457 `problem+json` response and no side effects occur.
4. **Given** the request succeeds, **when** inspected in Postgres, **then** the customer and its accounts exist in a single committed transaction — a crash between them is impossible by construction. [AD-4]
5. **Given** a request to any mutating endpoint without a valid bearer token, **when** processed, **then** the platform rejects it with a 401 response — there is no anonymous surface anywhere in the API. [NFR15, AD-14]

This is the **first story in the project** — no code exists yet (verified: repo has only `README.md` and an empty `docs/`). It therefore also bootstraps the project skeleton, Postgres schema tooling, and OpenAPI toolchain that every later story builds on. Do not gold-plate this scaffolding beyond what Story 1.1 itself needs — later stories add their own tables, routes, and adapters incrementally (see "Database/Entity Creation Timing" note below).

## Tasks / Subtasks

- [x] **Task 1: Project skeleton & local environment** (AC: 1, 3, 4, 5)
  - [x] `go mod init` — module path `github.com/andborges/digital-asset-wallet-platform`, matching the repo's actual `origin` remote (`andborges/digital-asset-wallet-platform`). Originally scaffolded as a placeholder (`github.com/andre/...`) and renamed across every import once the real remote was confirmed.
  - [x] Create the source tree exactly as fixed in the Architecture Spine's Structural Seed (paths below) — create only the packages this story needs; leave the rest as empty directories with a `.gitkeep` or simply create them lazily in later stories, whichever the dev agent's tooling prefers
  - [x] `deploy/compose/docker-compose.yml`: a `postgres` service (image `postgres:18`, a named volume, health check) and an `api` service (builds from the repo, runs `walletd api`). This is the seed of the compose stack every later story's process (`watcher`, `broadcaster`, `recon`, `dispatcher`) will be added to — do not add those services now, they don't exist yet.
  - [x] `.env.example` documenting the env vars this story introduces (see Dev Notes → Config)
- [x] **Task 2: Database schema — customers & accounts** (AC: 1, 4)
  - [x] goose migration `0001_create_customers_and_accounts.sql`: `customers` table and `accounts` table (see Dev Notes → Schema). **Do not add a `balance` column to `accounts`** — AD-3 mandates balances are always derived from postings, and the `postings`/`journal_entries` tables don't exist until Story 1.3. A zero balance for a new account is simply the absence of postings; Story 1.2 (not this one) implements the derivation query.
  - [x] Wire goose to run embedded migrations (`embed.FS`) on `walletd api` startup, or via a `migrate` subcommand — dev agent's choice, but migrations must be embedded in the binary per the Consistency Conventions table ("Migrations: goose, plain SQL, embedded")
  - [x] Add a minimal CI workflow (`go build && go vet && go test ./...`) — cheap now, and Story 2.1 only needs to *add* an import-boundary check to an existing pipeline rather than create one from scratch
- [x] **Task 3: Idempotency mechanism** (AC: 2, 3)
  - [x] goose migration `0002_create_idempotency_keys.sql`: `idempotency_keys` table (see Dev Notes → Schema)
  - [x] **Middleware chain order: authentication first, then idempotency.** An unauthenticated request must fail with 401 before the idempotency layer ever inspects it — never let an unauthenticated caller probe idempotency-key state.
  - [x] **Transaction propagation between middleware and handler (the riskiest wiring detail in this story — get this exactly right):** the idempotency middleware begins the Postgres transaction (via the `WithTx` helper) and stores it in the request `context.Context`. The handler and `core.CreateCustomer`/its repo accept that transaction from context rather than opening their own. The middleware — not the handler — commits, and it commits only after both the business writes *and* the idempotency-key row are written. This is what makes "customer + accounts + idempotency row, one transaction" (AD-4) actually true rather than aspirational; a design where the handler commits its own transaction and the middleware separately inserts the idempotency row afterward reopens the exact race AD-4 exists to close.
  - [x] Idempotency middleware behavior: missing `Idempotency-Key` header on a mutating route → 400 `problem+json`, transaction never opened. Header present and key already stored → return the stored `(status, body)` verbatim without calling the handler or opening a transaction. Header present, key not stored, request body matches nothing yet → open the transaction, call the handler, capture its response, insert the idempotency row, commit.
  - [x] **Byte-for-byte replay fidelity (AC2 says "byte-for-byte" — take it literally):** store the captured response body as `bytea`, not `jsonb`. Postgres `jsonb` does not preserve key order, whitespace, or exact numeric formatting on round-trip, so re-serializing through `jsonb` can legitimately produce different bytes than what was originally written to the wire. Capture and store the exact bytes the handler wrote; return those exact bytes on replay.
  - [x] Edge case — same `Idempotency-Key` replayed with a **different** request body (not in the formal ACs above, but the standard companion case for any idempotency-key design, and exactly what Story 6.3's fault-injection suite plans to test against this mechanism): reject with 409 `problem+json`, do not silently return the old response. Compute `request_hash` (e.g. SHA-256 of method+path+body) to detect this.
  - [x] Edge case — concurrent duplicate requests (two requests with the same key racing past the "not yet stored" check simultaneously): the `idempotency_keys.key` unique constraint makes one of them lose on insert. The loser's transaction rolls back; it then re-reads the now-stored row and returns the winner's `(status, bytes)` rather than surfacing a 500. This is a rollback-then-fetch, not a retry-the-whole-request.
- [x] **Task 4: Bearer-token authentication** (AC: 5)
  - [x] Middleware checking `Authorization: Bearer <token>` against a static, env-configured set of valid tokens (v1 has one internal consumer; there is no token-issuance API in this story or PRD — see Dev Notes → Config for the env var shape). Missing or invalid token → 401 `problem+json`. Every route is behind this middleware; there is no anonymous route in this service, ever. This middleware runs **before** the idempotency middleware in the chain (see Task 3).
- [x] **Task 5: OpenAPI spec & generated handler scaffolding** (AC: 1)
  - [x] `api/openapi.yaml`: bootstrap the spec (OpenAPI 3.0.3) with `POST /v1/customers`, the `bearerAuth` security scheme applied globally, an `Idempotency-Key` header parameter, and a reusable `ProblemDetails` schema (RFC 9457 shape: `type`, `title`, `status`, `detail`, `instance`) used for every error response from here on
  - [x] Configure `oapi-codegen` v2 to generate into `internal/adapter/api/server.gen.go` using the stdlib (`net/http` `ServeMux`, Go 1.22+ pattern routing) server target — confirm current config field names against the installed `oapi-codegen` version's own `--help`/example config before assuming a specific YAML shape, since generator config surface can shift between minor versions
  - [x] Implement the generated `ServerInterface` for the customers endpoint in `internal/adapter/api/customers.go`
- [x] **Task 6: Customer creation use case** (AC: 1, 4)
  - [x] `internal/core`: a `Customer` domain type, an `AccountRepository`/`CustomerRepository` port (interface), and a `CreateCustomer` use case that provisions the customer plus the four fixed (chain, asset) accounts — Base/ETH, Base/USDC, Arbitrum/ETH, Arbitrum/USDC — as one call
  - [x] `internal/adapter/postgres`: implement the port against `pgxpool.Pool`; the customer insert and the four account inserts happen in one `pgx.Tx`
  - [x] Wire: auth middleware → idempotency middleware (opens/commits the shared transaction, per Task 3) → API handler → `core.CreateCustomer` → Postgres repo, all operating on the transaction from context
- [x] **Task 7: Tests**
  - [x] Table-driven unit tests for the idempotency middleware (replay-same-body, replay-different-body, missing-header) and the auth middleware (missing/invalid/valid token) in isolation
  - [x] An integration test that runs against a real Postgres (the compose stack's `postgres` service, or `testcontainers-go` — dev agent's choice, but it must be a real database, not a mock, per the project's own rigor thesis) exercising all four formal ACs end-to-end: create → verify 4 accounts exist with zero derivable balance → replay → replay-with-different-body → missing-Idempotency-Key → missing-auth

## Dev Notes

### Architecture patterns and constraints this story must follow

- **Hexagonal boundary (AD-1, AD-2).** `internal/core` imports nothing from `internal/adapter/*`; adapters import core, never each other. This is the first story, so there is no CI import-boundary check yet to enforce it mechanically — write the code as if the check already exists, because Story 2.1 adds that check and it must pass against code written now.
- **No CLI framework yet.** `cmd/walletd/main.go` dispatches its single `api` subcommand via plain stdlib (`os.Args`/`flag`) — do not add `cobra` or similar for one subcommand. Later epics add `watcher`, `broadcaster`, `recon`, `dispatcher`; revisit the dispatch mechanism then if the subcommand count justifies it, not now.
- **No outbox table in this story.** AD-4's text says "every observable state change commits with its outbox event," which might read as if customer creation needs one — it doesn't. FR29's webhook catalog (deposit pending/credited, withdrawal state changes, approval required, reconciliation alerts) never includes customer creation, and the `outbox_events` table itself isn't introduced until Epic 2 Story 2.1, the first story that actually needs to emit an event. Building an outbox table here would be gold-plating against a rule that doesn't apply to this story's data.
- **One transaction per observable change (AD-4).** The customer + its four accounts + the idempotency-key row are one Postgres transaction. This is the concrete precedent every later story's "atomic" AC follows — get the transaction-scoping pattern right here (e.g. a small `WithTx` helper in `internal/adapter/postgres`) because Stories 1.3 onward reuse it verbatim for journal postings.
- **Idempotency by unique constraint, not application logic (AD-5).** The `idempotency_keys.key` column is `PRIMARY KEY` (or `UNIQUE NOT NULL`) — a concurrent duplicate request must fail on the database constraint, not on an application-level `SELECT ... WHERE key = ?` race. Insert-then-catch-conflict, not check-then-insert.
- **Balances are derived, never stored (AD-3).** Repeated because it's the single easiest mistake in this story: do not add a `balance` numeric column to `accounts`. If a later reviewer asks "where's the balance," the answer is "there is no balance column — it's computed from postings, starting Story 1.3."
- **Money convention (Consistency Conventions table).** Not exercised yet in this story (no amounts are handled), but the `accounts` table's `asset` values must already anticipate the convention used from Story 1.2 onward: integer base units, `NUMERIC(78,0)` — do not use a floating-point type anywhere the schema will later hold money, even in this story's tables.
- **ID convention.** UUIDv7 for `customers.id` and `accounts.id` (Consistency Conventions table: "UUIDv7 for all entities"). See Dev Agent Record / latest tech research below for the current library.
- **Error convention.** RFC 9457 `application/problem+json` for every error response from this story onward — this is the first story to emit errors, so the `ProblemDetails` shape defined here in `openapi.yaml` is what every later story's error responses reuse. Get the shape right: `type`, `title`, `status`, `detail`, `instance` fields; never include a key handle or secret in `detail` (a later NFR13 concern, but the *shape* is set now).
- **Auth convention.** v1 uses static bearer tokens per consumer (Consistency Conventions table, and Solution Design §7: "static bearer tokens per consumer in v1"). There is no token-management API — tokens are operator-provisioned via configuration. Do not build a token-issuance endpoint; it's out of scope for the PRD entirely.
- **Logging.** `log/slog` structured JSON (Consistency Conventions table). Every log line touching the new customer should carry its UUID once it exists — this becomes load-bearing for NFR18 (end-to-end traceability) once deposits exist in Epic 2, so establish the pattern (a logger with the customer ID as a structured field) here rather than retrofitting it later.

### Source tree components this story creates

```text
digital-asset-wallet-platform/
  cmd/walletd/                    # NEW — main.go + "api" subcommand only (watcher/broadcaster/recon/dispatcher subcommands don't exist until Epics 2-4)
  internal/core/
    customer.go                   # NEW — Customer domain type
    account.go                    # NEW — Account domain type (no balance field)
    ports.go                      # NEW — CustomerRepository / AccountRepository interfaces
    create_customer.go            # NEW — the use case
  internal/adapter/
    api/
      server.gen.go                # NEW — oapi-codegen output, do not hand-edit
      customers.go                 # NEW — ServerInterface implementation for POST /v1/customers
      middleware_idempotency.go    # NEW — generic, reused by every later mutating story
      middleware_auth.go           # NEW — generic, reused by every route
      problem.go                   # NEW — RFC 9457 helpers
    postgres/
      migrations/0001_create_customers_and_accounts.sql   # NEW
      migrations/0002_create_idempotency_keys.sql          # NEW
      customer_repo.go             # NEW
      idempotency_store.go         # NEW
      tx.go                        # NEW — WithTx transaction helper, reused everywhere from here on
  api/openapi.yaml                 # NEW — the contract; grows with every future story, never regress existing paths
  deploy/compose/docker-compose.yml # NEW — postgres + api only; later epics add watcher/broadcaster/recon/dispatcher services
  .env.example                     # NEW
  go.mod / go.sum                  # NEW
```

Everything above is **NEW** — there is no existing code to preserve or avoid breaking (verified via `git log` and a repo tree scan: only `README.md` and an empty `docs/` directory exist as of this story). This will not be true again after Story 1.1 ships; every story from 1.2 onward must read the files it modifies before changing them.

### Schema (this story's tables only)

```sql
-- 0001_create_customers_and_accounts.sql
CREATE TABLE customers (
    id          uuid PRIMARY KEY,           -- UUIDv7, generated by the application
    created_at  timestamptz NOT NULL DEFAULT now()
);
-- No external_ref or other column beyond what AC1 requires (id + created_at) — nothing in
-- epics.md's FR1/AC1 asks for a caller-supplied reference; adding one here would be scope
-- invention. If a future story needs it, add it in that story's own migration.

CREATE TABLE accounts (
    id          uuid PRIMARY KEY,           -- UUIDv7
    customer_id uuid NOT NULL REFERENCES customers(id),
    chain       text NOT NULL,               -- 'base' | 'arbitrum'
    asset       text NOT NULL,               -- 'eth' | 'usdc'
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (customer_id, chain, asset)
);
-- No balance column. Balances are derived from postings starting Story 1.3 (AD-3).
-- Note for Story 1.3: this table is (chain, asset) scoped per-account. Story 1.3's internal-transfer
-- AC in epics.md takes only (source, destination, asset, amount) with no chain parameter — if that
-- turns out to mean transfers are chain-agnostic (aggregate-across-chain), reconcile this schema then.
-- Flagging now so it's a known decision point in Story 1.3, not a surprise.

-- 0002_create_idempotency_keys.sql
CREATE TABLE idempotency_keys (
    key             text PRIMARY KEY,
    request_hash    bytea NOT NULL,
    response_status int NOT NULL,
    response_body   bytea NOT NULL,        -- exact bytes written to the wire, NOT jsonb (jsonb doesn't
                                            -- round-trip byte-for-byte: no guaranteed key order/whitespace/
                                            -- numeric formatting preservation — AC2 requires literal byte fidelity)
    created_at      timestamptz NOT NULL DEFAULT now()
);
-- No retention/cleanup policy for this table in v1 — unbounded growth is an accepted, deferred
-- concern, not an oversight. Revisit if row count becomes an operational issue.
```

### Config (env vars this story introduces)

- `DATABASE_URL` — Postgres connection string (pgx format)
- `API_BEARER_TOKENS` — comma-separated list of valid bearer tokens for v1's single consumer (static, operator-provisioned; not a feature to build beyond reading this list)
- `API_LISTEN_ADDR` — e.g. `:8080`

### Testing standards

- Table-driven Go tests (`internal/adapter/api/*_test.go` for middleware, in isolation, with a fake handler)
- Integration test against a real Postgres — this project's stated thesis is rigor over shortcuts (PRD Success Metric 5, "portfolio quality"); do not substitute a mocked repository for the integration test in Task 7. `testcontainers-go` against `postgres:18` or the compose stack's `postgres` service are both acceptable.
- No reorg/chain/crash-recovery testing applies to this story (no chain code, no long-running processes yet) — that begins Epic 2.

### Project Structure Notes

- No conflicts or variances to reconcile — this is the first code in the repository, so it establishes the structure rather than fitting into one.
- The Architecture Spine's full source tree (in `ARCHITECTURE-SPINE.md` → Structural Seed) shows the target shape at v1 completion, including `internal/adapter/evm/`, `internal/adapter/signer/`, `internal/adapter/webhook/`, and `contracts/` — **do not create these now**; they belong to Epics 2, 3, and 4 respectively and an empty premature package is exactly the "gold-plating" this story should avoid.

### References

- [Source: _bmad-output/planning-artifacts/epics.md#Epic 1: Foundation — Accounts, Ledger & Deposit Addresses / Story 1.1] — canonical ACs and epic framing
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md#AD-1, AD-2, AD-3, AD-4, AD-5, AD-14] — structural rules this story must satisfy
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md#Consistency Conventions] — money/ID/error/auth/logging/migration conventions
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md#Structural Seed] — target source tree and stack versions
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md#4. The money model] — why `accounts` has no balance column (forwarder-float rationale, double-entry rationale)
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md#7. Security posture] — static bearer token convention
- [Source: _bmad-output/planning-artifacts/implementation-readiness-report-2026-07-14.md] — confirms zero blocking gaps against this epic/story set as of 2026-07-14

### Latest technical specifics (verified 2026-07-14 for this story's dependencies)

- **UUIDv7:** use `github.com/google/uuid`, call `uuid.NewV7()` (returns `(UUID, error)`; `UUID` is a `[16]byte`). It implements RFC 9562 and is the mainstream choice. Do **not** wait for a Go stdlib `uuid` package — a proposal exists but has no target release (author speculates 1.27+; not in Go 1.26, which is this project's pinned version). `gofrs/uuid/v5` is an acceptable fallback only; its own docs flag v6/v7 support as not yet part of its stable API.
- **oapi-codegen v2 config** (confirm exact field names against the installed version's own example/`--help`, since generator config surface shifts between minor versions, but as of 2026-07-14 this is current):
  ```yaml
  package: api
  generate:
    models: true
    std-http-server: true
    embedded-spec: true
  output: server.gen.go
  ```
  This generates `ServerInterface` (one method per `operationId` — name the `POST /v1/customers` operation accordingly, e.g. `operationId: createCustomer` → `CreateCustomer(w http.ResponseWriter, r *http.Request)`), plus `RegisterHandlers(mux *http.ServeMux, si ServerInterface)` to wire the implementation into the stdlib `ServeMux`. Do not hand-edit `server.gen.go`; implement `ServerInterface` in a separate file (`customers.go`).
- **RFC 9457 problem+json:** no dominant library exists (unlike UUIDs or pgx) — hand-roll a small struct (`Type`, `Title`, `Status`, `Detail`, `Instance` fields with matching `json` tags) plus a helper that sets `Content-Type: application/problem+json` and writes it. This is the correct choice here, not a shortcut — pulling in a fragmented, low-adoption third-party package for a 15-line struct would be the wrong trade.
- **pgx v5 pool:** `pgxpool.New(ctx, dsn)` for the simple case, or `pgxpool.ParseConfig` + `pgxpool.NewWithConfig` if connection tuning is needed later. Package: `github.com/jackc/pgx/v5/pgxpool`.
- **goose v3 migrations:** the `Provider`-based API is goose's own current recommendation (supersedes the legacy `goose.SetBaseFS`/`goose.Up` global-state API):
  ```go
  //go:embed migrations/*.sql
  var embedMigrations embed.FS

  fsys, _ := fs.Sub(embedMigrations, "migrations")
  provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
  _, err = provider.Up(ctx)
  ```
  `goose.NewProvider` takes a `*sql.DB`, not a `*pgxpool.Pool` directly — open a small dedicated `*sql.DB` for migrations via `github.com/jackc/pgx/v5/stdlib` (`stdlib.OpenDBFromPool(pool)`), run migrations at `walletd api` startup before serving traffic, then continue using `pgxpool.Pool` for all application queries.

## Senior Developer Review (AI)

**Reviewed:** 2026-07-14 · **Reviewer model:** Opus 4.8 (adversarial layers) · **Outcome: Approved (after fixes applied)**

Three parallel adversarial review layers ran against the diff (`799e07f..HEAD`, excluding generated `go.sum` and planning artifacts): Blind Hunter (general adversarial), Edge Case Hunter (branch/boundary exhaustive), and Acceptance Auditor (diff vs. the 5 ACs + AD-1…AD-15). All five ACs and every architecture constraint were confirmed satisfied. The layers converged strongly on a small set of real defects in the idempotency middleware — the single most-reused piece of code in the project — all of which were fixed and covered with new tests before marking this story done.

### Action Items — resolved

- [x] **[Critical] Handler panic leaked the open transaction + pooled connection.** `middleware_idempotency.go` had no rollback on the panic path; a few panics would exhaust the pool and wedge the service. **Fix:** a `defer` rolls the transaction back on every non-commit exit (including panic unwind), using a detached context so it runs even if the client cancelled. Covered by `TestIdempotencyMiddleware_HandlerPanicRollsBackTransaction`.
- [x] **[High] Middleware committed and permanently cached non-2xx handler responses (idempotency-key poisoning).** A transient 500 was stored under the key and replayed forever, so a legitimate retry could never succeed — defeating the retry-safety FR23/FR24 exist for. **Fix:** commit + store are now gated on a 2xx status; any non-2xx rolls back and passes through unstored. Covered by `TestIdempotencyMiddleware_NonSuccessResponseIsNotStoredAndRollsBack`.
- [x] **[High] Concurrent duplicate with a *different* body replayed the winner's response instead of 409.** The conflict branch skipped the request-hash comparison the non-concurrent path performs. **Fix:** both paths now funnel through one `replayOrConflict` helper, so a different-body duplicate 409s whether the collision is sequential or concurrent. Covered by `TestIdempotencyMiddleware_ConcurrentDuplicateDifferentBodyReturns409`.
- [x] **[Medium] Unbounded `io.ReadAll` of the request body (memory-exhaustion vector).** **Fix:** `http.MaxBytesReader` caps the body at 1 MiB; oversized bodies get 413 before the handler runs. Covered by `TestIdempotencyMiddleware_OversizedBodyRejected`.
- [x] **[Medium] No server timeouts / graceful shutdown.** Bare `http.ListenAndServe` (Slowloris exposure) and `context.Background()` with no signal handling (SIGTERM killed in-flight transactions). **Fix:** `http.Server` with Read/ReadHeader/Write/Idle timeouts, plus `signal.NotifyContext` + `srv.Shutdown` with a 20s drain. Verified by running the built binary and confirming a clean drain on SIGTERM (not unit-testable via the handler-only integration test).
- [x] **[Medium] OpenAPI spec-vs-impl drift.** The spec declared the request body `additionalProperties: false` but nothing validated it. **Fix:** customer creation genuinely takes no body, so the `requestBody` was removed from `api/openapi.yaml` (with a description saying any body is ignored) and `server.gen.go` regenerated — spec now matches behavior (AD-14 spec-first honesty).
- [x] **[Medium] Idempotency middleware forced a key + write transaction on non-mutating methods.** A future GET route would have been required to carry an Idempotency-Key. **Fix:** GET/HEAD/OPTIONS/TRACE now pass straight through. Covered by `TestIdempotencyMiddleware_NonMutatingMethodBypasses`.
- [x] **[Low] Bearer token compared via non-constant-time map lookup.** **Fix:** `middleware_auth.go` now uses `crypto/subtle.ConstantTimeCompare` against every configured token (no early return, fail-closed on empty list).
- [x] **[Low] Empty handler response committed a synthetic 200.** **Fix:** a handler that writes nothing is now treated as a programming error (500, no commit, not stored). Covered by `TestIdempotencyMiddleware_EmptyHandlerResponseIsError`.
- [x] **[Low] Conflict re-lookup reused the possibly-cancelled request context.** **Fix:** the re-lookup and rollback/commit now use `context.WithoutCancel`, so they still complete if the client disconnected.

### Action Items — deferred (documented, not blocking)

- **[Low] Replay restores only `Content-Type`, not arbitrary response headers.** No handler sets other headers today (no `Location` on the 201); capturing all headers is a clean future enhancement to `StoredResponse` if/when a handler needs one. Not required by any AC.
- **[Low] Idempotency keys are globally scoped, not per-consumer/per-route.** Correct and safe for the single v1 consumer; revisit when multi-tenancy arrives (already a future-path item in the architecture).

## Change Log

- **Code review (2026-07-14):** applied 10 fixes from adversarial review (1 Critical, 2 High, 4 Medium, 3 Low) — panic-safe transaction rollback, 2xx-gated commit/store (no error caching), symmetric different-body 409, request-body size cap, server timeouts + graceful shutdown, OpenAPI body honesty + regeneration, non-mutating-method bypass, constant-time token comparison, empty-response guard, detached-context re-lookup. Added 6 new middleware tests; full suite green. Details in the Senior Developer Review section above.
- Implemented all 5 ACs end-to-end: customer + 4 accounts creation, byte-for-byte idempotent replay, 409 on key-reuse-with-different-body, atomic transaction across customer+accounts+idempotency row, bearer-token authentication.
- Bootstrapped the project: `go.mod` (module `github.com/andborges/digital-asset-wallet-platform`), hexagonal source tree (`internal/core`, `internal/adapter/{api,postgres}`, `cmd/walletd`), Docker Compose stack (`postgres:18` + `api`), minimal GitHub Actions CI.
- **Post-review fix:** module path renamed from the placeholder `github.com/andre/...` to `github.com/andborges/digital-asset-wallet-platform` to match the repo's real `origin` remote — a public Go module whose import path doesn't match its actual location breaks `go get`/`go install` for anyone else. Renamed across `go.mod` and every internal import; `docker-compose.yml`'s `postgres:18` volume mount was also corrected (see below) after being caught by manually running the stack.
- **Runtime bug fix (found by manually running the compose stack, not by tests):** `postgres:18`'s image requires its data volume mounted at `/var/lib/postgresql` (the parent directory), not `/var/lib/postgresql/data` — mounting at the old path makes the container refuse to start entirely (a hard `Error`, not a warning). Fixed in `deploy/compose/docker-compose.yml`. The integration test's `testcontainers-go` setup was unaffected since it lets the module manage its own data directory, which is why this didn't surface until the compose stack was actually run.
- Added `.gitignore` (excludes `.env` and local build/IDE artifacts, keeps `.env.example` tracked) and a project `README.md` covering what the project is, its architecture, and how to run it locally.
- **Post-review addition (user request):** added a `Makefile` (`build`, `vet`, `fmt`, `fmt-check`, `lint`, `test`, `test-unit`, `env`, `up`, `down`, `run`, `help`) as the standard command surface for this Go project — analogous to `package.json` scripts in Node. Written for GNU Make 3.81 (macOS's default) specifically, avoiding `.ONESHELL` and other 3.82+-only features. Added a `testing.Short()` guard to the Docker-backed integration test so `make test-unit`/`go test -short` meaningfully skips it. `.github/workflows/ci.yml` and `README.md` updated to use the Makefile targets instead of raw `go` commands.
- Added goose migrations `0001` (customers, accounts) and `0002` (idempotency_keys), wired via `postgres.Migrate` using goose's `Provider` API against a `*sql.DB` borrowed from the `pgxpool.Pool` via `pgx/v5/stdlib`.
- Generated `internal/adapter/api/server.gen.go` from `api/openapi.yaml` via `oapi-codegen` v2 (`std-http-server`, `models`, `embedded-spec`); the OpenAPI schema is the single source of truth for the `ProblemDetails` (RFC 9457) and `Customer` shapes — no hand-duplicated types.
- **Architecture correction found during implementation:** the original plan would have had `internal/adapter/postgres` implement interface types (`Tx`, `TxBeginner`, `IdempotencyStore`, `StoredResponse`, `StoredEntry`, `ErrKeyConflict`) defined in `internal/adapter/api`, which would have required `postgres` to import `api` — a direct violation of AD-1/AD-2 ("adapters import core, never each other"). Fixed by moving these five cross-cutting types into `internal/core/ports.go` alongside `CustomerRepository`, documented there as a deliberate, narrow exception: they represent architectural invariants (AD-4's one-transaction-per-change, AD-5's idempotency-by-constraint) rather than ledger domain concepts, but core is the only package both adapters may import.
- **Schema addition found during implementation:** `idempotency_keys` needed a `response_content_type` column beyond the story's original schema sketch — without it, a *replayed* response (read back from Postgres) would silently lose its `Content-Type` header even though AC2's byte-for-byte body fidelity still held. Added to migration `0002` with a comment explaining why.
- The generated `ServerInterfaceWrapper` performs its own required-header validation for `Idempotency-Key` and would return a plain-text 400 by default; overrode `StdHTTPServerOptions.ErrorHandlerFunc` in `cmd/walletd/main.go` to route through `WriteProblem` so every error response — including the generator's own — is RFC 9457 `problem+json`, consistent with Dev Notes.

## Dev Agent Record

### Agent Model Used

claude-fable-5

### Debug Log References

None — no debugging session artifacts beyond the red/green test runs recorded in Completion Notes.

### Completion Notes List

- TDD followed throughout: every source file has a corresponding test file written first and confirmed failing (red) before implementation (green) — `internal/core/create_customer_test.go`, `internal/adapter/api/problem_test.go`, `internal/adapter/api/middleware_auth_test.go`, `internal/adapter/api/middleware_idempotency_test.go`.
- Full regression suite green: `go build ./...`, `go vet ./...`, `go test ./...` — 21 tests across 2 packages, including a real-Postgres integration test (`testcontainers-go`, `postgres:18`) exercising AC1, AC2 (both the same-body-replay and different-body-409 paths), AC3, AC4 (verified directly via `information_schema` that no `balance` column exists, and that exactly 4 accounts with the correct (chain, asset) pairs were created), and AC5.
- Toolchain installed locally to run this story's tests for real rather than only reasoning about them: Go 1.26.5 (via Homebrew, matching the pinned spine version) and `oapi-codegen` v2.7.2 (matching the spine's verified version).
- Dependency versions actually resolved (all match the story's "Latest technical specifics" research): `google/uuid` v1.6.0, `jackc/pgx/v5` v5.10.0, `pressly/goose/v3` v3.27.2, `oapi-codegen/runtime` v1.5.0, `testcontainers-go` v0.43.0.
- No new dependencies beyond what the story anticipated, except `github.com/getkin/kin-openapi` (a transitive requirement of oapi-codegen's `embedded-spec: true` option, not a direct choice) and `github.com/testcontainers/testcontainers-go` + its `postgres` module (explicitly sanctioned by Task 7 as a dev-agent choice for the integration test).
- All Story 1.1 Dev Notes constraints honored and spot-checked: no `balance` column on `accounts` (verified via `information_schema` in the integration test, not just by omission); no outbox table; no CLI framework; `internal/core` imports nothing from `internal/adapter/*` (verified by `go build`/`go vet` succeeding with the import graph as designed — Story 2.1 will add the CI-enforced mechanical check).

### File List

**New files:**
- `go.mod`, `go.sum`
- `.env.example`
- `.github/workflows/ci.yml`
- `api/openapi.yaml`
- `cmd/walletd/main.go`
- `deploy/compose/docker-compose.yml`, `deploy/compose/Dockerfile`
- `internal/core/customer.go`, `internal/core/create_customer.go`, `internal/core/create_customer_test.go`, `internal/core/ports.go`
- `internal/adapter/api/customers.go`, `internal/adapter/api/middleware_auth.go`, `internal/adapter/api/middleware_auth_test.go`, `internal/adapter/api/middleware_idempotency.go`, `internal/adapter/api/middleware_idempotency_test.go`, `internal/adapter/api/problem.go`, `internal/adapter/api/problem_test.go`, `internal/adapter/api/integration_test.go`, `internal/adapter/api/oapi-codegen-config.yaml`, `internal/adapter/api/server.gen.go` (generated — do not hand-edit)
- `internal/adapter/postgres/tx.go`, `internal/adapter/postgres/customer_repo.go`, `internal/adapter/postgres/idempotency_store.go`, `internal/adapter/postgres/migrate.go`, `internal/adapter/postgres/migrations/0001_create_customers_and_accounts.sql`, `internal/adapter/postgres/migrations/0002_create_idempotency_keys.sql`

**Modified files:**
- `_bmad-output/implementation-artifacts/sprint-status.yaml` (status transitions for this story and epic-1)
