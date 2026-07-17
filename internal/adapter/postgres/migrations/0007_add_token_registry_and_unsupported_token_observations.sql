-- +goose Up
-- token_registry is genuinely data, not code (FR34, Story 2.3): classifying a Transfer
-- log's contract_address against this table at scan time is what lets a SECOND CONTRACT
-- ADDRESS for an asset this system already knows (e.g. a bridged/wrapped USDC variant at a
-- different address on the same chain) be added as a registry row alone — never a new
-- scanXTransfers function or a new hardcoded asset comparison in
-- internal/adapter/evm/scanner.go. Recognizing a genuinely NEW asset type still requires
-- extending core.Asset's closed enum regardless of this table (re-review 2026-07-17,
-- corrects an earlier overclaiming comment caught by review) — this registry's job is
-- "which contract maps to which already-known asset," not "invent new assets."
-- contract_address is CHECK-constrained to lowercase and stored lowercased by the write
-- side (postgres.TokenRegistry.UpsertToken), so a case-insensitive Ethereum address always
-- matches its own row regardless of checksum casing on either side of the comparison —
-- without this, a manually-inserted operator row using checksummed case would silently
-- produce a second row for the same real address (re-review 2026-07-17).
CREATE TABLE token_registry (
    chain            text NOT NULL CHECK (chain IN ('base', 'arbitrum')),
    contract_address text NOT NULL CHECK (contract_address ~ '^0x[0-9a-fA-F]{40}$' AND contract_address = lower(contract_address)),
    -- CHECK tightened to 'usdc' only (Design Notes) — mirrors Story 2.2's crediting_policy
    -- CHECK tightening: no code path exists yet for a hypothetical third ERC-20 asset, and
    -- native ETH has no contract address to register in the first place.
    asset            text NOT NULL CHECK (asset = 'usdc'),
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, contract_address)
);

-- unsupported_token_observations records a Transfer log landing on a known deposit
-- address from a contract NOT in token_registry — visible for operator triage, never
-- credited, never touching journal_entries/postings/deposits (FR11). Its own
-- UNIQUE(chain, tx_hash, log_index) is an independent guarantee from deposits' identical
-- constraint: the same log is classified exactly once, by construction of a single scan
-- pass, so it can appear in at most one of the two tables.
CREATE TABLE unsupported_token_observations (
    id               uuid PRIMARY KEY,
    chain            text NOT NULL CHECK (chain IN ('base', 'arbitrum')),
    address          text NOT NULL CHECK (address ~ '^0x[0-9a-fA-F]{40}$'),
    contract_address text NOT NULL CHECK (contract_address ~ '^0x[0-9a-fA-F]{40}$'),
    tx_hash          text NOT NULL,
    log_index        integer NOT NULL,
    amount           NUMERIC(78,0) NOT NULL CHECK (amount > 0),
    block_number     bigint NOT NULL,
    observed_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (chain, tx_hash, log_index)
);
-- Supports ListObservations' ORDER BY observed_at DESC LIMIT (re-review 2026-07-17): the
-- read endpoint is platform-wide and unpaginated (bounded only by a fixed LIMIT), so this
-- index keeps that query cheap as the table grows rather than requiring a full sort.
CREATE INDEX idx_unsupported_token_observations_observed_at ON unsupported_token_observations (observed_at DESC);

-- +goose Down
DROP TABLE unsupported_token_observations;
DROP TABLE token_registry;
