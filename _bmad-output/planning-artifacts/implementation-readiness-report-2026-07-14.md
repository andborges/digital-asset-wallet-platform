---
stepsCompleted: ["step-01-document-discovery", "step-02-prd-analysis", "step-03-epic-coverage-validation", "step-04-ux-alignment", "step-05-epic-quality-review", "step-06-final-assessment"]
documentsUsed:
  prd: _bmad-output/planning-artifacts/prds/prd-digital-asset-wallet-platform-2026-07-13/prd.md
  architecture: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md
  architectureCompanion: _bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/SOLUTION-DESIGN.md
  epics: _bmad-output/planning-artifacts/epics.md
  ux: none
---

# Implementation Readiness Assessment Report

**Date:** 2026-07-14
**Project:** digital-asset-wallet-platform

## PRD Analysis

### Functional Requirements

FR1: The platform maintains accounts per customer with per-asset balances.
FR2: Applications can query a customer's current balances per asset and chain.
FR3: Applications can list a customer's transaction history (deposits, withdrawals, internal transfers) with status.
FR4: Customer-to-customer internal transfers move balances ledger-only — atomically, with no on-chain movement — and are idempotent.
FR5: Every balance change is traceable to exactly one cause: an on-chain event, an internal transfer, or an operator adjustment. The ledger is the auditable record and must always be explainable against the chain.
FR6: The platform generates one deposit address per customer, reused across all supported EVM chains — the same address receives on both Base and Arbitrum, so deposits are attributed by (address, chain), never by address alone.
FR7: Address-to-customer attribution is persisted durably; incoming funds to any issued address are always attributable, including after restarts and re-deploys.
FR8: Chain watchers track incoming transfers to issued addresses across the L2 confirmation tiers (sequencer → safe → finalized) on both chains. Deposit records carry an explicit lifecycle state (observed → safe → finalized → credited; orphaned on reorg).
FR9: Deposits are credited at the finalized tier (L1 finality), flat across both assets and all amounts. The crediting policy is an explicit per-chain/per-asset setting; v1 sets every entry to finalized so a future tiered policy is a configuration change, not rework.
FR10: Deposits observed but not yet final are visible as pending — queryable by applications with their current tier.
FR11: Supported deposits: native ETH and USDC (ERC-20) on Base and Arbitrum. Transfers of unsupported tokens to issued addresses must not corrupt the ledger.
FR12: The platform detects changes to observed chain history, including sequencer reordering prior to L1 inclusion and L1 reorgs affecting batch inclusion.
FR13: Pre-finality deposit records are safely reversed or re-credited when history changes. Because crediting waits for finality, a credited balance is never reversed by a reorg.
FR14: After watcher downtime or chain outages, the platform rescans missed ranges and recovers without missing or double-processing any deposit.
FR15: Applications request withdrawals through the API with an idempotency key; the requested amount is placed on hold against the customer's balance immediately.
FR16: Every withdrawal moves through an explicit state machine (created → approved → signed → broadcast → confirmed/failed) with no ambiguous intermediate states; in-flight withdrawals resume correctly after a crash.
FR17: Withdrawals above a configurable per-asset threshold enter an awaiting-approval state and require explicit operator approval before signing; withdrawals below the threshold proceed automatically.
FR18: Before signing, the platform validates every withdrawal against the v1 policy set: available balance covers amount plus estimated fee; destination address is well-formed and not a known-invalid target (e.g. zero address); threshold routing per FR17 has been applied.
FR19: Stuck or failed withdrawals are first-class: a withdrawal that fails terminally releases its hold; one stuck in-flight is surfaced to the operator with a resolution path documented in the operator runbook.
FR20: The platform manages nonces and transaction sequencing per hot wallet; consuming applications never handle nonces, gas, or raw transactions.
FR21: The platform estimates fees correctly per chain, accounting for both the L2 execution fee and the amortized L1 data fee.
FR22: Applications can query a fee estimate for a prospective withdrawal before submitting it.
FR23: Every mutating API operation accepts an idempotency key; a duplicate request returns the original result and never causes a second money movement.
FR24: Retries, crashes, and redeliveries never double-apply an operation — ledger effects are exactly-once even though delivery is at-least-once.
FR25: An independent reconciliation process continuously compares the internal ledger against on-chain reality on a defined cycle.
FR26: Any drift raises an alarm within one reconciliation cycle, with enough detail for the operator to investigate (account, asset, expected vs. observed).
FR27: Reconciliation detects at minimum: deposits on-chain but missing internally, internal records without on-chain counterparts, pending-withdrawal mismatches, and double-counted internal transfers.
FR28: Reconciliation status and run history are queryable — "reconciliation is green" is an observable fact, not an inference.
FR29: The platform pushes webhooks to the consuming application for: deposit pending, deposit credited, withdrawal state changes, approval required, and reconciliation alerts.
FR30: Webhook delivery is at-least-once with retries and backoff; every event carries a unique event ID so consumers can deduplicate.
FR31: Webhook consumers can verify event authenticity (e.g., signed payloads).
FR32: All chain-specific logic (RPC access, confirmation-tier mapping, fee mechanics, token contracts) is isolated behind a chain-adapter interface.
FR33: Adding a third EVM chain requires changes only inside the chain-adapter layer — this is the acceptance test of the abstraction.
FR34: USDC is the first token, not the only one: supporting an additional ERC-20 token on an already-supported chain is additive work (token registry/configuration), not rework.

Total FRs: 34

### Non-Functional Requirements

NFR1: Zero double-credits and zero duplicate withdrawals — upheld under injected reorgs, request retries, and crash-recovery tests, not just the happy path.
NFR2: No acknowledged data is ever lost. Every acknowledged API write and every credited deposit survives a crash; in-flight operations resume or fail cleanly, never ambiguously.
NFR3: A credited balance is never reversed by chain events (guaranteed by finalized-tier crediting, FR9/FR13).
NFR4: V1 is sized for thousands of deposits and withdrawals per day (order 10³, not 10⁶), with headroom for ~10× bursts.
NFR5: Deposit credit latency is dominated by the finality wait (~13–20 min), by policy. The platform's own overhead — finality observed to balance credited to webhook sent — adds no more than 1 minute.
NFR6: Read APIs (balances, history, fee estimates) respond in under 500 ms p95 at v1 volume.
NFR7: V1 runs best-effort on a single instance; availability is not an SLA. Durability is.
NFR8: After downtime, the platform recovers unattended where possible: watchers rescan missed ranges completely, in-flight withdrawals resume from their persisted state, and no operator reconstruction of state is ever required.
NFR9: Chain liveness failures are an expected operating condition, not an incident: the platform degrades explicitly and recovers unattended.
NFR10: Reconciliation runs in two modes — streaming break-detection as events are processed, plus a batch deep pass comparing full ledger state against the chain at least daily, targeting hourly.
NFR11: Every balance change carries its cause; operator actions (approvals, adjustments) are logged with actor, timestamp, and reason. The audit trail is append-only.
NFR12: A written threat model exists and is reviewed; every identified high-risk item has a stated mitigation or an explicit accepted-risk entry.
NFR13: Signing keys never leave the key-handling boundary; keys and secrets never appear in logs, errors, or API responses.
NFR14: Key generation, backup, and recovery are first-class: the key ceremony and disaster-recovery procedure are documented, tested, and covered by the threat model.
NFR15: The internal API is authenticated; no anonymous surface. Webhook payloads are verifiable by the consumer.
NFR16: Raw transactions are constructed and verified by the platform itself — no third-party signing UI in the path.
NFR17: The operator hears it from the platform first: alerts fire for reconciliation drift, stuck withdrawals, withdrawals awaiting approval, watcher lag beyond threshold, and chain-liveness loss.
NFR18: A deposit is traceable end to end — chain event → ledger entry → webhook — from logs and queries alone.
NFR19: The correctness claims above are backed by a test suite that injects reorgs, duplicate requests, crashes mid-state-machine, and RPC failures. The suite is runnable by an external reviewer.

Total NFRs: 19

### Additional Requirements

- **Scope boundary (v1 In/Out):** In — Base and Arbitrum; ETH/USDC deposits and withdrawals; accounts/balances/history; internal transfers; address generation; fee estimation; idempotent APIs; state machine with manual approval; deposit monitoring at finalized tier; reorg handling; reconciliation; webhooks; platform-held keys; test suite; threat model. Out — non-EVM chains, third-party signer integrations, consumer UI, staking/swaps/DeFi, compliance tooling (KYC/AML), multi-tenant/WaaS packaging.
- **Success Metrics (5):** correctness (zero double-credit/duplicate), reconciliation (drift caught by platform), security (threat model + mitigations), extensibility (3rd-chain acceptance test, verified by proxy in v1), portfolio quality (external reviewer can read/run/trace).
- **Counter-metrics (2):** reconciliation-green-without-checking guarded by seeded-fault verification; approval queue must not become a silent pile-up (time-in-awaiting-approval tracked with an alerting ceiling).
- **Open Questions (3):** key storage mechanism (owned by architecture — resolved as AD-10 in the spine); first consuming application identity (unresolved, owner André, revisit before epic breakdown — **still open**, see gap analysis); operational threshold values (per-asset approval threshold, watcher-lag ceiling, approval-queue ceiling — deferred to pre-launch checklist, **still open**).
- **Assumptions Index (A1–A8):** all reviewed and confirmed with the user on 2026-07-13, including the A2 correction (deposit addresses reused across EVM chains, not per-chain) that FR6 already reflects.

### PRD Completeness Assessment

The PRD is internally consistent, `status: final`, and reads as unusually rigorous for its stage: every FR/NFR is testable, cross-referenced (e.g. FR9↔FR13, NFR3), and grounded in cited research (L2 confirmation-tier norms, Base/Arbitrum outage history, the Bybit incident). Two PRD-level open items remain genuinely open (not resolved downstream) and are carried into the gap analysis below: **first consuming application identity** and **operational threshold values**. Neither blocks architecture or epic structure, but both should be resolved before or during sprint planning since they affect concrete test data and launch checklists.

## Epic Coverage Validation

### Coverage Matrix

Verified two ways: (1) against `epics.md`'s own FR Coverage Map, and (2) by grepping every FR number's literal occurrence inside the Epic/Story sections themselves (not just the map) — every FR appears at least once in an actual story's acceptance criteria, confirming claimed coverage is real, not aspirational.

| FR | Requirement (summary) | Epic.Story | Status |
| --- | --- | --- | --- |
| FR1 | Accounts per customer, per-asset balances | 1.1 | ✓ Covered |
| FR2 | Query balances per asset/chain | 1.2 | ✓ Covered |
| FR3 | List transaction history with status | 1.4 | ✓ Covered |
| FR4 | Ledger-only internal transfers, atomic, idempotent | 1.3 | ✓ Covered |
| FR5 | Every balance change traceable to one cause | 1.3, 1.1 | ✓ Covered |
| FR6 | One deposit address per customer, reused across EVM chains | 1.5 | ✓ Covered |
| FR7 | Durable address-to-customer attribution | 1.5 | ✓ Covered |
| FR8 | Watchers track deposits across confirmation tiers | 2.1 | ✓ Covered |
| FR9 | Credit at finalized tier via policy table | 2.2 | ✓ Covered |
| FR10 | Pending deposits visible with current tier | 2.1 | ✓ Covered |
| FR11 | Supported deposits; unsupported tokens don't corrupt ledger | 2.3 | ✓ Covered |
| FR12 | Detect sequencer reordering / L1 reorgs | 2.4 | ✓ Covered |
| FR13 | Pre-finality records reversed/re-credited; credited irreversible | 2.2, 2.4 | ✓ Covered |
| FR14 | Rescan missed ranges after downtime, no loss/double-processing | 2.5 | ✓ Covered |
| FR15 | Withdrawal request with idempotency key; immediate hold | 3.2 | ✓ Covered |
| FR16 | Explicit withdrawal state machine; resumes after crash | 3.4, 3.5 | ✓ Covered |
| FR17 | Threshold-based awaiting-approval routing | 3.3 | ✓ Covered |
| FR18 | Pre-signing policy validation | 3.3 | ✓ Covered |
| FR19 | Stuck/failed withdrawals first-class with resolution path | 3.5 | ✓ Covered |
| FR20 | Nonce/transaction sequencing per hot wallet | 3.4 | ✓ Covered |
| FR21 | Correct per-chain fee estimation | 3.1 | ✓ Covered |
| FR22 | Fee estimate query before withdrawal | 3.1 | ✓ Covered |
| FR23 | Idempotency key on every mutating operation | 1.1 | ✓ Covered |
| FR24 | Exactly-once ledger effects under at-least-once delivery | 1.3 | ✓ Covered |
| FR25 | Independent reconciliation vs. on-chain reality | 5.1 | ✓ Covered |
| FR26 | Drift alarm within one reconciliation cycle | 5.1 | ✓ Covered |
| FR27 | Reconciliation detects the four minimum drift classes | 5.1 | ✓ Covered |
| FR28 | Reconciliation status/history queryable | 5.2 | ✓ Covered |
| FR29 | Webhooks for deposit/withdrawal/approval/recon events | 4.1 (event-writing ACs also in 2.1, 2.2, 3.2, 3.3) | ✓ Covered |
| FR30 | At-least-once delivery, retries, backoff, dedupe ID | 4.1 | ✓ Covered |
| FR31 | Verifiable event authenticity (signed payloads) | 4.2 | ✓ Covered |
| FR32 | Chain-specific logic isolated behind adapter interface | 2.1 | ✓ Covered |
| FR33 | Third chain touches only the adapter layer | 2.1 (structural/CI, no dedicated story — by design, see below) | ✓ Covered |
| FR34 | Additional ERC-20 token is additive, not rework | 2.3 | ✓ Covered |

### Missing Requirements

None. All 34 FRs have at least one story with FR-cited acceptance criteria.

One item flagged for awareness, not a gap: **FR33** has no dedicated story — it is validated as a structural/CI compliance criterion embedded in Story 2.1 (chain-adapter import-boundary check), since v1 ships with exactly two chains and cannot demonstrate a third-chain addition. This was a deliberate, discussed decision during epic design (confirmed with the user), not an oversight, and matches the PRD's own Success Metric 4 framing ("verified by proxy in v1... falsified or confirmed outright at the first real chain addition").

### Coverage Statistics

- Total PRD FRs: 34
- FRs covered in epics: 34
- Coverage percentage: 100%

## UX Alignment Assessment

### UX Document Status

Not Found.

### Alignment Issues

None — no UX document exists to misalign.

### Warnings

None. UX is explicitly, not implicitly, out of scope: the PRD's Scope section lists "consumer-facing UI" under **Out (v1)**, the Users section describes exactly two v1 roles — application teams (integrate via API) and operators (act via "operator-authenticated routes on the same REST API driven by a small CLI") — and the Architecture Spine's Deferred section confirms "No UI in v1 scope" for the operator surface. This is a deliberate, PRD-level decision consistently carried through architecture and epics (Epic 3's approval stories and Epic 5's monitoring stories are API/CLI-shaped, never UI-shaped), not an oversight to flag.

## Epic Quality Review

Reviewed against create-epics-and-stories standards, applied rigorously rather than rubber-stamped.

### Epic Structure Validation

| Epic | User value? | Independent of future epics? | Verdict |
| --- | --- | --- | --- |
| 1. Foundation — Accounts, Ledger & Deposit Addresses | Yes — app teams create customers, query balances, transfer, get addresses | Yes — fully standalone | ✓ Pass |
| 2. Deposit Monitoring, Crediting & Reorg Safety | Yes — deposits detected, credited, survive reorgs/downtime | Yes — needs only Epic 1 | ✓ Pass |
| 3. Withdrawals, Fees & Treasury Sweeps | Yes — app teams withdraw with fee estimates, safe policy, approval | Yes — needs only Epics 1–2 | ✓ Pass |
| 4. Event Notifications (Webhooks) | Yes — app teams stop polling, get verified real-time events | Yes — needs only outbox rows from Epics 2–3 | ✓ Pass |
| 5. Independent Reconciliation & Operational Monitoring | Yes — operators get independent drift detection + ops alerts | Yes — needs only Epics 1–4 | ✓ Pass |
| 6. Security, Resilience & Portfolio Readiness | Yes — external reviewer can read/run/trace (PRD Success Metric 5, verbatim) | Yes — capstone, needs Epics 1–5 (all previous) | ✓ Pass |

**Minor naming note:** Epic 1's title leads with "Foundation," a word that can read as a technical-milestone red flag ("Database Setup"-shaped). Checked the substance, not just the title: the epic's goal statement and all five stories are user-value framed (create/query/transfer/list/address), and the scaffolding work (hexagonal skeleton, Postgres, CREATE2 contracts) rides along inside real user-facing stories rather than existing as its own empty-value story. **No violation** — cosmetic only.

### Story Quality Assessment

- **Sizing:** all 25 stories are single-capability, single-endpoint-or-process scoped; none bundle unrelated capabilities.
- **Given/When/Then structure:** consistently applied across all 25 stories.
- **Error conditions:** present where a mutating/query endpoint exists (400/401/404/422 problem+json responses in Stories 1.1–1.3, 1.5, 3.1–3.2); background-process stories (watchers, reconciliation) appropriately use dedup/idempotency ACs instead of HTTP error codes, which is the correct shape for their domain.
- **Specificity:** every AC cites the concrete mechanism (table names, status codes, AD-n rules) rather than vague outcomes like "user can withdraw."

### Dependency Analysis

**Within-epic ordering** — verified for all six epics; no story requires a later story in the same epic to be completable. Notably checked two patterns that could *look* like forward dependencies but aren't:

- **Story 2.1** notes its `deposit.pending` outbox row is "ready for Epic 4's dispatcher to deliver." This is descriptive context, not a functional dependency — the AC's testable claim (the row exists, atomically) is verifiable by querying Postgres directly, with or without Epic 4 built yet.
- **Story 3.2** establishes a binding convention ("every later withdrawal state transition writes its outbox event") that Stories 3.3–3.5 then follow. This is convention-setting for future stories, not Story 3.2 depending on them — 3.2 is fully completable and testable on its own before 3.3 exists.

**Database/entity creation timing** — verified table-by-table: Story 1.1 creates only `customers`/`accounts`/`idempotency_keys` (no `postings` table yet — Story 1.2's balance query and AD-3 both correctly treat "zero balance" as the absence of postings, not a stored value); `postings`/`journal_entries` introduced in 1.3; `deposit_addresses` in 1.5; `deposits`/`watcher_cursors` in 2.1; `withdrawals` in 3.2; `sweeps` in 3.6; `outbox_events` implicitly required starting Story 2.1 (first real event) — no upfront over-creation found anywhere.

**Starter template check:** N/A — Architecture Spine explicitly states no starter template (greenfield custom Go service); correctly, no Epic 1 story claims to scaffold from one.

### Findings by Severity

**🔴 Critical Violations:** None found.

**🟠 Major Issues:** None found.

**🟡 Minor Concerns:**
1. Epic 1's "Foundation" naming (cosmetic — see above, no action needed).
2. **Fixed during this review:** the Requirements Inventory's top-level FR Coverage Map (written before epics existed) attributed FR34 to "Epic 1/Epic 2"; the actual epic assignment (confirmed in both the Epic 2 summary line and Story 2.3) is Epic 2 only. Corrected in `epics.md` to remove the stale dual attribution.

## Summary and Recommendations

### Overall Readiness Status

**READY.**

### Critical Issues Requiring Immediate Action

None. Zero critical violations, zero major issues. FR coverage is 34/34 (100%), verified against literal story text, not just the claimed map. Epic and story dependency structure is clean in both directions (no epic requires a later epic; no story requires a later story in its own epic). UX absence is a deliberate, consistently-documented v1 decision, not a gap.

### Recommended Next Steps

1. **Proceed to Sprint Planning (`bmad-sprint-planning`)** — nothing found here blocks it.
2. **Resolve the two still-open PRD items before or during sprint planning**, since they affect concrete test data and launch readiness rather than epic structure itself:
   - **First consuming application identity** (PRD Open Question 2) — owner André, was already flagged as "revisit before epic breakdown"; epic breakdown is now done, so this is due.
   - **Operational threshold values** (PRD Open Question 3) — per-asset manual-approval threshold (Story 3.3), watcher-lag alert threshold, and approval-queue alerting ceiling (Story 5.3) — owner André + operator, flagged for the pre-launch checklist; sprint planning is a reasonable point to at least stub placeholder values so story implementation isn't blocked.
3. No architecture or epics rework is recommended based on this assessment — the one inconsistency found (FR34 dual attribution) was corrected in place during this review.

### Final Note

This assessment identified 1 corrected inconsistency and 1 cosmetic naming note across 6 categories (document discovery, PRD analysis, epic coverage, UX alignment, epic quality, dependency analysis) — no critical or major issues. The PRD, Architecture Spine, and Epics/Stories are aligned and ready for Phase 4 implementation, contingent only on resolving the two carried-forward PRD open questions above (neither blocks starting sprint planning).

---
**Assessed by:** bmad-check-implementation-readiness · **Date:** 2026-07-14
