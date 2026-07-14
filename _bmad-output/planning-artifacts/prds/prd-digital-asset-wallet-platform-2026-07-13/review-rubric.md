# PRD Quality Review — Digital Asset Wallet Platform

Reviewed against `.claude/skills/bmad-prd/assets/prd-validation-checklist.md`, 2026-07-13. Create-intent finalize pass; stakes calibrated as "portfolio project run like a product," internal-first Go wallet-platform backend. Known open items (key storage mechanism, first consuming application identity, approval threshold values, [ASSUMPTION] items awaiting confirmation) are already tracked and not re-reported as findings.

## Overall verdict

This is a genuinely strong PRD: it makes real decisions with the trade-offs stated out loud ("no technical moat today; rigor … is the product"), the FRs are dense with testable consequences, and the shape — capability spec with journeys deliberately downscaled — fits the product exactly. What's at risk is downstream extraction hygiene rather than substance: there is no Glossary and no Assumptions Index, one FR leans on an undefined term ("policy checks," FR18), and one success metric (SM4, third-chain extensibility) has no stated verification method inside v1 scope. All findings are fixable without re-opening any decision.

## Decision-readiness — strong

Decisions are stated as decisions, with what was given up named next to them. The crediting policy is the best example: FR9 commits to "credited at the **finalized** tier … flat across both assets and all amounts," the latency cost is owned in NFR5 ("dominated by the finality wait (~13–20 min), by policy"), and the addendum grounds the choice against Coinbase/Binance/Gate.io precedent. The custody decision does the same: §Problem & Context accepts the 6–18 month in-house cost "deliberately, with the threat model and key-management NFRs as the counterweight," and §Future Path names the standing revisit trigger that would re-open it. The Vision's honesty line — "The bet, stated honestly: there is no technical moat today" — is the opposite of the smoothed-to-neutral red flag. The three Open Questions are actually open, each with an owner and a revisit point.

No `[NOTE FOR PM]` callouts appear anywhere, but the tensions they would mark are handled instead by the Open Questions table and the addendum's "Deliberately unresolved" section, so nothing is being dodged. No findings.

## Substance over theater — strong

Every piece of content earns its place. The three user entries (§Users) each drive visible requirements: application teams → FR20 ("consuming applications never handle nonces, gas, or raw transactions") and FR23; operators → FR17, FR19, NFR16; the vision-tier external teams entry exists specifically "so v1 decisions don't foreclose it," and it demonstrably shapes FR9's config-not-rework design and F10. NFRs carry product-specific numbers, not boilerplate: "order 10³, not 10⁶" with "~10× bursts" (NFR4), "under 500 ms p95" (NFR6), "adds no more than 1 minute" (NFR5), and NFR7 makes the unusual-but-honest call that availability is *not* an SLA while durability is. The Vision could not swap into another PRD — it names Base/Arbitrum, ETH/USDC, reconciliation-green as the operator's normal week. The differentiation claim is explicitly *anti*-innovation-theater: "the differentiator is **fit and ownership**, not novel technology." No findings.

## Strategic coherence — strong

The thesis is stated twice and everything hangs off it: the product is "the ledger side (deposit monitoring, reorg handling, transaction state machines, reconciliation) that no vendor or mature OSS project covers" (§Vision), and rigor is the moat. The feature set is exactly that ledger side — F3/F4/F5/F7/F8 are the four hard problems from §Problem & Context, one to one. Success Metrics validate the thesis rather than measuring activity (correctness under injected faults, drift detected by platform not users, reviewed threat model, adapter-bounded extensibility, repository-as-evidence) — no DAU/MAU theater anywhere. Counter-metrics exist and are sharp: "Reconciliation green because it checks nothing" with a seeded-fault guard, and "Approvals as a silent queue" with a tracked alerting ceiling. MVP scope kind is a coherent problem-solving/platform proof, and the scope logic matches ("None of it is needed to prove the core," §Future Path). No findings.

## Done-ness clarity — strong

Judged unforgivingly, as the rubric asks, this still holds up. Nearly every FR carries a verifiable consequence: FR33 names its own acceptance test ("this is the acceptance test of the abstraction"), FR13 states a platform invariant ("a credited balance is never reversed by a reorg"), FR23 gives the observable ("a duplicate request returns the original result and never causes a second money movement"), FR26 enumerates the required alarm detail ("account, asset, expected vs. observed"), and NFR18 binds the correctness claims to an externally runnable fault-injection suite. Vague-adjective language is rare; NFR8's "recovers unattended where possible" is a hedge, but the three concrete clauses that follow (complete rescan, resume from persisted state, "no operator reconstruction of state is ever required") carry the testability. The residue is below.

### Findings

- **medium** FR18's "policy checks" is undefined (§F5, FR18) — "sufficient available balance, well-formed destination address, and passing policy checks": the first two are testable, the third is circular — no v1 policy set is named anywhere (the only policy elsewhere is the FR17 approval threshold). An engineer cannot know what "done" means for this clause. *Fix:* enumerate the v1 policy-check set in FR18 (even if it is only "approval-threshold routing per FR17"), or explicitly defer the set to architecture with a pointer.
- **medium** SM4 has no verification method inside v1 scope (§Success Metrics #4, FR33) — "adding a third EVM chain requires changes only inside the chain-adapter layer" is the acceptance test, but v1 ships two chains, so the metric can only be proven by doing out-of-scope work. As written, v1 "success" on this axis is unfalsifiable. *Fix:* state how SM4 is evaluated at v1 close — e.g., a testnet third-chain spike, or an explicit note that SM4 is assessed by design review until a third chain is attempted.
- **low** FR19's "defined resolution path" is not defined and has no owner (§F5, FR19) — a stuck withdrawal "is surfaced to the operator with a defined resolution path," but nothing says where that path gets defined (runbook? architecture? ops doc?). *Fix:* name the artifact that will hold it (e.g., operator runbook, a deliverable alongside the threat model).
- **low** Operational alert thresholds are unset with no tracking venue (NFR16; §Success Metrics counter-metric 2) — "watcher lag beyond threshold" and the approvals "alerting ceiling" have no values *and*, unlike the approval threshold values (Open Question 3, pre-launch checklist), no owner or revisit point. *Fix:* fold both into Open Question 3's pre-launch checklist entry.

## Scope honesty — strong

Omissions are explicit and de-scoping is done out loud. The §Scope "Out (v1)" list covers exactly the things a reader might silently assume in (multi-tenant, compliance tooling, consumer UI, third-party signers), each with a reason where one is needed ("deferred until the product path is real"). The journey downscaling is declared rather than smuggled: "User journeys are deliberately downscaled … the persona success statements above carry what a journey section would" (§Users). Nine inline `[ASSUMPTION]` tags sit on real inferences (FR1 flat account model, FR6 per-chain addresses, NFR4–6 thresholds), and open-items density (9 assumptions + 3 Open Questions) is appropriate for the stakes. Two hygiene gaps, both bearing directly on the finalize pass's confirmation step:

### Findings

- **medium** No Assumptions Index (document tail) — the inline `[ASSUMPTION]` tags are never collected into an index, so there is no single place for the user to confirm or reject them during finalize, and the roundtrip check (every inline tag indexed, every index entry inline) cannot be run. For a create-intent finalize whose remaining work *is* assumption confirmation, this is the highest-leverage fix in the review. *Fix:* add an Assumptions Index section after Open Questions listing all nine tags with their locations and a confirm/reject slot.
- **low** Bare `[ASSUMPTION]` tags with no content on FR30 and FR31 (§F9) — every other tag states what was assumed ("[ASSUMPTION: apps want pending visibility …]"); FR30/FR31 end in a naked "[ASSUMPTION]", so the user being asked to confirm them cannot tell what inference they are confirming (presumably: that at-least-once + consumer dedup, and signed payloads, are acceptable designs). *Fix:* state the assumed inference inside each tag.

## Downstream usability — adequate

This is a chain-top PRD (it feeds `bmad-architecture` and epic breakdown — Open Question 1 says so explicitly), so extraction hygiene matters more than it would standalone. The good news: FR IDs run FR1–FR34 contiguous and unique; every cross-reference resolves (NFR3→FR9/FR13, NFR11→FR5, NFR14→FR31, NFR8→FR14); the addendum's pointer to the brief addendum resolves on disk; sections stand alone well (F7 idempotency, F8 reconciliation each extract cleanly). Domain vocabulary is used consistently in practice — "confirmation tier," "finalized," "hold," "drift," "chain adapter," "idempotency key" don't drift across FRs, NFRs, and SMs. But consistency is currently luck, not contract:

### Findings

- **medium** No Glossary (missing section) — architecture and story creation will source-extract terms like *confirmation tier*, *crediting policy*, *hold*, *drift*, *chain adapter*, *watcher*, *internal transfer* with no canonical definitions; the addendum's confirmation-tier model covers one cluster but is not a glossary. *Fix:* add a short Glossary (roughly ten terms) — the definitions already exist in prose and just need collecting.
- **low** NFR13a breaks NFR numbering continuity (§Security) — NFR1–NFR18 with an interpolated "NFR13a" reads as a late insertion and will wrinkle downstream ID references. *Fix:* renumber (accepting downstream ref churn now, while there is none) or keep it and note it as intentional.

## Shape fit — strong

The shape matches the product exactly per the rubric's internal-tool guidance: single internal API consumer, single operator role → capability-spec shape, journeys replaced by persona success statements with the substitution declared (§Users), and Success Metrics that are operational/evidential rather than user-facing. The PRD resists over-formalization (no UJ theater for a single-operator backend) without going under-formalized — the persona success statements do carry real acceptance content ("they hear it from the platform first — with enough detail to act" maps directly onto NFR16/FR26). The addendum split is also right: fee mechanics, tier timings, incident references, and industry grounding are exactly the depth that would have bloated the body. No findings.

## Mechanical notes

- **Assumptions Index roundtrip:** fails — nine inline `[ASSUMPTION]` tags (FR1, FR6, FR10, FR11, FR30, FR31, NFR4, NFR5, NFR6), zero indexed. Reported as a scope-honesty finding above.
- **Bare tags:** FR30 and FR31 carry contentless `[ASSUMPTION]` tags. Reported above.
- **ID continuity:** FR1–FR34 clean; NFR1–NFR18 clean except interpolated NFR13a (reported above). SMs and counter-metrics are numbered lists without IDs — acceptable at these stakes, but architecture may want stable SM IDs if it traces to them.
- **Cross-references:** all internal FR/NFR refs resolve. The addendum's external pointer to `_bmad-output/planning-artifacts/briefs/brief-digital-asset-wallet-platform-2026-07-13/addendum.md` resolves on disk.
- **Glossary drift:** none observed in usage (terms are consistent), but no Glossary exists to anchor them — reported above.
- **UJ protagonists:** N/A by declared shape choice (§Users); no floating UJs because there are no UJs.
- **Frontmatter:** `status: draft` — correct for a finalize pass in progress; flip on confirmation.
- **Required sections for stakes/type:** Vision, Problem, Users, Scope, FRs, NFRs, SMs (+counter-metrics), Future Path, Open Questions all present. Missing only Glossary and Assumptions Index, both reported.
