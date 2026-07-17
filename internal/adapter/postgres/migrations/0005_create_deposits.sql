-- +goose Up
CREATE TABLE deposits (
    id            uuid PRIMARY KEY,
    -- No customer_id FK by design (AD-8): the watcher's only attribution key is the
    -- address itself — customer_id is resolved at read time via a join against
    -- deposit_addresses, never looked up mid-scan (see deposit_reader.go).
    chain         text NOT NULL CHECK (chain IN ('base', 'arbitrum')),
    asset         text NOT NULL CHECK (asset IN ('eth', 'usdc')),
    address       text NOT NULL CHECK (address ~ '^0x[0-9a-fA-F]{40}$'),
    tx_hash       text NOT NULL,
    -- log_index = -1 is the native-ETH-transfer sentinel (never a real EVM log index):
    -- native transfers have no log to key on, so this lets both native and ERC-20
    -- transfers share one (chain, tx_hash, log_index) key.
    log_index     integer NOT NULL,
    -- amount > 0 (re-review 2026-07-17, Story 2.2): a zero or negative amount would let
    -- CreditFinalizedDeposits silently write a no-op-but-"credited" deposit, or reverse
    -- the debit/credit direction — defense in depth alongside that method's own runtime
    -- Sign() check.
    amount        NUMERIC(78,0) NOT NULL CHECK (amount > 0),
    block_number  bigint NOT NULL,
    -- CHECK constraints added re-review 2026-07-16, defense in depth alongside
    -- DepositReader's WHERE-state filter: nothing at the DB layer should be able to
    -- produce a chain/asset/state value the rest of the system can't handle.
    state         text NOT NULL CHECK (state IN ('observed', 'safe', 'finalized', 'orphaned', 'credited')),
    observed_at   timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (chain, tx_hash, log_index)
);
CREATE INDEX idx_deposits_address ON deposits (address);
-- The unique constraint is AD-5's enforcement that re-observing the same on-chain event
-- on a repoll is a no-op by construction — a database guarantee (INSERT ... ON CONFLICT
-- DO NOTHING), never an application-level existence check.

CREATE TABLE watcher_cursors (
    chain       text NOT NULL,
    -- tier is "observed" or "safe" (AD-5 groundwork) — one cursor per (chain, tier) so
    -- the observed-scan cursor and the safe-promotion cursor advance independently.
    tier        text NOT NULL,
    last_block  bigint NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, tier)
);

CREATE TABLE outbox_events (
    id          uuid PRIMARY KEY,
    -- Generic, not deposit-specific (AD-4, AD-13): event_type + jsonb payload is reused
    -- by Story 2.2's credit event and Epic 4's dispatcher without a schema change.
    event_type  text NOT NULL,
    payload     jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE outbox_events;
DROP TABLE watcher_cursors;
DROP TABLE deposits;
