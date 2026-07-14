# Reconciliation: project-context.md vs. PRD

**Source input:** `_bmad/project-context.md`
**PRD:** `prd.md` + `addendum.md` (prd-digital-asset-wallet-platform-2026-07-13)
**Date:** 2026-07-13

## Item-by-item coverage

| # | Source item | PRD coverage | Verdict |
|---|-------------|--------------|---------|
| 1 | Abstraction by blockchain | F10 (FR32: chain-adapter interface isolating RPC, confirmation tiers, fee mechanics, token contracts; FR33: third-chain acceptance test). Also Success Metric 4 (extensibility). | Covered |
| 2 | Address generation | F2 (FR6: unique deposit address per customer/account per chain; FR7: durable address-to-customer attribution). | Covered |
| 3 | Fee estimation | F6 (FR21: L2 execution fee + amortized L1 data fee, naive L1-style estimation explicitly non-conforming; FR22: pre-submission estimate query). Addendum details per-chain mechanics (Arbitrum `NodeInterface.gasEstimateComponents()`, Base GasPriceOracle). | Covered |
| 4 | Idempotency | F7 as an explicit cross-cutting section (FR23: idempotency key on every mutating operation; FR24: exactly-once ledger effects under at-least-once delivery). Reinforced in FR4 (internal transfers), FR15 (withdrawals), NFR1, Success Metric 1. | Covered |
| 5 | Transaction state machine | F5 FR16 (explicit withdrawal state machine: created → approved → signed → broadcast → confirmed / failed; crash-safe resume), FR17 (awaiting-approval state), FR19 (stuck/failed states first-class), NFR2/NFR8 (resume from persisted state). | Covered — see emphasis note 1 |
| 6 | Deposit monitoring | F3 (FR8: chain watchers across sequencer → safe → finalized tiers; FR9: credit-at-finalized policy as per-chain/per-asset config; FR10: pending visibility; FR11: unsupported-token safety). Addendum grounds the tier model and industry crediting norms. | Covered |
| 7 | Reorg handling | F4 (FR12: detect history changes incl. sequencer reordering and L1 reorgs; FR13: safe reversal pre-finality, credited-never-reversed invariant; FR14: rescan after downtime without miss/double-process). NFR3, NFR9 (sequencer halts as expected condition). | Covered |
| 8 | Reconciliation | F8 (FR25: independent continuous ledger-vs-chain comparison; FR26: drift alarms within one cycle; FR27: minimum break catalog; FR28: queryable status/history). NFR10 (streaming + batch modes), Success Metric 2, counter-metric guard (seeded-fault verification). | Covered |
| 9 | Tests | NFR18 (fault-injecting suite: reorgs, duplicate requests, mid-state-machine crashes, RPC failures; runnable by an external reviewer). In-scope list; Success Metrics 1 and 5. | Covered |
| 10 | Threat model | NFR12 (written, reviewed, every high-risk item mitigated or accepted-risk), NFR13–NFR15 (key boundary, authn, no third-party signing UI). Addendum: incident references (Bybit, ETC 51%, Base outages). Success Metric 3. Open Question 1 delegates key-storage mechanism to it. | Covered |
| — | "Built with Go" (context line) | PRD F-section intro: "A single Go service exposing an internal API." | Covered |

## Gaps

None. Every functional capability in project-context.md has at least one numbered FR/NFR, and tests and threat model are both first-class deliverables (scope list, NFRs, success metrics).

## Emphasis notes (mismatches, not gaps)

1. **"Transaction state machine" narrowed to withdrawals.** The source phrase is generic; the PRD instantiates an explicit state machine only for withdrawals (FR16). Deposits get a lifecycle via confirmation tiers (FR8–FR10, FR13) and internal transfers are atomic (FR4), but neither is framed as a state machine. This is a reasonable reading — withdrawals are where ambiguous intermediate states cause duplicate payouts — but if the source author meant a uniform state machine across all transaction types (deposits, withdrawals, internal transfers), the PRD under-specifies deposit/transfer state modeling. Worth a one-line confirmation with the author.

2. **PRD substantially exceeds the source.** Accounts/ledger (F1), event notifications/webhooks (F9), manual-approval thresholds (FR17), and all NFRs have no counterpart in project-context.md. This is expected PRD elaboration, not drift, but note these additions carry no source-level mandate.

## Verdict

Full coverage of all ten source items plus the Go constraint. One emphasis question (state-machine scope) recommended for author confirmation; no dropped requirements.
