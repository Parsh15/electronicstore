// Package db owns the pgx connection pool and the small set of query helpers
// every handler uses.
//
// Most endpoints return data the database already knows how to shape, so the
// helpers lean on Postgres' JSON functions: QueryJSON runs a query wrapped in
// json_agg and hands back raw JSON, which the handler writes straight to the
// response. That keeps ~130 endpoints free of hand-written struct scanning
// while every value still travels as a bound parameter.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var pool *pgxpool.Pool

// Connect opens the pool. Settings match the brief: 10 max, 2 warm, an hour
// lifetime, half an hour idle — comfortable for Supabase's connection limits.
func Connect(ctx context.Context, dsn string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("bad database DSN: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	cfg.ConnConfig.ConnectTimeout = 10 * time.Second

	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("cannot create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.Ping(pingCtx); err != nil {
		p.Close()
		return fmt.Errorf("cannot reach the database: %w", err)
	}
	pool = p
	return nil
}

// GetDB returns the pool. Panics if Connect was not called — a programming
// error, not a runtime condition.
func GetDB() *pgxpool.Pool {
	if pool == nil {
		panic("db.GetDB called before db.Connect")
	}
	return pool
}

func Close() {
	if pool != nil {
		pool.Close()
	}
}

func Stats() map[string]any {
	if pool == nil {
		return map[string]any{"connected": false}
	}
	s := pool.Stat()
	return map[string]any{
		"connected":       true,
		"totalConns":      s.TotalConns(),
		"idleConns":       s.IdleConns(),
		"acquiredConns":   s.AcquiredConns(),
		"maxConns":        s.MaxConns(),
		"acquireCount":    s.AcquireCount(),
		"emptyAcquires":   s.EmptyAcquireCount(),
		"canceledAcquire": s.CanceledAcquireCount(),
	}
}

// ---------------------------------------------------------------- JSON helpers

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, so every helper below
// works inside or outside a transaction.
type Querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// QueryJSON wraps sql in json_agg and returns a JSON array. Always returns at
// least "[]" so the frontend never has to guard against null.
func QueryJSON(ctx context.Context, q Querier, sql string, args ...any) ([]byte, error) {
	var out []byte
	wrapped := "select coalesce(json_agg(t), '[]'::json)::text from (" + sql + ") t"
	if err := q.QueryRow(ctx, wrapped, args...).Scan(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// QueryJSONRow returns a single row as a JSON object, or ErrNotFound.
func QueryJSONRow(ctx context.Context, q Querier, sql string, args ...any) ([]byte, error) {
	var out []byte
	wrapped := "select to_json(t)::text from (" + sql + ") t limit 1"
	err := q.QueryRow(ctx, wrapped, args...).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && out == nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ScalarJSON runs a query whose single column is already JSON (a stored
// function returning jsonb, for instance).
func ScalarJSON(ctx context.Context, q Querier, sql string, args ...any) ([]byte, error) {
	var out []byte
	if err := q.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if out == nil {
		out = []byte("null")
	}
	return out, nil
}

// Rows returns generic maps — used by the CSV and PDF writers, which need
// ordered column names as well as values.
func Rows(ctx context.Context, q Querier, sql string, args ...any) (cols []string, data [][]any, err error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for _, f := range rows.FieldDescriptions() {
		cols = append(cols, f.Name)
	}
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			return nil, nil, err
		}
		data = append(data, v)
	}
	return cols, data, rows.Err()
}

// InTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic. Every multi-step operation in this backend goes through
// here, so partial writes are structurally impossible.
func InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := GetDB().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------- errors

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Classify turns a Postgres error into something a handler can map to a status
// code and a message that is safe to show a user.
//
// Postgres' own messages are safe here because every one that reaches a client
// comes from a CHECK constraint or an explicit RAISE in functions.sql — both
// authored for this app. Internal errors are classified as generic instead.
func Classify(err error) (status int, message string) {
	if err == nil {
		return 200, ""
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		return 404, "Not found."
	}
	if errors.Is(err, ErrConflict) {
		return 409, "That conflicts with something already saved."
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505", "23514": // not_null / check violation
			return 400, cleanMessage(pg)
		case "23503": // foreign key
			return 409, "Something else still refers to this record."
		case "23502":
			return 400, "A required field was missing."
		case "23P01":
			return 409, "That overlaps an existing record."
		case "22P02", "22003", "22007":
			return 400, "One of the values was not in a valid format."
		case "P0001": // RAISE EXCEPTION from our own functions
			return 400, pg.Message
		case "40001", "40P01": // serialisation failure / deadlock
			return 409, "The store was busy. Please try again."
		case "42501":
			return 403, "The database refused that operation."
		}
		if pg.Code == "23505" {
			return 409, "That already exists."
		}
		if strings.HasPrefix(pg.Code, "23") {
			return 409, cleanMessage(pg)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504, "The database took too long to respond."
	}
	return 500, "Something went wrong. Please try again."
}

func cleanMessage(pg *pgconn.PgError) string {
	switch pg.ConstraintName {
	case "components_quantity_check":
		return "Quantity cannot go below zero."
	case "suppliers_name_key":
		return "A supplier with that name already exists."
	case "components_code_key":
		return "That component code is already in use."
	case "component_units_unit_id_key":
		return "That unit ID is already in use."
	case "project_parts_project_id_component_id_key":
		return "That component is already on this project's BOM."
	case "profiles_email_unique", "profiles_email_key":
		return "An account with that email already exists."
	case "comments_one_owner":
		return "A comment must belong to exactly one record."
	}
	if pg.Message != "" {
		return pg.Message
	}
	return "That value was not accepted."
}
