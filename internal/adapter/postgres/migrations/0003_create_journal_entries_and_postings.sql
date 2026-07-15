-- +goose Up
CREATE TABLE journal_entries (
    id          uuid PRIMARY KEY,
    cause_type  text NOT NULL,
    cause_id    text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cause_type, cause_id)
);
-- cause_type/cause_id: e.g. ('internal_transfer', <idempotency key>) starting Story 1.3.
-- The unique constraint is AD-3's enforcement that the same cause can never produce two
-- journal entries — a database guarantee, not an application-level check.

CREATE TABLE postings (
    id                uuid PRIMARY KEY,
    journal_entry_id  uuid NOT NULL REFERENCES journal_entries(id),
    account_id        uuid NOT NULL REFERENCES accounts(id),
    amount            NUMERIC(78,0) NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_postings_account_id ON postings (account_id);
-- amount is signed: positive increases the account, negative decreases it. A balanced
-- journal entry's postings sum to zero across the accounts it touches (AD-3) — enforced
-- by the writer's application logic starting Story 1.3, not a DB constraint here.
-- No rows are written by this story (1.2) — these tables exist so the balances query
-- (SUM(postings.amount) per account) is real SQL against real, empty tables rather than
-- a special-cased "table doesn't exist yet" branch. Story 1.3 is the first writer.

-- +goose Down
DROP TABLE postings;
DROP TABLE journal_entries;
