package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// CopyTables lists every table `agen migrate` copies, parents before
// children. Runtime state that the new fleet rebuilds (leases, assignments,
// instances, nests' enrolment is kept) is not copied.
var CopyTables = []string{
	"definitions", "deployments", "nests", "api_tokens", "join_tokens", "webhook_secrets",
	"sessions", "conversations", "messages", "runs", "spans", "effects", "delegations", "delegation_calls", "logs",
	"tasks", "approvals", "trigger_events", "platform_secrets",
}

// skipTables are deliberately not copied.
var skipTables = map[string]string{
	"schema_migrations": "created by the migrations",
	"leases":            "leader leases are re-acquired",
	"assignments":       "the scheduler recomputes them",
	"instances":         "Nests report their own",
	"hub_ca":            "a new deployment has its own CA; Nests enrol again",
	"hub_token_key":     "a new deployment signs call tokens with its own key (tokens are short-lived)",
}

// CopyReport is the number of rows copied per table.
type CopyReport map[string]int64

// CopyTo copies all durable state from s into dst (e.g. a local SQLite store
// into a new Postgres store for a distributed deployment). dst must be empty;
// both stores are migrated to the same schema version first (Open does it).
// Each table is copied in one transaction; on error dst may be partially
// filled and should be dropped.
func (s *Store) CopyTo(ctx context.Context, dst *Store) (CopyReport, error) {
	if err := s.checkTablesKnown(ctx); err != nil {
		return nil, err
	}
	for _, t := range CopyTables {
		var n int64
		if err := dst.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t).Scan(&n); err != nil {
			return nil, fmt.Errorf("target %s: %w", t, err)
		}
		if n > 0 {
			return nil, fmt.Errorf("target is not empty: table %s has %d rows", t, n)
		}
	}
	report := CopyReport{}
	for _, t := range CopyTables {
		n, err := copyTable(ctx, s.db, dst, t)
		if err != nil {
			return report, fmt.Errorf("copy %s: %w", t, err)
		}
		report[t] = n
	}
	if dst.dialect == Postgres {
		// Copied explicit ids leave the serial behind.
		if _, err := dst.db.ExecContext(ctx, "SELECT setval(pg_get_serial_sequence('logs', 'id'), COALESCE((SELECT MAX(id) FROM logs), 0) + 1, false)"); err != nil {
			return report, err
		}
	}
	return report, nil
}

// checkTablesKnown fails if the source has a table this code does not know
// how to handle (a new migration must be added to CopyTables or skipTables).
func (s *Store) checkTablesKnown(ctx context.Context) error {
	q := "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'"
	if s.dialect == Postgres {
		q = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'"
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	known := map[string]bool{}
	for _, t := range CopyTables {
		known[t] = true
	}
	var unknown []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return err
		}
		if !known[t] && skipTables[t] == "" {
			unknown = append(unknown, t)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return fmt.Errorf("don't know how to migrate table(s) %s", strings.Join(unknown, ", "))
	}
	return rows.Err()
}

func copyTable(ctx context.Context, src *sql.DB, dst *Store, table string) (int64, error) {
	rows, err := src.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	ph := make([]string, len(cols))
	for i := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	insert := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(ph, ", ") + ")"
	var n int64
	err = dst.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, insert)
		if err != nil {
			return err
		}
		defer stmt.Close()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok { // SQLite TEXT may scan as bytes
					vals[i] = string(b)
				}
			}
			if _, err := stmt.ExecContext(ctx, vals...); err != nil {
				return err
			}
			n++
		}
		return rows.Err()
	})
	return n, err
}
