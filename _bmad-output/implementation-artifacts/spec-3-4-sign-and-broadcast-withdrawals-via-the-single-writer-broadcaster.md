---
title: 'Story 3.4: Sign & Broadcast Withdrawals via the Single-Writer Broadcaster'
type: 'feature'
created: '2026-07-21'
status: 'done'
review_loop_iteration: 0
followup_review_recommended: true
context: []
warnings: ['oversized']
baseline_revision: '0c3f61b3624e67525f249a59731b14cca82a35a2'
final_revision: '0c3f61b3624e67525f249a59731b14cca82a35a2 (NOT_COMMITTED past this point: user global policy — no auto-commits. Story 3.4 and its review-pass patches remain uncommitted on top of this revision.)'
---

<intent-contract>

## Intent

**Problem:** An `approved` withdrawal today advances no further — nothing signs, allocates a nonce, or broadcasts it on-chain, and nothing settles its hold once (or if) it confirms. Consuming applications must never handle gas, nonces, or raw transactions themselves (FR20).

**Approach:** A new `broadcaster --chain=<c>` process (one per chain, Postgres-advisory-locked like the watcher, AD-11) polls for `approved` withdrawals, allocates the next nonce from persisted per-chain state, signs via a new core `Signer` port (KMS in prod/LocalStack via endpoint override, software signer for tests — AD-10), broadcasts, and polls broadcast withdrawals for on-chain confirmation. A confirmed withdrawal settles its hold against a new `treasury` platform account (per (chain, asset), mirroring `forwarder-float`'s existing pattern); an on-chain-reverted withdrawal releases its hold back to the customer's available balance. Crash-recovery/stuck-withdrawal resolution is explicitly Story 3.5's job, not this one's — this story only needs to leave the DB in a well-defined, resumable-by-design state.

## Boundaries & Constraints

**Always:**
- Exactly one `broadcaster` process per chain: `pg_try_advisory_lock` at startup, exits if another instance already holds it — same mechanism as `runWatcher`'s existing lock (`cmd/walletd/main.go`'s `watcherLockID`), but a distinct numeric ID namespace so the two locks for the same chain never collide.
- The `core.Signer` port is chain-library-agnostic (`Sign(ctx, chain, digest [32]byte) (signature [65]byte, err error)`, no go-ethereum types) — AD-1's import boundary confines all go-ethereum/raw-transaction/RLP code to `internal/adapter/evm`. Core orchestrates: ask the EVM adapter to build the unsigned tx and its signing digest, ask the Signer port to sign that digest, ask the EVM adapter to assemble the signed tx and broadcast it. The EVM adapter never calls the Signer adapter directly (adapters-don't-call-adapters) — only core sits between them.
- Nonce allocation and the `broadcast_attempts` row insert commit in the SAME Postgres transaction, BEFORE the KMS sign/broadcast calls happen (AD-11's exact wording) — this is what makes a nonce, once allocated, durably attributable to one withdrawal even if the process crashes before broadcasting.
- Nonce state is per-chain only (`chain_nonce_state(chain PK, next_nonce)`), not per-address — AD-10 pins exactly one hot-wallet address system-wide, valid on both chains, so chain alone is a sufficient key.
- `accounts.account_type`'s CHECK widens to add `'treasury'`; the existing partial unique index scoped to platform rows (`(chain, asset) WHERE customer_id IS NULL`) widens to `(chain, asset, account_type) WHERE customer_id IS NULL` — today it only allows ONE platform row per (chain, asset) at all, which would collide the moment a `treasury` row is seeded alongside the existing `forwarder-float` row for the same pair. Seed one `treasury` row per `SupportedChainAssetPairs` entry, mirroring migration 0006's `forwarder-float` seeding exactly.
- Confirmation settlement posts `debit hold, credit treasury` (SOLUTION-DESIGN.md: "the settle entry extinguishes the hold against platform treasury") — the ONLY arithmetically valid pairing given hold was originally credited `+amount` at Story 3.2's placement (postings must net to zero). On-chain failure (reverted receipt) posts `debit hold, credit available` (SOLUTION-DESIGN.md: "terminal failure releases the hold back to available") — reversing 3.2's placement exactly.
- Confirmation is checked at the chain's `finalized` tag (mirrors AD-7's deposit-crediting choice, `evm.Scanner.Head`'s existing pattern) — settlement is irreversible (extinguishes a hold, credits treasury), the same stakes as a deposit credit.
- No key handle, private key material, or secret ever appears in a log line, error, or API response (NFR13) — enforced by construction: the Signer port's return type is a signature only, never key material, and no broadcaster log statement includes anything from the Signer.
- `withdrawals.status` CHECK widens to add `'signed'`, `'broadcast'`, `'confirmed'`, `'failed'` (mirrors Stories 3.2/3.3's own precedent of each story widening the CHECK for the value its own transition needs).

**Block If:** (none — the one genuine ambiguity, the settlement posting direction, is resolved above by the postings-must-net-to-zero invariant plus SOLUTION-DESIGN.md's explicit "against platform treasury"/"back to available" wording; do not re-litigate without new information contradicting both.)

**Never:**
- Broadcaster crash-recovery: resuming a withdrawal already sitting at `signed` with a `broadcast_attempts` row but no `tx_hash` yet (crashed between nonce-allocation-commit and broadcast), or declaring a broadcast "stuck" after a timeout — both explicitly Story 3.5's scope (epic-3-context.md's own dependency note). A withdrawal that lands in that state today simply waits until 3.5 exists; this story does not attempt to resume it.
- Fee-bump / replacement transactions (same nonce, higher fee) — `broadcast_attempts` is `UNIQUE(withdrawal_id)`, exactly one attempt per withdrawal in v1 (deferred per the architecture's own adversarial review: "replacement... performed only by the broadcaster" is a future AD-11 clause, not this story's).
- Any fee/gas-cost posting to the `Fees` platform account — nothing in this story's acceptance criteria calls for deducting actual gas cost from the customer; Story 3.1's fee estimate is used only for Story 3.3's pre-broadcast affordability check, already built. Gas is paid from the platform's own treasury/hot-wallet balance, not customer-metered, in v1.
- Sweep logic, the Signer/KMS keys' own provisioning/rotation, or anything Story 3.6's territory.
- Exposing a nonce, raw transaction, or gas parameter in any API response (FR20) — none of this story's changes touch a customer-facing endpoint at all; the broadcaster is a standalone process with no HTTP surface.

## I/O & Edge-Case Matrix

| Scenario | Input / State | Expected Output / Behavior | Error Handling |
|----------|--------------|---------------------------|----------------|
| Approved withdrawal, happy path | Withdrawal at `approved` | Nonce allocated + `broadcast_attempts` row committed, then signed, assembled, broadcast; status → `broadcast` with `tx_hash` | none |
| Broadcast withdrawal confirms successfully | Chain reports a successful receipt at `finalized` | Status → `confirmed`; journal entry (debit hold, credit treasury); `withdrawal.confirmed` outbox event | none |
| Broadcast withdrawal's tx reverts on-chain | Chain reports a failed receipt at `finalized` | Status → `failed`; journal entry (debit hold, credit available); `withdrawal.failed` outbox event | none |
| Two broadcaster processes started for the same chain | Second process cannot acquire the advisory lock | Second process exits immediately with a clear error | none |
| No `treasury` platform account row for a (chain, asset) | Registry gap (shouldn't happen in a correctly migrated deployment) | Confirmation settlement fails loud, logged server-side; withdrawal stays `broadcast` for retry next poll | fail loud, mirrors Story 3.3's identical "registry gap" principle |
| Signer returns an error (KMS unavailable, software signer misconfigured) | Any signer backend failure | No nonce is wasted on a never-broadcast tx — the nonce+`broadcast_attempts` row is already committed before signing is attempted, so a sign failure leaves that withdrawal at `signed` with no `tx_hash`; broadcaster logs the failure and moves on (Story 3.5 territory to resume it) | logged server-side, never a fatal process crash |
| Broadcaster process restarts | Any withdrawals already `broadcast` (tx_hash known) | Poll loop resumes checking their receipts normally — this is NOT crash recovery, since the tx was already fully broadcast before restart | none |

</intent-contract>

## Code Map

- `internal/adapter/postgres/migrations/0011_add_broadcaster_signing_and_treasury.sql` -- `chain_nonce_state` table; `broadcast_attempts` table; widen `accounts.account_type` CHECK + its platform partial unique index; seed `treasury` platform rows; widen `withdrawals.status` CHECK
- `internal/core/withdrawal.go` -- add `WithdrawalStatusSigned/Broadcast/Confirmed/Failed`; extend `Withdrawal` with `TxHash`, `Nonce` (nullable/zero until signed)
- `internal/core/ports.go` -- new `Signer` port; new `TransactionBroadcaster` port (build unsigned tx + digest, assemble signed tx, send raw tx, check receipt — all go-ethereum-shaped data crossing as opaque `[]byte`/`[32]byte`/`[65]byte`, never a go-ethereum type in a core signature); extend `WithdrawalRepository` with `ClaimApprovedWithdrawals`, `RecordBroadcast`, `SettleConfirmed`, `SettleFailed` (or equivalent — implementer's latitude on exact method split, as long as the transaction boundaries in Boundaries & Constraints hold)
- `internal/core/sign_and_broadcast_withdrawal.go` -- new use case: claims one approved withdrawal, orchestrates build→sign→assemble→persist→broadcast; a second use case (or the same one, poll-mode) checks broadcast withdrawals' receipts and settles confirm/fail
- `internal/adapter/postgres/withdrawal_repo.go` -- new repository methods implementing the above, each following the established "lock row(s) FOR UPDATE → verify status → write transition + postings + outbox event, all in caller's tx" shape
- `internal/adapter/evm/broadcaster.go` -- new `TransactionBroadcaster` implementation: constructs an unsigned EIP-1559 tx (chain, nonce, to, value, gas), returns its signing digest; assembles a signed tx from a returned signature; sends via `eth_sendRawTransaction`; checks a receipt against the `finalized` tag (reuses `Scanner`'s existing `Head`/tag-query pattern)
- `internal/adapter/signer/software/software_signer.go` -- new `core.Signer` implementation: an in-memory ECDSA key (env-configured, e.g. `SIGNER_PRIVATE_KEY` hex, dev/test only), plain `ecdsa.Sign`-then-recovery-id logic
- `internal/adapter/signer/kms/kms_signer.go` -- new `core.Signer` implementation: AWS SDK v2 KMS client (`Sign` API, `ECC_SECG_P256K1`), vendored DER→r/s decode + low-s normalization + v-recovery-by-trial-against-the-known-public-key (per AD-10: vendored from `matelang/go-ethereum-aws-kms-tx-signer/v2`'s approach, owned code not a live dependency); works against real AWS KMS in prod and LocalStack KMS locally via `AWS_ENDPOINT_URL`/custom endpoint resolver override, same adapter, same code path
- `cmd/walletd/main.go` -- new `broadcaster` subcommand (`--chain=base|arbitrum`, mirrors `runWatcher`'s flag/lock/migrate/poll-loop shape exactly); composition root wires `SIGNER_BACKEND=kms|software` to pick the Signer implementation
- `go.mod` -- add `github.com/aws/aws-sdk-go-v2`, `.../config`, `.../service/kms`

## Tasks & Acceptance

**Execution:**
- [x] `internal/adapter/postgres/migrations/0011_add_broadcaster_signing_and_treasury.sql` -- `CREATE TABLE chain_nonce_state (chain text PRIMARY KEY CHECK (chain IN ('base','arbitrum')), next_nonce bigint NOT NULL DEFAULT 0, updated_at timestamptz NOT NULL DEFAULT now())`, seeded with `next_nonce=0` for both chains (real value corrected by ops before go-live, same "placeholder, ops must revisit" pattern as migration 0010's thresholds); `CREATE TABLE broadcast_attempts (id uuid PRIMARY KEY, withdrawal_id uuid NOT NULL UNIQUE REFERENCES withdrawals(id), chain text NOT NULL, nonce bigint NOT NULL, tx_hash text, created_at timestamptz NOT NULL DEFAULT now())`; drop and re-add the platform partial unique index scoped to `(chain, asset, account_type)`; widen `accounts.account_type` CHECK to include `'treasury'`; seed one `treasury` row per `SupportedChainAssetPairs` entry (`customer_id NULL`); widen `withdrawals.status` CHECK to include `'signed','broadcast','confirmed','failed'`; add `tx_hash text` and `nonce bigint` columns to `withdrawals` (denormalized read-convenience, source of truth remains `broadcast_attempts`)
- [x] `internal/core/withdrawal.go` -- four new status constants; `Withdrawal.TxHash string`, `Withdrawal.Nonce *int64`
- [x] `internal/core/ports.go` -- `Signer` interface; `TransactionBroadcaster` interface (`BuildUnsignedWithdrawal(ctx, chain, nonce, to, amount) (digest [32]byte, unsignedTx []byte, err error)`, `AssembleSignedTx(unsignedTx []byte, signature [65]byte) (signedTx []byte, txHash string, err error)`, `SendRawTransaction(ctx, chain, signedTx []byte) error`, `GetFinalizedReceipt(ctx, chain, txHash string) (found, success bool, err error)`); extend `WithdrawalRepository` per Code Map
- [x] `internal/core/sign_and_broadcast_withdrawal.go` + test -- claim-one-and-advance use case (approved→signed→broadcast, one full build/sign/assemble/persist/send cycle per call, so the broadcaster's poll loop calls it once per approved withdrawal per tick) and a poll-receipts use case (broadcast→confirmed/failed per withdrawal with a known `tx_hash`)
- [x] `internal/adapter/postgres/withdrawal_repo.go` + test -- the new repository methods; confirmation settlement's two postings (debit hold, credit treasury) and failure settlement's two postings (debit hold, credit available), each its own balanced journal entry + outbox event, atomically with the status transition
- [x] `internal/adapter/evm/broadcaster.go` + test -- unsigned-tx construction and signing-digest computation (unit-testable without a real chain — deterministic given inputs); a real-anvil integration test for the full send-and-confirm path (skips gracefully if `anvil` isn't installed, matching every existing real-anvil test's pattern)
- [x] `internal/adapter/signer/software/software_signer.go` + test -- sign a known digest, verify the recovered public key matches the configured key's address
- [x] `internal/adapter/signer/kms/kms_signer.go` + test -- unit tests against a fake KMS client returning canned real DER-encoded signatures (generated once via a real ECDSA key, so the low-s/recovery-id logic is exercised against genuine cryptographic material, not fabricated bytes); an opt-in, env-gated (default-skipped) LocalStack integration test mirroring `fee_estimator_test.go`'s `RUN_LIVE_FORK_TESTS` opt-in pattern
- [x] `cmd/walletd/main.go` -- `broadcaster` subcommand; advisory lock (`broadcasterLockID`, a distinct numeric namespace from `watcherLockID`); composition root; poll loop (claim-and-advance, then poll-receipts, each tick)
- [x] `go.mod`/`go.sum` -- add the AWS SDK v2 KMS dependencies

**Acceptance Criteria:**
- Given a withdrawal reaches "approved," when the chain's broadcaster picks it up, then it signs via the Signer port, allocates the next nonce from persisted per-chain state, and broadcasts — transitioning "signed" → "broadcast" (FR20, AD-10).
- Given exactly one broadcaster process per chain, when the broadcaster starts, then it takes a Postgres advisory lock keyed by chain and exits if another instance already holds it (AD-11).
- Given the signer is invoked, whether KMS-backed or software, when any log line, error, or API response related to signing is inspected, then no key handle, private key material, or secret ever appears in it (NFR13).
- Given a broadcast succeeds and is later confirmed on-chain, when observed, then the withdrawal transitions to "confirmed" and its hold is settled (debit hold, credit treasury) (FR16).
- Given a broadcast tx reverts on-chain, when observed, then the withdrawal transitions to "failed" and its hold is released back to available (debit hold, credit available).

## Spec Change Log

## Review Triage Log

### 2026-07-21 — Review pass
- intent_gap: 0
- bad_spec: 0
- patch: 15 (high 2, medium 8, low 5)
- defer: 1 (medium 1)
- reject: 1 (low 1)
- addressed_findings:
  - `[high]` `[patch]` USDC withdrawals' gas estimation would revert against a real ERC-20 contract: `BuildUnsignedWithdrawal` simulated `EstimateGas` with the REAL withdrawal amount encoded in `transfer(to, amount)`, but neither this call nor Arbitrum's analogous `eth_call` ever sets a `from` address — the contract's own `require(balance[from] >= amount)` would fail against the zero/default sender's real (zero) USDC balance on any real chain/contract, reverting estimation for every real USDC withdrawal (both reviewers found this independently; `fee_estimator.go`'s own `representativeTransaction` already established the exact fix for Story 3.1's analogous case, which this diff didn't reuse). Fixed: `EstimateGas` now simulates with a separate `estimateData` encoding amount 0, while the real, signed, broadcast transaction still carries the genuine amount. New regression test asserts both halves.
  - `[high]` `[patch]` `SIGNER_BACKEND` silently defaulted to `"software"` when unset, even though that backend's own package doc says "dev/test only" — every other required config value in `runBroadcaster` already fails loud via `requiredStringEnv`, but the one flag deciding whether real withdrawals get signed by AWS KMS or a plaintext in-memory key did not. Fixed: `SIGNER_BACKEND` is now required, no default; documented in `.env.example` alongside the other new signer env vars.
  - `[medium]` `[patch]` EIP-1559 `GasFeeCap`/`GasTipCap` were both set to the raw suggested price with zero headroom; since fee-bump/replacement is explicitly out of this story's scope, a withdrawal whose cap is outpaced by even a small base-fee increase before inclusion would simply never mine, with no remediation until Story 3.5. Fixed: `GasFeeCap` gets a fixed 20% buffer over the suggested price (does not eliminate the risk, reduces how often it's hit — no replacement logic added, matching the "Never" boundary). New test asserts the headroom.
  - `[medium]` `[patch]` `ClaimApprovedWithdrawal`'s `chain_nonce_state` UPDATE and `RecordBroadcastTxHash`'s `broadcast_attempts` tx_hash UPDATE both lacked `RowsAffected()` checks, unlike every sibling write in the same file. Fixed both, mirroring the established convention exactly; currently-unreachable-but-checked-anyway, same reasoning as the existing checks beside them.
  - `[medium]` `[patch]` `settleWithdrawal`'s missing-hold-account and missing-available-account branches used ad hoc `fmt.Errorf` with no matchable sentinel, unlike the parallel missing-treasury-account branch's `ErrNoTreasuryAccount`. Fixed: added `ErrNoHoldAccount`/`ErrNoAvailableAccount`, each with a dedicated test (constructed by deleting the account's own pre-existing postings first, then the account row, to stay FK-valid — an artificially induced gap, never a realistic production state).
  - `[medium]` `[patch]` `secp256k1.PrivKeyFromBytes` silently reduces a scalar >= the curve order N modulo N rather than erroring, so `software.NewSigner` could construct a DIFFERENT, valid-looking key than a malformed/corrupted `SIGNER_PRIVATE_KEY` value actually specified, with no error at all. Fixed: an explicit `ModNScalar.SetByteSlice` overflow check now rejects this before key construction; new tests cover both the rejection and the N-1 boundary that must still be accepted.
  - `[medium]` `[patch]` No test exercised the low-s normalization boundary at exactly `s == N/2` (only `s > N/2` was covered, and only reachable via ECDSA's negation-symmetry trick, which can't land exactly on the boundary). Extracted the 3-line normalization into its own `normalizeLowS` function so the exact boundary is unit-testable in isolation from signature recovery; added tests for `s == N/2` (unchanged) and `s == N/2 + 1` (flipped).
  - `[medium]` `[patch]` No test exercised `ErrNoChainNonceState` (the registry-gap sentinel `ClaimApprovedWithdrawal` returns), unlike its `ErrNoTreasuryAccount` sibling. Added a dedicated test mirroring the existing treasury-gap test's shape.
  - `[medium]` `[patch]` `TestBroadcaster_RealAnvil_SendAndConfirm` signed via `crypto.Sign` directly rather than through any real `core.Signer` implementation, so no test (run or unrun) ever exercised a real `TransactionBroadcaster` together with a real `Signer` the way `cmd/walletd`'s composition root actually wires them. Fixed: the test now signs via the real `internal/adapter/signer/software.Signer` — a test-only cross-adapter import, the same already-endorsed pattern `internal/adapter/api/integration_test.go`'s own import of `internal/adapter/evm` uses.
  - `[low]` `[patch]` `ClaimApprovedWithdrawal`'s `ORDER BY created_at` had no tiebreaker, so two withdrawals created within the same timestamp-precision window had a nondeterministic claim order across repeated calls. Added `, id` as a secondary sort key.
  - `[low]` `[patch]` `settleWithdrawal`'s `ORDER BY id FOR UPDATE` lock-ordering comment didn't document that its actual deadlock safety rests on AD-11 (one broadcaster process per chain), not on the ordering being globally deterministic the way `CreateWithdrawal`'s identical-looking ordering genuinely is (arbitrary customer pairs). Added a comment clarifying this, so a future reader doesn't assume the same guarantee applies.
  - `[low]` `[patch]` `.env.example` had no documentation for any of `SIGNER_BACKEND`/`SIGNER_PRIVATE_KEY`/`SIGNER_KMS_KEY_ID`, unlike every other role's env vars. Added, consistent with the existing file's own per-role documentation convention.
- **Deferred** (`{implementation_artifacts}/deferred-work.md`): a sustained Signer/RPC outage causes `runBroadcaster`'s poll loop to strand one additional approved withdrawal at `WithdrawalStatusSigned` every poll interval (nonce + `broadcast_attempts` row already committed before the failure surfaces) until the entire approved-withdrawal backlog is exhausted, none of which self-heals until Story 3.5. Not patched: proactively detecting "the signer/RPC is unhealthy, stop claiming" is circuit-breaker-style logic that borders on Story 3.5's own explicitly-scoped "stuck withdrawal" territory (this story's own Never section excludes declaring broadcasts stuck), so building it now would preempt/duplicate that story's design.
- **Rejected** (noise): three separate `CHECK (chain IN ('base','arbitrum'))` constraints across `chain_nonce_state`/`broadcast_attempts`/`withdrawals` rather than a shared domain type — mechanical, but this repeats every prior migration's identical pattern (0005 through 0010), not a new inconsistency this story introduces.

## Design Notes

- **Why the Signer port only ever sees a digest, never a transaction.** Keeping `core.Signer.Sign` chain-library-agnostic (`[32]byte` in, `[65]byte` out) is what lets it live in `internal/core` at all — AD-1's import-boundary check fails the build the moment any file outside `internal/adapter/evm` imports go-ethereum. All RLP/transaction-shape knowledge stays in the EVM adapter's `TransactionBroadcaster`; core only orchestrates opaque byte blobs between two ports it's allowed to know about.
- **Why nonce allocation + the `broadcast_attempts` row commit BEFORE signing/broadcasting.** This is AD-11's own wording ("in the same transaction that records the broadcast attempt") and is what makes the system's crash-recovery story (Story 3.5) well-defined at all: once a nonce is durably committed against a specific withdrawal, a crash before the tx is actually sent leaves an inspectable, resumable record (`broadcast_attempts` row, no `tx_hash` yet) rather than an ambiguous gap. This story creates that resumable shape; Story 3.5 is what actually resumes it.
- **Why the settlement posting direction is `debit hold / credit treasury` (confirm) and `debit hold / credit available` (fail), not something else.** Hold was created at Story 3.2's placement as `credit hold` (+amount) paired with `debit available` (-amount) — confirmed directly in `withdrawal_repo.go`'s existing `CreateWithdrawal`. Postings within one journal entry must net to zero, so extinguishing that specific hold requires a `-amount` posting on hold; the only account SOLUTION-DESIGN.md names as the settlement counterparty is treasury ("extinguishes the hold against platform treasury") for success, or available itself ("releases the hold back to available") for failure — both readings are the unique arithmetically-valid completion of "hold: -amount" plus a real named counterparty, not a guess.
- **Why `treasury` needs its own migration now, not Story 3.6's.** `SOLUTION-DESIGN.md`'s account taxonomy table already names "Platform treasury: the hot wallet's holdings" as one of v1's five fixed account types — Story 3.4 is the FIRST story that actually needs a real `treasury` row to post against (Story 3.6's sweeps post INTO the same rows this story creates, they don't create them).
- **Why confirmation checks the `finalized` tag, not a fixed confirmation count.** Mirrors AD-7's identical choice for deposit crediting — settling a withdrawal's hold is exactly as irreversible an operation (extinguishes a hold, credits treasury, in v1 there is no un-confirm path) and reuses the exact tag-query mechanism `evm.Scanner.Head` already implements.
- **Why `broadcast_attempts` is a real table and not just columns on `withdrawals`.** Mirrors the ER diagram's own shape (`WITHDRAWAL ||--o{ BROADCAST_ATTEMPT`) and anticipates Story 3.6 reusing the same table for sweeps (a `sweep_id` column added in 3.6's own migration, the same "widen it in your own migration" convention every prior story here has followed) — `withdrawals` itself keeps only denormalized `tx_hash`/`nonce` columns for cheap reads, `broadcast_attempts` remains the source of truth.

## Verification

**Commands:**
- `go build ./... && go vet ./... && gofmt -l .` -- expected: clean
- `go test ./internal/core/... ./internal/adapter/postgres/... ./internal/adapter/evm/... ./internal/adapter/signer/...` -- expected: all green (pre-existing, unrelated reorg-detection failures excepted)
- `make check-import-boundary` -- expected: still passes — no go-ethereum import anywhere outside `internal/adapter/evm`, including the new `internal/adapter/signer/kms` package (it imports AWS SDK, not go-ethereum, so this should hold naturally)

**Manual checks (if no CLI):**
- With `anvil` installed locally: run the broadcaster against a local anvil chain with the software signer, create+approve a withdrawal via the API, confirm the broadcaster signs, broadcasts, and — once anvil mines the block — settles it to "confirmed" with the treasury/hold postings visible in Postgres.
- LocalStack KMS integration (opt-in, not required for this story's own CI-equivalent `go test ./...`): start a LocalStack container, create an `ECC_SECG_P256K1` key, point `SIGNER_BACKEND=kms` + the AWS endpoint override at it, and confirm the KMS signer produces a signature whose recovered address matches the configured hot-wallet address — this is the one piece of "same adapter, same code path against LocalStack" AD-10 promises that this run cannot fully exercise inside this sandboxed environment (no LocalStack container available here); flagged as a residual verification gap, not skipped silently.

## Auto Run Result

**Status:** done

**Summary:** Implemented Story 3.4 end to end: a new `broadcaster --chain=<c>` subcommand (advisory-locked per chain, mirroring the existing `watcher`) claims `approved` withdrawals, allocates a nonce from persisted per-chain state and commits a `broadcast_attempts` row before ever calling out to a new `core.Signer` port (AWS KMS via a vendored DER-decode/low-s-normalize/recovery-id-by-trial recipe, and an in-memory software signer — both deliberately implemented via `github.com/decred/dcrd/dcrec/secp256k1/v4` directly rather than go-ethereum, respecting the `internal/adapter/evm`-only import boundary) and a new `core.TransactionBroadcaster` port (build unsigned EIP-1559 tx + digest, assemble signed tx, send, check `finalized` receipts). Confirmation settles the hold against a new platform `treasury` account (debit hold, credit treasury); an on-chain revert releases the hold back to available (debit hold, credit available) — both directions derived rigorously from the postings-must-net-to-zero invariant plus `SOLUTION-DESIGN.md`'s explicit wording, not guessed. A full adversarial + edge-case review pass then found and fixed 15 real issues, including two high-severity ones (a USDC gas-estimation bug that would have reverted every real USDC withdrawal, and a silent insecure signer-backend default) before this landed.

**Files changed:**

*New:*
- `internal/adapter/postgres/migrations/0011_add_broadcaster_signing_and_treasury.sql` — `chain_nonce_state`, `broadcast_attempts` tables; widened `accounts.account_type` CHECK + its platform partial unique index to include `account_type`; seeded `treasury` platform rows; widened `withdrawals.status` CHECK; added `withdrawals.tx_hash`/`nonce`
- `internal/core/sign_and_broadcast_withdrawal.go`, `poll_withdrawal_receipts.go` (+ tests) — the two new use cases orchestrating claim→sign→assemble→broadcast and poll→settle
- `internal/adapter/evm/broadcaster.go` (+ test) — `TransactionBroadcaster` implementation; the USDC gas-estimation fix and gas-fee-cap headroom live here
- `internal/adapter/signer/kms/kms_signer.go`, `internal/adapter/signer/software/software_signer.go` (+ tests) — the two `core.Signer` implementations; the scalar-overflow rejection and the extracted, boundary-tested `normalizeLowS` live here
- `internal/adapter/postgres/withdrawal_broadcast_repo_test.go` — integration coverage for all five new repository methods, including every registry-gap sentinel

*Modified:*
- `internal/adapter/postgres/withdrawal_repo.go` — `ClaimApprovedWithdrawal`, `RecordBroadcastTxHash`, `ListBroadcastWithdrawals`, `SettleConfirmedWithdrawal`, `SettleFailedWithdrawal`, shared `settleWithdrawal`; `ErrNoChainNonceState`/`ErrNoTreasuryAccount`/`ErrNoHoldAccount`/`ErrNoAvailableAccount`
- `internal/core/customer.go`, `ports.go`, `withdrawal.go` — `AccountTypeTreasury`; `Signer`/`TransactionBroadcaster` ports; new status constants, `TxHash`/`Nonce` fields, two new error sentinels
- `cmd/walletd/main.go` — `broadcaster` subcommand, `broadcasterLockID`, composition root (including the now-required `SIGNER_BACKEND`)
- `.env.example` — documented `SIGNER_BACKEND`/`SIGNER_PRIVATE_KEY`/`SIGNER_KMS_KEY_ID`
- `go.mod`/`go.sum` — AWS SDK v2 KMS dependencies

**Review findings breakdown** (2026-07-21 pass, Blind Hunter + Edge Case Hunter, run independently without shared context, findings deduplicated):
- 15 patch (2 high, 8 medium, 5 low) — all applied and re-verified; see the Review Triage Log above for the full list. The two high-severity ones: USDC withdrawal gas estimation would have reverted against any real ERC-20 contract (both reviewers found this independently); `SIGNER_BACKEND` silently defaulted to the dev-only software signer in production with no error.
- 1 defer (medium) — logged in `deferred-work.md`: a sustained Signer/RPC outage strands the approved-withdrawal queue one withdrawal per poll tick, with no self-healing until Story 3.5 exists; fixing it properly borders on that story's own scope.
- 1 reject (low, noise): three independent `CHECK (chain IN (...))` constraints instead of a shared domain type — repeats every prior migration's own pattern, not new here.
- 0 intent_gap, 0 bad_spec — every finding was a mechanical patch, an appropriately-deferred design question, or noise; nothing required renegotiating the frozen intent-contract (including the settlement posting direction, which held up under adversarial scrutiny with no counter-argument raised).

**Verification performed:**
- `go build ./...`, `go vet ./...`, `gofmt -l .`, `make check-import-boundary` — all clean, both before and after the review-pass patches
- `go test ./internal/core/... ./internal/adapter/postgres/... ./internal/adapter/evm/... ./internal/adapter/signer/...` — all green except the same 4 pre-existing, unrelated `TestTrackDeposits_*` reorg-detection failures already called out in this spec's own Verification section (confirmed via `git stash` to fail identically on the unmodified baseline)
- The KMS crypto recipe (DER decode, low-s normalization including the exact `s == N/2` boundary, recovery-id-by-trial, wrong-key rejection) is exercised against genuine `crypto/ecdsa`-generated cryptographic material throughout, never fabricated bytes
- `internal/adapter/postgres/withdrawal_broadcast_repo_test.go` runs against a real Postgres 18 testcontainer, including every registry-gap sentinel (`ErrNoChainNonceState`, `ErrNoTreasuryAccount`, `ErrNoHoldAccount`, `ErrNoAvailableAccount`) with dedicated, FK-valid fixtures
- The real-anvil broadcaster test (`TestBroadcaster_RealAnvil_SendAndConfirm`) now signs through the real `software.Signer` rather than raw `crypto.Sign`, but still skips in this environment — `anvil` is not installed here, matching every other real-anvil test in this repo
- The opt-in LocalStack KMS integration test is written and env-gated but was never run — no LocalStack container is available in this sandboxed environment; disclosed, not silently skipped

**Residual risks:**
- The 1 deferred item above remains open, tracked in `deferred-work.md`, alongside the two items deferred from Story 3.3's own review pass.
- No git commit was created (user's global no-auto-commit policy). All changes — Stories 3.3 and 3.4 alike — remain uncommitted, stacked on top of each other; see `final_revision` frontmatter.
- The LocalStack KMS path and the real-anvil broadcaster path are both written but unexercised in this environment (no LocalStack container, no `anvil` binary here) — the underlying cryptographic recipe and the EVM tx-assembly logic are independently verified against genuine material via unit tests, but the two together, against real external systems, remain unverified until run somewhere both are available.
- `followup_review_recommended: true` — this story introduces the platform's first real signing/broadcasting surface (new external KMS dependency, new cryptographic recipe, real money leaving the ledger for the first time via treasury settlement) and the review pass already caught two high-severity, production-breaking-class bugs before this landed; that combination of novelty, custody-adjacent risk, and already-demonstrated defect density warrants one more independent look.
