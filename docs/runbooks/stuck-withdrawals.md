# Runbook: Stuck Withdrawals

Story 3.5. Audience: on-call operator responding to a `withdrawal.stuck` outbox event.
There is no operator-facing API for anything in this runbook (v1's own "no operator-identity
system yet" carve-out, same as every other manual-intervention story) — everything below is
a direct read/write against the platform database.

## What `withdrawal.stuck` means

The broadcaster (`walletd broadcaster --chain=<base|arbitrum>`) checks, every poll cycle,
whether any withdrawal has spent too long at either of two statuses without progressing,
never yet alerted:

- **`WithdrawalStatusSigned`** — claimed (a nonce was allocated) but no broadcast attempt has
  succeeded yet, for longer than `WITHDRAWAL_STUCK_THRESHOLD` (default 30 minutes) since the
  claim. This is the case where the broadcaster cannot get the transaction onto the network at
  all — e.g. the RPC endpoint is persistently erroring, or every resend attempt is failing.
- **`WithdrawalStatusBroadcast`** — successfully sent to the network (`tx_hash` is known) but
  not yet confirmed, for longer than the same threshold since the broadcast.

The first time a given withdrawal crosses the relevant threshold, exactly one
`withdrawal.stuck` outbox event is written and `withdrawals.stuck_alerted_at` is set — never
repeated for the same withdrawal, regardless of which status triggered it.

**This is a monitoring signal, not a failure.** Most of the time this resolves on its own:
a `broadcast`-status alert usually clears once the network catches up (congestion, a slow
block, a gas-price spike outrunning the transaction's `GasFeeCap`) and the broadcaster's
normal receipt-polling settles it to `confirmed` or `failed` with no operator action at all. A
`signed`-status alert is more likely to need a look at the broadcaster's own logs (RPC
connectivity, resend errors) since nothing has reached the network yet — see the liveness-gate
section below for one common cause. `stuck_alerted_at` is never cleared, even after the
withdrawal resolves — it is a historical fact ("this one needed watching"), not a live status.

The outbox event's payload carries everything needed to investigate, including which status
triggered the alert:

```json
{
  "withdrawalId": "...",
  "customerId": "...",
  "chain": "base",
  "asset": "eth",
  "amount": "...",
  "destinationAddress": "0x...",
  "status": "signed",
  "txHash": ""
}
```

`txHash` is empty when `status` is `signed` — the transaction has never been successfully
broadcast, so there is nothing to look up on-chain yet. Skip straight to checking the
broadcaster's logs for this withdrawal's `id` rather than following Step 2 below, which
assumes a `tx_hash` exists.

## The liveness gate

The broadcaster only claims a **new** withdrawal (allocates a fresh nonce via
`ClaimApprovedWithdrawal`) when the chain's watcher looks live — derived from how fresh
`watcher_cursors.updated_at` is for that chain's `observed` tier
(`internal/adapter/postgres/watcher_liveness.go`). If the watcher has stalled (crashed, stuck
on an RPC error, etc.), its cursor stops advancing, and the broadcaster stops claiming new
withdrawals for that chain until the watcher recovers — new claims allocate a nonce, and
allocating a nonce while the chain's own state is unknown is the one thing this gate exists to
prevent.

This gate **never** blocks resuming a withdrawal that is already `signed` or `broadcast` —
only fresh claims. So a `signed`-status stuck alert is not, by itself, evidence of a live
watcher outage: check the watcher process's own logs/health first, but don't assume the two
are connected just because both exist.

To check watcher liveness directly:

```sql
SELECT chain, tier, updated_at, now() - updated_at AS age
FROM watcher_cursors
WHERE tier = 'observed'
ORDER BY chain;
```

A large `age` for a chain (bigger than its configured staleness threshold) means that chain's
watcher is not advancing — investigate the watcher process itself, not the broadcaster.

## Step 1: Check the withdrawal's current state in the database

```sql
SELECT id, customer_id, chain, asset, amount, destination_address, status, tx_hash, nonce,
       created_at, updated_at, stuck_alerted_at
FROM withdrawals
WHERE id = '<withdrawal_id>';
```

If `status` is no longer `broadcast` (already `confirmed` or `failed`), the broadcaster
already resolved it after the alert fired — nothing to do; this happens routinely and is not
itself a problem.

If `status` is still `broadcast`, continue to Step 2.

## Step 2: Check the transaction's real on-chain state

Use `tx_hash` from Step 1 (or the outbox payload). Two equivalent options:

**Block explorer** (fastest for a human): open the appropriate explorer for the chain
(e.g. Basescan for `base`, Arbiscan for `arbitrum`) and search `tx_hash`. Look for:
- **Pending** — still sitting in the mempool, not yet included in a block.
- **Confirmed / Success** — mined and successful. It just hasn't reached the `finalized` tag
  yet on this chain (Base/Arbitrum's L2 finality can lag well behind L1 inclusion) — the
  broadcaster will settle it to `confirmed` automatically on a later poll. No action needed.
- **Confirmed / Failed (reverted)** — mined but reverted. The broadcaster will settle it to
  `failed` automatically once it reaches `finalized`. No action needed.
- **Not found at all** — never made it into a block and has since dropped from the mempool
  (commonly: underpriced relative to current base fee, or superseded by a differently-nonced
  transaction from this same hot wallet — see AD-11/AD-10, only one process and one address
  ever send from this wallet, so "superseded" here only ever means this withdrawal's own
  prior attempt). This is the case that needs Step 3.

**Direct RPC call** (for scripting, or when the explorer itself is degraded):

```bash
curl -s -X POST "$CHAIN_RPC_URL" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["<tx_hash>"]}'
```

- `"result": null` — no receipt; either still pending or dropped. Cross-check
  `eth_getTransactionByHash` with the same `tx_hash`: if that ALSO returns `null`, the
  transaction is not in the mempool either — dropped.
- `"result": {"status": "0x1", ...}` — mined and successful.
- `"result": {"status": "0x0", ...}` — mined and reverted.

Also worth checking, if the transaction looks dropped: whether a LATER nonce from this same
hot wallet address has since confirmed on-chain (`eth_getTransactionCount` against the
address, compared to this withdrawal's own `nonce` column).

- **Nonce moved past, and a receipt for this `tx_hash` was found in Step 2 after all** — not
  actually the dropped case; re-read Step 2's result. Every resend of this withdrawal sends
  byte-identical bytes (Story 3.5's persist-before-send ordering), so the transaction that took
  this nonce's slot IS this `tx_hash` (AD-11/AD-10: nothing else ever sends from this wallet),
  and its receipt should be findable by that same hash.
- **Nonce moved past, but NO receipt is findable for this `tx_hash` at all** — this should not
  happen under normal operation, and points to one of: a reorg that replaced the block
  containing this exact transaction with a competing history that instead mined a
  differently-nonced transaction (rare, but possible during periods of chain instability); a
  second broadcaster process running against this same chain outside AD-11's single-writer
  guarantee (configuration error — check for a duplicate `walletd broadcaster` deployment); or
  an out-of-band transaction sent from this hot wallet's address by some means outside this
  application entirely. Treat this as an incident, not a routine stuck-withdrawal case:
  escalate before proceeding to Step 4, since the money's actual on-chain fate is genuinely
  unclear and Step 4's ledger entry assumes it is not.

## Step 3: Decide

- **Receipt found (success or revert), just not yet finalized** → do nothing. Re-check in a
  while; the broadcaster's own poll loop will settle it.
- **Transaction genuinely dropped, and the chain's nonce for the hot wallet has NOT moved past
  this withdrawal's nonce** (confirmed via Step 2) → it may still be re-sent and mined
  normally; the broadcaster's own resend path (Story 3.5) will keep re-sending the same signed
  bytes on every poll. Give it more time before considering Step 4. (v1 has no
  fee-bump/replacement mechanism — Story 3.4/3.5's own explicit boundary — so a transaction
  stuck below the current base fee will not mine until the base fee drops back down, or until
  an operator manually intervenes here.)
- **Transaction genuinely dropped, and the chain's nonce HAS moved past this withdrawal's
  nonce, but a receipt for this withdrawal's own `tx_hash` explains it** (per Step 2's first
  bullet — one of this withdrawal's own resends landed) → not actually dropped; treat as
  "receipt found" above.
- **Transaction genuinely dropped, the chain's nonce HAS moved past this withdrawal's nonce,
  and no receipt for this `tx_hash` is findable anywhere** (per Step 2's second bullet) →
  **do not proceed to Step 4.** This is the incident case — escalate first; only resolve it
  manually (Step 4, or something else entirely depending on what the incident turns up) once
  the on-chain fate of the funds is actually understood.

**Never** run Step 4 while there is any chance the original transaction could still confirm —
doing so risks a customer's withdrawal being marked `failed` (hold released back to
`available`, funds effectively returned) while the original transaction *also* eventually
mines and moves the money on-chain, which would be a real double-release of funds. Step 4 is
for the case where the on-chain evidence rules that out.

## Step 4: Manually force the withdrawal to `failed`

This performs, by hand, exactly what `SettleFailedWithdrawal`
(`internal/adapter/postgres/withdrawal_repo.go`) does automatically for an on-chain revert:
one balanced journal entry (debit the customer's hold account, credit the customer's own
available account — releasing the hold) plus the status transition plus the paired
`withdrawal.failed` outbox event, all in one transaction. **Never** run a bare
`UPDATE withdrawals SET status = 'failed'` — that skips the ledger entry entirely and leaves
the customer's hold account permanently overstated (their money would be stuck in `hold`
forever, never returned to `available`).

Run this as ONE transaction (`psql` or any client that supports `BEGIN`/`COMMIT`), replacing
`<withdrawal_id>` throughout:

```sql
BEGIN;

-- Lock the withdrawal row and re-verify it is still 'broadcast' — the same guard
-- settleWithdrawal's own SELECT ... FOR UPDATE applies. If this SELECT returns zero rows or
-- a status other than 'broadcast', STOP — do not proceed; something else already resolved
-- it (re-check Step 1).
SELECT id, customer_id, chain, asset, amount, status
FROM withdrawals
WHERE id = '<withdrawal_id>'
FOR UPDATE;

-- Lock this customer's hold and available accounts for this withdrawal's (chain, asset) —
-- mirrors settleWithdrawal's own lock-ordering discipline.
SELECT id, account_type
FROM accounts
WHERE customer_id = (SELECT customer_id FROM withdrawals WHERE id = '<withdrawal_id>')
  AND chain = (SELECT chain FROM withdrawals WHERE id = '<withdrawal_id>')
  AND asset = (SELECT asset FROM withdrawals WHERE id = '<withdrawal_id>')
  AND account_type IN ('hold', 'available')
ORDER BY id
FOR UPDATE;

-- One balanced journal entry: cause_type/cause_id mirror withdrawalFailureCauseType's own
-- convention exactly (cause_type = 'withdrawal_failure', cause_id = the withdrawal's own
-- id) — this is what UNIQUE(cause_type, cause_id) (AD-3) relies on to make a double-run of
-- this whole procedure fail loudly on the INSERT below rather than double-releasing funds.
INSERT INTO journal_entries (id, cause_type, cause_id, created_at)
VALUES (gen_random_uuid(), 'withdrawal_failure', '<withdrawal_id>', now());

-- Debit hold, credit available — the same two postings settleWithdrawal writes.
INSERT INTO postings (id, journal_entry_id, account_id, amount, created_at)
SELECT gen_random_uuid(),
       (SELECT id FROM journal_entries WHERE cause_type = 'withdrawal_failure' AND cause_id = '<withdrawal_id>'),
       a.id,
       CASE WHEN a.account_type = 'hold' THEN -w.amount ELSE w.amount END,
       now()
FROM withdrawals w
JOIN accounts a ON a.customer_id = w.customer_id AND a.chain = w.chain AND a.asset = w.asset
WHERE w.id = '<withdrawal_id>' AND a.account_type IN ('hold', 'available');

-- Transition to failed — scoped to status = 'broadcast', mirroring settleWithdrawal's own
-- WHERE clause, so this is a no-op (0 rows) rather than a silent double-transition if
-- something else already moved it.
UPDATE withdrawals
SET status = 'failed', updated_at = now()
WHERE id = '<withdrawal_id>' AND status = 'broadcast';

-- The paired outbox event, same event_type/payload shape SettleFailedWithdrawal writes —
-- required so downstream consumers subscribed to "withdrawal.failed" see this exactly like
-- any automated failure.
INSERT INTO outbox_events (id, event_type, payload, created_at)
SELECT gen_random_uuid(),
       'withdrawal.failed',
       jsonb_build_object(
         'withdrawalId', w.id,
         'journalEntryId', (SELECT id FROM journal_entries WHERE cause_type = 'withdrawal_failure' AND cause_id = w.id::text),
         'customerId', w.customer_id,
         'chain', w.chain,
         'asset', w.asset,
         'amount', w.amount::text
       ),
       now()
FROM withdrawals w
WHERE w.id = '<withdrawal_id>';

-- Sanity check before committing: confirm exactly one row moved, and the ledger balances.
SELECT status, stuck_alerted_at FROM withdrawals WHERE id = '<withdrawal_id>';
SELECT a.account_type, COALESCE(SUM(p.amount), 0) AS balance
FROM accounts a
LEFT JOIN postings p ON p.account_id = a.id
WHERE a.customer_id = (SELECT customer_id FROM withdrawals WHERE id = '<withdrawal_id>')
  AND a.chain = (SELECT chain FROM withdrawals WHERE id = '<withdrawal_id>')
  AND a.asset = (SELECT asset FROM withdrawals WHERE id = '<withdrawal_id>')
GROUP BY a.account_type;

-- Only COMMIT once every statement above succeeded and the sanity check looks right —
-- otherwise ROLLBACK and escalate instead of guessing.
COMMIT;
```

If the `UPDATE withdrawals` statement affects zero rows, or the journal-entry `INSERT` fails
on the `UNIQUE(cause_type, cause_id)` constraint, **stop and `ROLLBACK`** — it means the
withdrawal was no longer `broadcast` (or was already manually/automatically settled) by the
time this transaction ran, and proceeding would either be a no-op or, worse, a double
release. Re-run Step 1 to see its current state before trying again.

`stuck_alerted_at` is left untouched by this whole procedure — it stays set, exactly as
Design Notes intends: a historical record that this withdrawal needed attention, independent
of how it eventually resolved.
