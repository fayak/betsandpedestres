package dbinit

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations
var migrationsFS embed.FS

// EnsureDatabaseAndMigrate ensures `targetDB` exists (creating it using adminConn to the cluster),
// then connects to it and applies embedded SQL migrations in filename order.
// adminConn should be a connection string to a maintenance DB (usually "postgres") with rights to CREATE DATABASE.
// Example adminConn: "postgres://user:pass@host:5432/postgres?sslmode=disable"
func EnsureDatabaseAndMigrate(ctx context.Context, adminConn, targetDB, owner string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminConn)
	if err != nil {
		return fmt.Errorf("admin connect: %w", err)
	}
	defer admin.Close(ctx)

	var exists bool
	if err := admin.QueryRow(ctx,
		`select exists (select 1 from pg_database where datname = $1)`, targetDB,
	).Scan(&exists); err != nil {
		return fmt.Errorf("check database existence: %w", err)
	}

	if !exists {
		createStmt := fmt.Sprintf(`create database "%s"`, targetDB)
		if owner != "" {
			createStmt += fmt.Sprintf(` with owner "%s"`, owner)
		}
		if _, err := admin.Exec(ctx, createStmt); err != nil {
			return fmt.Errorf("create database %q: %w", targetDB, err)
		}
	}

	targetConn, err := replaceDBName(adminConn, targetDB)
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, targetConn)
	if err != nil {
		return fmt.Errorf("target connect: %w", err)
	}
	defer conn.Close(ctx)

	lockKey := int64(0x62657473) // 'bets' namespace
	if _, err := conn.Exec(ctx, `select pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(context.Background(), `select pg_advisory_unlock($1)`, lockKey)

	if _, err := conn.Exec(ctx, `
		create table if not exists schema_migrations (
			filename text primary key,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		var done bool
		if err := conn.QueryRow(ctx, `select exists (select 1 from schema_migrations where filename=$1)`, f).Scan(&done); err != nil {
			return fmt.Errorf("check applied %s: %w", f, err)
		}
		if done {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("exec migration %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `insert into schema_migrations (filename) values ($1)`, f); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", f, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", f, err)
		}
	}

	return nil
}

// RefreshBalancesMatView triggers a concurrent refresh of the cached balances MV.
// Call this from a maintenance job after bursts of ledger activity.
func RefreshBalancesMatView(ctx context.Context, targetConn string) error {
	conn, err := pgx.Connect(ctx, targetConn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)

	// Must be outside a transaction; pgx starts statements autocommit by default.
	if _, err := conn.Exec(ctx, `refresh materialized view concurrently user_balances_mv`); err != nil {
		return fmt.Errorf("refresh MV: %w", err)
	}
	return nil
}

// replaceDBName replaces the database name segment of a PostgreSQL URL.
// It assumes a URL of the form postgres://.../<dbname>?...
func replaceDBName(conn, db string) (string, error) {
	i := strings.LastIndex(conn, "/")
	if i < 0 {
		return "", errors.New("unexpected conn string format; expected '/' before db name")
	}
	j := strings.Index(conn[i+1:], "?")
	if j == -1 {
		return conn[:i+1] + db, nil
	}
	return conn[:i+1] + db + conn[i+1+j:], nil
}
