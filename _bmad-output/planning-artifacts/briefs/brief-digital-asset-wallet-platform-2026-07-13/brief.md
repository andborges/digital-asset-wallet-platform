---
title: Product Brief — Digital Asset Wallet Platform
status: draft
created: 2026-07-13
updated: 2026-07-13
---

# Product Brief: Digital Asset Wallet Platform

## Executive Summary

The Digital Asset Wallet Platform is a Go backend that lets a company safely hold digital assets on behalf of its customers: it generates deposit addresses, watches the chain to credit incoming funds, and executes withdrawals — reliably, idempotently, and with an auditable record that always matches what is actually on chain. It launches internally, powering customer deposit and withdrawal flows on two EVM Layer-2 networks (Base and Arbitrum), with a per-blockchain abstraction that keeps additional chains cheap to add.

This is the part of wallet infrastructure that everyone ends up building by hand. Commercial vendors (Fireblocks, Coinbase CDP, Turnkey, Privy) concentrate on key management and signing; no mature open-source or vendor offering cleanly covers the *ledger side* — deposit monitoring, reorg handling, transaction state machines, and independent reconciliation between the internal ledger and the chain. Meanwhile the vendor market is consolidating fast (Privy→Stripe, Dynamic→Fireblocks, BitGo IPO), making vendor-continuity risk a live argument for owning this layer.

Today the project is a passion/portfolio effort, but it is run like a product: real requirements, tests, and a threat model from day one. If it proves itself internally, the platform itself — not just the application it powers — is the candidate product: wallet infrastructure other companies could run.

## The Problem

Any company that credits customers when crypto arrives and pays them out on request faces the same hard backend problems, and gets them wrong at real cost:

- **Deposits are not binary.** On Base and Arbitrum, a transaction passes through sequencer confirmation, L1 batch inclusion ("safe"), and L1 finality (~13+ minutes). Credit too early and a reorg or sequencer reordering can double-credit funds — the failure mode behind the Ethereum Classic 51%-attack losses that forced exchanges to raise confirmation requirements 8×.
- **Withdrawals must never happen twice.** Retries, crashes, and duplicate API calls are normal; without strict idempotency and a transaction state machine, they become duplicate payouts.
- **Fees are chain-specific and easy to get wrong.** L2 fees combine an L2 execution fee with an amortized L1 data fee, surfaced differently on Arbitrum and Base. Naive L1-style estimation systematically undercharges.
- **The ledger drifts from the chain.** Missed deposits, stuck withdrawals, and double-counted internal transfers are documented, recurring failure modes across the industry. Without independent reconciliation, drift is discovered by customers, not operators.

The alternatives are unattractive: institutional vendors price at six figures and scale costs with volume; embedded-wallet vendors solve a different problem (consumer key custody); and the open-source ecosystem covers signing, not orchestration. Companies at this layer build in-house — usually under deadline pressure, without tests or a threat model.

## The Solution

A single Go service (the platform) that a consuming application talks to over an internal API, providing:

- **Address generation** — unique deposit addresses per customer/account.
- **Deposit monitoring** — chain watchers that track confirmations across the L2 confirmation tiers and credit deposits only at a deliberately chosen finality policy.
- **Reorg handling** — detection and safe reversal/re-crediting when the observed chain history changes.
- **Withdrawals via a transaction state machine** — every outbound transaction moves through explicit states (created → signed → broadcast → confirmed/failed) with no ambiguous intermediate conditions.
- **Idempotency** — every mutating operation is safely retryable; duplicate requests cannot duplicate money movement.
- **Fee estimation** — correct per-chain estimation for Base (OP-stack GasPriceOracle) and Arbitrum (gas-folded L1 costs).
- **Reconciliation** — an independent process that continuously compares the internal ledger against on-chain reality and surfaces drift before customers do.
- **Blockchain abstraction** — chain-specific logic isolated behind an interface, so chains beyond Base and Arbitrum are additive work, not rework.

The platform holds and manages signing keys itself — custody is in scope, not delegated to a third-party signer. That decision buys independence from vendor risk and pricing, and pays for it with security obligations the project takes seriously: a written threat model and tests are first-class deliverables, not afterthoughts.

**Assets:** native ETH and ERC-20 tokens, with USDC as the first token.

## What Makes This Different

Honestly: for the internal use case, the differentiator is *fit and ownership* — a ledger-grade wallet backend shaped exactly to the company's flows, with no per-transaction vendor pricing and no exposure to vendor M&A churn. For the future product path, the opening is real but unproven: the ledger-side orchestration layer (monitoring, state machine, reconciliation) is the gap in both the vendor and open-source landscape. There is no technical moat today; the bet is that rigor — tests, threat model, correct L2 mechanics — is rare enough in this niche to be the product.

## Who This Serves

- **Primary (now):** the company's own application teams, who integrate deposits and withdrawals through one internal API instead of touching chains directly. Success for them: they never think about reorgs, gas, or nonces.
- **Operators:** whoever answers for customer funds. Success for them: reconciliation is green, and when it isn't, they hear it from the platform first.
- **Future (if productized):** engineering teams at other companies facing the same build-vs-buy dead end.

## Scope

**In (v1):** Base and Arbitrum; deposits and withdrawals for ETH and USDC; address generation; fee estimation; idempotent APIs; transaction state machine; deposit monitoring with confirmation-tier policy; reorg handling; reconciliation; platform-held keys; test suite; written threat model.

**Out (v1):** non-EVM chains; third-party signer integrations; consumer-facing UI; staking, swaps, or DeFi interactions; compliance tooling (KYC/AML, travel rule); multi-tenant/WaaS features — deferred until the product path is real.

## Success Criteria

- **Correctness:** zero double-credits and zero duplicate withdrawals — including under injected reorgs, retries, and crash-recovery tests.
- **Reconciliation:** ledger-vs-chain drift is detected by the platform, not reported by users; any drift alarms within one reconciliation cycle.
- **Security:** a written threat model exists, is reviewed, and every identified high-risk item has a stated mitigation or an explicit accepted-risk entry.
- **Extensibility (proof of the abstraction):** adding a third EVM chain requires changes only inside the chain-adapter layer.
- **Portfolio quality:** the repository stands on its own as evidence of production-grade engineering — a reviewer can read the threat model, run the tests, and trace a deposit end to end.

## Vision

Internally proven first: the platform quietly runs customer deposits and withdrawals with boring reliability. If it succeeds, the platform itself becomes the product — self-hostable wallet infrastructure (the ledger-side layer vendors don't sell) that other companies run instead of building their own under deadline pressure. Multi-tenancy, policy controls, and a broader chain catalog are the road from here to there; none of them are needed to prove the core.

## Open Questions

1. **Key storage mechanics** — software keys, cloud KMS, or HSM? The custody *decision* is made (platform holds keys); the *mechanism* is an architecture-phase question the threat model must drive.
2. **Deposit crediting policy** — which confirmation tier (sequencer / safe / finalized) per asset and amount? A product decision with UX-vs-risk tradeoffs, to be settled in the PRD.
3. **Consuming application** — which internal app integrates first, and what volume should v1 be sized for?
