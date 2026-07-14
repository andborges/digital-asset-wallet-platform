package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andborges/digital-asset-wallet-platform/internal/core"
)

const pgUniqueViolation = "23505"

// IdempotencyStore implements core.IdempotencyStore against PostgreSQL.
type IdempotencyStore struct {
	pool *pgxpool.Pool
}

// NewIdempotencyStore constructs a core.IdempotencyStore backed by pool. Lookup reads
// directly from pool (no transaction needed for a plain read); Insert requires a
// transaction already open on ctx (AD-4), like every other repository in this package.
func NewIdempotencyStore(pool *pgxpool.Pool) *IdempotencyStore {
	return &IdempotencyStore{pool: pool}
}

func (s *IdempotencyStore) Lookup(ctx context.Context, key string) (core.StoredEntry, bool, error) {
	var entry core.StoredEntry
	err := s.pool.QueryRow(ctx,
		`SELECT request_hash, response_status, response_body, response_content_type FROM idempotency_keys WHERE key = $1`,
		key,
	).Scan(&entry.RequestHash, &entry.Response.Status, &entry.Response.Body, &entry.Response.ContentType)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.StoredEntry{}, false, nil
	}
	if err != nil {
		return core.StoredEntry{}, false, fmt.Errorf("lookup idempotency key: %w", err)
	}
	return entry, true, nil
}

func (s *IdempotencyStore) Insert(ctx context.Context, key string, requestHash []byte, resp core.StoredResponse) error {
	tx := txFromContext(ctx)

	_, err := tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, request_hash, response_status, response_body, response_content_type) VALUES ($1, $2, $3, $4, $5)`,
		key, requestHash, resp.Status, resp.Body, resp.ContentType,
	)
	if err == nil {
		return nil
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return core.ErrKeyConflict
	}
	return fmt.Errorf("insert idempotency key: %w", err)
}
