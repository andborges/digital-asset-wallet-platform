# Epic 3 Context: Withdrawals, Fees & Treasury Sweeps

<!-- Generated from planning artifacts. Regenerate with compile-epic-context if planning docs change. -->

## Goal

Application teams request withdrawals with an up-front, chain-correct fee estimate; the platform holds funds immediately, validates policy, routes above-threshold requests to explicit operator approval, and manages signing, nonces, and broadcasting on the platform's behalf so consuming applications never touch gas or raw transactions. Stuck or failed withdrawals are recoverable, never silently lost. This epic also performs the system's first real on-chain write path: sweeping accumulated forwarder balances (from Story 1.5's CREATE2 forwarders) into treasury as a first-class ledger event. It introduces the Signer port and the single-writer-per-chain broadcaster process that all outbound signing/broadcasting — withdrawals and sweeps alike — funnels through.

## Stories

- Story 3.1: Query Fee Estimate Before Submitting a Withdrawal
- Story 3.2: Request a Withdrawal With Idempotent Hold Placement
- Story 3.3: Enforce Pre-Signing Policy & Threshold-Based Approval Routing
- Story 3.4: Sign & Broadcast Withdrawals via the Single-Writer Broadcaster
- Story 3.5: Resolve Stuck & Failed Withdrawals
- Story 3.6: Sweep Forwarder Balances to Treasury

## Requirements & Constraints

- Withdrawal requests require an idempotency key; the amount is placed on hold atomically with record creation. A replayed key returns the original result — no second hold, no second movement.
- Fee estimates must combine both L2 fee components (execution fee + amortized L1 data fee); naive L1-style estimation undercharges. Unsupported chain/asset returns 400 problem+json.
- Withdrawal states: created → awaiting-approval? → approved → signed → broadcast → confirmed | failed. Every transition is unambiguous and resumable after a crash.
- Amounts above a configurable per-asset threshold enter awaiting-approval, requiring explicit operator approval (logged with actor/timestamp/reason); at-or-below-threshold proceeds automatically. Threshold values are an ops/pre-launch setting, not fixed here.
- Before signing (either path): available balance covers amount + estimated fee, destination is well-formed and not a known-invalid target (e.g. zero address), threshold routing already applied. Any failure blocks signing; v1 policy is deliberately minimal.
- The platform owns nonce allocation and sequencing per hot wallet; no API response ever exposes a nonce, raw transaction, or gas parameter.
- Terminal failure releases the hold immediately. Broadcast-but-unconfirmed beyond an operational window surfaces to the operator as "stuck" with a runbook resolution path. Broadcaster crash mid-transition must resume with no ambiguous double-broadcast.
- On degraded chain liveness, withdrawals are accepted and queue — never rejected — resuming automatically once liveness returns.
- Signing keys/secrets must never appear in logs, errors, or API responses, regardless of signer backend.
- Sweeps consolidate forwarder-float into treasury; postings never touch customer accounts. Only one sweep in-flight per (chain, forwarder, asset).
- Every transition this epic introduces writes a corresponding outbox event in the same transaction as the state change — the mechanism Epic 4's dispatcher later ships.

## Technical Decisions

- **Account taxonomy:** customer available, customer hold (in-flight withdrawal reserve), platform forwarder-float per chain×asset (credited-but-unswept), platform treasury, fees. This epic is the first to actually write hold and treasury postings. Balances are always derived from postings, never stored directly.
- **Transactionality:** every transition (hold, approval, signing, broadcast, confirm/fail, sweep) commits as one Postgres transaction with its outbox event; money-moving transitions carry balanced journal postings in the same transaction.
- **Idempotency:** API mutations dedupe via a unique idempotency-keys table; nonce allocation happens in the same transaction as the broadcast-attempt row, so a crash between allocation and broadcast leaves a resumable record, not a gap.
- **State machines:** withdrawal/sweep transition functions live once in core. API writes withdrawal rows through core; the broadcaster is the exclusive writer of broadcast/sweep progress.
- **Sweeps:** the broadcaster is sole creator/owner of sweep records — deploy-forwarder (if needed) + flush to treasury, gated by a partial unique index for one in-flight sweep per (chain, forwarder, asset). Trigger policy (threshold/schedule) is ops configuration.
- **Signer port:** all signing crosses one core-defined Signer interface. One KMS adapter implementation (`ECC_SECG_P256K1`; DER→r/s, low-s normalization, v-recovery via a vendored, owned library, not a live dependency) is used against real AWS KMS in prod and LocalStack KMS locally via endpoint override — same adapter, same code path. A plain software signer sits behind the same port for unit tests and as the documented LocalStack-gap fallback. One KMS key = one hot-wallet address valid on both chains. Raw transactions are constructed and verified by the platform itself — no third-party signing UI in the path.
- **Broadcaster:** exactly one process per chain sends all outbound transactions (withdrawals and sweeps, same funnel), enforced at runtime by a Postgres advisory lock keyed by chain taken at startup — the process exits if it can't acquire it. Nonces come from persisted per-chain state.
- **Fee mechanics:** chain-specific, isolated in the EVM adapter — Arbitrum via `NodeInterface.gasEstimateComponents()`, Base via the `GasPriceOracle` predeploy. Caching/refresh policy is an adapter-internal detail.
- **Degraded liveness:** each chain exposes an explicit liveness status from watcher heartbeat/cursor staleness. On degradation, the broadcaster holds and withdrawals queue rather than reject; recovery is unattended.
- Money is integer base units only (`NUMERIC(78,0)` / `*big.Int`); no floats touch money.

## Cross-Story Dependencies

- Story 3.2 establishes the withdrawal state machine and the outbox-per-transition rule; Stories 3.3–3.5 advance the same machine under that rule, not a parallel one.
- Story 3.3's policy check consumes Story 3.1's fee estimate and gates entry into Story 3.4's signing path.
- Story 3.4 depends on Story 3.3 reaching "approved," and is the first consumer of the Signer port and advisory-locked broadcaster; Story 3.5's crash-recovery and stuck-withdrawal handling operate on the same broadcaster process and rows Story 3.4 creates.
- Story 3.6 reuses the same broadcaster/Signer funnel as withdrawals and depends on Story 1.5's CREATE2 factory/forwarder contracts being deployable on-chain — the first story to actually drive those contracts.
- Outbox events written across this epic are inert until Epic 4's dispatcher ships; this epic's job is only to write them correctly in-transaction.
