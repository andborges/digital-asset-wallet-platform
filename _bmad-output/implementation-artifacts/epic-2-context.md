# Epic 2 Context: Deposit Monitoring, Crediting & Reorg Safety

<!-- Generated from planning artifacts. Regenerate with compile-epic-context if planning docs change. -->

## Goal

Deposits sent to a customer's issued address must be detected, tracked through L2 confirmation tiers, credited to the customer's balance only once truly final, and recovered correctly no matter what the chain does in the meantime — reorgs, sequencer reordering, or watcher downtime. This is the first epic with real chain-specific code, so it's also where the chain-adapter boundary has to hold structurally, not just on paper: nothing chain-specific may leak outside the adapter package.

## Stories

- Story 2.1: Track Incoming Deposits & Expose Pending Status
- Story 2.2: Credit Deposits at Finalized Tier via Policy Table
- Story 2.3: Handle Unsupported Token Transfers Without Ledger Corruption
- Story 2.4: Detect & Safely Reverse Reorged Pre-Finality Deposits
- Story 2.5: Recover From Watcher Downtime Without Loss or Double-Processing

## Requirements & Constraints

- Deposits progress through explicit lifecycle states: observed → safe → finalized → credited, with `orphaned` as the reorg exit. Tiers map to sequencer receipt (~200ms-2s, reorderable), safe (batch posted to L1), and finalized (L1 finality, ~13-20 min).
- Crediting happens only at the finalized tier in v1, driven by a per-(chain, asset) policy table — not hard-coded — so a future tiered policy is a config change, not rework.
- A credited balance must never be reversed by any chain event; only pre-finality records are ever mutated by history changes. This is a hard platform invariant, not a best-effort behavior.
- The platform must detect both sequencer reordering (pre-L1-inclusion) and L1 reorgs affecting batch inclusion, and must recover from watcher downtime or chain outages by rescanning missed ranges without ever missing or double-processing a deposit.
- Only native ETH and USDC are supported in v1; transfers of any other token must be recorded for visibility but never credited and never touch the ledger. The supported-token set is data (a registry), not code, so adding an ERC-20 on an already-supported chain is a config change.
- All chain-specific logic (RPC access, confirmation-tier mapping, fee mechanics, token contracts) must live entirely behind a chain-adapter interface; a CI check enforces that no chain ID or go-ethereum import exists outside it. This is the acceptance test for "adding a third chain touches only the adapter."
- Read/query paths (deposit status, pending visibility) share the same 500ms p95 latency expectation as other read APIs.
- Reorg and crash-recovery behavior must be testable deterministically (anvil fork mode with forced reorgs), not just asserted.

## Technical Decisions

- Attribution is strictly via the persisted (address, chain) table established in Epic 1 — watchers never re-derive an address and never attribute by address alone. Deposit senders may be EOAs with delegated code, so attribution logic must not rely on `EXTCODESIZE`/`tx.origin` heuristics.
- The chain's watcher process is the sole writer of deposit rows, including the credit transition itself — no other process path may write or advance a deposit row. Deposit, and later withdrawal/sweep, transition functions live once in the core and are the only write path to machine state.
- Every observable state change (including the credit) is one Postgres transaction combining the state transition, the balanced journal entry (debit forwarder-float, credit customer available), and the outbox event — all atomic, no partial-commit window.
- Idempotency at the chain-event level is a DB constraint, not application logic: unique `(chain, tx_hash, log_index)` makes re-observing the same event during a rescan a no-op by construction. Watcher progress is a persisted cursor per (chain, tier); rescanning from any earlier cursor is always harmless.
- On degraded chain liveness (heartbeat/cursor staleness), deposits simply stay pending — finality isn't advancing, nothing is lost, nothing is rejected. Recovery is unattended cursor-based rescan; degradation and recovery both raise alert events (consumed by the Epic 4 dispatcher).
- Unsupported-token observations get their own observation type, distinct from deposit records, and never produce a journal posting.
- Fault-injection testing for this epic runs against anvil in fork mode using `anvil_reorg` for deterministic reorg scenarios; CI never depends on live testnets.

## Cross-Story Dependencies

- Depends on Epic 1: the per-customer deposit address (Story 1.5) and per-(chain, asset) account provisioning (Story 1.1) must already exist before a watcher has anything to attribute deposits to.
- Every state transition here (observed, credited, orphaned) writes an outbox event in the same transaction; Epic 4's dispatcher is the consumer that actually delivers these as webhooks, but this epic is responsible for the events existing correctly.
- Epic 5's reconciliation process independently checks the deposit/journal state this epic produces, but never writes to it — this epic owns the only write path.
- Epic 6's consolidated fault-injection suite reuses this epic's reorg-during-pending-deposit and watcher-downtime scenarios (Stories 2.4, 2.5) as part of its combined test run.
