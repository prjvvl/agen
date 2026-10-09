// Package store is the platform's durable state (docs/architecture.md §12):
// definitions, deployments, nests, assignments, instances, tasks, trigger
// events, approvals, tokens and leases. One implementation over database/sql
// serves SQLite (local) and Postgres (distributed); both run the same SQL with
// `$N` placeholders used in order, and the same migrations as the Rust engine
// (spec/sql, copied into ./migrations by scripts/gen.sh).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver "pgx"
	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // sqlite driver "sqlite" (pure Go)
)

//go:embed migrations
var migrations embed.FS

// Postgres advisory lock guarding migrations; must match the Rust engine.
const migrationLockID = 1634166126

// Dialect of the underlying database.
type Dialect int

const (
	SQLite Dialect = iota
	Postgres
)

var (
	// ErrNotFound means the row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict means a uniqueness rule rejected the write.
	ErrConflict = errors.New("store: conflict")
	// ErrFenced means the caller's lease/epoch is no longer current.
	ErrFenced = errors.New("store: fenced (lease or epoch is stale)")
	// ErrForbidden means the caller may not perform the action.
	ErrForbidden = errors.New("store: forbidden")
)

// Store is safe for concurrent use.
type Store struct {
	db      *sql.DB
	dialect Dialect
	// leader is the lease row that fences assignment writes.
	leader string
	// kek seals the Hub's secrets in the Store (SetKEK); prevKEK only opens
	// values sealed before a rotation (SetPreviousKEK).
	kek, prevKEK []byte
}

// WithLeaderLease uses a different leader lease name (tests sharing a database).
func (s *Store) WithLeaderLease(name string) *Store {
	c := *s
	c.leader = name
	return &c
}

// LeaderLeaseName is the lease that fences scheduler writes.
func (s *Store) LeaderLeaseName() string { return s.leader }

// NowMs is the current time in unix milliseconds. Tests may override it.
var NowMs = func() int64 { return time.Now().UnixMilli() }

// NewID returns a new ULID string.
func NewID() string { return ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String() }

// Open connects to `sqlite:<path>` (or `sqlite::memory:`) or `postgres://…`
// and applies pending migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	var s *Store
	switch {
	case strings.HasPrefix(url, "sqlite:"):
		p := strings.TrimPrefix(strings.TrimPrefix(url, "sqlite:"), "//")
		// Write transactions take the lock up front (_txlock=immediate): a deferred
		// transaction that upgrades from read to write fails with SQLITE_BUSY
		// instead of waiting when another writer is active.
		dsn := p + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
		if p == ":memory:" {
			dsn = "file::memory:?cache=shared&_pragma=busy_timeout(10000)"
		}
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		if p == ":memory:" {
			db.SetMaxOpenConns(1)
		}
		s = &Store{db: db, dialect: SQLite, leader: LeaderLease}
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		db, err := sql.Open("pgx", url)
		if err != nil {
			return nil, err
		}
		s = &Store{db: db, dialect: Postgres, leader: LeaderLease}
	default:
		return nil, fmt.Errorf("store: unsupported url %q", url)
	}
	if err := s.db.PingContext(ctx); err != nil {
		s.db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		s.db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Dialect reports the database kind.
func (s *Store) Dialect() Dialect { return s.dialect }

// DB exposes the handle for read-only fleet queries of engine tables.
func (s *Store) DB() *sql.DB { return s.db }

type migration struct {
	version int64
	sql     string
}

func (s *Store) migrationsFor() ([]migration, error) {
	dir := "migrations/sqlite"
	if s.dialect == Postgres {
		dir = "migrations/postgres"
	}
	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		n, err := strconv.ParseInt(strings.SplitN(name, "_", 2)[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad migration name %s", name)
		}
		b, err := migrations.ReadFile(path.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{n, string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (s *Store) migrate(ctx context.Context) (err error) {
	ms, err := s.migrationsFor()
	if err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if s.dialect == Postgres {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("SELECT pg_advisory_lock(%d)", migrationLockID)); err != nil {
			return err
		}
		defer conn.ExecContext(context.Background(), fmt.Sprintf("SELECT pg_advisory_unlock(%d)", migrationLockID)) //nolint:errcheck
	} else {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				conn.ExecContext(context.Background(), "ROLLBACK") //nolint:errcheck
			} else {
				_, err = conn.ExecContext(ctx, "COMMIT")
			}
		}()
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, applied_ms BIGINT NOT NULL)"); err != nil {
		return err
	}
	applied := map[int64]bool{}
	rows, err := conn.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	for _, m := range ms {
		if applied[m.version] {
			continue
		}
		if s.dialect == Postgres {
			if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
				return err
			}
		}
		for _, stmt := range strings.Split(m.sql, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				if s.dialect == Postgres {
					conn.ExecContext(context.Background(), "ROLLBACK") //nolint:errcheck
				}
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_ms) VALUES ($1, $2)", m.version, NowMs()); err != nil {
			return err
		}
		if s.dialect == Postgres {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
		}
	}
	return nil
}

// isUnique reports whether err is a uniqueness violation on either backend.
func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") || strings.Contains(m, "SQLSTATE 23505") ||
		strings.Contains(m, "duplicate key value")
}

// inTx runs f in a transaction.
func (s *Store) inTx(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	return tx.Commit()
}
