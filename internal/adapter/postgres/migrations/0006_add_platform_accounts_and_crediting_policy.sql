-- +goose Up
-- Platform accounts (Story 2.2) reuse the existing accounts/postings/journal_entries
-- tables rather than a parallel ledger: customer_id IS NULL is a sufficient discriminator
-- for the one platform account type that exists today (forwarder-float). postings.
-- account_id already references accounts(id) generically, and TransactionRepository's
-- customer-scoped query (WHERE a.customer_id = $1) already excludes a NULL-customer_id
-- row for free — no new join logic needed anywhere that reads the ledger.
ALTER TABLE accounts ALTER COLUMN customer_id DROP NOT NULL;

-- Exactly one forwarder-float account per (chain, asset) — this partial unique index is
-- what CreditFinalizedDeposits' join (`float.customer_id IS NULL AND float.chain = ...
-- AND float.asset = ...`) relies on to resolve unambiguously to a single row. Ordinary
-- per-customer accounts are untouched: their own uniqueness is still the existing
-- UNIQUE (customer_id, chain, asset).
CREATE UNIQUE INDEX idx_accounts_platform_chain_asset ON accounts (chain, asset) WHERE customer_id IS NULL;

INSERT INTO accounts (id, customer_id, chain, asset) VALUES
    (gen_random_uuid(), NULL, 'base', 'eth'),
    (gen_random_uuid(), NULL, 'base', 'usdc'),
    (gen_random_uuid(), NULL, 'arbitrum', 'eth'),
    (gen_random_uuid(), NULL, 'arbitrum', 'usdc');

-- crediting_policy is a data table, not a Go constant (FR9): CreditFinalizedDeposits
-- joins against it at query time, so changing a policy row is a config change, never a
-- code change. The CHECK is tightened to 'finalized' only (re-review 2026-07-17): no code
-- path credits at 'observed'/'safe' today, and pre-allowing those values in the schema
-- let an operator silently strand every deposit for a (chain, asset) pair with zero
-- signal. Whichever future story actually implements crediting at another tier extends
-- this CHECK in its own migration, alongside the code that acts on it.
CREATE TABLE crediting_policy (
    chain       text NOT NULL CHECK (chain IN ('base', 'arbitrum')),
    asset       text NOT NULL CHECK (asset IN ('eth', 'usdc')),
    credit_tier text NOT NULL CHECK (credit_tier = 'finalized'),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, asset)
);

INSERT INTO crediting_policy (chain, asset, credit_tier) VALUES
    ('base', 'eth', 'finalized'),
    ('base', 'usdc', 'finalized'),
    ('arbitrum', 'eth', 'finalized'),
    ('arbitrum', 'usdc', 'finalized');

-- +goose Down
-- Delete dependents before the platform accounts themselves (re-review 2026-07-17): once
-- CreditFinalizedDeposits has run even once, postings.account_id (NOT NULL, no ON DELETE
-- CASCADE) references a forwarder-float account, so DELETE FROM accounts below would
-- otherwise fail with a foreign-key violation in exactly the environment where this down
-- migration would be used.
DELETE FROM postings WHERE account_id IN (SELECT id FROM accounts WHERE customer_id IS NULL);
DELETE FROM journal_entries WHERE cause_type = 'deposit_credit';
DROP TABLE crediting_policy;
DELETE FROM accounts WHERE customer_id IS NULL;
DROP INDEX idx_accounts_platform_chain_asset;
ALTER TABLE accounts ALTER COLUMN customer_id SET NOT NULL;
