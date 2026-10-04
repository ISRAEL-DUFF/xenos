// Package store owns Postgres access and schema migrations.
package store

import (
	"context"
	"database/sql"
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/israel-duff/xenos/internal/store/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	Pool *pgxpool.Pool
	Q    *db.Queries
}

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool, Q: db.New(pool)}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate applies all pending goose migrations.
func (s *Store) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(s.Pool)
	defer func(db *sql.DB) { _ = db.Close() }(db)
	return goose.UpContext(ctx, db, "migrations")
}
