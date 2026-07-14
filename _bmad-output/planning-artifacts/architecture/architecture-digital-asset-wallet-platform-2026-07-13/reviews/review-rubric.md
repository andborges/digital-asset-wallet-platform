# Rubric Review — ARCHITECTURE-SPINE.md (digital-asset-wallet-platform)

- **Reviewed:** 2026-07-14
- **Artifact:** `_bmad-output/planning-artifacts/architecture/architecture-digital-asset-wallet-platform-2026-07-13/ARCHITECTURE-SPINE.md`
- **Driving PRD:** `_bmad-output/planning-artifacts/prds/prd-digital-asset-wallet-platform-2026-07-13/prd.md` (+ addendum.md)
- **Verdict:** **CONDITIONALLY SAFE** — a strong, unusually enforceable spine with no critical findings, but one high-severity seam (the event taxonomy around the outbox) and one undecided ownership question (sweep initiation) will produce incompatible answers from independent epic agents if handed down as-is. Fix H-1 and M-1 before handoff; the rest can ride along as annotations.

---

## Rubric point 1 — Fixes the real divergence points; misses none

**Mostly yes.** The spine nails the divergence points that actually kill this class of system: dependency direction (paradigm + AD-1), shared-state topology (AD-2), money representation and ledger shape (AD-3, conventions), atomicity (AD-4), idempotency mechanics (AD-5), state-machine vocabulary (AD-6, naming convention row), crediting policy (AD-7), address scheme immutability (AD-8), signing boundary (AD-10), nonce ownership (AD-11), recon independence (AD-12), event delivery (AD-13), API shape (AD-14). The Consistency Conventions table (money, IDs, time, state-name vocabulary, error format) is exactly the layer independent story agents would otherwise each invent.

**Missed divergence points:**

- **Event taxonomy vs. the outbox invariant** — see finding H-1. The single biggest gap.
- **Sweep initiation ownership** — see finding M-1. Undecided, and not listed under Deferred either.
- **Fee-estimation serving path** — see finding M-2.
- **Chain-liveness degradation behavior for withdrawals** (NFR9: "queued or rejected with a clear error") — the spine never picks one. The API epic and the broadcaster epic can each pick differently and both look conforming. See M-3.
- **Who runs migrations and when** (on process startup? a dedicated step before roles come up?) — with 7 concurrent processes sharing one Postgres, this is a small but real divergence point. (Low; see L-4.)
- **Fee settlement in the ledger** — FR18 holds amount + *estimated* fee; the ledger has a fees account; nothing says how the estimated-vs-actual gas difference settles (customer bears estimate? platform absorbs delta?). Recon will surface this as drift if two stories answer differently. (Low-medium; folded into L-5.)

## Rubric point 2 — Every Rule enforceable

**Largely excellent** — this spine is unusually honest about enforcement: AD-1 names a CI import-boundary check; AD-5 puts idempotency in unique constraints, not discipline; AD-3/AD-4 are schema- and review-checkable; AD-12 mandates a startup verification and a seeded-fault test; AD-8 pins concrete addresses and an immutability rule.

**Findings:**

- **AD-11 (single-writer broadcaster)** asserts "exactly one broadcaster process per chain" with no mechanism. Compose config is reviewable, but nothing prevents runtime overlap (deploy restart races, an operator starting a second process). The nonce-in-transaction rule and the guarded state transitions (AD-6) mitigate double-broadcast of a given withdrawal, but the *single-writer* claim itself is unenforced — a Postgres advisory lock / lease per (chain, role) would make it constraint-shaped like the rest of the spine. (Medium — M-4.)
- **AD-12 "different RPC provider … verified at startup"** — startup can verify the two URLs differ; it cannot verify they are different *providers* (two URLs of the same vendor pass). Acceptable as config discipline, but the Rule slightly overstates what the check proves. (Low — L-1.)

Everything else is checkable in review, CI, or schema. Pass.

## Rubric point 3 — Deferred items can't cause divergence

**Mostly safe.** Each deferral is either single-owner ("decided in the contracts epic"), ops-time ("RPC provider selection", "alert transport"), or confined by an AD ("finality detection mechanics" inside AD-1's boundary). The RDS deferral has an explicit revisit trigger and is correctly judged non-load-bearing for the schema.

**Findings:**

- **"Alert transport" deferral hides an undeferred question: alert *detection*.** Deferring where alerts land is fine; but NFR17 alerts (watcher lag, stuck withdrawals, approval-queue ceiling, chain-liveness loss) need a *detecting process* and an emission mechanism, and no AD or deferral assigns them. Recon owns drift findings only. This compounds H-1. (Part of M-3.)
- **"Forwarder contract internals" deferral** creates a cross-epic interface (broadcaster/sweep code calls the flush function the contracts epic defines). Single ownership is assigned, so this is an ordering dependency rather than a divergence, but the epic breakdown must sequence contracts before sweeps or pin the flush ABI early. (Low — L-2.)
- **"Operator approval surface"** is effectively decided in the deferral text itself (operator-authenticated routes on the same REST API + CLI); "confirm at epic breakdown" is fine. No divergence risk.

## Rubric point 4 — Named tech internally consistent (no re-research)

The stack table claims verification on 2026-07-14, matching the frontmatter created/updated dates. Internal cross-references check out: AD-10's KMS spec matches the stack row; AD-14's "stdlib ServeMux" matches the stack's "std-http target"; chain IDs in the stack match the topology; the anvil/Foundry testing convention matches the stack.

**Findings:**

- **Date inconsistency:** the artifact folder and PRD are dated 2026-07-13; the spine frontmatter says created/updated **2026-07-14**. Cosmetic, but downstream tooling keying on folder dates will mis-sort. (Low — L-3.)
- **`status: draft`** in frontmatter on a spine being handed downstream — either promote it or the handoff is formally premature. (Low — L-3.)
- **Docker Compose "v5"** cannot be cross-checked against anything else in the document (no other reference to a compose version); flagged as unverifiable-internally, not as wrong.

## Rubric point 5 — PRD coverage (F1–F10, NFR1–19) — Capability Map spot-check

**F1–F10: all mapped**, each to a home and governing ADs; the map also correctly surfaces derived scope (sweeps) and doc deliverables (threat model, runbook → NFR12/NFR14). Spot-checks pass: FR4 internal transfers (AD-3 cause list + AD-5 caller-key dedupe), FR11 unsupported tokens (AD-14), FR21 per-chain fee mechanics (source tree names NodeInterface / GasPriceOracle per the addendum), FR17 approval state (AD-6 + shared `awaiting-approval` vocabulary), FR28 queryable recon history (AD-12).

**Gaps:**

- **NFR4–NFR6 (capacity/performance) are bound in frontmatter but touched by nothing.** No AD, convention, or deferral carries the 500 ms p95 read bar, the ≤1 min finality-to-webhook overhead budget, or the 10× burst headroom. NFR5 in particular spans two independent epics (watcher cadence + dispatcher cadence) — with no stated budget split, both can individually conform and jointly miss the minute. (Medium-low — L-5.)
- **NFR9's explicit degradation choice** (queue vs. reject withdrawals during sequencer halt) is uncovered — see M-3.
- **FR29's event list** includes events the spine's own outbox rule can't produce as written — see H-1.
- **Addendum's off-chain-recovery note** (funds sent to the customer address on unwatched EVM chains are operator-recoverable; runbook should cover it) isn't reflected in the docs row — trivially fixable annotation. (Low, folded into L-2.)

## Rubric point 6 — Owned dimensions all decided/deferred/open

The operational envelope is *substantially* covered — better than most spines: deployment target (single EC2 + compose), three environments with signer parity (AD-10), provider strategy (two managed RPC vendors, role-split per AD-12), backup/DR (WAL archiving + S3 base backups, key ceremony & DR docs carrying NFR2/NFR14), Postgres-vs-RDS with a revisit trigger, secrets handling, logging/traceability (NFR18 convention).

**Silent sub-dimensions:**

- **Metrics / health / alert-detection.** Observability is logs-only. NFR17 requires detecting watcher lag and chain-liveness loss — both are *measurements*, not log lines, and no health endpoint, metrics surface, or detecting process is decided, deferred, or opened. This is the one place a real dimension is silent rather than deferred. (Part of M-3.)
- **Process supervision / restart policy** (compose `restart:` semantics matter when crash-recovery is a headline NFR) and **migration execution ownership** — both silent. (Low — L-4.)

Everything else the initiative altitude owns is explicitly placed.

## Rubric point 7 — Mermaid validity & AD consistency

All three diagrams are syntactically valid Mermaid (graph TD, graph LR with subgraph, erDiagram; quoted labels and empty `""` labels are legal). Consistency:

- **Paradigm diagram:** all arrows point adapter → core; no adapter-to-adapter edges. Consistent with the hexagonal rule.
- **Topology:** exactly the AD-2 role set (api, watcher×2, broadcaster×2, recon, dispatcher), all state edges go to Postgres only (AD-2), recon alone uses provider B (AD-12), only broadcasters touch KMS (AD-10/AD-11), only dispatcher reaches the consumer webhook (AD-13). **Except:** the `api` process has no chain edge, yet F6/FR22 says the API serves fee estimates and the Capability Map puts fee estimation in adapter/evm — no process in the topology can serve a live fee quote to the API as drawn. See M-2.
- **ER diagram:** consistent with AD-3/AD-5/AD-6/AD-11 (postings balanced under journal entries, cursors per chain/tier, broadcast attempts under withdrawal/sweep, operator actions). **Except:** `JOURNAL_ENTRY ||--o{ OUTBOX_EVENT : "same tx"` makes every outbox event a child of a journal entry — which contradicts FR29's non-monetary events. This is the diagram-level face of H-1.

---

## Findings (severity-tiered)

### Critical

None. The spine is not unsafe wholesale; the invariants that protect money (AD-3/4/5/6/7/10/11) are coherent and mutually reinforcing.

### High

- **H-1 — Outbox event taxonomy contradicts FR29 and is unowned.** AD-4 defines the outbox row as part of a transaction containing journal postings + a state transition; AD-13 makes the dispatcher deliver only outbox rows; the ER diagram hangs OUTBOX_EVENT exclusively off JOURNAL_ENTRY. But FR29 mandates webhooks for **deposit pending** (state transition, no postings), **approval required** (ditto), and **reconciliation alerts** (no postings *and* no state machine — and AD-12 forbids recon writing the ledger without saying whether it may write outbox rows). As written, three independent epics (deposits, withdrawals, recon) must each break or reinterpret AD-4/AD-13/ER to ship their events, and they will do it three different ways. **Fix:** amend AD-4 to "every outbox event is written in the same transaction as the state it announces; money-moving events additionally include their postings," add non-monetary event kinds to the ER diagram, and state explicitly that recon writes outbox rows (findings/alerts) but never postings.

### Medium

- **M-1 — Sweep initiation is undecided and not deferred.** AD-9 makes sweeps ledger citizens and AD-11 makes the broadcaster the sender, but nothing decides *which process creates sweep records and on what trigger* (per-deposit? balance threshold? schedule? operator-initiated?). The Capability Map's "broadcaster role + core/sweep" implies the sender also decides — colliding with the watcher (which sees the deposits) and with AD-2's no-hidden-coordination rule. Two epics will answer this differently. Decide it or add it to Deferred with a single owner.
- **M-2 — Fee-estimation serving path is unresolved and contradicts the topology.** FR22 is served by the api process; fee mechanics live in adapter/evm; the topology gives `api` no RPC edge. Either api gets a chain edge (provider A), or a named process caches quotes into Postgres. The "fee caching policy" deferral covers refresh policy, not *which process talks to the chain*.
- **M-3 — NFR9/NFR17 operational behavior undecided: degradation choice and alert detection.** (a) During chain-liveness loss, are withdrawals queued or rejected? The PRD offers both; the spine picks neither; API and broadcaster epics can diverge. (b) No process owns detecting watcher lag, stuck withdrawals, approval-queue ceiling, or liveness loss; deferring alert *transport* does not defer alert *detection*, and observability is otherwise logs-only (no health/metrics dimension decided, deferred, or opened).
- **M-4 — AD-11's single-writer rule has no enforcement mechanism.** "Exactly one broadcaster per chain" is compose-reviewable but not runtime-enforced; a restart race or operator error yields two writers. Add a per-(chain) Postgres advisory lock or lease to the Rule so it is constraint-shaped like AD-5.

### Low

- **L-1 — AD-12 startup check overstated:** it can verify distinct URLs, not distinct *providers*; reword or accept as config discipline.
- **L-2 — Cross-epic interface in the forwarder deferral:** contracts epic must precede (or pin the flush ABI for) the sweep/broadcaster epic; also carry the addendum's unwatched-chain fund-recovery note into the runbook row of the Capability Map.
- **L-3 — Metadata inconsistencies:** frontmatter dated 2026-07-14 inside a 2026-07-13 artifact folder; `status: draft` on a spine at handoff; Docker Compose "v5" has no internal cross-check.
- **L-4 — Silent minor ops decisions:** migration execution ownership (which process/step runs goose) and compose restart/supervision policy — both cheap to fix, both mildly divergence-prone across 7 processes.
- **L-5 — Performance NFRs unbound:** NFR4–6 appear only in `binds`; in particular the NFR5 ≤1 min budget spans watcher + dispatcher cadence with no stated split; the estimated-vs-actual fee settlement in the ledger is likewise unstated and will otherwise surface as recon drift.

---

## Bottom line

Hand-down readiness: **after amending AD-4/AD-13 + ER for non-monetary events (H-1) and assigning sweep initiation (M-1)**, this spine is safe for independent epic agents. M-2 through M-4 should be one-line amendments in the same pass; the lows are annotations.
