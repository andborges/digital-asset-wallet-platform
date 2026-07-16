-- +goose Up
CREATE TABLE deposit_addresses (
    customer_id uuid PRIMARY KEY REFERENCES customers(id),
    -- EIP-55 checksummed EVM address ("0x" + 40 hex chars). The CHECK pins the format
    -- (case-insensitive hex — checksum casing is the application's job); the address is
    -- chain-invariant by construction (CREATE2, AD-8), which is why there is no chain
    -- column on this table.
    address     text NOT NULL CHECK (address ~ '^0x[0-9a-fA-F]{40}$'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (address)
);
-- One row per customer (1:1, matching the architecture spine's ERD: CUSTOMER ||--||
-- DEPOSIT_ADDRESS) — the address is computed once at customer creation (AD-8) and never
-- re-derived, so this table has exactly one writer path: CustomerRepository.CreateCustomer,
-- in the same transaction as the customer and account inserts (AD-4).
-- The UNIQUE index on address is not read by anything in this story, but Epic 2's
-- watchers will look up a customer by address to attribute deposits (AD-8: "watchers
-- attribute deposits only via that table, never by re-deriving") — the address alone
-- identifies the customer because it is identical on every supported chain; the chain a
-- deposit arrived on is an attribute of the deposit event, not of the address. The index
-- needs to exist now so that lookup is O(1) from day one, not retrofitted later.

-- +goose Down
DROP TABLE deposit_addresses;
