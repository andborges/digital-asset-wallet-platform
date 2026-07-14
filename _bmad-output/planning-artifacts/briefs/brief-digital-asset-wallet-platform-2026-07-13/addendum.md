---
title: Addendum — Digital Asset Wallet Platform Brief
status: draft
created: 2026-07-13
updated: 2026-07-13
---

# Addendum

Supporting depth for the product brief — material intended for downstream documents (PRD, architecture, threat model) rather than the brief itself. Research current as of 2026-07-13.

## Buy-vs-build landscape (mid-2026)

| Vendor | Positioning | Notes |
|---|---|---|
| Fireblocks | Institutional custody/treasury leader; MPC-CMP signing, policy engine | Six-figure entry pricing, volume-based; acquired Dynamic (embedded wallets) Oct 2025 |
| Coinbase CDP | Server + embedded wallets, keys in Coinbase enclaves, usage-priced | Developer-first; built-in policy engine, onramps/swaps |
| Turnkey | Low-level key-management primitives in AWS Nitro Enclaves (TEE) | Maximum control but you build tx orchestration yourself; AWS single-point-of-failure critique |
| Privy | Embedded consumer wallets (Shamir), 75M+ accounts | Acquired by Stripe June 2025 |
| Dfns | Delegated-signing WaaS with managed policy engine | $500M+ secured, 1M+ wallets |
| Circle Programmable Wallets | Wallet APIs from the USDC issuer | Circle IPO'd June 2025 |
| BitGo | Qualified custodian + wallet platform | Filed IPO, listing Q1 2026 |
| OSS (Openfort, Fystack, etc.) | Signing/key components | No mature OSS covers the ledger side: deposit monitoring, reconciliation, state machine |

**Why companies build in-house anyway:** cost at scale (vendor pricing scales with tx volume/wallet count); custody control and vendor risk (2025–26 M&A churn: Privy→Stripe, Dynamic→Fireblocks, Consensys→Web3Auth); jurisdiction-specific compliance may require direct key control. Counterpoint the brief must stay honest about: building custody in-house is typically a 6–18 month effort demanding specialized security work (key ceremony, HSM integration, disaster recovery).

## Base & Arbitrum technical context (for architecture/PRD)

- **Confirmation tiers.** Both are centralized-sequencer L2s: sequencer receipt (~200ms–2s, reorderable), "safe" (batch posted to L1), "finalized" (L1 finality, ~13+ min). Deposit-crediting policy must be explicit per tier; L2 reorgs are rare but sequencer reordering before L1 inclusion is possible, and post-L1-inclusion reorg requires an L1 reorg. (Chainstack, Arbitrum Docs, ChainLight)
- **Fees.** Two components: L2 execution fee (EIP-1559-style, chain-specific parameters) + amortized L1 data fee (blob-based since EIP-4844). Arbitrum folds L1 cost into gas via `NodeInterface.gasEstimateComponents()`; Base (OP-stack) exposes a separate L1 fee via the GasPriceOracle predeploy. Naive L1-style estimation undercharges. (Arbitrum Docs, Optimism Docs)

## Motivating incidents (threat-model inputs)

- **Bybit, Feb 2025 (~$1.5B ETH):** Safe{Wallet} front-end supply-chain compromise (Lazarus/TraderTraitor); signers approved a disguised transaction. Lessons: third-party signing UIs are attack surface; verify raw transactions; policy checks and withdrawal delays. (Sygnia, Forbes)
- **Ethereum Classic 51% attacks (2019, 2020):** 100+ block reorgs double-spent deposits; Coinbase observed ~$1.1M at risk, Gate.io lost ~$271k and raised confirmation requirements 500→4000. Canonical reorg-double-credit case. (Coinbase blog, Cointelegraph)
- **Reconciliation failure modes** cited by industry tooling: deposits recorded internally but missed by wallet ingestion, pending-withdrawal mismatches, double-counted internal transfers — motivating independent chain-vs-ledger reconciliation. (Cryptoworth, Cryptio)

## Rejected alternatives

- **External signer (Turnkey/Fireblocks-style) with platform as orchestrator only.** Rejected for v1: the custody decision is that the platform holds keys, preserving independence from vendor pricing/continuity risk. Revisit if the product path reaches customers whose compliance posture demands a qualified custodian.
- **Buying a full vendor platform.** Rejected: cost structure, vendor M&A churn, and the poor fit of vendor offerings to the ledger-side problem. Also contrary to the project's portfolio purpose — the point is to build this layer well.
