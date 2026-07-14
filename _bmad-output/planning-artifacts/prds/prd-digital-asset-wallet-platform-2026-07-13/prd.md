---
title: Digital Asset Wallet Platform PRD
status: final
created: 2026-07-13
updated: 2026-07-13
---

# Digital Asset Wallet Platform — PRD

## Vision

Twelve months from now, the platform runs in production behind our company's own internal system: customer deposits and withdrawals for ETH and USDC on Base and Arbitrum flow through it with boring reliability. Application teams integrate through one internal API and never think about reorgs, gas, or nonces; the operator's normal week is watching reconciliation stay green — and when it doesn't, hearing it from the platform before any customer notices.

Beyond internal adoption, **three external companies run the platform themselves**. Adoption is self-serve — a company can deploy and operate the platform from its documentation alone — with paid consulting available for teams that want implementation help. The platform is the part of wallet infrastructure everyone otherwise builds by hand: the ledger side (deposit monitoring, reorg handling, transaction state machines, reconciliation) that no vendor or mature OSS project covers.

V1 proves the core internally and is scoped accordingly: single-tenant, platform-held keys, our own application as the consumer. External self-serve adoption is a vision-level outcome, not a v1 requirement — v1 carries no multi-tenant, packaging, or onboarding scope. The bet, stated honestly: there is no technical moat today; rigor — tests, a reviewed threat model, correct L2 mechanics — is rare enough in this niche to be the product.

## Problem & Context

Any company that credits customers when crypto arrives and pays out on request faces four hard backend problems, each with a real-cost failure mode:

1. **Deposits are not binary.** On L2s, a transaction passes through confirmation tiers — sequencer receipt, "safe" (batch posted to L1), finalized (L1 finality, ~13–20 min). Credit too early and a reorg or sequencer reordering becomes a double-credit.
2. **Withdrawals must never happen twice.** Retries, crashes, and duplicate API calls are normal operation; without idempotency and an explicit state machine they become duplicate payouts.
3. **Fees are chain-specific.** L2 fees combine an execution fee with an amortized L1 data fee, surfaced differently on Arbitrum and Base. Naive L1-style estimation systematically undercharges.
4. **The ledger drifts from the chain.** Missed deposits, stuck withdrawals, double-counted internal transfers. Without independent reconciliation, drift is discovered by customers, not operators.

Institutional custody vendors price at six figures scaling with volume; embedded-wallet vendors solve consumer key custody — a different problem; OSS covers signing, not orchestration. No vendor or mature OSS project covers the **ledger side** — the layer everyone ends up building by hand, and the layer this platform is. Owning it also removes vendor-continuity risk in a fast-consolidating market (Privy→Stripe, Dynamic→Fireblocks, BitGo IPO).

Stated honestly: for the internal use case the differentiator is **fit and ownership**, not novel technology, and building custody in-house is typically a 6–18 month effort demanding specialized security work (key ceremony, HSM integration, disaster recovery) — a cost this PRD accepts deliberately, with the threat model and key-management NFRs as the counterweight. Direct key control also keeps jurisdiction-specific compliance options open, since some regimes require it.

The project is a passion/portfolio effort run like a product: real requirements, tests, and a threat model from day one. The repository itself is a deliverable — it must stand as evidence of production-grade engineering.

## Users

- **Application teams (primary, v1).** Our company's own developers, integrating deposits and withdrawals through one internal API. Success: they never think about reorgs, gas, or nonces.
- **Operators.** Whoever answers for customer funds. Success: reconciliation is green, and when it isn't, they hear it from the platform first — with enough detail to act.
- **External engineering teams (vision, post-v1).** Teams at other companies facing the same build-vs-buy dead end, deploying the platform self-serve with optional paid consulting. Not a v1 requirement; named so v1 decisions don't foreclose it.

User journeys are deliberately downscaled: v1 has one internal API consumer and one operator role — the persona success statements above carry what a journey section would.

## Scope

**In (v1):** Base and Arbitrum; deposits and withdrawals for ETH and USDC; accounts, balances, and transaction history; internal (ledger-only) transfers; address generation; fee estimation; idempotent APIs; transaction state machine with manual approval above a threshold; deposit monitoring with explicit confirmation-tier policy (credit at finalized); reorg handling; reconciliation (streaming + batch); webhook event notifications; platform-held keys; test suite; written threat model.

**Out (v1):** non-EVM chains; third-party signer integrations; consumer-facing UI; staking, swaps, or DeFi interactions; compliance tooling (KYC/AML, travel rule); multi-tenant/WaaS features, packaging, and self-serve onboarding — deferred until the product path is real.

## Features & Functional Requirements

A single Go service exposing an internal API. V1 supports ETH and USDC on Base and Arbitrum. Requirements state capabilities; technology choices live in the addendum and downstream architecture.

### F1. Accounts & Ledger

The platform owns the account and balance model; consuming applications query it rather than keeping their own shadow ledger.

- **FR1.** The platform maintains accounts per customer with per-asset balances.
- **FR2.** Applications can query a customer's current balances per asset and chain.
- **FR3.** Applications can list a customer's transaction history (deposits, withdrawals, internal transfers) with status.
- **FR4.** Customer-to-customer internal transfers move balances ledger-only — atomically, with no on-chain movement — and are idempotent.
- **FR5.** Every balance change is traceable to exactly one cause: an on-chain event, an internal transfer, or an operator adjustment. The ledger is the auditable record and must always be explainable against the chain.

### F2. Address Generation

- **FR6.** The platform generates one deposit address per customer, reused across all supported EVM chains — the same address receives on both Base and Arbitrum, so deposits are attributed by (address, chain), never by address alone.
- **FR7.** Address-to-customer attribution is persisted durably; incoming funds to any issued address are always attributable, including after restarts and re-deploys.

### F3. Deposit Monitoring & Crediting

- **FR8.** Chain watchers track incoming transfers to issued addresses across the L2 confirmation tiers (sequencer → safe → finalized) on both chains. Deposit records carry an explicit lifecycle state (observed → safe → finalized → credited; orphaned on reorg) — the state-machine discipline of FR16 applies to deposits as well as withdrawals.
- **FR9.** Deposits are credited at the **finalized** tier (L1 finality), flat across both assets and all amounts. The crediting policy is an explicit per-chain/per-asset setting; v1 sets every entry to finalized so a future tiered policy is a configuration change, not rework.
- **FR10.** Deposits observed but not yet final are visible as pending — queryable by applications with their current tier.
- **FR11.** Supported deposits: native ETH and USDC (ERC-20) on Base and Arbitrum. Transfers of unsupported tokens to issued addresses must not corrupt the ledger.

### F4. Reorg & Chain-History Handling

- **FR12.** The platform detects changes to observed chain history, including sequencer reordering prior to L1 inclusion and L1 reorgs affecting batch inclusion.
- **FR13.** Pre-finality deposit records are safely reversed or re-credited when history changes. Because crediting waits for finality, a credited balance is never reversed by a reorg — this is a platform invariant.
- **FR14.** After watcher downtime or chain outages (e.g., sequencer halts), the platform rescans missed ranges and recovers without missing or double-processing any deposit.

### F5. Withdrawals

- **FR15.** Applications request withdrawals through the API with an idempotency key; the requested amount is placed on hold against the customer's balance immediately.
- **FR16.** Every withdrawal moves through an explicit state machine (e.g., created → approved → signed → broadcast → confirmed / failed) with no ambiguous intermediate states; in-flight withdrawals resume correctly after a crash.
- **FR17.** Withdrawals above a configurable per-asset threshold enter an awaiting-approval state and require explicit operator approval before signing; withdrawals below the threshold proceed automatically.
- **FR18.** Before signing, the platform validates every withdrawal against the v1 policy set: available balance covers amount plus estimated fee; destination address is well-formed and not a known-invalid target (e.g., zero address); threshold routing per FR17 has been applied. The policy set is deliberately minimal in v1; an extensible policy engine is future-path work.
- **FR19.** Stuck or failed withdrawals are first-class: a withdrawal that fails terminally releases its hold; one stuck in-flight is surfaced to the operator with a resolution path documented in the operator runbook (a repo deliverable).
- **FR20.** The platform manages nonces and transaction sequencing per hot wallet; consuming applications never handle nonces, gas, or raw transactions.

### F6. Fee Estimation

- **FR21.** The platform estimates fees correctly per chain, accounting for both the L2 execution fee and the amortized L1 data fee (mechanics per chain in the addendum). Naive L1-style estimation is explicitly non-conforming.
- **FR22.** Applications can query a fee estimate for a prospective withdrawal before submitting it.

### F7. Idempotency (cross-cutting)

- **FR23.** Every mutating API operation accepts an idempotency key; a duplicate request returns the original result and never causes a second money movement.
- **FR24.** Retries, crashes, and redeliveries never double-apply an operation — ledger effects are exactly-once even though delivery is at-least-once.

### F8. Reconciliation

- **FR25.** An independent reconciliation process continuously compares the internal ledger against on-chain reality on a defined cycle.
- **FR26.** Any drift raises an alarm within one reconciliation cycle, with enough detail for the operator to investigate (account, asset, expected vs. observed).
- **FR27.** Reconciliation detects at minimum: deposits on-chain but missing internally, internal records without on-chain counterparts, pending-withdrawal mismatches, and double-counted internal transfers.
- **FR28.** Reconciliation status and run history are queryable — "reconciliation is green" is an observable fact, not an inference.

### F9. Event Notifications

- **FR29.** The platform pushes webhooks to the consuming application for: deposit pending, deposit credited, withdrawal state changes, approval required, and reconciliation alerts.
- **FR30.** Webhook delivery is at-least-once with retries and backoff; every event carries a unique event ID so consumers can deduplicate.
- **FR31.** Webhook consumers can verify event authenticity (e.g., signed payloads).

### F10. Blockchain Abstraction

- **FR32.** All chain-specific logic (RPC access, confirmation-tier mapping, fee mechanics, token contracts) is isolated behind a chain-adapter interface.
- **FR33.** Adding a third EVM chain requires changes only inside the chain-adapter layer — this is the acceptance test of the abstraction.
- **FR34.** USDC is the first token, not the only one: supporting an additional ERC-20 token on an already-supported chain is additive work (token registry/configuration), not rework — the token-side counterpart of FR33.

## Non-Functional Requirements

### Correctness & durability (non-negotiable)

- **NFR1.** Zero double-credits and zero duplicate withdrawals — upheld under injected reorgs, request retries, and crash-recovery tests, not just the happy path.
- **NFR2.** No acknowledged data is ever lost. Every acknowledged API write and every credited deposit survives a crash; in-flight operations resume or fail cleanly, never ambiguously.
- **NFR3.** A credited balance is never reversed by chain events (guaranteed by finalized-tier crediting, FR9/FR13).

### Capacity & performance

- **NFR4.** V1 is sized for thousands of deposits and withdrawals per day (order 10³, not 10⁶), with headroom for ~10× bursts.
- **NFR5.** Deposit credit latency is dominated by the finality wait (~13–20 min), by policy. The platform's own overhead — finality observed to balance credited to webhook sent — adds no more than 1 minute.
- **NFR6.** Read APIs (balances, history, fee estimates) respond in under 500 ms p95 at v1 volume.

### Availability & recovery

- **NFR7.** V1 runs best-effort on a single instance; availability is not an SLA. Durability is — see NFR2.
- **NFR8.** After downtime (platform crash, RPC failure, or sequencer outage), the platform recovers unattended where possible: watchers rescan missed ranges completely (FR14), in-flight withdrawals resume from their persisted state, and no operator reconstruction of state is ever required.
- **NFR9.** Chain liveness failures (e.g., Base sequencer halts of 2025–2026) are an expected operating condition, not an incident: the platform degrades explicitly (deposits pending, withdrawals queued or rejected with a clear error), and recovers per NFR8.

### Reconciliation & auditability

- **NFR10.** Reconciliation runs in two modes: streaming break-detection as events are processed, plus a batch deep pass comparing full ledger state against the chain at least daily, targeting hourly. Any drift alarms within one cycle of the mode that could have caught it.
- **NFR11.** Every balance change carries its cause (FR5); operator actions (approvals, adjustments) are logged with actor, timestamp, and reason. The audit trail is append-only.

### Security

- **NFR12.** A written threat model exists and is reviewed; every identified high-risk item has a stated mitigation or an explicit accepted-risk entry. The threat model is a deliverable with the same status as code.
- **NFR13.** Signing keys never leave the key-handling boundary (mechanism — software/KMS/HSM — is an architecture decision the threat model drives); keys and secrets never appear in logs, errors, or API responses.
- **NFR14.** Key generation, backup, and recovery are first-class: the key ceremony and disaster-recovery procedure are documented, tested, and covered by the threat model. Loss of a single instance or host must never mean loss of funds.
- **NFR15.** The internal API is authenticated; no anonymous surface. Webhook payloads are verifiable by the consumer (FR31).
- **NFR16.** Raw transactions are constructed and verified by the platform itself — no third-party signing UI in the path (Bybit lesson).

### Observability & operations

- **NFR17.** The operator hears it from the platform first: alerts fire for reconciliation drift, stuck withdrawals, withdrawals awaiting approval, watcher lag beyond threshold, and chain-liveness loss.
- **NFR18.** A deposit is traceable end to end — chain event → ledger entry → webhook — from logs and queries alone; this traceability is part of the portfolio-quality bar.

### Testability

- **NFR19.** The correctness claims above are backed by a test suite that injects reorgs, duplicate requests, crashes mid-state-machine, and RPC failures. The suite is runnable by an external reviewer.

## Success Metrics

V1 succeeds when all five hold:

1. **Correctness:** zero double-credits and zero duplicate withdrawals — including under injected reorgs, retries, and crash-recovery tests.
2. **Reconciliation:** ledger-vs-chain drift is detected by the platform, not reported by users; any drift alarms within one reconciliation cycle.
3. **Security:** a written threat model exists, is reviewed, and every identified high-risk item has a stated mitigation or an explicit accepted-risk entry.
4. **Extensibility:** adding a third EVM chain requires changes only inside the chain-adapter layer. Since v1 ships with two chains, this is verified by proxy in v1 — no chain-specific logic exists outside the adapter package, enforced in code review and CI — and falsified or confirmed outright at the first real chain addition.
5. **Portfolio quality:** the repository stands on its own as evidence of production-grade engineering — a reviewer can read the threat model, run the tests, and trace a deposit end to end.

**Counter-metrics** — signals that would look like success while masking failure:

- **Reconciliation green because it checks nothing.** Guard: seeded-fault verification — deliberately inject ledger/chain discrepancies in test and confirm the alarm fires (ties to NFR19).
- **Approvals as a silent queue.** Manual approval above threshold must not become a pile-up; time-in-awaiting-approval is tracked and has an alerting ceiling (ties to NFR17).

## Future Path (post-v1, not specified here)

If v1 proves the core internally, the platform itself — not just the application it powers — is the candidate product: self-serve deployment by external teams, with paid consulting as the revenue complement. The road from here to there is multi-tenancy, policy controls, a broader chain catalog, deployment packaging, and documentation an external engineer can act on alone. None of it is needed to prove the core, and none of it is specified in this PRD.

One standing revisit trigger: if the product path reaches customers whose compliance posture demands a qualified or external custodian, the platform-held-keys decision is re-opened — the orchestration layer (this platform's core) remains the product either way.

## Open Questions

1. **Key storage mechanism** — software keys, cloud KMS, or HSM. The custody decision is made (platform holds keys); the mechanism is an architecture-phase decision the threat model must drive. Owner: architecture. Revisit: at `bmad-architecture`.
2. **First consuming application** — volume is sized (thousands/day) but the identity of the first internal integrator is unconfirmed. Owner: André. Revisit: before epic breakdown.
3. **Operational threshold values** — the per-asset manual-approval threshold (FR17), the watcher-lag alert threshold, and the approval-queue alerting ceiling (NFR17) are ops/product settings to fix before launch. Owner: André + operator. Revisit: pre-launch checklist.

## Assumptions Index

All assumptions were reviewed with André on 2026-07-13 and confirmed as written, except A2, which was overturned:

- **A1 (FR1):** flat customer → account model; no sub-account hierarchy in v1. — Confirmed.
- **A2 (FR6):** *per-chain deposit addresses* — **Overturned:** one deposit address per customer, reused across all supported EVM chains; FR6 rewritten accordingly.
- **A3 (FR10):** applications get pending-deposit visibility before crediting. — Confirmed.
- **A4 (FR11):** unsupported tokens sent to issued addresses are recorded and surfaced to the operator, never credited. — Confirmed.
- **A5 (FR30):** webhook delivery is at-least-once with event IDs for consumer dedupe. — Confirmed.
- **A6 (FR31):** webhook authenticity via signed payloads. — Confirmed.
- **A7 (NFR4):** ~10× burst headroom over the nominal thousands-per-day volume. — Confirmed.
- **A8 (NFR5):** platform-added deposit latency ≤ 1 minute on top of the finality wait. — Confirmed.

## Glossary

- **Confirmation tiers:** *sequencer receipt* — transaction accepted by the L2 sequencer (~200ms–2s, reorderable); *safe* — batch posted to L1; *finalized* — the L1 block containing the batch is final (~13–20 min).
- **Reorg:** observed chain history replaced by a competing history, invalidating previously observed transactions.
- **Idempotency key:** caller-supplied unique key that makes a mutating request safely retryable.
- **Hold:** balance reserved for an in-flight withdrawal; neither spendable nor released until the withdrawal reaches a terminal state.
- **Drift:** disagreement between the internal ledger and on-chain reality.
- **Chain adapter:** the module isolating all chain-specific logic (RPC, tiers, fees, token contracts) behind a common interface.
- **Hot wallet:** platform-controlled signing keys kept online to execute withdrawals.
- **Ledger side:** deposit monitoring, reorg handling, transaction state machines, reconciliation — the orchestration layer this platform owns, as opposed to vendor-covered key management and signing.
