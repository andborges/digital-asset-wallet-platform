-- +goose Up
-- account_type distinguishes a customer's "available" balance from its "hold" balance for
-- the same (chain, asset) pair (Story 3.2): a hold account is a new sibling row in this
-- same table, not a separate schema — accounts already carries every column a hold
-- account needs, and postings/balance derivation is entirely generic over account_id
-- (Design Notes). Defaulting existing rows to 'available' preserves every account this
-- platform has ever provisioned (ordinary customer accounts and Story 2.2's platform
-- forwarder-float accounts alike) as exactly what they already are.
ALTER TABLE accounts ADD COLUMN account_type text NOT NULL DEFAULT 'available' CHECK (account_type IN ('available', 'hold'));

-- The existing plain UNIQUE(customer_id, chain, asset) constraint (migration 0001) is
-- named accounts_customer_id_chain_asset_key — Postgres's default naming convention for an
-- unnamed table-level UNIQUE constraint, confirmed empirically via \d accounts against a
-- real Postgres 18 instance before writing this DROP (never guessed) — mirrors migration
-- 0008's identical discipline for deposits' own unnamed UNIQUE constraint.
ALTER TABLE accounts DROP CONSTRAINT accounts_customer_id_chain_asset_key;
ALTER TABLE accounts ADD CONSTRAINT accounts_customer_id_chain_asset_account_type_key UNIQUE (customer_id, chain, asset, account_type);

-- Backfill one 'hold' row per existing CUSTOMER account (never a platform account:
-- customer_id IS NULL rows are Story 2.2's forwarder-float accounts, which never place a
-- withdrawal hold and have no sibling to backfill). Every pre-existing row selected here is,
-- at this point in the migration, account_type = 'available' by the column's own DEFAULT
-- applied just above — so this is exactly "one hold row per existing customer x
-- (chain, asset) pair," matching CreateCustomer's own available-plus-hold shape going
-- forward for every new customer.
INSERT INTO accounts (id, customer_id, chain, asset, account_type, created_at)
SELECT gen_random_uuid(), customer_id, chain, asset, 'hold', now()
FROM accounts
WHERE customer_id IS NOT NULL AND account_type = 'available';

-- withdrawals: one row per requested withdrawal. status starts and (for this story) stays
-- 'created' — CHECK (status = 'created'), tightened exactly like migration 0006's
-- crediting_policy precedent: no code path transitions a withdrawal to any other status
-- yet, so pre-allowing other values here would let a bug silently strand a withdrawal in
-- an unhandled state with zero signal. Stories 3.3-3.5 each extend this CHECK in their own
-- migration as they add the status value their own transition needs.
CREATE TABLE withdrawals (
    id                    uuid PRIMARY KEY,
    customer_id           uuid NOT NULL REFERENCES customers(id),
    chain                 text NOT NULL CHECK (chain IN ('base', 'arbitrum')),
    asset                 text NOT NULL CHECK (asset IN ('eth', 'usdc')),
    amount                NUMERIC(78,0) NOT NULL CHECK (amount > 0),
    destination_address   text NOT NULL CHECK (destination_address ~ '^0x[0-9a-fA-F]{40}$'),
    status                text NOT NULL CHECK (status = 'created'),
    -- The journal entry that placed this withdrawal's hold, stored directly rather than
    -- re-derived later from (cause_type, cause_id) string matching (Design Notes): every
    -- later story that needs "the journal entry that placed this withdrawal's hold" (e.g.
    -- releasing it on failure, Story 3.5) can join directly.
    hold_journal_entry_id uuid NOT NULL REFERENCES journal_entries(id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_withdrawals_customer_id ON withdrawals (customer_id);

-- +goose Down
DROP TABLE withdrawals;
-- Delete dependents before the hold accounts themselves (mirrors migration 0006's own
-- down-migration discipline): once even one withdrawal hold has been placed,
-- postings.account_id (NOT NULL, no ON DELETE CASCADE) references a hold account, so
-- DELETE FROM accounts below would otherwise fail with a foreign-key violation in exactly
-- the environment where this down migration would be used.
--
-- Deletes BOTH legs of every withdrawal_hold journal entry, not just the postings on hold
-- accounts (re-review, adversarial review): a hold's journal entry also posts to the
-- customer's own AVAILABLE account (the debit leg), which is never deleted by this
-- migration — filtering by hold-account membership alone left that posting dangling,
-- so the very next statement (DELETE FROM journal_entries WHERE cause_type =
-- 'withdrawal_hold') would fail with a foreign-key violation from postings.journal_entry_id
-- (NOT NULL, no ON DELETE CASCADE, migration 0003) referencing the surviving row.
DELETE FROM postings WHERE journal_entry_id IN (SELECT id FROM journal_entries WHERE cause_type = 'withdrawal_hold');
DELETE FROM journal_entries WHERE cause_type = 'withdrawal_hold';
DELETE FROM accounts WHERE account_type = 'hold';
ALTER TABLE accounts DROP CONSTRAINT accounts_customer_id_chain_asset_account_type_key;
ALTER TABLE accounts ADD CONSTRAINT accounts_customer_id_chain_asset_key UNIQUE (customer_id, chain, asset);
ALTER TABLE accounts DROP COLUMN account_type;
