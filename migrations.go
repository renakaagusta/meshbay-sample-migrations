package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationFiles ships the SQL files inside the binary, so the pre-deploy hook
// needs no extra files next to it.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

const (
	migrationTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version integer primary key,
	name text not null,
	applied_at timestamptz not null default now()
)`

	// migrationLock serialises concurrent pre-deploy hooks across replicas.
	migrationLock int64 = 7801330001
)

type migration struct {
	version   int
	name      string
	statement string
}

type schemaQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadMigrations() ([]migration, error) {
	entries, readError := migrationFiles.ReadDir("migrations")
	if readError != nil {
		return nil, fmt.Errorf("read migrations: %w", readError)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, found := strings.Cut(entry.Name(), "_")
		if !found {
			return nil, fmt.Errorf("migration %q must be named <version>_<name>.sql", entry.Name())
		}
		version, parseError := strconv.Atoi(prefix)
		if parseError != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q has no positive numeric version", entry.Name())
		}
		contents, contentsError := migrationFiles.ReadFile("migrations/" + entry.Name())
		if contentsError != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), contentsError)
		}
		migrations = append(migrations, migration{
			version:   version,
			name:      strings.TrimSuffix(entry.Name(), ".sql"),
			statement: string(contents),
		})
	}
	sort.Slice(migrations, func(left, right int) bool {
		return migrations[left].version < migrations[right].version
	})
	for index := 1; index < len(migrations); index++ {
		if migrations[index].version == migrations[index-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", migrations[index].version)
		}
	}
	return migrations, nil
}

// applyMigrations is what pre_deploy runs. Every migration is applied in its
// own transaction together with its schema_migrations row, so a failing
// migration leaves the database exactly as the previous release left it.
func applyMigrations(runContext context.Context, database *pgxpool.Pool) error {
	migrations, loadError := loadMigrations()
	if loadError != nil {
		return loadError
	}
	connection, acquireError := database.Acquire(runContext)
	if acquireError != nil {
		return fmt.Errorf("acquire connection: %w", acquireError)
	}
	defer connection.Release()

	if _, tableError := connection.Exec(runContext, migrationTable); tableError != nil {
		return fmt.Errorf("create schema_migrations: %w", tableError)
	}
	if _, lockError := connection.Exec(runContext, `SELECT pg_advisory_lock($1)`, migrationLock); lockError != nil {
		return fmt.Errorf("lock migrations: %w", lockError)
	}
	defer func() {
		if _, unlockError := connection.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLock); unlockError != nil {
			log.Printf("unlock migrations: %v", unlockError)
		}
	}()

	appliedVersions, versionError := appliedMigrations(runContext, connection)
	if versionError != nil {
		return versionError
	}
	applied := 0
	for _, current := range migrations {
		if _, already := appliedVersions[current.version]; already {
			log.Printf("migration %04d %s: already applied", current.version, current.name)
			continue
		}
		if migrateError := applyMigration(runContext, connection, current); migrateError != nil {
			return migrateError
		}
		applied++
	}
	version, versionError := readSchemaVersion(runContext, connection)
	if versionError != nil {
		return versionError
	}
	log.Printf("schema version: %d (applied %d migration(s))", version, applied)
	return nil
}

func applyMigration(runContext context.Context, connection *pgxpool.Conn, current migration) error {
	transaction, beginError := connection.Begin(runContext)
	if beginError != nil {
		return fmt.Errorf("begin migration %04d: %w", current.version, beginError)
	}
	if _, statementError := transaction.Exec(runContext, current.statement); statementError != nil {
		_ = transaction.Rollback(runContext)
		return fmt.Errorf("migration %04d %s failed and was rolled back: %w", current.version, current.name, statementError)
	}
	if _, recordError := transaction.Exec(
		runContext,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		current.version, current.name,
	); recordError != nil {
		_ = transaction.Rollback(runContext)
		return fmt.Errorf("record migration %04d: %w", current.version, recordError)
	}
	if commitError := transaction.Commit(runContext); commitError != nil {
		return fmt.Errorf("commit migration %04d: %w", current.version, commitError)
	}
	log.Printf("migration %04d %s: applied", current.version, current.name)
	return nil
}

func appliedMigrations(runContext context.Context, queryer schemaQueryer) (map[int]struct{}, error) {
	rows, queryError := queryer.Query(runContext, `SELECT version FROM schema_migrations`)
	if queryError != nil {
		return nil, fmt.Errorf("read applied migrations: %w", queryError)
	}
	defer rows.Close()
	applied := make(map[int]struct{})
	for rows.Next() {
		var version int
		if scanError := rows.Scan(&version); scanError != nil {
			return nil, fmt.Errorf("scan applied migration: %w", scanError)
		}
		applied[version] = struct{}{}
	}
	if rowsError := rows.Err(); rowsError != nil {
		return nil, fmt.Errorf("read applied migrations: %w", rowsError)
	}
	return applied, nil
}

// readSchemaVersion reports the highest applied migration. A database that has
// never been migrated has no schema_migrations table, which is schema 0 rather
// than an error: /health then shows that pre_deploy never ran.
func readSchemaVersion(runContext context.Context, queryer schemaQueryer) (int, error) {
	var version int
	queryError := queryer.QueryRow(runContext, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if queryError == nil {
		return version, nil
	}
	var postgresError *pgconn.PgError
	if errors.As(queryError, &postgresError) && postgresError.Code == "42P01" {
		return 0, nil
	}
	return 0, fmt.Errorf("read schema version: %w", queryError)
}
