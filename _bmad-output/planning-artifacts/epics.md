---
stepsCompleted: ["step-01-validate-prerequisites", "step-02-design-epics", "step-03-create-stories", "step-04-final-validation"]
inputDocuments:
  - _bmad-output/planning-artifacts/prds/prd-digital-asset-wallet-platform-2026-07-13/prd.md
  - _bmad-output/planning-artifacts/prds/prd-digital-asset-wallet-platform-2026-07-13/addendum.md
  - _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md
  - _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md
---

# digital-asset-wallet-platform - Epic Breakdown

## Overview

This document provides the complete epic and story breakdown for digital-asset-wallet-platform, decomposing the requirements from the PRD and the Architecture Spine into implementable stories. No UX design contract exists — the PRD scopes v1 with no consumer-facing UI (FR/NFR consumers are application teams via API, and operators via a small CLI/authenticated routes).

## Requirements Inventory

### Functional Requirements

FR1: The platform maintains accounts per customer with per-asset balances.
FR2: Applications can query a customer's current balances per asset and chain.
FR3: Applications can list a customer's transaction history (deposits, withdrawals, internal transfers) with status.
FR4: Customer-to-customer internal transfers move balances ledger-only — atomically, with no on-chain movement — and are idempotent.
FR5: Every balance change is traceable to exactly one cause: an on-chain event, an internal transfer, or an operator adjustment.
FR6: The platform generates one deposit address per customer, reused across all supported EVM chains — attribution is by (address, chain), never address alone.
FR7: Address-to-customer attribution is persisted durably; incoming funds to any issued address are always attributable, including after restarts and re-deploys.
FR8: Chain watchers track incoming transfers to issued addresses across L2 confirmation tiers (sequencer → safe → finalized) on both chains; deposit records carry an explicit lifecycle state.
FR9: Deposits are credited at the finalized tier, flat across both assets and all amounts, via an explicit per-chain/per-asset policy setting.
FR10: Deposits observed but not yet final are visible as pending — queryable by applications with their current tier.
FR11: Supported deposits: native ETH and USDC (ERC-20) on Base and Arbitrum. Transfers of unsupported tokens must not corrupt the ledger.
FR12: The platform detects changes to observed chain history, including sequencer reordering and L1 reorgs affecting batch inclusion.
FR13: Pre-finality deposit records are safely reversed or re-credited when history changes; a credited balance is never reversed by a reorg.
FR14: After watcher downtime or chain outages, the platform rescans missed ranges and recovers without missing or double-processing any deposit.
FR15: Applications request withdrawals through the API with an idempotency key; the requested amount is placed on hold immediately.
FR16: Every withdrawal moves through an explicit state machine (created → approved → signed → broadcast → confirmed/failed) with no ambiguous intermediate states; in-flight withdrawals resume correctly after a crash.
FR17: Withdrawals above a configurable per-asset threshold enter an awaiting-approval state requiring explicit operator approval; withdrawals below proceed automatically.
FR18: Before signing, the platform validates every withdrawal against the v1 policy set: balance covers amount + fee, destination well-formed and not a known-invalid target, threshold routing applied.
FR19: Stuck or failed withdrawals are first-class: terminal failure releases the hold; stuck in-flight withdrawals are surfaced to the operator with a documented resolution path.
FR20: The platform manages nonces and transaction sequencing per hot wallet; consuming applications never handle nonces, gas, or raw transactions.
FR21: The platform estimates fees correctly per chain, accounting for both the L2 execution fee and the amortized L1 data fee.
FR22: Applications can query a fee estimate for a prospective withdrawal before submitting it.
FR23: Every mutating API operation accepts an idempotency key; a duplicate request returns the original result and never causes a second money movement.
FR24: Retries, crashes, and redeliveries never double-apply an operation — ledger effects are exactly-once even though delivery is at-least-once.
FR25: An independent reconciliation process continuously compares the internal ledger against on-chain reality on a defined cycle.
FR26: Any drift raises an alarm within one reconciliation cycle, with enough detail for the operator to investigate.
FR27: Reconciliation detects at minimum: deposits missing internally, internal records without on-chain counterparts, pending-withdrawal mismatches, double-counted internal transfers.
FR28: Reconciliation status and run history are queryable — "reconciliation is green" is an observable fact.
FR29: The platform pushes webhooks for: deposit pending, deposit credited, withdrawal state changes, approval required, reconciliation alerts.
FR30: Webhook delivery is at-least-once with retries and backoff; every event carries a unique event ID so consumers can deduplicate.
FR31: Webhook consumers can verify event authenticity (e.g., signed payloads).
FR32: All chain-specific logic (RPC access, confirmation-tier mapping, fee mechanics, token contracts) is isolated behind a chain-adapter interface.
FR33: Adding a third EVM chain requires changes only inside the chain-adapter layer.
FR34: Supporting an additional ERC-20 token on an already-supported chain is additive work (token registry/configuration), not rework.

### NonFunctional Requirements

NFR1: Zero double-credits and zero duplicate withdrawals — upheld under injected reorgs, request retries, and crash-recovery tests.
NFR2: No acknowledged data is ever lost; every acknowledged API write and every credited deposit survives a crash; in-flight operations resume or fail cleanly.
NFR3: A credited balance is never reversed by chain events.
NFR4: V1 is sized for thousands of deposits/withdrawals per day, with headroom for ~10× bursts.
NFR5: Deposit credit latency is dominated by the finality wait; the platform's own overhead adds no more than 1 minute.
NFR6: Read APIs (balances, history, fee estimates) respond in under 500ms p95 at v1 volume.
NFR7: V1 runs best-effort on a single instance; availability is not an SLA, durability is.
NFR8: After downtime, the platform recovers unattended: watchers rescan completely, in-flight withdrawals resume, no operator reconstruction required.
NFR9: Chain liveness failures are an expected operating condition: the platform degrades explicitly and recovers unattended.
NFR10: Reconciliation runs in two modes — streaming break-detection plus a batch deep pass at least daily, targeting hourly.
NFR11: Every balance change carries its cause; operator actions are logged with actor, timestamp, reason; the audit trail is append-only.
NFR12: A written threat model exists and is reviewed; every high-risk item has a stated mitigation or accepted-risk entry.
NFR13: Signing keys never leave the key-handling boundary; keys and secrets never appear in logs, errors, or API responses.
NFR14: Key generation, backup, and recovery are first-class, documented, tested, and covered by the threat model.
NFR15: The internal API is authenticated; no anonymous surface. Webhook payloads are verifiable by the consumer.
NFR16: Raw transactions are constructed and verified by the platform itself — no third-party signing UI in the path.
NFR17: The operator hears it from the platform first: alerts fire for reconciliation drift, stuck withdrawals, awaiting-approval withdrawals, watcher lag, chain-liveness loss.
NFR18: A deposit is traceable end to end — chain event → ledger entry → webhook — from logs and queries alone.
NFR19: Correctness claims are backed by a test suite that injects reorgs, duplicate requests, crashes mid-state-machine, and RPC failures; runnable by an external reviewer.

### Additional Requirements

Architectural decisions from ARCHITECTURE-SPINE.md that bind epic/story structure (AD-n IDs are stable and should be cited in stories):

- No starter template — greenfield custom Go service; module structure is fixed by AD-1/AD-2 (see source tree below), not a scaffold generator.
- **Paradigm (AD-1, AD-2):** Hexagonal core in `internal/core/` with zero adapter imports; conventional layering inside adapters. One Go binary `walletd` with role subcommands (`api`, `watcher --chain=<c>`, `broadcaster --chain=<c>`, `recon`, `dispatcher`) run as separate OS processes coordinating only through PostgreSQL. CI must enforce the import boundary (no adapter/evm import outside itself).
- **Ledger (AD-3):** Double-entry journal; fixed account taxonomy (customer available, customer hold, platform forwarder-float per chain×asset, platform treasury, fees); balances derived from postings; unique constraint on `(cause_type, cause_id)`.
- **Transactionality (AD-4):** Every observable state change is one Postgres transaction; money changes carry postings + transition + outbox event atomically; state-only changes carry transition + outbox event with no postings.
- **Idempotency (AD-5):** Enforced by DB unique constraints — API idempotency-keys table, chain events on `(chain, tx_hash, log_index)`, persisted watcher cursors per (chain, tier).
- **State machines (AD-6):** Deposit, withdrawal, and sweep machines live once in core; the watcher is sole writer of deposit rows including the credit; API writes withdrawal rows through core; broadcaster writes broadcast/sweep progress.
- **Crediting policy (AD-7):** Policy table keyed (chain, asset); v1 = finalized everywhere; credited is irreversible.
- **Address generation (AD-8):** CREATE2 counterfactual forwarders; salt = customer UUID → bytes32 (pinned, cross-language CI test vectors between Go and Foundry); canonical deterministic deployer `0x4e59b44847B379578588920cA78FbF26c0B4956C`; addresses persisted once, never re-derived; forwarders are persistent contracts (EIP-6780-safe), modeled on BitGo ForwarderV4 — audit trail to be confirmed/commissioned in the contracts epic.
- **Sweeps (AD-9):** Broadcaster is sole creator/owner of sweep records; one in-flight sweep per (chain, forwarder, asset); sweep postings move forwarder-float → treasury and never touch customer accounts.
- **Signing (AD-10):** Signer port with one KMS adapter implementation used against real AWS KMS in prod and LocalStack KMS locally (endpoint override, same code path); software signer behind the same port for unit tests / LocalStack fallback. Threat model must ratify this mechanism.
- **Broadcasting (AD-11):** Exactly one broadcaster process per chain; enforced by a Postgres advisory lock at startup, not deployment discipline; nonces allocated from persisted per-chain state.
- **Reconciliation (AD-12):** Recon uses a different RPC provider than watchers/broadcasters; writes only its own tables + alert outbox events, never ledger/deposit/withdrawal/sweep rows; also owns operational monitoring (watcher lag, chain liveness, stuck withdrawals, approval-queue age).
- **Webhooks (AD-13):** Dispatcher is the only webhook sender; reads outbox rows, delivers at-least-once with backoff, HMAC-SHA256 signed, event ID = outbox row ID.
- **API (AD-14):** REST/JSON, spec-first OpenAPI, generated handlers (oapi-codegen, stdlib ServeMux target); every mutating route requires `Idempotency-Key` + authentication.
- **Degraded mode (AD-15):** Explicit per-chain liveness status; on degradation, deposits stay pending and withdrawals queue (never reject for liveness); broadcaster holds; recovery is unattended cursor rescan.
- **Stack (pinned in spine, verified 2026-07-14):** Go 1.26.x, PostgreSQL 18, go-ethereum v1.17.x, jackc/pgx v5, goose v3, oapi-codegen v2, Foundry v1.7.x, Solidity 0.8.36, AWS SDK for Go v2, Docker Compose v5, LocalStack (Hobby/free).
- **Conventions:** integer base units only for money (`NUMERIC(78,0)`/`*big.Int`, no floats); UUIDv7 IDs; RFC 3339 timestamps; RFC 9457 problem+json errors; `log/slog` structured JSON logs carrying entity UUIDs; 12-factor env config; static bearer tokens for v1 API auth; goose SQL migrations.
- **Environments:** test/CI = Compose + anvil (fork mode, `anvil_reorg`) + software signer, no live-testnet dependency; local runtime (v1's actual operating environment) = Compose + LocalStack (AWS/KMS emulation) + real Base Sepolia/Arbitrum Sepolia over two independent RPC providers; prod (documented, not built in v1) = AWS EC2 + real KMS.
- **Deliverables outside code:** written threat model, operator runbook (stuck-withdrawal resolution, key ceremony, backup/DR), both with the same status as code (NFR12, NFR14, FR19).
- **Operator surface (deferred detail, confirmed scope):** operator-authenticated REST routes + a small CLI — no dedicated UI in v1.
- **Deferred to their owning epic, not to be re-decided ad hoc:** forwarder contract internals (flush function, access control) against the BitGo reference; RPC vendor selection; alert transport (log/email/Slack); fee-estimation caching policy; Postgres-in-stack-vs-RDS revisit trigger.

### UX Design Requirements

Not applicable — the PRD scopes v1 with no consumer-facing UI (FR2/FR3/FR22/FR28 etc. are served via REST API; the operator surface is authenticated REST routes + a small CLI per the architecture's Deferred section, not a UI deliverable).

### FR Coverage Map

FR1: Epic 1 - Accounts per customer with per-asset balances
FR2: Epic 1 - Query customer balances per asset and chain
FR3: Epic 1 - List customer transaction history with status
FR4: Epic 1 - Ledger-only internal transfers, atomic and idempotent
FR5: Epic 1 - Every balance change traceable to exactly one cause
FR6: Epic 1 - One deposit address per customer, reused across EVM chains
FR7: Epic 1 - Durable address-to-customer attribution
FR8: Epic 2 - Chain watchers track deposits across confirmation tiers
FR9: Epic 2 - Credit at finalized tier via policy table
FR10: Epic 2 - Pending deposits visible with current tier
FR11: Epic 2 - Supported deposits (ETH/USDC); unsupported tokens don't corrupt ledger
FR12: Epic 2 - Detect chain history changes (sequencer reorder, L1 reorg)
FR13: Epic 2 - Pre-finality records safely reversed/re-credited; credited is irreversible
FR14: Epic 2 - Rescan missed ranges after downtime without loss or double-processing
FR15: Epic 3 - Withdrawal requests with idempotency key; immediate hold
FR16: Epic 3 - Explicit withdrawal state machine; resumes after crash
FR17: Epic 3 - Threshold-based awaiting-approval routing
FR18: Epic 3 - Pre-signing policy validation
FR19: Epic 3 - Stuck/failed withdrawals as first-class states with resolution path
FR20: Epic 3 - Nonce and transaction sequencing per hot wallet
FR21: Epic 3 - Correct per-chain fee estimation
FR22: Epic 3 - Fee estimate query before withdrawal submission
FR23: Epic 1 - Idempotency-key mechanism for every mutating operation (built here, reused in Epic 3)
FR24: Epic 1 - Exactly-once ledger effects under at-least-once delivery (mechanism); exercised again in Epic 3
FR25: Epic 5 - Independent reconciliation comparing ledger to chain
FR26: Epic 5 - Drift alarm within one reconciliation cycle
FR27: Epic 5 - Detects missing deposits, orphan records, pending-withdrawal mismatches, double-counted transfers
FR28: Epic 5 - Queryable reconciliation status and run history
FR29: Epic 4 - Webhooks for deposit/withdrawal/approval/reconciliation events
FR30: Epic 4 - At-least-once delivery with retries, backoff, dedupe-able event ID
FR31: Epic 4 - Verifiable event authenticity (signed payloads)
FR32: Epic 2 - Chain-specific logic isolated behind chain-adapter interface (structural, enforced from first chain-specific code)
FR33: Epic 2 - Adding a third EVM chain touches only the adapter layer (structural compliance criterion)
FR34: Epic 2 - Additional ERC-20 token is additive registry/config work (structural compliance criterion, delivered in Story 2.3)

## Epic List

### Epic 1: Foundation — Accounts, Ledger & Deposit Addresses
Application teams can create customer accounts, query auditable per-asset balances, move funds between customers ledger-only, and obtain a deposit address per customer valid across every supported EVM chain. This epic also stands up the project skeleton (hexagonal core, Postgres/goose, OpenAPI scaffold, CREATE2 factory + forwarder contracts, Docker Compose environments) and the generic idempotency-key mechanism every later mutating endpoint reuses.
**FRs covered:** FR1, FR2, FR3, FR4, FR5, FR6, FR7, FR23, FR24

### Epic 2: Deposit Monitoring, Crediting & Reorg Safety
Deposits sent to issued addresses are detected, tracked through confirmation tiers, credited at finality, and recovered safely across reorgs, sequencer reordering, and watcher downtime. This is where the chain-adapter boundary (FR32) materializes for real, since watchers are the first chain-specific code in the system.
**FRs covered:** FR8, FR9, FR10, FR11, FR12, FR13, FR14, FR32, FR33, FR34

### Epic 3: Withdrawals, Fees & Treasury Sweeps
Application teams request withdrawals with an up-front fee estimate; the platform validates policy, routes above-threshold requests to operator approval, manages nonces per hot wallet, and safely resolves stuck or failed withdrawals. Forwarder balances are swept to treasury as first-class ledger events. Introduces the Signer port (KMS in prod, LocalStack locally) and the single-writer broadcaster.
**FRs covered:** FR15, FR16, FR17, FR18, FR19, FR20, FR21, FR22

### Epic 4: Event Notifications (Webhooks)
Consuming applications stop polling and receive verifiable, deduplicable webhook events in real time for deposits, withdrawals, and approvals. Epics 2 and 3 already write outbox rows atomically with their state changes (AD-4); this epic delivers the dispatcher that ships them.
**FRs covered:** FR29, FR30, FR31

### Epic 5: Independent Reconciliation & Operational Monitoring
Operators get continuous, independent verification that the ledger matches on-chain reality, with drift alarmed within one cycle, plus operational monitoring of watcher lag, chain liveness, and approval-queue age — delivered as alerts through Epic 4's dispatcher.
**FRs covered:** FR25, FR26, FR27, FR28

### Epic 6: Security, Resilience & Portfolio Readiness
The written threat model, key-ceremony/DR/stuck-withdrawal runbook, and the consolidated fault-injection test suite (reorgs, duplicate requests, crashes mid-state-machine, RPC failures) reach the bar of an external reviewer being able to read the threat model, run the tests, and trace a deposit end to end.
**FRs covered:** none new — covers NFR12, NFR14, NFR19, NFR4–6

---

## Epic 1: Foundation — Accounts, Ledger & Deposit Addresses

Application teams can create customer accounts, query auditable per-asset balances, move funds between customers ledger-only, and obtain a deposit address per customer valid across every supported EVM chain. Routes key on the platform's `customer_id` (UUID); the deposit address is an attribute of the customer resource, not a route key, so the identity scheme stays decoupled from the address-derivation mechanism (AD-8).

### Story 1.1: Create Customer & Provision Per-Asset Accounts

As an application team,
I want to create a customer record via the API and have per-asset accounts provisioned automatically,
So that I can begin tracking a customer's balances from day one.

**Acceptance Criteria:**

**Given** a POST `/v1/customers` request with a valid `Idempotency-Key` header,
**When** the platform processes it,
**Then** a customer record is created and one account per supported (chain, asset) pair — (Base, ETH), (Base, USDC), (Arbitrum, ETH), (Arbitrum, USDC) — is provisioned with a zero balance, and the customer id is returned (FR1).

**Given** the same `Idempotency-Key` is replayed with the same request body,
**When** processed again,
**Then** the original response is returned byte-for-byte and no second customer or account rows are created (FR23).

**Given** a mutating request without an `Idempotency-Key` header,
**When** processed,
**Then** the platform rejects it with a 400 RFC 9457 `problem+json` response and no side effects occur.

**Given** the request succeeds,
**When** inspected in Postgres,
**Then** the customer and its accounts exist in a single committed transaction — a crash between them is impossible by construction (AD-4).

**Given** a request to any mutating endpoint without a valid bearer token,
**When** processed,
**Then** the platform rejects it with a 401 response — there is no anonymous surface anywhere in the API (NFR15, AD-14).

### Story 1.2: Query Customer Balances

As an application team,
I want to query a customer's current balance for each supported asset and chain,
So that I can display accurate, real-time balances without maintaining a shadow ledger.

**Acceptance Criteria:**

**Given** a customer with provisioned accounts and no ledger activity,
**When** I GET `/v1/customers/{id}/balances`,
**Then** each (chain, asset) pair returns a balance of "0" in integer base units — wei for ETH, 6-decimal units for USDC (FR2).

**Given** a customer id that does not exist,
**When** queried,
**Then** the platform returns a 404 RFC 9457 `problem+json` response.

**Given** the balances endpoint is called under normal v1 load,
**When** measured,
**Then** p95 latency is under 500ms (NFR6).

**Given** the balance is derived from postings rather than stored directly,
**When** a balance is returned,
**Then** it is always recomputable from the journal — no cached value can diverge from the postings (AD-3).

### Story 1.3: Ledger-Only Internal Transfer Between Customers

As an application team,
I want to move balance from one customer to another without any on-chain transaction,
So that I can support customer-to-customer transfers instantly and at zero gas cost.

**Acceptance Criteria:**

**Given** two customers with sufficient and matching-asset balances,
**When** I POST `/v1/transfers` with an `Idempotency-Key`, source, destination, asset, and amount,
**Then** a balanced journal entry (debit source, credit destination) commits atomically and both balances update immediately (FR4, AD-3, AD-4).

**Given** the source customer's balance is less than the requested amount,
**When** the transfer is submitted,
**Then** it is rejected with a 422 `problem+json` response and no postings are written.

**Given** the same `Idempotency-Key` is replayed,
**When** processed again,
**Then** the original result is returned and the balances are not moved a second time (FR24).

**Given** a transfer completes,
**When** the journal is inspected,
**Then** the entry's cause is exactly `internal_transfer` with the caller's idempotency key as its unique cause id — no other code path can produce a second entry for the same cause (FR5).

### Story 1.4: List Customer Transaction History

As an application team,
I want to list a customer's transaction history with status,
So that I can show customers an accurate record without querying the chain myself.

**Acceptance Criteria:**

**Given** a customer with one completed internal transfer,
**When** I GET `/v1/customers/{id}/transactions`,
**Then** the transfer appears with its type (`internal_transfer`), amount, asset, chain (if applicable), status, and timestamp (FR3).

**Given** a customer with no transactions,
**When** queried,
**Then** an empty paginated list is returned, not an error.

**Given** more transactions exist than the page size,
**When** queried,
**Then** the response is paginated with stable ordering (newest first) and a cursor for the next page.

**Given** the query reads generically from the cause-tagged journal,
**When** future cause types (deposit, withdrawal) are added in later epics,
**Then** they appear in this endpoint automatically, with no endpoint rewrite required.

### Story 1.5: Generate Per-Customer Deposit Address via CREATE2 Forwarder

As an application team,
I want each customer to have a single deposit address that works identically on every supported EVM chain,
So that my users can deposit ETH or USDC on Base or Arbitrum without me managing per-chain addresses.

**Acceptance Criteria:**

**Given** a customer is created,
**When** the platform provisions their deposit address,
**Then** it computes CREATE2(factory, salt, forwarder init code) with salt = the customer UUID left-padded to bytes32, persists the resulting address once, and the same address is valid on both Base and Arbitrum (FR6, AD-8).

**Given** the address is provisioned,
**When** I GET `/v1/customers/{id}`,
**Then** the response includes the deposit address as an attribute of the customer resource (FR7) — it is never a route parameter.

**Given** the deterministic factory deployer (`0x4e59b44847B379578588920cA78FbF26c0B4956C`) is not yet deployed on a target chain,
**When** the platform starts up against that chain,
**Then** it verifies the factory's presence and fails startup loudly rather than serving addresses that could collide or diverge (AD-8).

**Given** an address has already been persisted for a customer,
**When** the same customer's address is requested again,
**Then** the stored value is returned — the address is never re-derived on the fly.

**Given** the Go salt-encoding implementation and the Foundry/Solidity CREATE2 computation,
**When** run against the same input test vectors in CI,
**Then** both produce byte-identical addresses (AD-8 cross-language pinning).

## Epic 2: Deposit Monitoring, Crediting & Reorg Safety

Deposits sent to issued addresses are detected, tracked through confirmation tiers, credited at finality, and recovered safely across reorgs, sequencer reordering, and watcher downtime. This is where the chain-adapter boundary (AD-1) materializes for real, since watchers are the first chain-specific code in the system — FR33 (a third chain touches only the adapter) is validated structurally by Story 2.1's CI boundary check, not as its own story, since v1 ships with exactly two chains.

### Story 2.1: Track Incoming Deposits & Expose Pending Status

As an application team,
I want incoming transfers to a customer's deposit address tracked from first sighting through confirmation tiers,
So that I always know a deposit's current state without watching the chain myself.

**Acceptance Criteria:**

**Given** a customer has a persisted deposit address,
**When** ETH or USDC is transferred to it on Base or Arbitrum and the watcher for that chain polls,
**Then** a deposit record is created in "observed" state, uniquely keyed by (chain, tx_hash, log_index), and advances to "safe" once the batch posts to L1 (FR8).

**Given** a deposit is in "observed" or "safe" state,
**When** queried via the API,
**Then** it appears as pending with its current confirmation tier (FR10).

**Given** a deposit is first observed,
**When** the "observed" transition commits,
**Then** a `deposit.pending` outbox event is written in the same transaction, ready for Epic 4's dispatcher to deliver (FR29, AD-4).

**Given** all chain-specific RPC and tier-mapping logic lives in the EVM adapter,
**When** the codebase is reviewed,
**Then** no chain ID or go-ethereum import exists outside `internal/adapter/evm`, enforced by a CI import-boundary check (FR32, AD-1).

**Given** the same on-chain event is observed twice (e.g. re-poll overlap),
**When** processed,
**Then** the unique (chain, tx_hash, log_index) constraint prevents a duplicate deposit record (AD-5).

### Story 2.2: Credit Deposits at Finalized Tier via Policy Table

As an application team,
I want deposits credited to the customer's balance once they reach finality,
So that funds are safe to use without risk of reversal.

**Acceptance Criteria:**

**Given** a deposit reaches "finalized" tier and the crediting policy for (chain, asset) is "finalized" (the v1 default),
**When** the watcher processes it,
**Then** it writes the credit's journal entry (debit forwarder-float, credit customer available), transitions the deposit to "credited", and writes a `deposit.credited` outbox event — all atomically, in the same transaction (FR9, FR29, AD-4, AD-6, AD-7).

**Given** a deposit has been credited,
**When** any subsequent chain event affects that block range,
**Then** the credited balance is never reversed — only pre-finality records are ever affected (FR13, NFR3).

**Given** the crediting policy is a per-(chain, asset) table entry,
**When** a future entry is changed to a different tier,
**Then** it is a configuration change, not a code change (FR9 forward-compatibility).

**Given** a credited deposit,
**When** queried through Story 1.4's transaction history endpoint,
**Then** it appears with status "credited" and its journal cause.

### Story 2.3: Handle Unsupported Token Transfers Without Ledger Corruption

As an operator,
I want transfers of tokens the platform doesn't support to be recorded and surfaced without ever being credited or corrupting the ledger,
So that unexpected on-chain activity is visible and safe.

**Acceptance Criteria:**

**Given** a transfer of a token not in the supported registry (only native ETH and USDC in v1) arrives at a customer's address,
**When** observed,
**Then** it is recorded as an "unsupported_token" observation, never credited, and never creates a journal posting (FR11).

**Given** the token registry is data (chain, contract address), not code,
**When** a new ERC-20 is later added to an already-supported chain,
**Then** it requires only a registry entry, not a code change (FR34).

**Given** an unsupported-token observation exists,
**When** queried by an operator,
**Then** it is visible with the token's contract address and amount for manual triage.

### Story 2.4: Detect & Safely Reverse Reorged Pre-Finality Deposits

As an operator,
I want pre-finality deposit records to be automatically corrected when chain history changes,
So that sequencer reordering or L1 reorgs never produce an incorrect balance.

**Acceptance Criteria:**

**Given** a deposit is in "observed" or "safe" state,
**When** the watcher detects the underlying block or batch has been replaced by a competing history,
**Then** the deposit record transitions to "orphaned" and its provisional visibility reflects this, with no balance ever having been affected — crediting only happens at finality per Story 2.2 (FR12, FR13).

**Given** a reorg is followed by the transaction reappearing in the canonical chain,
**When** the watcher re-observes it,
**Then** a fresh deposit record is created and tracked from "observed" — never double-counted against the orphaned one.

**Given** this behavior,
**When** tested,
**Then** it is reproduced deterministically using anvil's `anvil_reorg` (NFR19 groundwork, formalized fully in Epic 6).

### Story 2.5: Recover From Watcher Downtime Without Loss or Double-Processing

As an operator,
I want the watcher to resume exactly where it left off after any downtime,
So that no deposit is ever missed or double-processed regardless of how long the platform was down.

**Acceptance Criteria:**

**Given** the watcher was down for a period during which deposits occurred,
**When** it restarts,
**Then** it resumes from its last persisted cursor per (chain, tier) and rescans every missed block range (FR14).

**Given** the rescan re-observes an already-recorded deposit,
**When** reprocessed,
**Then** the (chain, tx_hash, log_index) uniqueness constraint makes the rescan a no-op for that deposit — no duplicate record, no duplicate credit.

**Given** the watcher crashes mid-processing of a batch of blocks,
**When** it restarts,
**Then** it resumes from the last committed cursor, never skipping or reprocessing ambiguously (NFR8).

## Epic 3: Withdrawals, Fees & Treasury Sweeps

Application teams request withdrawals with an up-front fee estimate; the platform validates policy, routes above-threshold requests to operator approval, manages nonces per hot wallet, and safely resolves stuck or failed withdrawals. Forwarder balances are swept to treasury as first-class ledger events. Introduces the Signer port (KMS in prod, LocalStack locally) and the single-writer broadcaster.

### Story 3.1: Query Fee Estimate Before Submitting a Withdrawal

As an application team,
I want to get a fee estimate for a prospective withdrawal before submitting it,
So that I can inform my users of the total cost accurately.

**Acceptance Criteria:**

**Given** a chain, asset, and amount,
**When** I GET a fee estimate,
**Then** the platform returns an estimate combining the L2 execution fee and the amortized L1 data fee for that chain — Arbitrum via `NodeInterface.gasEstimateComponents()`, Base via the `GasPriceOracle` predeploy (FR21).

**Given** naive L1-style estimation would systematically undercharge,
**When** compared,
**Then** the estimate accounts for both fee components explicitly.

**Given** an unsupported chain/asset combination is queried,
**When** processed,
**Then** the platform returns a 400 `problem+json` response (FR22).

### Story 3.2: Request a Withdrawal With Idempotent Hold Placement

As an application team,
I want to request a withdrawal with an idempotency key,
So that the amount is held immediately and retries never create duplicate withdrawals.

**Acceptance Criteria:**

**Given** sufficient available balance,
**When** I POST `/v1/withdrawals` with an `Idempotency-Key`, destination, asset, and amount,
**Then** a withdrawal record is created in "created" state and the amount is placed on hold against the customer's balance atomically (FR15, AD-4).

**Given** insufficient balance,
**When** submitted,
**Then** the request is rejected with a 422 `problem+json` response and no hold is placed.

**Given** the same `Idempotency-Key` is replayed,
**When** reprocessed,
**Then** the original response is returned and no second hold is placed.

**Given** the withdrawal state machine established here,
**When** any future story advances a withdrawal to a new state (awaiting-approval, approved, signed, broadcast, confirmed, failed),
**Then** that transition writes a corresponding `withdrawal.*` outbox event in the same transaction as the state change — this rule binds every later withdrawal story, not just this one (FR29, AD-4).

### Story 3.3: Enforce Pre-Signing Policy & Threshold-Based Approval Routing

As an operator,
I want withdrawals above a configurable threshold routed to me for explicit approval, and every withdrawal checked against policy before signing,
So that large or invalid withdrawals never execute automatically.

**Acceptance Criteria:**

**Given** a withdrawal amount exceeds the configured per-asset threshold,
**When** it advances from "created",
**Then** it enters "awaiting-approval" state, writes an `approval.required` outbox event per Story 3.2's rule, and does not proceed until an operator explicitly approves it (FR17, FR29).

**Given** a withdrawal is at or below threshold,
**When** it advances,
**Then** it proceeds automatically toward signing without operator intervention.

**Given** a withdrawal is about to be signed, via either path,
**When** policy is checked,
**Then** available balance covers amount plus estimated fee, the destination address is well-formed and not a known-invalid target (e.g. the zero address), and threshold routing has already been applied — any failure blocks signing (FR18).

**Given** an operator approves an awaiting-approval withdrawal,
**When** approved,
**Then** it transitions to "approved" and proceeds; the approval is logged with actor, timestamp, and reason (NFR11).

### Story 3.4: Sign & Broadcast Withdrawals via the Single-Writer Broadcaster

As an application team,
I want the platform to manage nonces, signing, and broadcasting entirely on my behalf,
So that I never handle gas, nonces, or raw transactions.

**Acceptance Criteria:**

**Given** a withdrawal reaches "approved" state,
**When** the chain's broadcaster picks it up,
**Then** it signs via the Signer port (AWS KMS in prod, LocalStack KMS locally via endpoint override, software signer in tests), allocates the next nonce from persisted per-chain state, and broadcasts — transitioning "signed" → "broadcast" (FR20, AD-10).

**Given** exactly one broadcaster process per chain,
**When** the broadcaster starts,
**Then** it takes a Postgres advisory lock keyed by chain and exits if another instance already holds it (AD-11).

**Given** the signer is invoked, whether KMS-backed or software,
**When** any log line, error, or API response related to signing is inspected,
**Then** no key handle, private key material, or secret ever appears in it (NFR13).

**Given** a broadcast succeeds and is later confirmed on-chain,
**When** observed,
**Then** the withdrawal transitions to "confirmed" and its hold is settled — debited from customer available, released from hold (FR16).

**Given** consuming applications interact with this flow,
**When** any API response is inspected,
**Then** no response ever exposes a nonce, raw transaction, or gas parameter directly (FR20).

### Story 3.5: Resolve Stuck & Failed Withdrawals

As an operator,
I want stuck or failed withdrawals to be first-class and recoverable,
So that no customer's funds are ever silently lost or frozen.

**Acceptance Criteria:**

**Given** a withdrawal fails terminally (e.g. broadcast rejected),
**When** it transitions to "failed",
**Then** its hold is released back to available balance immediately (FR19).

**Given** a withdrawal is broadcast but not confirmed within an operationally-defined window,
**When** detected,
**Then** it is surfaced to the operator as "stuck" with a documented resolution path in the operator runbook.

**Given** the broadcaster process crashes mid-transition (e.g. after signing, before recording broadcast),
**When** it restarts,
**Then** the withdrawal resumes correctly from its last persisted state with no ambiguous double-broadcast (FR16, NFR8).

**Given** a chain is in degraded liveness (AD-15),
**When** a withdrawal would normally broadcast,
**Then** it is accepted and queues rather than being rejected or failing — resuming automatically once liveness returns.

### Story 3.6: Sweep Forwarder Balances to Treasury

As an operator,
I want confirmed deposit funds resting in per-customer forwarders consolidated into the treasury,
So that operational funds are usable and the ledger reflects the consolidation.

**Acceptance Criteria:**

**Given** credited deposits have accumulated in a customer's forwarder-float account,
**When** the broadcaster's sweep trigger fires (threshold or schedule, ops configuration),
**Then** it creates a sweep record, deploys the persistent forwarder contract if not already deployed, and flushes funds to treasury.

**Given** a sweep completes,
**When** the journal is inspected,
**Then** postings move platform forwarder-float → platform treasury and never touch any customer account (AD-9).

**Given** a sweep is already in-flight for a (chain, forwarder, asset),
**When** another sweep trigger fires for the same combination,
**Then** a partial unique index prevents a second concurrent sweep.

**Given** the broadcaster is the sole creator of sweep records,
**When** the codebase is reviewed,
**Then** no other process path creates or advances a sweep.

## Epic 4: Event Notifications (Webhooks)

Consuming applications stop polling and receive verifiable, deduplicable webhook events in real time for deposits, withdrawals, and approvals. Epics 2 and 3 already write outbox rows atomically with their state changes (AD-4); this epic delivers the dispatcher that ships them.

### Story 4.1: Register Webhook Endpoint & Receive Deposit/Withdrawal Events

As an application team,
I want to register a webhook endpoint and receive events for deposit and withdrawal state changes,
So that I stop polling and react to changes in real time.

**Acceptance Criteria:**

**Given** outbox rows already written by Epic 2 and Epic 3 stories (AD-4),
**When** the dispatcher polls the outbox,
**Then** it delivers each event to the registered webhook endpoint via HTTP POST (FR29).

**Given** a delivery fails (non-2xx response or timeout),
**When** retried,
**Then** exponential backoff is applied and delivery is retried until success or a max-attempts ceiling — never silently dropped (FR30).

**Given** an event has already been delivered,
**When** redelivered due to a retry race,
**Then** the consumer can deduplicate using the event ID, which equals the outbox row ID.

**Given** event types deposit.pending, deposit.credited, withdrawal state changes, and approval-required occur,
**When** they happen,
**Then** a webhook is sent for each.

### Story 4.2: Verify Webhook Authenticity via Signed Payloads

As an application team,
I want to verify that a webhook actually came from the platform,
So that I can trust and safely act on the event without risk of spoofing.

**Acceptance Criteria:**

**Given** a webhook is delivered,
**When** I inspect it,
**Then** it carries an HMAC-SHA256 signature header computed over the raw payload plus a timestamp field (FR31).

**Given** I recompute the HMAC with my shared secret,
**When** compared,
**Then** a valid signature matches, and a tampered or forged payload does not.

**Given** I am integrating for the first time,
**When** I consult the documentation,
**Then** a minimal reference verification snippet is provided in `docs/` so I don't have to reverse-engineer the scheme.

## Epic 5: Independent Reconciliation & Operational Monitoring

Operators get continuous, independent verification that the ledger matches on-chain reality, with drift alarmed within one cycle, plus operational monitoring of watcher lag, chain liveness, and approval-queue age — delivered as alerts through Epic 4's dispatcher.

### Story 5.1: Continuous Ledger-vs-Chain Reconciliation With Drift Alerts

As an operator,
I want the ledger continuously and independently compared against on-chain reality,
So that any drift is caught by the platform before a customer notices.

**Acceptance Criteria:**

**Given** the reconciliation process runs on its own RPC provider, distinct from watchers and broadcasters (AD-12),
**When** it executes its streaming break-detection pass,
**Then** it compares recently processed deposits and withdrawals against chain state in near-real-time (FR25).

**Given** the reconciliation process also runs a batch deep pass (daily floor, hourly target per NFR10),
**When** executed,
**Then** it detects at minimum: deposits on-chain but missing internally, internal records without on-chain counterparts, pending-withdrawal mismatches, and double-counted internal transfers (FR27).

**Given** any drift is detected,
**When** found,
**Then** an alarm fires within one reconciliation cycle with enough detail to investigate — account, asset, expected vs. observed (FR26).

**Given** the reconciliation process,
**When** the codebase is reviewed,
**Then** it writes only its own tables and alert outbox events — never journal, deposit, withdrawal, or sweep rows (AD-12).

**Given** a deliberate fault is seeded in test (a ledger/chain discrepancy),
**When** the test runs,
**Then** the alarm fires — proving reconciliation isn't "green because it checks nothing" (counter-metric 1).

### Story 5.2: Query Reconciliation Status & Run History

As an operator,
I want reconciliation status and run history to be queryable,
So that "reconciliation is green" is an observable fact I can check any time, not an inference.

**Acceptance Criteria:**

**Given** reconciliation runs have occurred,
**When** I query the reconciliation status endpoint,
**Then** I see the current status (green or drift-detected) and a history of past runs with their outcomes (FR28).

**Given** a specific run had findings,
**When** queried,
**Then** each finding's detail — account, asset, expected vs. observed, timestamp — is retrievable.

### Story 5.3: Operational Monitoring & Alerting

As an operator,
I want to be alerted proactively on watcher lag, chain liveness loss, stuck withdrawals, and an aging approval queue,
So that I hear about problems from the platform before they become incidents.

**Acceptance Criteria:**

**Given** watcher cursor lag exceeds a configured threshold,
**When** detected by reconciliation's operational monitor,
**Then** an alert event is raised (NFR17, AD-12).

**Given** a chain's liveness status degrades (AD-15),
**When** detected,
**Then** an alert is raised, and a corresponding alert fires on recovery.

**Given** a withdrawal has been "awaiting-approval" longer than a configured ceiling,
**When** detected,
**Then** an alert fires — guarding against the approval queue becoming a silent pile-up (counter-metric 2).

**Given** all of these alerts,
**When** raised,
**Then** they ride the same outbox/webhook path as Epic 4's events — no separate delivery mechanism.

## Epic 6: Security, Resilience & Portfolio Readiness

The written threat model, key-ceremony/DR/stuck-withdrawal runbook, and the consolidated fault-injection test suite reach the bar of an external reviewer being able to read the threat model, run the tests, and trace a deposit end to end. No new FRs; this epic covers NFR12, NFR14, NFR19, and NFR4–6.

### Story 6.1: Written Threat Model

As an external reviewer,
I want a written, reviewed threat model covering the platform's assets and attack surface,
So that I can assess the security posture without reverse-engineering it from code.

**Acceptance Criteria:**

**Given** the threat model document,
**When** reviewed,
**Then** it enumerates assets (hot-wallet key, customer funds, ledger integrity, API), threats per component, and for every high-risk item either a stated mitigation or an explicit accepted-risk entry (NFR12).

**Given** AD-10's signing mechanism (KMS in prod, LocalStack locally),
**When** the threat model is written,
**Then** it explicitly ratifies or revises that choice, per AD-10's own binding clause.

**Given** the Bybit incident (Feb 2025) and the Base sequencer outages documented in the PRD addendum,
**When** referenced,
**Then** the threat model cites them as concrete grounding for the raw-transaction-construction rule (NFR16) and the liveness-as-mode design (AD-15).

**Given** the threat model is a deliverable with the same status as code,
**When** the repository is reviewed,
**Then** it lives in `docs/` and is referenced from the README.

### Story 6.2: Key Ceremony, Backup & Disaster Recovery Runbook

As an operator,
I want documented, tested key-generation, backup, and recovery procedures,
So that loss of a single host never means loss of funds.

**Acceptance Criteria:**

**Given** the KMS key-generation procedure,
**When** documented,
**Then** it covers who performs it, what is recorded, and how the resulting key ARN is verified (NFR14).

**Given** a simulated host loss,
**When** the documented recovery procedure is followed against the local runtime (LocalStack and the Sepolia testnets),
**Then** the platform resumes signing without any loss of funds or duplicate signing.

**Given** the Postgres backup and restore procedure (WAL archiving plus base backups, per the spine's deployment envelope),
**When** a restore drill is run,
**Then** it either succeeds and meets the documented recovery target, or the RDS revisit trigger is logged.

### Story 6.3: Consolidated Fault-Injection Test Suite

As an external reviewer,
I want a single test suite that injects reorgs, duplicate requests, crashes mid-state-machine, and RPC failures,
So that I can run it myself and verify the correctness claims rather than trust them.

**Acceptance Criteria:**

**Given** the suite runs against anvil (fork mode, `anvil_reorg`),
**When** executed,
**Then** it exercises reorg-during-pending-deposit (Story 2.4), duplicate idempotent requests (Stories 1.1, 1.3, 3.2), crash mid-withdrawal-state-machine (Story 3.5), and RPC failure / watcher downtime (Story 2.5) as one runnable command (NFR19).

**Given** the suite is run by someone outside the project,
**When** they follow only the README,
**Then** it runs to completion with a clear pass/fail report and no manual environment setup beyond Docker Compose.

**Given** NFR1 (zero double-credits, zero duplicate withdrawals),
**When** the suite completes,
**Then** it asserts this holds across every injected fault, not just the happy path.

**Given** a credited deposit,
**When** I follow only its logs and API queries as an external reviewer,
**Then** I can trace it end to end — chain event → ledger entry → webhook delivery — with no other data source needed (NFR18).

### Story 6.4: Load & Latency Validation Against the Performance Envelope

As an operator,
I want the performance envelope to be a tested contract, not an aspiration,
So that I know the platform holds up under expected v1 volume before it matters.

**Acceptance Criteria:**

**Given** a load-smoke test simulating thousands of deposits and withdrawals per day with a 10× burst,
**When** run,
**Then** the platform sustains it without error (NFR4).

**Given** the deposit credit path,
**When** measured from finality-observed to balance-credited to webhook-sent,
**Then** the platform's own overhead is no more than 1 minute (NFR5).

**Given** read APIs — balances, history, fee estimates — under v1 volume,
**When** measured,
**Then** p95 latency is under 500ms (NFR6).

**Given** this test,
**When** integrated,
**Then** it runs in CI as a guard against regression, not a one-time manual check.
