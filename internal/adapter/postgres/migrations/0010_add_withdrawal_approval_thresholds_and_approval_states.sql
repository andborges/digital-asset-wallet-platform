-- +goose Up
-- withdrawal_approval_thresholds is a data table, not a Go constant (mirrors migration
-- 0006's crediting_policy precedent exactly, FR9-style "policy is data"): PRD open
-- question 3 flags per-asset thresholds as an ops/pre-launch setting still to be decided,
-- so the seeded values below are explicitly a placeholder, not a final number. Whoever
-- operates this platform before real launch must revisit these rows.
CREATE TABLE withdrawal_approval_thresholds (
    chain            text NOT NULL CHECK (chain IN ('base', 'arbitrum')),
    asset            text NOT NULL CHECK (asset IN ('eth', 'usdc')),
    threshold_amount NUMERIC(78,0) NOT NULL CHECK (threshold_amount > 0),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, asset)
);

-- Placeholder threshold values (PRD open question 3 — NOT a final, reviewed number):
-- 1 whole ETH (10^18 wei) for ETH, 1,000 USDC (10^6 base units per USDC) for USDC, on
-- both chains. Seeding a value (rather than leaving the table empty) lets every other
-- check in this story's own test suite exercise both the auto-approval and
-- awaiting-approval routes without a separate seeding step.
INSERT INTO withdrawal_approval_thresholds (chain, asset, threshold_amount) VALUES
    ('base', 'eth', 1000000000000000000),
    ('base', 'usdc', 1000000000),
    ('arbitrum', 'eth', 1000000000000000000),
    ('arbitrum', 'usdc', 1000000000);

-- withdrawals.status widens from 'created' only to the three values this story's own
-- transitions produce — CreateWithdrawal now advances a withdrawal straight past
-- 'created' into 'awaiting-approval' or 'approved' in the same request (Design Notes:
-- "api-through-core, single writer", AD-6). Stories 3.4/3.5 each widen this CHECK further
-- in their own migration as they add the status value their own transition needs, mirroring
-- this story's own precedent from migration 0009. The constraint name
-- 'withdrawals_status_check' was confirmed empirically (via \d withdrawals against a real
-- throwaway Postgres 18 container migrated up through 0009) before writing this DROP,
-- never guessed — mirroring migration 0009's own down-migration discipline.
ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check CHECK (status IN ('created', 'awaiting-approval', 'approved'));

-- approved_at/approved_by/approval_reason are stored directly on withdrawals, not
-- derived from a separate audit-log table (Design Notes, mirroring hold_journal_entry_id's
-- own design note from migration 0009): the one thing every later consumer needs — who
-- approved this, when, why — is cheapest as plain columns on the row itself. All three are
-- nullable: a withdrawal auto-approved by the threshold check (never touched by an
-- operator) has no actor/reason to log, and a withdrawal still awaiting approval has none
-- of the three yet.
ALTER TABLE withdrawals ADD COLUMN approved_at timestamptz;
ALTER TABLE withdrawals ADD COLUMN approved_by text;
ALTER TABLE withdrawals ADD COLUMN approval_reason text;

-- +goose Down
-- Re-narrowing the CHECK back to 'created' only can only succeed if no withdrawal row
-- currently sits in 'awaiting-approval' or 'approved' — but that is exactly the normal,
-- expected outcome of this story's own transitions once CreateWithdrawal/ApproveWithdrawal
-- have run even once. Delete any such row first (mirrors migration 0008's identical
-- down-migration discipline: down-migrations are dev/rollback tooling, not a production
-- path, so sacrificing an already-advanced withdrawal row here is the right tradeoff,
-- rather than failing mid-rollback on a CHECK violation).
DELETE FROM withdrawals WHERE status != 'created';
ALTER TABLE withdrawals DROP COLUMN approval_reason;
ALTER TABLE withdrawals DROP COLUMN approved_by;
ALTER TABLE withdrawals DROP COLUMN approved_at;
ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check CHECK (status = 'created');
DROP TABLE withdrawal_approval_thresholds;
