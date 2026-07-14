# Reconciliation: Brief + Addendum vs. PRD + Addendum

Source: `_bmad-output/planning-artifacts/briefs/brief-digital-asset-wallet-platform-2026-07-13/{brief.md,addendum.md}`
Target: `_bmad-output/planning-artifacts/prds/prd-digital-asset-wallet-platform-2026-07-13/{prd.md,addendum.md}`
Date: 2026-07-13

Excluded per instructions (known intentional deltas): finalized crediting tier; webhooks + account/balance model; manual approval threshold; internal transfers in v1; NFR numbers; vision extended to three external self-serve adopters (the 12-month horizon and the paid-consulting complement are read as part of this delta).

---

## (a) Capabilities, constraints, or nuances dropped

### A1. ERC-20 token generality — "USDC as the first token" narrowed to "ETH and USDC" (significant)

Brief, Solution section: **"Assets: native ETH and ERC-20 tokens, with USDC as the first token."** The brief positions the platform as supporting ERC-20 tokens as a class, with USDC merely the first instance — implying adding a second token should be additive, parallel to how chains are additive.

PRD: every asset mention is "ETH and USDC" (Vision, Scope, F-preamble, FR11). The chain abstraction gets an explicit acceptance test (FR33: third chain touches only the adapter layer), but there is **no analogous requirement that adding a second ERC-20 token is additive work**. FR32 isolates "token contracts" inside the chain adapter, which is architecture hygiene, not the product claim the brief made. The extensibility success criterion covers chains only.

Impact: a v1 built strictly to the PRD could hard-code USDC in ledger, crediting, and withdrawal paths and still pass every FR and success metric — losing a capability the brief asserted.

Suggested fix: add an FR (or extend FR33/Success Metric 4) stating that adding a further ERC-20 token requires only configuration/adapter-layer changes, mirroring the chain test.

### A2. Key lifecycle / disaster recovery — the custody cost the brief's addendum named (significant)

Brief addendum, buy-vs-build section: *"building custody in-house is typically a 6–18 month effort demanding specialized security work (**key ceremony, HSM integration, disaster recovery**)"* — explicitly flagged as a "counterpoint the brief must stay honest about."

PRD: NFR13 covers the key-handling boundary and secret leakage; the key-storage *mechanism* is correctly deferred to architecture. But **key ceremony, key backup, and disaster recovery are absent entirely** — no FR, no NFR, no open question, no addendum entry. Key loss is fund loss; recovery-of-keys is a different property from NFR2's recovery-of-data, and deferring the storage mechanism does not defer the *requirement* that keys be recoverable and their generation auditable.

Suggested fix: at minimum add key backup/recovery to NFR-Security or to Open Question 1's scope ("mechanism *including generation ceremony and recovery*"), so the threat model is obligated to address it.

### A3. Jurisdiction/compliance rationale for direct key control (minor)

Brief addendum lists three reasons companies build in-house: cost at scale, vendor risk, and **"jurisdiction-specific compliance may require direct key control."** The PRD's Problem section carries the first two; the compliance rationale is dropped. It also connects to the brief addendum's revisit trigger for the rejected external-signer alternative ("revisit if... compliance posture demands a qualified custodian") — preserved only via the PRD addendum's pointer to the brief addendum's Rejected Alternatives, not surfaced in the PRD itself. Low risk (the pointer exists), but the rationale list in the PRD body is silently incomplete.

### A4. Per-chain deposit addresses — silent design decision, not in the brief (minor)

Brief: "unique deposit addresses per customer/account." PRD FR6: "per customer/account **per chain**." On EVM chains one keypair yields the same address on both chains; per-chain-unique addresses is a real design choice (more keys, different attribution model, affects UX of "your deposit address"). Not a drop but an unflagged addition presented as if it were the brief's requirement — worth confirming it is intended rather than accidental wording.

## (b) Qualitative material the FR structure dropped

### B1. The 6–18 month honesty counterpoint (overlaps A2)

Beyond the missing DR capability, the *framing* is gone: the brief's addendum insisted the project stay honest that in-house custody is a 6–18 month specialized-security effort. The PRD keeps the "no technical moat" honesty but presents platform-held keys with only its upside (vendor independence) — the acknowledged cost side of the custody decision appears nowhere in PRD or PRD addendum.

### B2. "Fit and ownership" as the internal differentiator (minor)

Brief, "What Makes This Different": *"for the internal use case, the differentiator is **fit and ownership** — a ledger-grade wallet backend shaped exactly to the company's flows."* The PRD carries the vendor-risk/ownership half and the "rigor is the product" bet, but the two-sided structure — internal differentiator (fit/ownership, proven) vs. product differentiator (ledger-side gap, "real but unproven") — is flattened. The "opening is real but unproven" hedge on the *market gap itself* is softened into the v1-scoping caveat. Tone/positioning loss only; no requirement impact.

### B3. Custody framing: "pays for it with security obligations" (minor, overlaps B1)

Brief Solution: custody in scope "pays for it with security obligations the project takes seriously." The obligation shows up as NFR12/NFR13/NFR15, but the causal framing — custody choice ⇒ heightened security bar — is lost, which is exactly the framing that would have pulled key ceremony/DR (A2) into scope.

Preserved well (no action): portfolio-purpose framing ("passion/portfolio effort run like a product," repository-as-deliverable, portfolio-quality metric); "hear it from the platform first" operator voice; "never think about reorgs, gas, or nonces"; the Bybit lessons including withdrawal delays (PRD addendum); ETC 51% grounding; rejected alternatives via pointer; "boring reliability."

## (c) Contradictions between brief and PRD

No hard contradictions found. Checked and cleared:

- **Reorg reversal vs. never-reversed credits:** brief's "safe reversal/re-crediting" and PRD FR13's "a credited balance is never reversed" are consistent because FR13 scopes reversal to pre-finality records; the finalized-crediting policy makes both true.
- **Finality timing:** brief "~13+ min" vs. PRD "~13–20 min" — refinement, not conflict. (PRD-internal nit: NFR5 says "~15–20 min" where FR9/Problem say "~13–20 min"; harmless but worth aligning.)
- **State machine states:** brief's created→signed→broadcast→confirmed/failed vs. PRD's added "approved" state — covered by the intentional approval-threshold delta.
- **Scope Out lists:** identical, PRD adds packaging/onboarding consistent with the vision delta.
- **Success criteria:** the brief's five criteria map 1:1 to the PRD's five success metrics; counter-metrics are additive.

## Summary of recommended edits

1. Add token-extensibility requirement (FR near FR33, or widen Success Metric 4): second ERC-20 token is config/adapter-layer work. (A1)
2. Add key backup / generation-ceremony / disaster-recovery to the security NFRs or Open Question 1, and restate the custody cost honestly in Problem & Context or the PRD addendum. (A2, B1, B3)
3. Add the jurisdiction/compliance clause to the build-in-house rationale, or explicitly pull the brief addendum's "why companies build in-house" into the PRD addendum pointer. (A3)
4. Confirm "per chain" in FR6 is an intended design decision; if so, mark it [DECISION] rather than letting it read as inherited from the brief. (A4)
5. Optional: restore the two-sided differentiator framing (internal fit/ownership vs. unproven product opening) in Vision or Problem & Context. (B2)
6. Align NFR5's "~15–20 min" with FR9's "~13–20 min." (c, nit)
