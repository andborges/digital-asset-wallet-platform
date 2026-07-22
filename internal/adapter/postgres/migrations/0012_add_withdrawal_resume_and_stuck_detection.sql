-- +goose Up
-- broadcast_attempts.signed_tx durably persists the EXACT signed transaction bytes (hex
-- encoded) BEFORE the send call is ever attempted (Story 3.5's core restructuring, Design
-- Notes: AWS KMS's ECDSA signing is not guaranteed deterministic, so a crash-forced
-- re-sign could legitimately produce different, equally valid bytes for the same nonce —
-- an ambiguous double-broadcast this story exists to close). Nullable: a freshly claimed
-- withdrawal (WithdrawalStatusSigned, broadcast_attempts row inserted by
-- ClaimApprovedWithdrawal) has no signed_tx yet until RecordSignedTx runs — the exact
-- resumable, well-defined state a crash between claim and sign leaves behind. No CHECK or
-- index needed: this column is never queried by its own value, only read back whole for a
-- specific withdrawal_id (already indexed via the table's own UNIQUE(withdrawal_id)).
ALTER TABLE broadcast_attempts ADD COLUMN signed_tx text;

-- withdrawals.stuck_alerted_at records when this withdrawal's one-time "withdrawal.stuck"
-- outbox event was written (Story 3.5, Design Notes: a monitoring signal layered on the
-- existing 'broadcast' status, never a new terminal state) — NULL until DetectStuckWithdrawals
-- first alerts on it, and never cleared afterward even once the withdrawal later confirms or
-- fails (a historical fact, I/O & Edge-Case Matrix's own last row). Nullable, no CHECK: every
-- withdrawal starts and typically stays NULL; only a withdrawal that has actually gone stuck
-- ever gets a value. No index needed either — ListStuckCandidates' own WHERE clause is
-- primarily selective on (chain, status), for which no index exists yet in this codebase
-- either (mirrors ListBroadcastWithdrawals'/ClaimApprovedWithdrawal's own unindexed
-- WHERE chain/status scans — this table is not expected to grow large enough in v1 to need
-- one, the same tradeoff already accepted there).
ALTER TABLE withdrawals ADD COLUMN stuck_alerted_at timestamptz;

-- +goose Down
ALTER TABLE withdrawals DROP COLUMN stuck_alerted_at;
ALTER TABLE broadcast_attempts DROP COLUMN signed_tx;
