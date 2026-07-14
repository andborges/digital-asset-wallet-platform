# Reconciliation — ARCHITECTURE-SPINE.md vs PRD + Addendum

Input: `prd.md` + `addendum.md` (prd-digital-asset-wallet-platform-2026-07-13) against `ARCHITECTURE-SPINE.md` (2026-07-14 draft).
Method: every FR1–FR34 and NFR1–NFR19 checked for a governing AD, convention, seed element, or Deferred entry; addendum constraints and qualitative requirements checked separately. Coverage standard per instruction: an item is covered if something in the spine clearly governs it — prose restatement not required.

## 1. Coverage confirmed (no action)

- **F1 (FR1–FR5):** AD-3 (double-entry, cause-per-entry), AD-4, AD-5 (internal-transfer idempotency), ER diagram (CUSTOMER/ACCOUNT/POSTING). Covered.
- **F2 (FR6–FR7):** AD-8; attribution by (address, chain); DEPOSIT_ADDRESS entity. Covered (but see finding G-2 on the addendum's recovery-path consequence).
- **F3 (FR8, FR9, FR11):** AD-6 deposit machine states match FR8 verbatim; AD-7 policy table matches FR9's "config, not rework" requirement; FR11 unsupported-token rule explicit in AD-14. Covered. FR10 — see G-6.
- **F4 (FR12–FR14):** AD-6/AD-7 (history changes only affect sub-finalized records), AD-5 (persisted cursor, harmless over-rescan). Covered.
- **F5 (FR15–FR17, FR19, FR20):** AD-4/AD-5/AD-6 (awaiting-approval state present), AD-11 (nonces), ER "hold/settle/release", runbook in `docs/`, operator approval surface in Deferred. Covered. FR18 — see G-7.
- **F6 (FR21–FR22):** AD-1 + source-tree note naming Arbitrum NodeInterface / Base GasPriceOracle (matches addendum fee mechanics exactly); AD-14 binds F6 queries; caching deferred explicitly. Covered.
- **F7 (FR23–FR24):** AD-5 (constraint-enforced), AD-14 (Idempotency-Key header mandatory). Covered.
- **F8 (FR25, FR26, FR28):** AD-12 — independent RPC provider, streaming + batch (daily floor / hourly target = NFR10 verbatim), queryable run history, seeded-fault test (= counter-metric 1 guard). Covered. FR27 — see G-8.
- **F9 (FR29–FR31):** AD-4 + AD-13 (outbox, at-least-once, backoff, HMAC-SHA256, event ID = outbox row ID). Covered. One seam noted at G-9.
- **F10 (FR32–FR34):** AD-1 with CI import-boundary check (matches success metric 4's enforcement clause); asset identity convention (chain, symbol, contract) carries FR34's token-registry direction. Covered.
- **NFR1–NFR3:** AD-4/AD-5/AD-7 + testing convention (anvil_reorg, kill-mid-transition). Covered.
- **NFR7, NFR8:** single-EC2 deployment; AD-5 cursors + AD-6 resume-from-persisted-state. Covered.
- **NFR10, NFR11:** AD-12; AD-3 + logging convention (actor/timestamp/reason). Covered.
- **NFR12:** threat model as `docs/` deliverable + capability-map row. Covered as an artifact home (content is downstream by design). See C-1 for the ordering problem.
- **NFR13 (boundary + no secrets in logs), NFR15, NFR16:** AD-10, AD-14, Auth + Errors conventions. Covered.
- **NFR14:** `docs/` key ceremony & DR; deployment note states WAL archiving + S3 backups "carries NFR2/NFR14"; the in-stack-Postgres residual risk is self-flagged in Deferred with a revisit trigger keyed to the NFR2 bar. Covered-with-acknowledged-risk (not a dropped item).
- **NFR18, NFR19:** Logging convention (UUID-per-line, chain event → ledger → webhook traceability) and Testing convention (reorgs, duplicates, crashes, no live-testnet dependency — externally runnable). Covered.
- **Addendum items honored:** fee mechanics per chain; confirmation-tier model and finalized crediting (Flashblocks lesson); dedupe on (chain, tx_hash, log_index) satisfying the addendum's "(address, chain, tx hash), never address alone"; Bybit lesson (AD-10/NFR16, raw-tx self-verification); API-protocol open item legitimately resolved to REST (AD-14) — that one was architecture's to decide; rescan-after-outage (Base/Arbitrum outage history) via AD-5.
- **Spine additions consistent with PRD:** sweeps (AD-9, AD-11) are derived scope forced by the forwarder design and are handled the FR5-compliant way (ledger-visible, state-machined); not a contradiction.

## 2. Contradictions

### C-1 — AD-10 fixes the key mechanism the threat model was required to drive (NFR13, Open Question 1, addendum "Deliberately unresolved") — HIGH
PRD NFR13: "mechanism — software/KMS/HSM — is an architecture decision **the threat model drives**." Addendum: "the *mechanism* must be driven by the threat model." The spine adopts AWS KMS (`ECC_SECG_P256K1`, specific signer library, one key = one hot wallet) in AD-10 [ADOPTED] while the threat model is still an empty `docs/` deliverable. The decision may well survive threat-model review — KMS is a defensible default — but the spine inverts the mandated dependency and nowhere marks AD-10 as provisional-pending-threat-model. Either record the threat-model rationale that drove KMS, or downgrade AD-10's mechanism clause from ADOPTED to conditional (the Signer-port boundary itself is uncontested).

### C-2 — AD-8's CREATE2 forwarders silently invalidate the addendum's unwatched-chain recovery premise (FR6 implications section) — HIGH
The addendum's address-reuse analysis rests on "same key → same address under EVM address derivation" and concludes funds sent to a customer address on an **unwatched** EVM chain (Ethereum L1, Optimism, Polygon, …) "are recoverable by the operator **since the platform holds the keys** — the threat model and operator runbook should cover this recovery path." AD-8 replaces key-derived EOAs with counterfactual CREATE2 forwarders, which changes both the mechanism and the guarantee: recovery now requires deploying the deterministic factory and the customer's forwarder on the unwatched chain (possible on standard EVM chains, but operationally nontrivial), and is **not possible at the same address** on chains with non-standard CREATE2/deployment semantics (e.g., zkSync Era) — funds sent there are simply lost, a strictly worse outcome than the EOA premise. The spine neither carries the runbook/threat-model recovery-path requirement (the `docs/` line doesn't mention it, no Deferred entry) nor acknowledges the changed risk. FR6's two-chain requirement is still met; the dropped item is the addendum constraint. Needs: a Deferred or docs-scope entry for the unwatched-chain recovery procedure, and a threat-model line item for the non-CREATE2-chain loss case.

## 3. Gaps — requirements with no home

### G-3 — Performance/capacity envelope: NFR4, NFR5, NFR6 — MEDIUM
No AD, convention, seed note, or Deferred entry mentions the sizing bar (10³/day with ~10× burst, A7), the ≤1-minute platform overhead from finality-observed to webhook-sent (A8), or the 500 ms p95 read budget. Nothing in the architecture *conflicts* with them, but nothing governs them either — e.g., watcher/dispatcher polling cadence directly determines NFR5 and is unconstrained; AD-3's "balances derived, cacheable" leaves the NFR6 posture undecided. One conventions row or Deferred entry stating the envelope (and that polling cadences must fit inside the 1-minute budget) would close this.

### G-4 — Counter-metric 2 and the *condition-type* NFR17 alerts have no home — MEDIUM
The Deferred "Alert transport" entry covers where alerts land and says the platform's contract is "the outbox event + queryable status." That works for event-shaped alerts (drift finding, stuck withdrawal, approval required). It does not cover the two **threshold-evaluated conditions** NFR17 requires: watcher lag beyond threshold and chain-liveness loss — neither is an outbox event anything currently emits — nor PRD counter-metric 2's explicit requirement that **time-in-awaiting-approval is tracked and has an alerting ceiling**. Counter-metric 1 got an explicit AD-12 clause; counter-metric 2 got nothing. Something must own evaluating these conditions (recon role? watcher self-report?) — currently no component does.

### G-5 — NFR9 explicit-degradation semantics dropped — MEDIUM
NFR9 requires that during sequencer/chain-liveness loss the platform "degrades explicitly (deposits pending, **withdrawals queued or rejected with a clear error**)." Recovery (rescan, resume) is well covered by AD-5/AD-6, but the degraded-mode *behavior* — what the broadcaster and API do with withdrawal requests while a chain is down, and how "chain down" is even detected — is governed nowhere. The addendum's incident section ("treat sequencer downtime … as a first-class scenario") reinforces that this deserved at least a Deferred entry.

### G-6 — FR10 pending-deposit query surface omitted from AD-14's binds — LOW
AD-14 binds "F1, F5, F6, F8 queries"; F3's query requirement (pending deposits queryable with their current tier) is not listed, and the capability map routes F3 only to watcher + AD-5/6/7, none of which govern an API surface. Almost certainly an oversight rather than a decision — FR3 history-with-status gets close but FR10's "current tier" is deposit-specific. Fix: add F3 to AD-14's binds.

### G-7 — FR18's v1 policy-check set exists only as the word "policy" — LOW
Balance-covers-amount-plus-fee, well-formed/non-zero destination, threshold routing: the paradigm prose puts "withdrawal policy" in core, but no AD states the check set or that it runs pre-sign, and the capability map's F5 row (AD-4/6/10/11) doesn't include it. Terse is fine; zero governing sentence is a gap for a requirement the PRD spelled out check-by-check (and the Bybit lesson explicitly says "enforce policy checks").

### G-8 — FR27's double-counted-internal-transfer detection sits outside AD-12's frame — LOW
AD-12 defines recon as ledger-vs-chain comparison. Internal transfers have no on-chain counterpart, so "double-counted internal transfers" (FR27's fourth mandatory detection class) is a ledger-internal consistency check that the ledger-vs-chain framing cannot catch. AD-3 (balanced postings) and AD-5 (caller-key dedupe) *prevent* the fault, but FR27 requires *detecting* it independently. One clause extending recon's scope to internal-consistency invariants would close it.

### G-9 — Recon-alert webhooks need outbox writes AD-4 doesn't provide for — LOW (consistency seam)
FR29 requires reconciliation-alert webhooks; AD-13's dispatcher reads only outbox rows, and AD-4 writes outbox rows only inside journal+state-transition transactions. Recon "never writes journal postings" (AD-12), so its findings have no defined path into the outbox. Trivially fixable (findings write outbox rows sans journal entry), but as written the invariants don't compose for this event class.

## 4. Qualitative / tone requirements

- **Portfolio-quality bar (success metric 5, NFR18, FR19 runbook):** carried — `docs/` names threat model, runbook, key ceremony & DR; logging convention explicitly cites the traceability bar; test suite externally runnable. No gap.
- **Operator experience ("hears it from the platform first, enough detail to act"):** partially carried — FR26 detail via AD-12 findings, approval surface deferred deliberately; the residue is exactly G-4 (the platform can't be first to tell the operator about conditions nothing measures).
- **Self-serve vision non-foreclosure:** carried — AD-14 rationale, extension seams listed in the final Deferred entry.
- **"Boring reliability" tone:** consistent throughout; no spine claim overpromises beyond PRD scope (single instance, no SLA, honest deferrals).

## 5. Verdict

The spine is a faithful, unusually tight projection of the PRD: 31 of 34 FRs and 16 of 19 NFRs have clear governing homes, both counter-metrics' spirit is half-carried, and the two deliberately-unresolved addendum items were the two the spine resolved. The material findings are the two contradictions (C-1 threat-model ordering, C-2 forwarder recovery premise) and the three medium gaps (performance envelope, condition-type alerting incl. counter-metric 2, degraded-mode semantics). All are closable with one AD amendment, two Deferred entries, and one conventions row — no structural rework indicated.
