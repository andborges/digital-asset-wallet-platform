-- +goose Up
-- block_hash is the hash of the block deposit was observed in, captured at observation
-- time (Design Notes: "detection is a stored-hash comparison, not a depth heuristic").
-- Every poll, TrackDeposits.Execute re-checks every observed/safe deposit's stored
-- block_hash against the chain's CURRENT hash at that same height — a mismatch (or the
-- height no longer existing at all) is unambiguous proof the block was replaced by a
-- reorg, with no confirmation-count tuning needed or wanted.
--
-- Nullable, not NOT NULL (re-review 2026-07-17): a plain NOT NULL with no DEFAULT would
-- fail this ALTER outright against any deposits table that already has rows (any real
-- environment where Stories 2.1-2.3's watcher has already run) — Postgres can't retroactively
-- invent a historical block_hash for pre-existing rows, and this migration can't make RPC
-- calls to backfill one. RecordObserved always populates it for every newly-recorded
-- deposit from this story onward; checkForReorgs skips reorg-checking any row where it's
-- NULL (a legacy row whose historical hash was never captured has nothing to compare).
ALTER TABLE deposits ADD COLUMN block_hash text CHECK (block_hash IS NULL OR block_hash ~ '^0x[0-9a-fA-F]{64}$');

-- The existing plain UNIQUE(chain, tx_hash, log_index) constraint (migration 0005) is
-- named deposits_chain_tx_hash_log_index_key — Postgres's default naming convention for
-- an unnamed table-level UNIQUE constraint, confirmed empirically via \d deposits against
-- a real Postgres instance before writing this DROP (never guessed).
ALTER TABLE deposits DROP CONSTRAINT deposits_chain_tx_hash_log_index_key;

-- Replaces it with a partial unique index scoped to non-orphaned rows (Design Notes: "the
-- partial unique index is the crux of AC2"). A re-broadcast of the exact same signed
-- transaction after a reorg carries the identical tx_hash (Ethereum transaction hashes are
-- a function of signed content, not block context) — the old plain UNIQUE constraint would
-- have silently blocked that legitimate re-observation via ON CONFLICT DO NOTHING,
-- conflating "reappeared for real" with "already recorded." Scoping uniqueness to
-- WHERE state != 'orphaned' fixes this precisely: at most one ACTIVE record per event, but
-- an orphaned record no longer counts against a fresh one.
CREATE UNIQUE INDEX idx_deposits_active_chain_tx_hash_log_index ON deposits (chain, tx_hash, log_index) WHERE state != 'orphaned';

-- +goose Down
-- Re-adding the plain UNIQUE constraint below can only succeed if no (chain, tx_hash,
-- log_index) triple currently has more than one row — but that's exactly what AC2 makes a
-- legitimate, permanent outcome once a reorg-and-reappear has happened even once (proven
-- by TestReorgDetection_EndToEnd). Delete the orphaned half of any such colliding pair
-- first (re-review 2026-07-17) so the down-migration always succeeds rather than failing
-- mid-rollback on a duplicate-key violation. Down-migrations are dev/rollback tooling, not
-- a production path, so sacrificing an already-superseded orphaned audit row here is the
-- right tradeoff.
DELETE FROM deposits d
WHERE d.state = 'orphaned'
  AND EXISTS (
    SELECT 1 FROM deposits active
    WHERE active.chain = d.chain AND active.tx_hash = d.tx_hash AND active.log_index = d.log_index
      AND active.state != 'orphaned'
  );
DROP INDEX idx_deposits_active_chain_tx_hash_log_index;
ALTER TABLE deposits ADD CONSTRAINT deposits_chain_tx_hash_log_index_key UNIQUE (chain, tx_hash, log_index);
ALTER TABLE deposits DROP COLUMN block_hash;
