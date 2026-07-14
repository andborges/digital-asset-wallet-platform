package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate runs every pending goose migration embedded in this binary against pool.
// Migrations are embedded, never read from disk, per the Consistency Conventions
// table ("Migrations: goose, plain SQL, embedded").
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	// goose's Provider API takes a *sql.DB; pgxpool.Pool doesn't expose one directly,
	// so migrations run over a database/sql connection borrowed from the same pool via
	// pgx's stdlib driver, while application queries continue to use pgxpool.Pool.
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys)
	if err != nil {
		return fmt.Errorf("create goose provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}
