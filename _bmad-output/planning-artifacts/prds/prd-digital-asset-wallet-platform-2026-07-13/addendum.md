# PRD Addendum — Digital Asset Wallet Platform

Depth that belongs downstream (architecture, threat model, UX of ops tooling) or supports the PRD without fitting its body.

## Fee estimation mechanics (per chain)

Two components on both chains: L2 execution fee (EIP-1559-style, chain-specific parameters) + amortized L1 data fee (blob-based since EIP-4844).

- **Arbitrum:** `NodeInterface.gasEstimateComponents()` — L1 cost folded into gas.
- **Base (OP-stack):** GasPriceOracle predeploy — L1 fee surfaced separately.

## Confirmation-tier model

- **Sequencer receipt:** ~200ms–2s; reorderable before L1 inclusion.
- **Safe:** batch posted to L1.
- **Finalized:** L1 finality, ~13–20 min (Base docs: ~20 min, "in practice impossible to reverse").

V1 credits at finalized, flat across assets/amounts; the policy table is per-chain/per-asset config so tiering by amount later is config, not rework.

## Address reuse across EVM chains (FR6 implications)

One deposit address per customer, valid on every EVM chain (same key → same address under EVM address derivation). Consequences for architecture:

- Attribution and deduplication must key on (address, chain, tx hash), never address alone — the same address legitimately receives on both Base and Arbitrum.
- The address is also valid on EVM chains the platform does not watch (Ethereum L1, Optimism, Polygon, …). Funds sent there are not observed and not credited. Recovery depends on the address mechanism: with CREATE2 forwarders (the architecture's choice), funds are recoverable only on chains where the same deterministic factory can be deployed at the same address (EVM-equivalent chains); on chains with divergent CREATE2 semantics (e.g. zkSync-style) they are effectively unrecoverable. The threat model and consumer documentation must state the supported-chain boundary explicitly, alongside FR11's unsupported-token case.
- Adding a chain (FR33) requires no address migration: existing customer addresses work on the new chain from day one; the new watcher simply starts observing them.

## Industry grounding for the crediting policy (research, Jul 2026)

- Coinbase: ~1 L1 batch confirmation (~5–15 min) for Arbitrum/Base deposits; delays very large L2 deposits.
- Binance: up to ~20 L1 batch confirmations on Arbitrum (~30–60 min typical credit).
- Gate.io: waits for finality on both L2 and L1 (~30 min for Base/Arbitrum) — closest public precedent to our chosen policy.
- No major exchange publicly credits at sequencer tier; no public tiered-by-amount crediting policy found (tiering exists only in the conservative direction — extra scrutiny for large deposits).
- Reconciliation norms: streaming break-detection plus hourly-to-daily batch reconciliation; auditors increasingly expect independent reconciliation.

## Incident references for the threat model

- **Bybit, Feb 2025 (~$1.5B):** Safe{Wallet} front-end supply-chain compromise. Lessons: third-party signing UIs are attack surface; verify raw transactions; enforce policy checks and withdrawal delays.
- **ETC 51% attacks, 2019/2020:** 100+ block reorgs; Gate.io raised confirmations 500→4000.
- **Base sequencer outages:** Jun 2026 (two back-to-back: 116 min + 20 min; stale journal state in block builder + recovery race), Aug 2025 (~30 min "unsafe head delay"), Sep 2024. Arbitrum inscription-surge outage Dec 2023. All liveness failures, not reorgs — treat sequencer downtime (deposits/withdrawals halted, watcher gaps) as a first-class scenario.
- Base's early Flashblocks implementation produced tail-flashblock reorgs (streamed preconfirmations not included in the final block) — concrete argument against sequencer-tier crediting.
- No publicly documented 2025–2026 L2 deposit double-credit incident at an exchange — frame double-credit as a theoretical reorg/rescan risk, not a cited event.

## Deliberately unresolved (architecture-phase)

- **Key storage mechanism:** software keys vs. cloud KMS vs. HSM — custody *decision* is made (platform holds keys); the *mechanism* must be driven by the threat model.
- **API protocol:** REST vs. gRPC for the internal API — not a product decision.

## Rejected alternatives

Recorded with rationale in the product brief's addendum (`_bmad-output/planning-artifacts/briefs/brief-digital-asset-wallet-platform-2026-07-13/addendum.md`): external signer (Turnkey/Fireblocks-style) rejected for v1; buying a vendor platform rejected on cost structure, M&A churn, ledger-side misfit, and portfolio purpose. Landscape table lives there too.
