-- +goose Up
CREATE TABLE idempotency_keys (
    key                    text PRIMARY KEY,
    request_hash           bytea NOT NULL,
    response_status        int NOT NULL,
    response_body          bytea NOT NULL,
    response_content_type  text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);
-- response_content_type exists so a replayed response can restore its original
-- Content-Type header (e.g. application/json vs application/problem+json) — discovered
-- necessary during implementation: without it, a replayed response would silently lose
-- its header even though the body stays byte-exact (AC2 talks about the body, but a
-- response with the wrong Content-Type isn't really "the original response, byte-for-byte").
-- response_body is bytea, NOT jsonb: jsonb doesn't round-trip byte-for-byte (no guaranteed key
-- order/whitespace/numeric formatting preservation) and AC2 requires literal byte fidelity on replay.
-- No retention/cleanup policy for this table in v1 — unbounded growth is an accepted, deferred
-- concern, not an oversight.

-- +goose Down
DROP TABLE idempotency_keys;
