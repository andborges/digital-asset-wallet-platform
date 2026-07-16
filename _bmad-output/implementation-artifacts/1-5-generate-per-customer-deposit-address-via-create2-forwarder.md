---
baseline_commit: ae53053dc46b8e1c1dfd78de125d959aa419946f
---

# Story 1.5: Generate Per-Customer Deposit Address via CREATE2 Forwarder

Status: done

<!-- Note: Validation is optional. Run validate-create-story for quality check before dev-story. -->

## Story

As an application team,
I want each customer to have a single deposit address that works identically on every supported EVM chain,
so that my users can deposit ETH or USDC on Base or Arbitrum without me managing per-chain addresses.

## Acceptance Criteria

1. **Given** a customer is created, **when** the platform provisions their deposit address, **then** it computes `CREATE2(factory, salt, forwarder init code)` with `salt` = the customer UUID left-padded to bytes32, persists the resulting address once, and the same address is valid on both Base and Arbitrum (FR6, AD-8).
2. **Given** the address is provisioned, **when** I `GET /v1/customers/{id}`, **then** the response includes the deposit address as an attribute of the customer resource (FR7) — it is never a route parameter.
3. **Given** the deterministic factory deployer (`0x4e59b44847b379578588920cA78FbF26c0B4956C`) is not yet deployed on a target chain, **when** the platform starts up against that chain, **then** it verifies the factory's presence and fails startup loudly rather than serving addresses that could collide or diverge (AD-8).
4. **Given** an address has already been persisted for a customer, **when** the same customer's address is requested again, **then** the stored value is returned — the address is never re-derived on the fly.
5. **Given** the Go salt-encoding implementation and the Foundry/Solidity CREATE2 computation, **when** run against the same input test vectors in CI, **then** both produce byte-identical addresses (AD-8 cross-language pinning).

**Architectural decisions this story must make (not explicit in the epics AC text, but required to satisfy AC1–AC5 honestly — this story introduces `internal/adapter/evm/` and `contracts/` for the first time, so these choices set precedent for every later story that touches them):**

- **Only the salt scheme lives directly in `internal/core`; the CREATE2 hash itself is derived through a port implemented in `internal/adapter/evm`, using `go-ethereum`'s own `crypto.CreateAddress2` — not hand-rolled with a second crypto library.** The capability map pins "core (salt scheme)" — the pure, crypto-free transformation from a customer UUID to its bytes32 salt (`customerSalt`) stays in `internal/core`, no port needed, since it's just byte layout. But the actual CREATE2 hash (`keccak256(0xff ++ factory ++ salt ++ initCodeHash)[12:]`) is a formula where a single byte-order or padding bug permanently corrupts every customer address ever issued — this is exactly the kind of correctness-critical primitive this codebase always routes through a port to an adapter, the same as every repository (`CustomerRepository`, `BalanceRepository`, `TransferRepository`), rather than reimplementing by hand. `internal/core` therefore defines `DepositAddressDeriver` (one method: `DeriveAddress(salt [32]byte) (string, error)`), and `internal/adapter/evm` implements it using go-ethereum's `crypto.CreateAddress2` — the same battle-tested helper the wider Ethereum ecosystem already relies on for this exact formula — confirm the exact function name/package path (`github.com/ethereum/go-ethereum/crypto`) against the vendored `v1.17.x` during implementation rather than assuming it from memory. This keeps `go-ethereum` confined to `adapter/evm` (AD-1's blanket import clause) *and* keeps `core` free of adapter imports (AD-1/AD-2's more fundamental "core imports no adapter" rule) *and* avoids owning a hand-rolled reimplementation of a formula whose correctness is irreversible once wrong. `golang.org/x/crypto` (already an indirect dependency) is not needed at all under this design — `core`'s only salt-encoding code needs no crypto import, and `go-ethereum` itself brings whatever Keccak implementation `crypto.CreateAddress2` needs internally.
- **`internal/adapter/evm` has two distinct jobs in this story, only one of which touches a live chain.** (1) `DepositAddressDeriver`'s implementation is pure computation over fixed constants (canonical deployer address, platform factory address, forwarder init-code hash) — it needs zero RPC access and is confined to this package purely because of the `go-ethereum` import rule, not because it's "chain-specific" in the RPC sense. (2) The startup deployer-presence check genuinely is chain-specific (an `eth_getCode` RPC call against a specific chain endpoint) and is where `go-ethereum`'s `ethclient` (pinned `v1.17.x` per the architecture spine's Stack table) is actually exercised against a live connection. Keep these as separate files (see Task 4) so it's obvious at a glance which parts need a chain and which don't.
- **Story 2.1, not this story, adds the CI import-boundary check.** `ci.yml` already carries the comment *"Story 2.1 adds an import-boundary check to this same job — do not create a second workflow file for it."* This story must still get the AD-1 direction right by hand (`internal/core` imports neither `go-ethereum` nor anything under `internal/adapter/evm`) — it just isn't mechanically enforced in CI until 2.1. Don't pull that scope forward, and don't skip the discipline just because nothing checks it yet.
- **This story does not deploy anything on-chain — not the canonical deployer (already live on Base/Arbitrum and their testnets, per the architecture spine and solution design §8) and not the platform's own Factory or any customer's Forwarder.** Deposit-address provisioning (AC1) is pure off-chain computation: a CREATE2 address is the deterministic address a *not-yet-deployed* contract *would* have — that's the entire point of "counterfactual" in AD-8's name. The platform's Factory contract gets deployed, and a given customer's Forwarder gets deployed, only when Story 3.6 (`Sweep Forwarder Balances to Treasury`) first needs to flush funds from it. AC3's startup check verifies only the *canonical deployer's* presence (the precondition for that future deployment to be trustworthy), not the platform's own Factory.
- **The platform's own Factory contract address is itself computed via the same `go-ethereum` CREATE2 helper, not hand-copied.** Deploying Factory through the canonical deployer with a fixed factory-salt (e.g. `bytes32(0)`) and the Factory's own init code makes its address identical on every chain (AD-8) — exactly like a customer's forwarder address. Compute it in `internal/adapter/evm` by calling `crypto.CreateAddress2` a second time (canonical deployer as the "factory" input, the fixed factory-salt, Factory's compiled init-code hash) rather than deploying once to anvil and hand-copying the resulting address into a constant — one code path (go-ethereum's own helper) for every CREATE2 address in this system, no room for a manual transcription error on a value that is genuinely irreversible once customers exist against it.
- **The Forwarder contract's bytecode must be final now, even though its `flush`/`flushTokens` logic isn't exercised until Story 3.6.** AD-8 is explicit that changing factory address, init code, or salt scheme "changes every customer address" — so whatever `contracts/src/Forwarder.sol` compiles to at the end of *this* story is what every deposit address in the system will be permanently derived from. Model it on BitGo's ForwarderV4 (the architecture's own named reference): persistent (EIP-6780-safe, no `selfdestruct`-and-redeploy), a `receive()` for native ETH, and a real (not stubbed) flush mechanism for ETH and ERC-20 balances to a fixed treasury target — even though nothing calls it yet. Do not ship a placeholder contract "to be finished in Epic 3"; finishing it later would change the bytecode and silently invalidate every address issued between now and then.
- **The deposit address is computed and persisted in the *same* Postgres transaction as customer + account provisioning (AD-4), not a second write.** `CustomerRepository.CreateCustomer` already receives `customer` and `accounts` and writes both under the transaction opened by `IdempotencyMiddleware`; this story extends that one call to also insert the computed deposit address row, matching the existing "customer + accounts, one transaction" precedent from Story 1.1 exactly rather than introducing a second round-trip or a separate repository interface for something that must commit atomically with the row it depends on (the address is a pure function of the customer id the same transaction is about to create).
- **No new "pending" or "not yet computed" state for the deposit address.** Because deriving it is pure math over already-known constants (no RPC, no chain state), there is no reason it can't be computed and persisted synchronously inside the existing `CreateCustomer` call — `depositAddress` is a required field on the `Customer` API schema from the moment a customer exists, never optional/nullable pending a later step.

## Tasks / Subtasks

- [x] **Task 1: `contracts/` — Foundry project with Factory + Forwarder** (AC: 1, 5)
  - [x] `contracts/foundry.toml`, `contracts/src/Forwarder.sol`, `contracts/src/Factory.sol`: bootstrapped via `forge init contracts --no-git` (Counter.sol/.t.sol/.s.sol scaffold removed). Foundry `v1.7.1` installed via `foundryup` (matches the spine's `v1.7.x`). `solc_version = "0.8.36"` pinned explicitly in `foundry.toml` — auto-detection alone did not resolve/install it, but `forge build --use 0.8.36` confirmed the version exists and works; pinning it makes plain `forge build`/`forge test` reproducible without extra flags.
  - [x] `Forwarder.sol`: persistent (no `selfdestruct`), `receive()` for native ETH, ~~`flush(address payable)`/`flushToken(IERC20, address)` gated by an `immutable owner` set from `msg.sender` at construction (the deploying Factory)~~ **[SUPERSEDED by the 2026-07-16 rework — the owner-gated design locked funds permanently (finding ①); as shipped, `flush()`/`flushToken(IERC20)` are permissionless and send only to the immutable `TREASURY` constant. See "Review Findings — Rework Resolution (2026-07-16)".]** — a no-argument constructor deliberately, so the creation bytecode (and therefore the init-code hash every address depends on) never varies per deployment.
  - [x] `Factory.sol`: `deploy(bytes32 salt)` using `new Forwarder{salt: salt}()`, plus `computeAddress(bytes32 salt) public view` mirroring the CREATE2 formula on-chain.
  - [x] `contracts/test/CreateAddressVectors.t.sol`: two Foundry tests — `testPlatformFactoryAddressMatchesExpected` (Factory's own CREATE2 address from the canonical deployer + fixed factory-salt `bytes32(0)`) and `testForwarderAddressesMatchExpected` (3 fixed sample customer UUIDs against a fixed test factory address, via `vm.etch` placing the real `Factory` bytecode at a deterministic address so the actual contract logic is exercised, not reimplemented). Both assert hardcoded expected addresses — the Solidity half of AC5.
  - [x] Init-code hashes and the derived platform factory address were captured by temporarily adding `console2.log` calls to the test and running `forge test --match-test testPrintVectors -vv` once, then hardcoding the printed values as literals in both this test and `internal/adapter/evm/address.go`/`address_test.go` (Task 4) — never hand-typed independently. **[Note (2026-07-16 re-review): the values cited by this subtask predate the rework's bytecode change and were regenerated; `testPrintVectors` is now a permanently committed test, and the regeneration procedure is documented in `contracts/README.md`.]** **Real bug caught in the process:** the architecture spine's canonical-deployer address citation, `0x4e59b44847B379578588920cA78FbF26c0B4956C`, has an invalid EIP-55 checksum (solc rejected it as a compile error) — the correct checksum for the same 20 bytes is `0x4e59b44847b379578588920cA78FbF26c0B4956C` (single-character case difference). Fixed everywhere in this story file and in the contracts; must also be used correctly in the Go constant (Task 4).

- [x] **Task 2: `internal/core` — salt scheme + `DepositAddressDeriver` port** (AC: 1, 5)
  - [x] `internal/core/deposit_address.go`: `customerSalt(customerID string) ([32]byte, error)` — pure, no I/O, no crypto import.
  - [x] `internal/core/deposit_address_test.go`: TDD (red confirmed via `undefined: customerSalt` before implementation) — malformed UUID rejected, byte-exact zero-padding, distinct UUIDs → distinct salts.
  - [x] `internal/core/ports.go`: added `DepositAddressDeriver` (`DeriveAddress(salt [32]byte) (string, error)`), doc-commented with the rationale.
  - [x] `internal/core/customer.go`: added `Customer.DepositAddress string` (required).
  - [x] `internal/core/ports.go`: extended `CustomerRepository.CreateCustomer` to `(ctx, customer, accounts, depositAddress string) error`.
  - [x] `internal/core/create_customer.go`: `CreateCustomer` now takes `addressDeriver DepositAddressDeriver`; `Execute` computes the salt, derives the address, sets it on `Customer`, passes it to `repo.CreateCustomer`. TDD: `create_customer_test.go` extended with a `fakeDepositAddressDeriver`, red confirmed (signature mismatch) before implementation. All 5 use-case tests green, including derivation-error propagation and salt-correctness assertions.

- [x] **Task 3: Postgres — persist the deposit address atomically with customer creation** (AC: 1, 4)
  - [x] `internal/adapter/postgres/migrations/0004_create_deposit_addresses.sql`: `deposit_addresses (customer_id uuid PRIMARY KEY REFERENCES customers(id), address text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (address))`.
  - [x] `internal/adapter/postgres/customer_repo.go`: `CreateCustomer` now takes `depositAddress string` and queues its insert in the same batch/transaction as the account inserts (one commit, AD-4).
  - [x] `internal/adapter/postgres/customer_reader.go` (new): `CustomerReader` implementing `core.CustomerReader.GetCustomer` via a single joined `SELECT` (`customers` JOIN `deposit_addresses`) — no existence-check-then-query needed since every customer has exactly one deposit-address row by construction.

- [x] **Task 4: `internal/adapter/evm` — `DepositAddressDeriver` implementation, chain config, startup deployer-presence check** (AC: 1, 3, 5)
  - [x] `internal/adapter/evm/address.go`: implements `core.DepositAddressDeriver` using `go-ethereum` `v1.17.4`'s `crypto.CreateAddress2(b common.Address, salt [32]byte, inithash []byte) common.Address` (confirmed via `go doc` before use). `canonicalDeployerAddress`, `forwarderInitCodeHash`/`factoryInitCodeHash` (from Task 1), `platformFactoryAddress` computed via the same `create2Address` helper against the canonical deployer + fixed factory-salt `bytes32(0)`.
  - [x] `internal/adapter/evm/address_test.go`: TDD (red confirmed: `undefined: platformFactoryAddress`/`create2Address` etc. before implementation). Table-driven tests against the same fixed vectors as `contracts/test/CreateAddressVectors.t.sol` — all pass, byte-identical to the Solidity side (AC5 cross-language pinning, genuinely verified both directions).
  - [x] `internal/adapter/evm/chain.go`: minimal `Chain{Name, RPCURL}` config type.
  - [x] `internal/adapter/evm/deployer.go`: `VerifyDeployerPresence(ctx, chain)` (dials via `ethclient.DialContext`, confirmed signature via `go doc`) delegating to a testable `verifyDeployerPresence(ctx, codeAtClient, chainName)` behind a minimal fake-able interface.
  - [x] `internal/adapter/evm/deployer_test.go`: fake-backed unit tests for both branches (present/absent/RPC-error), TDD red-confirmed first. Plus `TestVerifyDeployerPresence_RealAnvil`, which shells out to a locally-installed `anvil` (Foundry ships no testcontainers module for it, so this follows the documented fallback) — **passes for real**, confirming anvil's genesis-preinstalled canonical deployer.
  - [x] `cmd/walletd/main.go`: wiring deferred to this task's own commit alongside Task 5 (both need the same `main.go` edit); see Task 5's final subtask.
  - **Real bugs caught during this task, corrected in place:** (1) the canonical deployer's EIP-55 checksum typo (see Task 1) also had to be fixed in this package's constant. (2) A hand-transcription error while copying the `forge`-printed `forwarderInitCodeHash`/`factoryInitCodeHash` hex strings dropped a trailing hex digit from each (63 chars instead of 64) — caught immediately by `mustHexToHash32`'s length check panicking at package `init()`, exactly the kind of failure this story's "never hand-typed independently" discipline exists to catch. Fixed by re-extracting the values programmatically from `forge test`'s JSON output instead of reading terminal-wrapped text by eye.

- [x] **Task 5: OpenAPI + generated handlers — `GET /v1/customers/{id}`** (AC: 2)
  - [x] `api/openapi.yaml`: added `depositAddress` (string, required) to `Customer`; new path `/customers/{id}` (`operationId: getCustomer`), same response shape as every other per-customer route.
  - [x] Regenerated `internal/adapter/api/server.gen.go` via `oapi-codegen v2.7.2` (confirmed from the existing generated file's header, not re-guessed) — confirmed generated signature `GetCustomer(w, r, id openapi_types.UUID)` and `Customer.DepositAddress string` empirically via the regenerated output.
  - [x] `internal/core/ports.go`: `CustomerReader` port (built alongside Task 2, above).
  - [x] `internal/core/get_customer.go`: `GetCustomer` use case (built alongside Task 2).
  - [x] `internal/adapter/postgres/customer_reader.go`: `CustomerReader` implementation (built alongside Task 3).
  - [x] `internal/adapter/api/customers.go`: `GetCustomer` handler added; `CreateCustomer`'s response now also includes `DepositAddress`.
  - [x] `cmd/walletd/main.go`: wired `postgres.NewCustomerReader` + `core.NewGetCustomer`, extended `NewServerInterface`, added `BASE_RPC_URL`/`ARBITRUM_RPC_URL` required env vars, and the AC3 startup deployer-presence check (loops `evm.VerifyDeployerPresence` over both configured chains before serving, returning an error that triggers the existing `os.Exit(1)` path on failure). `integration_test.go`'s `newTestHandler` updated to match the new wiring.

- [x] **Task 6: Tests** (cross-cutting; also see per-task test subtasks above)
  - [x] `internal/adapter/api/integration_test.go` extended: AC1 (well-formed, EIP-55-checksummed `depositAddress` on creation, verified round-tripping through `common.HexToAddress(...).Hex()`, plus a direct Postgres check that exactly one `deposit_addresses` row exists matching the returned address — proving the same-transaction write), AC2 (new `TestGetCustomer_EndToEnd`: `GET /v1/customers/{id}` includes the address), AC4 (repeated `GET` calls return byte-identical addresses), 404 for an unknown id, 401 for a missing bearer token.
  - [x] Cross-language pinning (AC5): the same fixed CREATE2 vectors are asserted independently in `contracts/test/CreateAddressVectors.t.sol` (Solidity, via the real `Factory` contract) and `internal/adapter/evm/address_test.go` (Go, via `go-ethereum`'s `crypto.CreateAddress2`) — both pass, genuinely proving byte-identical addresses. Added `make contracts-build`/`make contracts-test` targets. `ci.yml` updated: added the `foundry-rs/foundry-toolchain@v1` step (pinned `v1.7.1`) before `make build`/`make lint`/`make test` (the real-anvil deployer test needs `anvil` on PATH) and added a `make contracts-test` step after them.

## Dev Notes

### Architecture patterns and constraints this story must follow

- **Hexagonal boundary (AD-1, AD-2), sharpened by this story.** This is the first story to introduce `internal/adapter/evm/`. The rule is not just "adapters import core, never each other" — it's specifically that **no `go-ethereum` import and no chain-ID reference exists anywhere outside `internal/adapter/evm`**. The CREATE2 hash computation is routed through a new `DepositAddressDeriver` port for exactly this reason (see Architectural Decisions above) — the same pattern as every repository port already in this codebase, not a special case.
- **One transaction per state change (AD-4).** The deposit address row is not a separate write — it lands in the same transaction as `INSERT INTO customers` / `INSERT INTO accounts`, exactly like Story 1.1 made customer+accounts atomic.
- **Immutability is the whole point of AD-8.** Factory address, forwarder init code, and salt scheme are described as immutable *once live* — this story is what makes them live. Get the Forwarder's real (non-placeholder) bytecode right the first time; there is no "fix it in a later story" path that doesn't also mean "every address issued before the fix is now wrong."
- **Auth applies unconditionally (NFR15, AD-14).** The new `GET /v1/customers/{id}` route is not an exception.
- **Money/ID/time conventions are unchanged** — this story adds no money movement and no new ledger accounts; `depositAddress` is a string, not money, and needs no `NUMERIC`/`big.Int` handling.

### Source tree components this story creates or touches

```text
digital-asset-wallet-platform/
  contracts/                            # NEW — Foundry project (bootstrap via `forge init`)
    foundry.toml
    src/Factory.sol                     # NEW
    src/Forwarder.sol                   # NEW — final bytecode; see Architectural Decisions
    test/*.t.sol                        # NEW — cross-language CREATE2 test vectors (AC5)
  internal/core/
    deposit_address.go                  # NEW — pure salt encoding only (no crypto import)
    deposit_address_test.go             # NEW
    customer.go                         # MODIFY — Customer.DepositAddress field
    ports.go                            # MODIFY — CustomerRepository.CreateCustomer signature + new DepositAddressDeriver + CustomerReader ports
    create_customer.go                  # MODIFY — derive (via port) + attach the deposit address
    get_customer.go                     # NEW — GetCustomer use case (GET /customers/{id})
  internal/adapter/
    postgres/
      migrations/0004_create_deposit_addresses.sql   # NEW
      customer_repo.go                  # MODIFY — insert deposit_addresses row in the same tx
    evm/                                 # NEW package — first chain-specific adapter code
      address.go                        # NEW — DepositAddressDeriver impl (go-ethereum crypto.CreateAddress2)
      address_test.go                   # NEW — Go half of AC5's cross-language vectors
      chain.go                          # NEW — per-chain RPC config
      deployer.go                       # NEW — canonical-deployer presence check (go-ethereum ethclient)
      deployer_test.go                  # NEW
    api/
      server.gen.go                     # MODIFY (regenerated) — do not hand-edit
      customers.go                      # MODIFY — GetCustomer handler + wiring
      integration_test.go               # MODIFY — extend with deposit-address ACs
  api/openapi.yaml                       # MODIFY — GET /customers/{id}, Customer.depositAddress
  cmd/walletd/main.go                    # MODIFY — chain RPC config, startup deployer check, DI wiring
  Makefile                               # MODIFY (likely) — a contracts-test target if needed
  .github/workflows/ci.yml               # MODIFY (likely) — Foundry install step if `make test` needs it
```

No changes to `internal/adapter/signer/`, `internal/adapter/webhook/`, or any `watcher`/`broadcaster`/`recon`/`dispatcher` role — those remain out of scope until Epics 2–3.

### Schema

New migration only (`0004_create_deposit_addresses.sql`); `customers` and `accounts` (`0001`) are unchanged — the deposit address is a new 1:1-related table, not a new column on `customers`, matching the architecture spine's ERD.

### Config

New required env vars for the `api` role: `BASE_RPC_URL`, `ARBITRUM_RPC_URL` (one RPC provider per chain — the `recon` role's independent second provider, AD-12, doesn't exist until Epic 5). Document both in `.env.example` alongside the existing `DATABASE_URL`/`API_BEARER_TOKENS`/`API_LISTEN_ADDR`, following that file's existing comment style.

### Testing standards

- Table-driven Go unit tests for the pure salt encoding (`internal/core`) and for the CREATE2 hash (`internal/adapter/evm`, against `go-ethereum`) — isolated from any chain or database. Together these are AC5's Go half.
- Foundry (`forge test`) for the Solidity half of AC5 — same fixed test vectors as the Go tests, byte-identical addresses asserted on both sides.
- `internal/adapter/evm`'s deployer-presence check: a fake-backed unit test for both branches, plus a real-`anvil` test proving the "present" branch works over real RPC (anvil ships the canonical deployer preinstalled by default, so this needs no manual chain setup).
- Integration test (real Postgres via `testcontainers-go`, unchanged pattern from Stories 1.1–1.4) extended to cover the new `depositAddress` field on customer creation and the new `GET /v1/customers/{id}` route.
- CI (`ci.yml`) currently runs `make build`/`make lint`/`make test` only — if the cross-language test vectors need `forge` installed to run in CI, that's a `ci.yml` change this story owns (the import-boundary check is explicitly *not* this story's job — see Architectural Decisions — but making the tests this story adds actually runnable in CI is).

### Project Structure Notes

- This is the first story to create `internal/adapter/evm/` and `contracts/` — both are named, expected locations in the architecture spine's source tree, not new structure being invented.
- Read `internal/core/customer.go`, `ports.go`, `create_customer.go`, `internal/adapter/postgres/customer_repo.go`, and `internal/adapter/api/customers.go` in full before modifying them — do not guess at their current shape from this story's summary of them above.

### References

- [Source: _bmad-output/planning-artifacts/epics.md#Epic 1: Foundation — Accounts, Ledger & Deposit Addresses / Story 1.5] — canonical ACs
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md#AD-1, AD-8, Stack, Capability → Architecture Map, Deferred] — chain isolation rule, CREATE2/forwarder rule, pinned versions, F2's placement in core, forwarder-internals deferral
- [Source: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md#3.5, 4, 8] — CREATE2-over-HD-wallet rationale, account taxonomy (forwarder-float), deployment environments (anvil vs real testnets)
- [Source: internal/core/customer.go, ports.go, create_customer.go] — current `Customer`/`CustomerRepository`/`CreateCustomer` shape this story extends
- [Source: internal/adapter/postgres/customer_repo.go] — the exact one-transaction insert pattern this story's deposit-address write must join
- [Source: internal/adapter/postgres/migrations/0001_create_customers_and_accounts.sql] — prior migration's style/conventions to follow for `0004`
- [Source: .github/workflows/ci.yml] — the standing comment reserving the import-boundary check for Story 2.1
- [Source: _bmad-output/implementation-artifacts/1-4-list-customer-transaction-history.md] — most recent prior story; see Previous story intelligence below

### Previous story intelligence (from Story 1.4)

- **TDD throughout, and adversarial code review is a standing convention** — expect a fresh-context review pass (Blind Hunter + Edge Case Hunter + Acceptance Auditor) before this story is marked done, same as 1.1–1.4.
- **Resolve unstated architectural gaps explicitly, in-story** — this story's own "Architectural decisions" section above follows that established convention (1.3 did it for lock ordering, 1.4 for pagination correctness and the `status` constant's scope; this story does it for the core/evm split, the no-on-chain-deployment boundary, and bytecode finality).
- **Confirm generator/tool output empirically, never hand-guess it** — 1.3/1.4 confirmed `oapi-codegen`'s exact generated type names by actually running it. This story has two analogous cases: the `forge build` init-code hash (Task 1/2) and whatever `oapi-codegen` generates for `GetCustomer`'s parameters — confirm both for real.
- **1.4's own code review found and fixed a real bug on the first real-infrastructure test run** (a `uuid`-typed SQL parameter bound with an empty string). Expect the same category of surprise here on the first real-`anvil` run of the deployer-presence check or the first `forge build`/Go cross-language comparison — budget time for it rather than assuming the first attempt is correct.
- No dependency additions expected to be silent: this story is the first to add `github.com/ethereum/go-ethereum` (direct, confined entirely to `internal/adapter/evm` — `internal/core` adds no new dependency at all, since its only new code is pure salt encoding). It should appear explicitly in `go.mod`'s direct-requires section, not as an incidental indirect bump.

## Change Log

- Implemented all 5 ACs: a customer's CREATE2 deposit address is computed once at customer creation (salt = customer UUID left-padded to bytes32, per AD-8), persisted atomically with the customer/accounts in one Postgres transaction, exposed via a new `GET /v1/customers/{id}` endpoint, never re-derived on subsequent reads, and verified byte-identical between Go and Solidity via fixed cross-language test vectors.
- Introduced `internal/adapter/evm/` (first chain-specific adapter package) and `contracts/` (first Foundry/Solidity project) — both were empty/nonexistent before this story, per the architecture spine's own scoping.
- Added `core.DepositAddressDeriver` port: the CREATE2 hash formula is implemented in `internal/adapter/evm/address.go` using `go-ethereum`'s own `crypto.CreateAddress2`, not hand-rolled with a second crypto library in core — only the pure, crypto-free salt encoding (`customerSalt`) stays directly in `internal/core`. This keeps `go-ethereum` and chain-ID references fully confined to `internal/adapter/evm` (AD-1) while following the same port/DI pattern as every other repository in this codebase.
- Added `contracts/src/Factory.sol` (CREATE2 deployment of per-customer forwarders) and `contracts/src/Forwarder.sol` (persistent, EIP-6780-safe, `flush`/`flushToken` gated by an immutable owner set from `msg.sender` at construction — a no-argument constructor deliberately, so the creation bytecode never varies per deployment). No on-chain deployment happens in this story (see the story's Architectural Decisions) — Story 3.6 is the first to actually deploy Factory/Forwarder instances.
- Added migration `0004_create_deposit_addresses.sql` (one row per customer, unique on `address` for Epic 2's future watcher lookups) and extended `CustomerRepository.CreateCustomer` to insert it in the same transaction/batch as the customer and account rows (AD-4).
- Added `core.CustomerReader`/`core.GetCustomer` (read-side port + use case) and `postgres.CustomerReader` (single joined `SELECT`, no existence-check-then-query needed) for the new `GET /v1/customers/{id}` route.
- `cmd/walletd/main.go`: added required `BASE_RPC_URL`/`ARBITRUM_RPC_URL` env vars and a startup check (`evm.VerifyDeployerPresence`) that fails loudly if the canonical CREATE2 deployer (`0x4e59b44847b379578588920cA78FbF26c0B4956C`) has no code on either configured chain (AC3).
- Added `github.com/ethereum/go-ethereum` (`v1.17.4`, direct, confined to `internal/adapter/evm`) to `go.mod`.
- Installed Foundry `v1.7.1` (`foundryup`) and bootstrapped `contracts/` via `forge init`; pinned `solc_version = "0.8.36"` explicitly in `foundry.toml` (auto-detection alone didn't resolve/install it, though the version itself is real and works once forced).
- Added `make contracts-build`/`make contracts-test` targets and a `foundry-rs/foundry-toolchain@v1` step to `ci.yml` (pinned to `v1.7.1`) so both the Foundry suite and the Go suite's real-`anvil` deployer-presence test actually run in CI, not just locally.
- **Two real bugs caught and fixed during implementation, both while deriving the CREATE2 constants:**
  1. The architecture spine's own citation of the canonical CREATE2 deployer address, `0x4e59b44847B379578588920cA78FbF26c0B4956C`, has an invalid EIP-55 checksum — solc rejected it as a compile error while building `contracts/`. The correct checksum for the same 20 bytes is `0x4e59b44847b379578588920cA78FbF26c0B4956C` (one character's case differs). Fixed everywhere it's cited: the story file, `contracts/src/Factory.sol`, `contracts/test/CreateAddressVectors.t.sol`, and `internal/adapter/evm/address.go`.
  2. A hand-transcription of the `forge`-printed `Forwarder`/`Factory` init-code hashes (copied by eye from wrapped terminal output) silently dropped one trailing hex digit from each (63 characters instead of 64). Caught immediately by `mustHexToHash32`'s length check panicking at Go package `init()` — exactly the failure mode this story's "never hand-typed independently" discipline exists to catch. Fixed by re-extracting the exact values programmatically from `forge test`'s JSON output instead of reading terminal text by eye.
- Extended `internal/adapter/api/integration_test.go` (real Postgres via `testcontainers-go`, unchanged pattern from Stories 1.1–1.4): a new subtest in `TestCreateCustomer_EndToEnd` asserting a well-formed, EIP-55-checksummed `depositAddress` is returned and persisted exactly once, atomically with the customer; a new `TestGetCustomer_EndToEnd` covering AC2 (address included), AC4 (stable across repeated reads), 404 for an unknown id, and 401 for a missing bearer token.

- **Addressed code review findings — 9 items resolved (Date: 2026-07-16).** Consolidated contract rework (flush design ①③, Foundry pinning P3, SafeERC20 P4, zero-guard P5) with regenerated cross-language CREATE2 vectors, plus build-independent patches (P1 startup timeout, P2 go-ethereum boundary, P6 `errors.Is`) and the ② ziren guardrail comment. Forwarder now flushes permissionlessly to an immutable placeholder `TREASURY` constant (to be finalized at the Story 6.2 key ceremony). Two low-severity items (W1, W2) deferred. See "Review Findings — Rework Resolution (2026-07-16)" above and `deferred-work.md`.

## Dev Agent Record

### Agent Model Used

claude-opus-4-8

### Debug Log References

None beyond the two real bugs caught and fixed during implementation (see Change Log): the canonical-deployer EIP-55 checksum typo (compile-time solc error) and the hand-transcribed init-code-hash digit drop (caught by a Go `init()`-time panic from `mustHexToHash32`'s length check). Both were caught on the first real run of the relevant step, not left latent.

### Completion Notes List

- TDD followed throughout the Go layers: `internal/core/deposit_address_test.go`, `create_customer_test.go` (extended), `get_customer_test.go`, `internal/adapter/evm/address_test.go`, and `deployer_test.go` were each confirmed red (compile failure against not-yet-implemented symbols/signatures) before the corresponding implementation was written.
- The Solidity layer (`contracts/`) was developed test-first in spirit but verified empirically rather than red/green in the strict TDD sense: `forge build`/`forge test` were run immediately after each contract was written, and the cross-language test vectors were derived by running the actual compiled contracts (via `forge test`'s JSON output), never invented ahead of time.
- Full regression suite green on a cache-cleared run: `go build ./...`, `go vet ./...`, `gofmt -l .` (clean), `go test ./...` (including the real-Postgres integration suite and the real-`anvil` deployer-presence test — all passing, no `-short` skips), and `forge test` (2/2 passing) in `contracts/`.
- `oapi-codegen v2.7.2` (same pinned version as Stories 1.1–1.4, confirmed from the existing generated file's header) regenerated `server.gen.go` for real; confirmed the generated `GetCustomer(w, r, id openapi_types.UUID)` signature and `Customer.DepositAddress string` field empirically rather than assuming them.
- Foundry `v1.7.1` installed via `foundryup` (matches the architecture spine's pinned `v1.7.x`); Solidity `0.8.36` confirmed to be a real, working version once explicitly forced (`solc_version` in `foundry.toml`) — auto-detection alone did not resolve/install it for an unknown reason, not investigated further since forcing it works reproducibly.
- No new "pending" state was introduced for the deposit address, per the story's own Architectural Decisions: it is computed synchronously inside `CreateCustomer.Execute` (pure math, no RPC, no chain state) and is a required field on the API response from the moment a customer exists.
- This story does not deploy the platform's Factory or any customer's Forwarder on-chain anywhere (by design — see Architectural Decisions); `contracts/`'s tests exercise the compiled contracts' logic (via `vm.etch` placing real bytecode at a fixed test address) without any real deployment.
- **Code-review rework (2026-07-16):** the Forwarder/Factory bytecode changed (new permissionless-to-fixed-treasury flush design + Foundry reproducibility pinning), so all CREATE2 init-code hashes and expected addresses were regenerated from `forge` output — never hand-typed — and re-pinned in both the Solidity and Go vector suites, which pass byte-identically. The `TREASURY` constant is an explicit, loudly-documented placeholder (`0x…dEaD`); finalizing it to the real hot-wallet address at the Story 6.2 key ceremony is the single follow-up that must precede any production deposit-address issuance (it re-derives every address). No new files were added in the rework; no new module dependencies (`evm.IsChecksummedAddress` reuses the already-vendored `go-ethereum/common`).

### File List

**New files:**
- `contracts/.gitignore`
- `contracts/README.md`
- `contracts/foundry.toml`
- `contracts/src/Factory.sol`
- `contracts/src/Forwarder.sol`
- `contracts/test/CreateAddressVectors.t.sol`
- `contracts/lib/forge-std/` (vendored via `forge init`, no nested `.git`)
- `internal/adapter/evm/address.go`
- `internal/adapter/evm/address_test.go`
- `internal/adapter/evm/chain.go`
- `internal/adapter/evm/deployer.go`
- `internal/adapter/evm/deployer_test.go`
- `internal/adapter/postgres/customer_reader.go`
- `internal/adapter/postgres/migrations/0004_create_deposit_addresses.sql`
- `internal/core/deposit_address.go`
- `internal/core/deposit_address_test.go`
- `internal/core/get_customer.go`
- `internal/core/get_customer_test.go`

**Modified files:**
- `.env.example` (documented `BASE_RPC_URL`/`ARBITRUM_RPC_URL`)
- `.github/workflows/ci.yml` (added Foundry toolchain step + `make contracts-test`)
- `Makefile` (added `contracts-build`/`contracts-test` targets)
- `api/openapi.yaml` (added `GET /customers/{id}`, `Customer.depositAddress`)
- `cmd/walletd/main.go` (chain RPC config, startup deployer check, DI wiring for `DepositAddressDeriver`/`CustomerReader`/`GetCustomer`)
- `go.mod` / `go.sum` (added `github.com/ethereum/go-ethereum v1.17.4` and its transitive dependencies)
- `internal/adapter/api/customers.go` (`GetCustomer` handler; `CreateCustomer` response now includes `DepositAddress`)
- `internal/adapter/api/integration_test.go` (updated `newTestHandler` wiring; added deposit-address assertions to `TestCreateCustomer_EndToEnd`; added `TestGetCustomer_EndToEnd`)
- `internal/adapter/api/server.gen.go` (regenerated — do not hand-edit)
- `internal/adapter/postgres/customer_repo.go` (`CreateCustomer` now persists the deposit address atomically)
- `internal/core/create_customer.go` (derives and attaches the deposit address via the new port)
- `internal/core/create_customer_test.go` (extended with `fakeDepositAddressDeriver`)
- `internal/core/customer.go` (`Customer.DepositAddress` field)
- `internal/core/ports.go` (`DepositAddressDeriver`, `CustomerReader` ports; extended `CustomerRepository.CreateCustomer` signature)
- `_bmad-output/implementation-artifacts/sprint-status.yaml` (status transitions for this story)

## Review Findings

_Adversarial code review 2026-07-15 (Blind Hunter + Edge Case Hunter + Acceptance Auditor). 3 decision-needed, 6 patch, 2 deferred, 3 dismissed as noise._

**Cross-cutting note:** Findings D1, D3, P3, P4, P5 all mutate `contracts/` bytecode. Because AD-8 pins the Forwarder/Factory init-code hashes into `internal/adapter/evm/address.go` **now**, every one of these must be resolved in a single consolidated contract pass *before* any customer address is issued — fixing them piecemeal later would re-derive every address ever issued. Treat the contract layer as not-yet-final until these are settled.

### Decision-Needed

- [x] [Review][Decision → RESOLVED] Deposited funds are permanently unrecoverable — Forwarder's `owner` is the Factory, which exposes no flush entrypoint [contracts/src/Forwarder.sol:29,49; contracts/src/Factory.sol:20-41] — `Forwarder` sets `owner = msg.sender` at construction, and every Forwarder is deployed via `new Forwarder{salt}()` inside `Factory.deploy`, so the owner is always the Factory *contract*. But `Factory` only has `deploy`/`computeAddress` — no owner-gated method that relays a call into a deployed Forwarder's `flush`/`flushToken`. No EOA can call `flush` (reverts "caller is not the owner") and the owning Factory can't either. Story 3.6's sweep would find every customer's balance locked forever. Fixing this (adding a flush-relay to Factory, or rethinking ownership) changes Factory's creation bytecode → changes `platformFactoryAddress` → changes every issued address, so it must be decided now. **Decision:** how should sweep authority be structured on the finalized bytecode?
- [x] [Review][Decision → RESOLVED, not a compromise] `ProjectZKM/Ziren` zkVM runtime in the module graph [go.mod:20; go.sum:11-12] — **Investigated 2026-07-16.** The dep enters via `go-ethereum@v1.17.4`'s `crypto/keccak_ziren.go`, which is gated `//go:build ziren`. The default `crypto/keccak.go` (`//go:build !ziren`) uses standard `golang.org/x/crypto/sha3.NewLegacyKeccak256()`. The `ziren` tag is set nowhere in the Makefile, CI, or source, so `walletd`'s normal build links the standard Keccak — the CREATE2 path never touches the zkVM code. The module still appears in `go.mod`/`go.sum` because Go records source-level imports from build-tagged files regardless of active tags (expected behavior); `v1.17.4` is a genuine published version. **Not a supply-chain compromise.** Residual low-severity operational note (folded into the rework as a guardrail): never build/deploy `walletd` with `-tags ziren`, and consider a CI assertion to that effect, since it would silently swap the money-critical Keccak to third-party code.
- [x] [Review][Decision → RESOLVED] `flush`/`flushToken` send to a caller-supplied address, not the "fixed treasury target" the spec commits to [contracts/src/Forwarder.sol:49,57] — the Architectural Decisions require "a real flush mechanism … to a fixed treasury target"; both functions instead take `to` as a runtime argument with no immutable treasury constant. `onlyOwner`-gated so not a security hole, but a substantive deviation from stated intent, and (like D1) locked in by AD-8 bytecode finality. Entangled with D1 — decide alongside it. **Decision:** immutable treasury constant vs. parameterized destination.

### Patch

- [x] [Review][Patch] No timeout on the startup deployer-presence check — can hang forever instead of "failing loudly" (violates AC3) [cmd/walletd/main.go:91-95; internal/adapter/evm/deployer.go:23] — the loop uses the root `signal.NotifyContext` context (no deadline); `ethclient.DialContext` is lazy over HTTP so the real call is `CodeAt`, which inherits the deadline-less context. A black-holed/stalled RPC host makes `walletd api` block at startup with no error and no serving — the opposite of AC3's fail-loud requirement. Fix: wrap each check in `context.WithTimeout`.
- [x] [Review][Patch] `go-ethereum` imported outside `internal/adapter/evm`, violating AD-1 (the boundary this story explicitly sharpens) [internal/adapter/api/integration_test.go:26] — test imports `github.com/ethereum/go-ethereum/common` for `assertWellFormedDepositAddress`. Test-only, but the spec states the rule absolutely and Story 2.1's import-boundary CI check will trip on it unless it excludes `_test.go`. Fix: dependency-free EIP-55/format assertion, or route the check through the `evm` package.
- [x] [Review][Patch] Foundry build is not reproducible — init-code hash (and thus every address) is fragile [contracts/foundry.toml:1-9] — only `solc_version` is pinned; default `bytecode_hash = "ipfs"` embeds a CBOR metadata trailer (hashing source/comments/paths) into `creationCode`, and the optimizer is unpinned. An innocuous comment edit or a differing optimizer default between dev and CI silently changes `keccak256(type(Forwarder).creationCode)`, invalidating the hardcoded hashes in `address.go` and every derived address. Fix: `bytecode_hash = "none"` and pin `optimizer`/`optimizer_runs`.
- [x] [Review][Patch] `flushToken` reverts on non-standard ERC-20s (e.g. USDT) — sweep permanently broken for those assets [contracts/src/Forwarder.sol:57] — `require(token.transfer(to, amount), ...)` decodes a `bool` return; tokens that return no data revert in the ABI decoder. Current assets (ETH/USDC) are compliant, but the contract is generic and bytecode-final. Fix: SafeERC20-style low-level `call` with optional-return handling.
- [x] [Review][Patch] `flushToken` has no zero-balance guard [contracts/src/Forwarder.sol:57] — calls `transfer(to, 0)` (some tokens revert on this) and emits a misleading `Flushed(..., 0)`. Fix: `if (amount == 0) return;` before transferring.
- [x] [Review][Patch] `err == pgx.ErrNoRows` instead of `errors.Is` [internal/adapter/postgres/customer_reader.go:41] — sibling `idempotency_store.go` uses `errors.Is(err, pgx.ErrNoRows)`; direct `==` turns any future wrapped not-found into a 500 instead of 404. Fix: `errors.Is`.

### Deferred

- [x] [Review][Defer] `GetCustomer`'s INNER JOIN reports a customer as 404 if it has no `deposit_addresses` row; the 1:1 invariant isn't FK-enforced [internal/adapter/postgres/customer_reader.go:34-42] — deferred, unreachable in greenfield (current `CreateCustomer` always writes both atomically); relevant only if pre-migration/partial-write rows ever exist.
- [x] [Review][Defer] Deployer-presence check accepts *any* contract code at the deployer address, not the canonical deployer's specific bytecode [internal/adapter/evm/deployer.go:42] — deferred, hardening beyond AC3's "presence" wording; a `keccak256(code)` comparison would make the guarantee stronger on a forked/misconfigured chain.

### Review Findings — Rework Resolution (2026-07-16)

Consolidated contract-rework pass + build-independent patches (dev-story, decisions confirmed with André):

- **① + ③ (flush design) — RESOLVED via "permissionless → fixed treasury".** `Forwarder.sol` rewritten: removed `owner`/`onlyOwner`/constructor entirely; `flush()`/`flushToken(IERC20)` are now permissionless but can only ever send to an immutable `TREASURY` constant baked into bytecode. Funds can no longer be locked (no owner-relay gap) and cannot be misdirected (fixed destination), matching SOLUTION-DESIGN §7's "a contract that can only flush to treasury". `Factory.sol` unchanged (no relay needed under this model). **`TREASURY` is a documented placeholder (`0x…dEaD`)** — it MUST be finalized to the real hot-wallet address at the Story 6.2 key ceremony; doing so re-derives every address, which is acceptable only pre-production (see the loud contract comment). This is the one remaining follow-up carried out of this story.
- **P3 (Foundry reproducibility) — RESOLVED.** `foundry.toml` now pins `bytecode_hash = "none"` (strips the CBOR metadata trailer) and `optimizer = true` / `optimizer_runs = 200`, so the init-code hash is byte-stable across machines/time.
- **P4 (non-standard ERC-20) — RESOLVED.** `flushToken` uses a SafeERC20-style low-level `call` that tolerates no-return tokens (USDT etc.) instead of a typed `bool` return.
- **P5 (zero-balance guard) — RESOLVED.** Both `flush` and `flushToken` early-return on a zero balance.
- **Cross-language vectors (AC5) regenerated** for the new bytecode and pinned byte-identically in `contracts/test/CreateAddressVectors.t.sol` and `internal/adapter/evm/address_test.go` (both suites pass): factory init-code hash `9f6b39d1…`, forwarder init-code hash `8105f189…`, platform factory `0xCc0939512Fdb0811bD89aB1E13D6bB131AC3e7A7`.
- **P1 (startup hang) — RESOLVED.** `cmd/walletd/main.go` wraps each `VerifyDeployerPresence` probe in a 10s `context.WithTimeout`, so a stalled RPC endpoint fails startup loudly (AC3) instead of hanging.
- **P2 (boundary) — RESOLVED.** Dropped the `go-ethereum/common` import from `integration_test.go`; the EIP-55 check now routes through a new exported `evm.IsChecksummedAddress`, keeping go-ethereum confined to `internal/adapter/evm` (verified: no go-ethereum import anywhere outside that package).
- **P6 (`errors.Is`) — RESOLVED.** `customer_reader.go` uses `errors.Is(err, pgx.ErrNoRows)`.
- **② (Ziren dependency) — RESOLVED as not-a-compromise** (see the checked item above); added a guardrail comment in `address.go` documenting that walletd must never build with `-tags ziren`.
- **Deferred (W1 INNER JOIN 1:1 invariant, W2 deployer bytecode-hash check)** — left deferred; logged in `deferred-work.md`.

Full regression green: `go build/vet ./...`, `gofmt` clean, `go test ./...` (incl. real-Postgres integration + real-anvil deployer test), and `forge test` (2/2) all pass.

### Re-Review Findings (2026-07-16)

_Second adversarial pass (Blind Hunter + Edge Case Hunter + Acceptance Auditor) after the rework. All 9 prior rework resolutions (P1–P6, ①③, ②) independently verified as genuinely present in the code, including an empirical re-run of both vector suites (forge 2/2, Go evm+core green). 1 decision-needed, 10 patch, 4 defer, 8 dismissed as noise._

#### Decision-Needed

- [x] [Review][Decision → RESOLVED: env-configured expected chain IDs] Startup probe never verifies chain identity — a swapped, duplicated, or wrong-chain RPC URL passes the AC3 gate [internal/adapter/evm/deployer.go:42; internal/adapter/evm/chain.go:7; cmd/walletd/main.go:89-92] — `Chain` carries only a name and URL; the probe checks that *some* code exists at `0x4e59…` on whatever chain the URL happens to point at (the canonical deployer is live on most EVM chains, so a wrong endpoint still passes). One `eth_chainId` call against an expected chain ID would make the check mean what it claims — but the expected-ID config is ambiguous: hardcoding Base/Arbitrum Sepolia breaks the `.env.example`-documented anvil-local workflow (chain id 31337) and the eventual mainnet cutover, so it needs a per-chain configurable expected ID (env var? config default with override?). **Decision:** add chain-ID verification, and if so, how should expected IDs be configured?

#### Patch

- [x] [Review][Patch] TREASURY placeholder follow-up is tracked only inside this story file and a contract comment — not in the cross-story tracker [contracts/src/Forwarder.sol:45; _bmad-output/implementation-artifacts/deferred-work.md] — the one item that MUST precede any production deposit-address issuance (replacing `0x…dEaD`, which re-derives every address; missing it burns every swept customer deposit) has no entry in `deferred-work.md`, the file future reviews actually re-read. Add a loud blocking-before-production entry there and a hazard note in `contracts/README.md`.
- [x] [Review][Patch] `evm_version` not pinned in foundry.toml — reproducibility pinning incomplete by its own stated standard [contracts/foundry.toml:21-23] — Foundry's default `evm_version` changes across releases; a dev on a different Foundry than CI's v1.7.1 can emit different bytecode → different init-code hash. The pinned cross-language vectors act as a tripwire (CI fails loudly, no silent corruption), but the failure would be confusing and the fix is one line. Pin `evm_version` to the value the current hashes were generated under and verify `forge test` still passes.
- [x] [Review][Patch] Init-code-hash regeneration procedure references a test that doesn't exist [internal/adapter/evm/address.go:29-35] — the comment cites `forge test --match-test testPrintVectors`, but no such test is committed. Refreshing five mirrored hex constants (where one wrong nibble corrupts every address) currently requires undocumented manual steps. Commit a real print-vectors test in `contracts/test/` and document the procedure in `contracts/README.md`.
- [x] [Review][Patch] Real-anvil test: hardcoded port 8546 + readiness loop drops the `BlockNumber` error [internal/adapter/evm/deployer_test.go:72,88-104] — (a) a port collision on a CI runner or dev machine fails the suite flakily; (b) when `DialContext` succeeds but `BlockNumber` keeps failing, `lastErr` is overwritten with the nil `dialErr`, so the "anvil did not become ready" gate is skipped and the test proceeds against a non-ready node. Use a dynamically chosen free port and track the actual last error.
- [x] [Review][Patch] `contracts/README.md` is untouched Foundry boilerplate with false instructions [contracts/README.md:52] — tells developers to deploy via `script/Counter.s.sol:CounterScript` (doesn't exist) and documents none of what matters: the vector-regeneration procedure, the TREASURY placeholder hazard, why bytecode stability is sacred, or that `forge-std` is deliberately vendored (v1.16.2, no submodule). Rewrite.
- [x] [Review][Patch] AC5 compositional gap: the exact production derivation tuple is never asserted directly [internal/adapter/evm/address_test.go; contracts/test/CreateAddressVectors.t.sol] — forwarder vectors use the arbitrary `TEST_PLATFORM_FACTORY`, so `CREATE2(0xCc09…, customerSalt, forwarderInitCodeHash)` — the exact call `DeriveAddress` makes — is only pinned compositionally. Add one vector against the real platform factory address on both sides.
- [x] [Review][Patch] Migration comment claims a "(address, chain)" lookup the schema can't express, and `address` has no format constraint [internal/adapter/postgres/migrations/0004_create_deposit_addresses.sql:4,12-15] — the table has no chain column (correct — addresses are chain-invariant — but the comment misleads), and `address text NOT NULL` accepts any text. Fix the comment; add a `CHECK (address ~ '^0x[0-9a-fA-F]{40}$')`.
- [x] [Review][Patch] The `ziren` Keccak guardrail is comment-only [internal/adapter/evm/address.go:14-24] — nothing fails if someone builds with `-tags ziren` (which swaps the money-critical Keccak for third-party zkVM code). Add a `//go:build ziren` canary test file that fails loudly under the tag.
- [x] [Review][Patch] `depositAddress` is a bare `type: string` in the OpenAPI contract [api/openapi.yaml:285-290] — the description promises a checksummed EVM address but the schema enforces nothing. Add `pattern: '^0x[0-9a-fA-F]{40}$'` (+ min/maxLength 42).
- [x] [Review][Patch] Story file Tasks 1–2 still describe the superseded owner-gated flush design [this file, Task 1 subtasks] — "gated by an `immutable owner`" and the old vector provenance contradict the 2026-07-16 rework record in the same document; anyone implementing from Tasks alone would rebuild the rejected design. Annotate the stale subtasks as superseded.

#### Defer

- [x] [Review][Defer] Every restart requires both third-party RPC probes to succeed — a transient Base/Arbitrum provider outage blocks restarting an API whose serving paths are pure Postgres [cmd/walletd/main.go:94-107] — deferred: this is exactly what AC3 specifies ("verifies … at startup, fails loudly"), so the code is correct to spec; the availability tradeoff (no retry, no memory of prior success) should be revisited as an ops concern before production (Epic 5/6).
- [x] [Review][Defer] New `GetCustomer` 500 branches forward raw `err.Error()` to clients [internal/adapter/api/customers.go:70,79] — deferred: new instance of the standing platform-wide item (logged in the 1-2, 1-3, and 1-4 reviews; "fix globally, not per-endpoint"); location appended to the existing deferred-work entry.
- [x] [Review][Defer] Idempotency replays of pre-change `CreateCustomer` responses violate the now-required `depositAddress` schema [internal/adapter/api/idempotency.go; api/openapi.yaml:277] — deferred: `idempotency_records` has no TTL, so a stored 201 body from before this story replays without `depositAddress`. Unreachable in a fresh environment; only matters for environments deployed pre-change (flush `idempotency_records` on deploy, or accept). First instance of a general response-shape-evolution-vs-replay question worth a policy.
- [x] [Review][Defer] `integration_test.go` imports `internal/adapter/evm` — adapter→adapter, test-only [internal/adapter/api/integration_test.go] — deferred: this exact shape was endorsed in the rework (test as composition root, like `cmd/walletd`), but Story 2.1's import-boundary CI check must define a `_test.go`/composition-root policy or it will trip on this line.

#### Dismissed (8)

`contracts/lib/forge-std` "missing from the change" (untracked but present on disk, vendored v1.16.2, listed in the File List for commit — reviewers saw only the constructed diff, which deliberately excluded the vendored tree); INNER JOIN 404 for address-less customers (already deferred, W1); presence check accepts any bytecode (already deferred, W2); any-token-can-read-any-customer authz (already deferred since the 1-2 review); `_safeTransfer` reverting on 1–31-byte return data (matches OpenZeppelin SafeERC20 semantics — such a token is broken and fails under OZ too); no upfront RPC-URL format validation (a bad URL still fails loudly at startup within the 10s probe); go-ethereum's dependency surface (a spec-mandated architectural decision with recorded rationale); `.env.example` placeholder URLs failing out of the box (deliberate fail-loud, documented in that file's own comment block).

#### Re-Review Resolution (2026-07-16, same day)

All 11 patch items (10 + the resolved decision) applied and verified; the 4 defers logged in `deferred-work.md`:

- **Chain identity (decision → patched):** `evm.Chain` gains `ChainID`; `VerifyDeployerPresence` now checks `eth_chainId` against it before the code-presence probe. New required env vars `BASE_CHAIN_ID`/`ARBITRUM_CHAIN_ID` (documented in `.env.example` with Sepolia values; anvil = 31337). Unit tests cover the mismatch/error branches; the real-anvil test pins 31337.
- **`evm_version = "osaka"` pinned** in `foundry.toml` (forge 1.7.1's resolved default for solc 0.8.36) — verified byte-neutral: all pinned hashes/addresses unchanged after the pin.
- **`testPrintVectors` is now a committed test** in `CreateAddressVectors.t.sol` (making `address.go`'s comment true), and the full regeneration procedure is documented in the rewritten `contracts/README.md` (which also covers the TREASURY hazard, bytecode-stability rules, and the deliberate forge-std v1.16.2 vendoring).
- **Production-tuple AC5 vectors added on both sides:** `testProductionForwarderAddressesMatchExpected` (Solidity, real `PLATFORM_FACTORY` via `vm.etch`) and `TestDeriveAddress_MatchesForgeProductionVectors` (Go, through `DeriveAddress` itself) pin `CREATE2(0xCc09…, salt, forwarderHash)` byte-identically — extracted via the committed print test, never hand-typed.
- **Anvil test fixed:** dynamic free port instead of hardcoded 8546; readiness loop now tracks the dial OR call error (previously the nil dial error overwrote the call error, skipping the gate); verification gets its own timeout.
- **`ziren` canary:** `internal/adapter/evm/ziren_forbidden.go` (build-tagged `ziren`, deliberate undefined identifier) turns any `-tags ziren` build into a compile error — verified failing.
- **Migration 0004:** address format `CHECK (address ~ '^0x[0-9a-fA-F]{40}$')` added; misleading "(address, chain)" comment corrected (the address is chain-invariant; chain is an attribute of the deposit event).
- **OpenAPI:** `depositAddress` now carries `pattern`/`minLength`/`maxLength` (spec-only; generated code unchanged by construction).
- **TREASURY placeholder** recorded as a "⚠️ BLOCKING BEFORE PRODUCTION" entry in `deferred-work.md` (and in `contracts/README.md`) — Story 6.2 must treat finalizing it as an acceptance criterion.
- **Stale Task 1 text** annotated as superseded (owner-gated flush design, pre-rework vector provenance).

Full regression after all patches: `go build/vet ./...`, `gofmt` clean, `go test ./...` (incl. real-Postgres integration + real-anvil deployer test), `forge test` (4/4) — all green.
