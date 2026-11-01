package storage

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Произвольный ключ advisory lock, чтобы реплики не переносили версии одновременно.
const legacyMigrationLockKey = 0x626c696e64

// Migrate применяет все встроенные миграции, которых ещё нет в базе.
func (s *Store) Migrate(ctx context.Context) error {
	return s.WithMigrations(ctx, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// WithMigrations вызывает fn с провайдером goose: через него работает cmd/migrate.
func (s *Store) WithMigrations(ctx context.Context, fn func(*goose.Provider) error) error {
	dir, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	// Закрытие db не закрывает пул.
	db := stdlib.OpenDBFromPool(s.pool)
	p, err := goose.NewProvider(goose.DialectPostgres, db, dir, goose.WithSessionLocker(locker))
	if err != nil {
		db.Close()
		return fmt.Errorf("storage: migrations: %w", err)
	}
	defer p.Close()

	if err := s.adoptLegacyMigrations(ctx, p); err != nil {
		return fmt.Errorf("storage: adopt legacy migrations: %w", err)
	}
	return fn(p)
}

// adoptLegacyMigrations переносит версии из schema_migrations, которую вёл прежний
// механизм, в таблицу goose: иначе goose применил бы эти миграции повторно.
func (s *Store) adoptLegacyMigrations(ctx context.Context, p *goose.Provider) error {
	// GetDBVersion создаёт таблицу версий goose, если её ещё нет.
	if _, err := p.GetDBVersion(ctx); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, legacyMigrationLockKey); err != nil {
		return err
	}
	var legacy bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&legacy); err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO goose_db_version (version_id, is_applied, tstamp)
		SELECT substring(version FROM '^[0-9]+')::bigint, true, applied_at
		FROM schema_migrations ORDER BY version`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
