// Package pgsvc gives each service its own Postgres schema in the shared database.
package pgsvc

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

var schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// Open creates the schema if needed, migrates it and returns a pool whose search_path is that schema.
func Open(ctx context.Context, url, schema string, migrations fs.FS) (*pgxpool.Pool, error) {
	if !schemaName.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	if err := migrate(ctx, cfg.ConnConfig, schema, migrations); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func migrate(ctx context.Context, cfg *pgx.ConnConfig, schema string, migrations fs.FS) error {
	conn := stdlib.OpenDB(*cfg)
	defer func() { _ = conn.Close() }()

	// The schema name is validated above; identifiers cannot be bind parameters.
	_, err := conn.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize())
	var pgErr *pgconn.PgError
	// Concurrent IF NOT EXISTS can still lose the catalog race with a unique violation.
	if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "23505") {
		return fmt.Errorf("create schema: %w", err)
	}
	// Per-schema advisory lock so several instances of one service migrate one after another.
	h := fnv.New64a()
	h.Write([]byte("observatory.pgsvc." + schema))
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(int64(h.Sum64())), lock.WithLockTimeout(1, 120))
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, conn, migrations,
		goose.WithTableName(schema+".goose_db_version"), goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate %s: %w", schema, err)
	}
	return nil
}
