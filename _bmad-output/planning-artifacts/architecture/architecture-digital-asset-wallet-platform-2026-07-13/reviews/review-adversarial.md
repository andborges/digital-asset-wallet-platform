# Adversarial Review — Architecture Spine (digital-asset-wallet-platform)

**Reviewed:** `ARCHITECTURE-SPINE.md` (2026-07-14 draft)
**Lens:** construct two units one level down (epics built by independent agents) that each obey every AD to the letter yet build incompatibly. Every such pair is a hole; each finding proposes the minimal AD tightening that closes it.

**Units assumed at the level below:** E-Ledger (accounts + journal + F1 API), E-Watcher (deposit monitoring + reorg, F3/F4), E-Withdraw (withdrawal API + broadcaster, F5), E-Recon (F8), E-Contracts (factory + forwarder + address scheme, F2), E-Dispatch (webhooks, F9), E-Sweep (treasury consolidation).

**Verdict:** The spine is strong on *mechanics* (atomicity, idempotency, single-writer, boundaries) but repeatedly silent on *ownership* — who is the sole creator/mutator of each shared row and what the shared shapes are. Ten of fourteen attacks land on "two compliant owners of one thing." Three are critical: nothing names who executes the credit, nothing pins where money sits between credit and sweep, and the salt→address encoding is underspecified in a way that is irreversible once live (AD-8's own immutability rule makes this the single most dangerous gap).

---

## CRITICAL

### A1 — Two compliant owners of the deposit-credit path (E-Watcher vs E-Ledger)

**The hole.** AD-6 gives the deposit machine `observed → safe → finalized → credited`; AD-7 says *credit at finalized*; AD-4 says the credit is one transaction of postings + transition + outbox. No AD says **which process executes the `finalized → credited` transition and writes the postings.**

**Two compliant constructions.**
- E-Watcher reads AD-7's policy table itself and, in the same tx where it marks `finalized`, also writes the journal entry and flips to `credited`. Fully AD-4/AD-6/AD-7 compliant.
- E-Ledger, reading "F1 Accounts & ledger lives in core/ledger + adapter/postgres" plus AD-3's "every money movement is a journal entry," builds a crediting worker inside the `api` role (or a new role — AD-2 doesn't close the role list, it just lists examples) that polls for `finalized` deposits and credits them. Also fully compliant.

Ship both: double-credit (each path credits; AD-5's `(chain, tx_hash, log_index)` uniqueness dedupes *chain-event insertion*, not journal-entry creation — the journal has no unique key tied to the deposit). Ship neither: deposits pile up at `finalized` forever and each team's tests pass. Related: who *inserts* the deposit row at all? E-Ledger could plausibly create "expected deposit" rows from the API side while E-Watcher inserts on observation — two writers of one table.

**Close it — tighten AD-6/AD-7:** "The watcher role is the **sole writer of deposit rows and sole executor of every deposit transition, including `finalized → credited`**: it writes the credit's journal postings, the transition, and the outbox event in one transaction (AD-4). No other process inserts or mutates a deposit row. Exactly-once crediting is enforced by a unique constraint `journal_entries(cause_type, cause_id)` — at most one journal entry per deposit-credit cause." (That last constraint also closes the AD-5 gap that dedupes events but not their money effects.)

### A2 — Money's resting place between credit and sweep is undefined; two compliant sweep-posting schemes double-count (E-Sweep vs E-Ledger)

**The hole.** AD-7 credits the customer at `finalized` — while the funds physically sit on the forwarder. AD-9 says a sweep gets "journal postings like any withdrawal" but never says **which accounts a sweep touches**. AD-3's account list (customer available, customer hold, platform treasury, fees) has no account representing un-swept forwarder float.

**Two compliant constructions.**
- E-Ledger models the credit as `external → customer available` and treats treasury as pure platform bookkeeping; it expects sweeps to post `forwarder-float → treasury` (platform-side only) and invents a float account to make that balance.
- E-Sweep, told sweeps post "like any withdrawal" (withdrawals debit customer accounts), posts `customer available → platform treasury` on flush. Compliant with AD-9's literal text — and it silently zeroes the customer's balance on consolidation, or double-counts against E-Ledger's scheme depending on merge order. Recon (AD-12) then cannot even define "ledger vs chain" for forwarder balances because the ledger has no account that corresponds to forwarder addresses.

**Close it — tighten AD-9 (and extend AD-3's chart):** "The chart of accounts includes a platform **forwarder-float** account per (chain, asset). A deposit credit posts `external → customer available` **and** `external-chain → forwarder-float` legs per the canonical posting map; a sweep posts exactly `forwarder-float → platform treasury` and **never touches customer accounts** — customer balances are invariant under consolidation. Recon compares forwarder on-chain balances to the forwarder-float account." (Exact leg naming may vary; the AD must state the invariant *sweeps never move customer balances* and name the float account.)

### A3 — Salt encoding and address persistence are unpinned; AD-8's own immutability rule makes divergence permanent (E-Contracts vs E-Watcher/E-Ledger)

**The hole.** AD-8: "salt = customer ID," computed off-chain, attribution by (address, chain) — and "changing the salt scheme changes every customer address," i.e., a wrong first guess is *unfixable*. But CREATE2 takes `bytes32`. Customer ID is a UUIDv7 (Conventions). The spine never says how a 16-byte UUID becomes a 32-byte salt, who assigns the customer ID/salt, or whether the derived address is persisted.

**Two compliant constructions.**
- E-Contracts (Solidity/Foundry side) defines salt = `keccak256(abi.encodePacked(customerId))` over the UUID string, and its `forge` tests pass.
- E-Ledger/E-Watcher (Go side) computes salt = UUID's 16 raw bytes left-padded to bytes32, derives the address in `core`, and hands it to customers. Both "salt = customer ID." Every customer address the platform publishes differs from what the contracts predict; deposits either land unattributed or the discrepancy is discovered after real funds hit addresses derived under scheme #1 — which AD-8 forbids ever changing.
- Second pair inside the same hole: E-Ledger computes addresses *on demand* (pure function, nothing persisted — compliant); E-Watcher, needing address→customer attribution, re-derives with its own encoding. Silent attribution failure with zero constraint violations.

**Close it — tighten AD-8:** "Salt = the customer UUID's 16 big-endian bytes right-aligned in bytes32 (zero-padded high bytes) — **one derivation function in `internal/core`, cross-tested in CI against a Foundry test vector fixture** (same (uuid, factory, initcode) → address table asserted on both sides). The API persists `deposit_addresses(customer_id, chain, address)` at customer creation; the watcher attributes **only** via this table, never by re-derivation." (The specific encoding matters less than that exactly one is named and cross-verified before anything is live.)

---

## HIGH

### A4 — Sweep origination has no owner and no idempotency key (E-Sweep vs E-Watcher)

AD-9 makes sweeps ledger citizens; AD-11 makes the broadcaster the sole *sender*. Nobody is named as the sole *creator* of sweep records, and AD-5's dedupe keys (idempotency table, chain-event triple, caller's key) cover none of "decide to sweep forwarder X."
**Constructions:** E-Watcher creates a sweep record whenever a deposit hits `credited` (it's right there in the transaction). E-Sweep independently gives the broadcaster a balance-scanning loop that creates sweeps for any forwarder above a dust threshold. Both compliant; together they create two sweep records → two broadcast attempts → the second flush moves a *later* deposit's funds prematurely or burns gas on empty forwarders. No constraint fires: sweeps have no natural unique key in the spine.
**Close it — tighten AD-9/AD-11:** "The broadcaster role is the sole creator and executor of sweep records. Sweep creation dedupes on a unique partial index `sweeps(chain, forwarder_address) WHERE state NOT IN (confirmed, failed)` — at most one in-flight sweep per forwarder per chain. Trigger policy (threshold, schedule) lives in the broadcaster."

### A5 — "One guarded transition function per machine" doesn't say one *copy* (E-Withdraw api-side vs E-Withdraw broadcaster-side, or any pair)

AD-6's rule is satisfiable by *per-epic implementations*: the withdrawal machine's `created → approved` lives in the api handler code (one guarded function!), while `signed → broadcast → confirmed` lives in the broadcaster with its own guard and its own state-constant strings. Each is "one guarded transition function" for the transitions it performs. They diverge on whether `awaiting-approval` exists (AD-6 literally marks it `?`), on legal-predecessor sets, and one team's raw `UPDATE withdrawals SET state=...` in a hotfix violates nothing textual.
**Close it — tighten AD-6:** "Each machine's state enum and its single transition function live in `internal/core/<machine>`; that function is the **only code that may change the state column** — the postgres adapter exposes no generic state-setter, and a CI grep/DB trigger (`state` column updatable only via function that validates `(from, to)` against the core-owned transition table) enforces it. The `awaiting-approval` gate condition is decided in core policy, not per-role."

### A6 — Posting map per transition is unwritten; hold/settle/release timing clashes (E-Withdraw api vs E-Withdraw broadcaster vs E-Ledger)

AD-3 names hold accounts and the ER diagram says a withdrawal has "hold/settle/release" entries, but no AD says **which transitions carry which postings**. Constructions: E-Ledger's API places the hold at `created`; E-Withdraw assumes the hold happens at `approved` and posts settle at `broadcast`; a third reading settles only at `confirmed`. All AD-4 compliant. Failure modes: window where a customer double-spends available balance (hold placed late), or `failed` post-broadcast leaves the hold stranded because each side thinks the other releases it — and nobody owns posting the *gas actually paid* on a reverted withdrawal.
**Close it — new AD (or AD-3 appendix):** a canonical posting map: "`created`: available→hold (amount+max fee); `confirmed`: hold→external + hold→fees (actual gas), remainder hold→available; `failed` pre-broadcast: hold→available in full; `failed` post-broadcast (reverted on-chain): hold→available minus gas, gas hold→fees. No transition outside this map writes withdrawal postings." (Values illustrative; the AD must enumerate the map so exactly one epic implements each row.)

### A7 — OpenAPI spec has one file, N author-epics, and no shared-schema owner (E-Ledger vs E-Withdraw vs E-Recon)

AD-14 fixes spec-first + oapi-codegen but not: (a) whether generated code is committed or generated at build, (b) who owns `api/openapi.yaml`'s shared `components` (Amount, Asset, Problem, pagination). Constructions: E-Ledger commits generated code and defines `Amount` as an integer; E-Withdraw regenerates in CI and defines its own `WithdrawalAmount` as a string of base units (correct per the Money convention — which JSON integers silently break past 2^53). Both compliant; the merged spec has two amount shapes and the build fights over generated files.
**Close it — tighten AD-14:** "Generated code is committed; CI regenerates and fails on diff. Shared components — `Asset` (chain, symbol, contract|native), `Amount` (**string** of integer base units), `Problem`, pagination, `Idempotency-Key` header — are defined once in `components/` and owned by the ledger/API epic; other epics add paths and reference shared components, never redefine them."

### A8 — Recon may compliantly write deposit *states* (E-Recon vs E-Watcher)

AD-12 forbids recon writing **journal postings** — only. A compliant E-Recon that detects an orphaned deposit "fixes" it by flipping the deposit row to `orphaned` (a state write, not a posting) and closes its finding. E-Watcher also owns orphaning via reorg detection. Two writers of deposit state; recon's flip can race the watcher's rescan, and a recon bug now mutates the system it is supposed to independently check.
**Close it — tighten AD-12:** "Recon's only writes are `recon_runs` and `recon_findings`. It writes no row of any other table — no state transitions, no outbox events. All remediation, including state corrections, happens via the watcher's rescan or operator adjustment routes."

## MEDIUM

### A9 — Chain-event row vs deposit row conflation across tiers (E-Watcher internal, but two compliant schemas)

AD-5's unique `(chain, tx_hash, log_index)` + AD-6's per-(chain, tier) cursors admit two schemas: (1) each tier scan *inserts* an event row — the `safe`/`finalized` passes then hit the unique constraint and, reading "retries hit constraint violations, never double-applies," treat it as a no-op → deposits never advance past `observed`; (2) one append-only `chain_events` table distinct from `deposits`, tier passes *transition* the deposit. Both are textual readings of AD-5.
**Close:** one sentence in AD-5: "`chain_events` is append-only and distinct from `deposits`; tier scans that re-encounter a known event advance the deposit's state via the transition function — the uniqueness constraint dedupes insertion, not progression."

### A10 — Outbox has one `delivered` flag but the Deferred section invites a second consumer (E-Dispatch vs alerting)

AD-13 makes the dispatcher the only *webhook sender*; Deferred says NFR17 alerting's "contract is the outbox event." An alerting build that tails the outbox and marks rows consumed is compliant and starves the dispatcher (or vice versa).
**Close:** tighten AD-13: "The dispatcher is the outbox's sole consumer; its progress is its own cursor/status column. Any other reader (alerting, ops) reads outbox rows without writing them."

### A11 — Event payload envelope is unowned (every outbox-writing epic vs E-Dispatch)

AD-4 has every epic writing outbox rows; AD-13 has the dispatcher forwarding them opaquely. Nothing fixes the payload envelope: E-Watcher emits `{deposit:{...}, amount: "123"}`; E-Withdraw emits flat `{withdrawal_id, amount_wei: 123}`. Consumers get an incoherent API. Types are named dot-namespaced, payloads are not named at all.
**Close:** add to AD-13: "Outbox payloads conform to one envelope `{id, type, occurred_at, data}` with `data` schemas defined per event type alongside the OpenAPI spec (shared `Amount`/`Asset` components per A7); core provides the single constructor for outbox rows."

### A12 — Idempotency-key scope: global vs per-consumer vs per-route (E-Withdraw vs internal-transfer in E-Ledger)

AD-5 says "unique key, stored response." E-Withdraw scopes uniqueness per (consumer, key); E-Ledger's internal-transfer endpoint uses a globally-unique key column. Two different tables or one table with clashing constraints; a consumer reusing a key on a different route gets an unrelated stored response replayed, or two consumers collide.
**Close:** one clause in AD-5: "One `idempotency_keys` table, unique on `(consumer, key)`, storing a request hash — same key + different body ⇒ 422, same key + same body ⇒ replay stored response. All mutating routes use this table."

### A13 — Unsupported-token observations (FR11): deposit rows or not? (E-Watcher vs E-Ledger)

AD-14 says record them, never credit. E-Watcher records them as `deposits` in a terminal quirk state — now E-Ledger's invariant "every finalized deposit is credited" (the natural test from A1's fix) is false, or the crediting path needs an asset allowlist check duplicated from the watcher. Alternative: a separate observations table.
**Close:** one sentence: "Unsupported-token observations live in their own table (`unsupported_observations`), not the deposit state machine; the deposit machine only ever holds creditable (chain, asset) pairs per the AD-7 policy table."

## LOW

### A14 — Goose migration sequence collisions across parallel epics
Every epic ships migrations in one shared `adapter/postgres` directory; two branches both take `00007_*.sql`. Goose versioned mode breaks on out-of-order merges. **Close:** convention row: timestamped migration names (`goose create -s` off / timestamp format) + CI check that migrations apply cleanly from zero on merge.

### A15 — Stuck-transaction replacement ownership
Fee-bump/replacement (same nonce, higher fee) is arguably a new "broadcast" — AD-11 mostly covers it, but E-Recon or an ops tool could compliantly submit a cancellation tx since AD-11 restricts "sign or broadcast" ambiguity to "no other code path" without naming replacements. **Close:** one clause in AD-11: "replacement and cancellation transactions are broadcast attempts on the same record, performed only by the broadcaster."

---

## Summary of proposed tightenings

| Finding | Severity | Minimal fix |
| --- | --- | --- |
| A1 credit-path owner | Critical | Watcher sole deposit writer incl. credit; unique `journal_entries(cause_type, cause_id)` |
| A2 sweep accounting | Critical | Forwarder-float account; sweeps never touch customer accounts |
| A3 salt encoding | Critical | Pin bytes32 derivation; cross-language CI test vectors; persist addresses; attribute only via table |
| A4 sweep origination | High | Broadcaster sole sweep creator; unique in-flight sweep per (chain, forwarder) |
| A5 transition packaging | High | State enum + transition fn in core only; no generic state-setter |
| A6 posting map | High | Enumerate transition→postings map as an AD |
| A7 OpenAPI ownership | High | Committed generated code + CI diff; shared components owned once; Amount is string |
| A8 recon state writes | High | Recon writes only recon_runs/findings — no rows anywhere else |
| A9 event vs deposit rows | Medium | chain_events append-only, distinct from deposits |
| A10 outbox consumers | Medium | Dispatcher sole consumer; others read-only |
| A11 event envelope | Medium | One envelope + per-type schemas, single constructor in core |
| A12 idempotency scope | Medium | Unique (consumer, key) + request hash |
| A13 unsupported tokens | Medium | Separate table, outside deposit machine |
| A14 migration numbering | Low | Timestamped migrations + CI apply check |
| A15 tx replacement | Low | Replacements are broadcaster-only broadcast attempts |
