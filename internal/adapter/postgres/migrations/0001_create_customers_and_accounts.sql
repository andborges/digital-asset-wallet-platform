-- +goose Up
CREATE TABLE customers (
    id          uuid PRIMARY KEY,
    created_at  timestamptz NOT NULL DEFAULT now()
);
-- No external_ref or other column beyond what AC1 requires (id + created_at) — nothing in
-- epics.md's FR1/AC1 asks for a caller-supplied reference; adding one here would be scope
-- invention. If a future story needs it, add it in that story's own migration.

CREATE TABLE accounts (
    id          uuid PRIMARY KEY,
    customer_id uuid NOT NULL REFERENCES customers(id),
    chain       text NOT NULL,
    asset       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (customer_id, chain, asset)
);
-- No balance column. Balances are derived from postings starting Story 1.3 (AD-3).
-- Note for Story 1.3: this table is (chain, asset) scoped per-account. Story 1.3's internal-transfer
-- AC in epics.md takes only (source, destination, asset, amount) with no chain parameter — if that
-- turns out to mean transfers are chain-agnostic (aggregate-across-chain), reconcile this schema then.
-- Flagging now so it's a known decision point in Story 1.3, not a surprise.

-- +goose Down
DROP TABLE accounts;
DROP TABLE customers;
