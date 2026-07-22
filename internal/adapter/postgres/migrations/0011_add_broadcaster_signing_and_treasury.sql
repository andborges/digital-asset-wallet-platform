-- +goose Up
-- chain_nonce_state is per-chain only, not per-address (Story 3.4, AD-10): exactly one
-- hot-wallet address is pinned system-wide, valid on both chains, so chain alone is a
-- sufficient key for "the next nonce this broadcaster process should allocate." Seeded at
-- 0 for both chains — a placeholder, same "ops must revisit before go-live" pattern as
-- migration 0010's withdrawal_approval_thresholds seed: the real starting nonce for the
-- actual configured hot-wallet address on a real chain must be confirmed by ops (e.g. via
-- eth_getTransactionCount) before this broadcaster ever runs against a wallet with
-- pre-existing on-chain transaction history.
CREATE TABLE chain_nonce_state (
    chain      text NOT NULL PRIMARY KEY CHECK (chain IN ('base', 'arbitrum')),
    next_nonce bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO chain_nonce_state (chain, next_nonce) VALUES
    ('base', 0),
    ('arbitrum', 0);

-- broadcast_attempts is a real table, not just columns on withdrawals (Design Notes): it
-- mirrors the ER diagram's own WITHDRAWAL ||--o{ BROADCAST_ATTEMPT shape and anticipates
-- Story 3.6 reusing it for sweeps. UNIQUE(withdrawal_id) is v1's "exactly one attempt per
-- withdrawal" rule (Boundaries & Constraints: no fee-bump/replacement transactions in this
-- story). tx_hash is nullable: AD-11's own wording requires the nonce allocation and this
-- row's insert to commit in the SAME transaction, BEFORE the sign/broadcast calls happen —
-- so a row can legitimately exist with no tx_hash yet (the exact resumable, well-defined
-- state a crash between commit and broadcast leaves behind, Story 3.5's territory to
-- resume).
CREATE TABLE broadcast_attempts (
    id            uuid PRIMARY KEY,
    withdrawal_id uuid NOT NULL UNIQUE REFERENCES withdrawals(id),
    chain         text NOT NULL CHECK (chain IN ('base', 'arbitrum')),
    nonce         bigint NOT NULL,
    tx_hash       text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- accounts.account_type widens to add 'treasury' (SOLUTION-DESIGN.md's fixed v1 account
-- taxonomy: "Platform treasury: the hot wallet's holdings"). The CHECK constraint's current
-- name, accounts_account_type_check, and the platform partial unique index's current name,
-- idx_accounts_platform_chain_asset, were both confirmed empirically (via \d accounts
-- against a real, throwaway Postgres 18 instance migrated up through 0010) before writing
-- these DROP statements, never guessed — mirroring migration 0009/0010's own discipline for
-- this exact class of risk.
ALTER TABLE accounts DROP CONSTRAINT accounts_account_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_account_type_check CHECK (account_type IN ('available', 'hold', 'treasury'));

-- The existing idx_accounts_platform_chain_asset scopes to (chain, asset) WHERE
-- customer_id IS NULL — today it allows only ONE platform row per (chain, asset) at all,
-- which would collide the instant a 'treasury' row is seeded alongside the existing
-- 'forwarder-float' row (account_type = 'available', per migration 0009's own column
-- DEFAULT) for the same pair. Widening the index's key to (chain, asset, account_type)
-- lets both platform account types coexist per pair, one row each, while still preventing
-- more than one treasury (or more than one forwarder-float) row per (chain, asset).
DROP INDEX idx_accounts_platform_chain_asset;
CREATE UNIQUE INDEX idx_accounts_platform_chain_asset_type ON accounts (chain, asset, account_type) WHERE customer_id IS NULL;

-- Seed one treasury row per SupportedChainAssetPairs entry (customer_id NULL), mirroring
-- migration 0006's forwarder-float seeding exactly.
INSERT INTO accounts (id, customer_id, chain, asset, account_type) VALUES
    (gen_random_uuid(), NULL, 'base', 'eth', 'treasury'),
    (gen_random_uuid(), NULL, 'base', 'usdc', 'treasury'),
    (gen_random_uuid(), NULL, 'arbitrum', 'eth', 'treasury'),
    (gen_random_uuid(), NULL, 'arbitrum', 'usdc', 'treasury');

-- withdrawals.status widens to add this story's own transitions ('signed' -> 'broadcast'
-- -> 'confirmed'/'failed'), mirroring migrations 0009/0010's own precedent of each story
-- widening this CHECK for the value its own transition needs. withdrawals_status_check's
-- current name (re-added, unchanged, by migration 0010) was confirmed empirically the same
-- way as accounts_account_type_check above, never guessed.
ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check CHECK (status IN ('created', 'awaiting-approval', 'approved', 'signed', 'broadcast', 'confirmed', 'failed'));

-- tx_hash/nonce are denormalized read-convenience columns on withdrawals (Design Notes):
-- source of truth remains broadcast_attempts, but every consumer that just wants "this
-- withdrawal's tx hash/nonce" (an operator dashboard, a support query) can read them
-- directly off withdrawals without a join. Both nullable: unset until this story's
-- SignAndBroadcastWithdrawal use case actually signs and broadcasts.
ALTER TABLE withdrawals ADD COLUMN tx_hash text;
ALTER TABLE withdrawals ADD COLUMN nonce bigint;

-- +goose Down
ALTER TABLE withdrawals DROP COLUMN nonce;
ALTER TABLE withdrawals DROP COLUMN tx_hash;

-- Mirrors migration 0009/0010's own down-migration discipline: down-migrations are
-- dev/rollback tooling, not a production path, so sacrificing any withdrawal already
-- advanced past 'approved' is the accepted tradeoff rather than failing mid-rollback on a
-- CHECK violation. Postings/journal entries for any settlement this story's use cases wrote
-- (confirm or fail postings) are deleted before the withdrawal rows themselves, the same
-- "dependents before the row they depend on" ordering as every prior migration's down here.
DELETE FROM postings WHERE journal_entry_id IN (
    SELECT id FROM journal_entries WHERE cause_type IN ('withdrawal_settlement', 'withdrawal_failure')
);
DELETE FROM journal_entries WHERE cause_type IN ('withdrawal_settlement', 'withdrawal_failure');
DELETE FROM broadcast_attempts WHERE withdrawal_id IN (SELECT id FROM withdrawals WHERE status IN ('signed', 'broadcast', 'confirmed', 'failed'));
DELETE FROM withdrawals WHERE status IN ('signed', 'broadcast', 'confirmed', 'failed');
ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_status_check;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_status_check CHECK (status IN ('created', 'awaiting-approval', 'approved'));

DELETE FROM postings WHERE account_id IN (SELECT id FROM accounts WHERE account_type = 'treasury');
DELETE FROM accounts WHERE account_type = 'treasury';
DROP INDEX idx_accounts_platform_chain_asset_type;
CREATE UNIQUE INDEX idx_accounts_platform_chain_asset ON accounts (chain, asset) WHERE customer_id IS NULL;
ALTER TABLE accounts DROP CONSTRAINT accounts_account_type_check;
ALTER TABLE accounts ADD CONSTRAINT accounts_account_type_check CHECK (account_type IN ('available', 'hold'));

DROP TABLE broadcast_attempts;
DROP TABLE chain_nonce_state;
