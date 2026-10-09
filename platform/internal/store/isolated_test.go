package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
)

// isolatedPostgres opens a Store on a database of its own in the test
// Postgres (created on first use), for tests that touch the Hub's one-row
// key tables, which other packages' tests share in the main test database.
// It returns nil without AGEN_TEST_POSTGRES_URL.
func isolatedPostgres(t *testing.T) *Store {
	t.Helper()
	admin := os.Getenv("AGEN_TEST_POSTGRES_URL")
	if admin == "" {
		return nil
	}
	const name = "agen_singletons"
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE DATABASE " + name); err != nil && !strings.Contains(err.Error(), "already exists") {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	s, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, q := range []string{"DELETE FROM hub_token_key", "DELETE FROM hub_ca", "DELETE FROM platform_secrets"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// eachIsolated runs f on a private SQLite Store and, when available, an
// isolated Postgres database.
func eachIsolated(t *testing.T, f func(t *testing.T, s *Store)) {
	t.Run("sqlite", func(t *testing.T) {
		for name, s := range backends(t) {
			if name == "sqlite" {
				f(t, s)
			}
		}
	})
	if pg := isolatedPostgres(t); pg != nil {
		t.Run("postgres", func(t *testing.T) { f(t, pg) })
	} else if os.Getenv("AGEN_REQUIRE_PG") == "1" {
		t.Fatal("AGEN_REQUIRE_PG=1 but AGEN_TEST_POSTGRES_URL is unset")
	}
}
