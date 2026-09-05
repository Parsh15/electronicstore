// Migrations. The three SQL files that define the database live in
// db/migrations and are compiled into the binary, so a fresh deployment needs
// no separate psql step: the backend brings its own schema.
//
// Each file is applied at most once, recorded in schema_migrations. Every file
// is itself idempotent (create ... if not exists / create or replace), so
// re-applying after a manual change is harmless.
package db

import (
	"context"
	"embed"
	"fmt"
	"log"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies any migration files not yet recorded. Safe to call on every
// boot and safe to call concurrently: an advisory lock serialises instances so
// two containers starting together cannot both run the same file.
func Migrate(ctx context.Context) error {
	conn, err := GetDB().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `select pg_advisory_lock(48271)`); err != nil {
		return fmt.Errorf("cannot take the migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `select pg_advisory_unlock(48271)`) }()

	if _, err := conn.Exec(ctx, `
		create table if not exists public.schema_migrations (
		  version    text primary key,
		  applied_at timestamptz not null default now())`); err != nil {
		return fmt.Errorf("cannot create schema_migrations: %w", err)
	}

	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 001_schema, 002_functions, 003_views — order matters

	applied := 0
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx,
			`select exists (select 1 from public.schema_migrations where version = $1)`,
			name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}

		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}

		// Each file runs as one implicit transaction via a single Exec, so a
		// failure halfway through leaves nothing behind.
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s failed: %w", name, err)
		}
		if _, err := conn.Exec(ctx,
			`insert into public.schema_migrations (version) values ($1)
			 on conflict (version) do nothing`, name); err != nil {
			return err
		}
		log.Printf("migrate: applied %s", name)
		applied++
	}

	if applied == 0 {
		log.Printf("migrate: schema already current (%d files)", len(names))
	}
	return nil
}

// SchemaReady reports whether the core tables exist — used by /api/health so a
// misconfigured deployment says so plainly instead of 500ing on every request.
func SchemaReady(ctx context.Context) (bool, error) {
	var n int
	err := GetDB().QueryRow(ctx, `
		select count(*) from information_schema.tables
		 where table_schema = 'public'
		   and table_name in ('profiles','sessions','components','projects','funds')`).Scan(&n)
	return n == 5, err
}
