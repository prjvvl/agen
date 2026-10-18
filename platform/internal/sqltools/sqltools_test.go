package sqltools

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(t *testing.T, s *Server, name string, args map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	return s.Call(context.Background(), name, b)
}

func check(t *testing.T, s *Server, table string) {
	t.Helper()
	out, err := call(t, s, "tables", nil)
	if err != nil || !strings.Contains(out, table) {
		t.Fatalf("tables: %q %v", out, err)
	}
	out, err = call(t, s, "describe", map[string]any{"table": table})
	if err != nil || !strings.Contains(out, "amount") {
		t.Fatalf("describe: %q %v", out, err)
	}
	out, err = call(t, s, "query", map[string]any{"sql": "SELECT region, SUM(amount) AS total FROM " + table + " GROUP BY region ORDER BY region"})
	if err != nil || !strings.Contains(out, "region\ttotal") || !strings.Contains(out, "eu\t30") || !strings.Contains(out, "(2 rows)") {
		t.Fatalf("query: %q %v", out, err)
	}
	if out, _ := call(t, s, "query", map[string]any{"sql": "SELECT * FROM " + table, "limit": 1}); !strings.Contains(out, "more rows; showing 1") {
		t.Fatalf("limit: %q", out)
	}
	for _, w := range []string{"DELETE FROM " + table, "INSERT INTO " + table + " (region, amount) VALUES ('x', 1)", "DROP TABLE " + table} {
		if _, err := call(t, s, "query", map[string]any{"sql": w}); err == nil {
			t.Fatalf("write allowed: %s", w)
		}
	}
	if _, err := call(t, s, "describe", map[string]any{"table": "x; DROP TABLE y"}); err == nil {
		t.Fatal("describe took a non-identifier")
	}
}

func TestSQLiteIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sales.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE sales (region TEXT, amount INTEGER)", "INSERT INTO sales VALUES ('eu', 10), ('eu', 20), ('us', 5)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(t, s, "sales")
}

func TestPostgresIsReadOnly(t *testing.T) {
	url := os.Getenv("AGEN_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AGEN_TEST_POSTGRES_URL not set")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{"DROP TABLE IF EXISTS sqltools_sales", "CREATE TABLE sqltools_sales (region TEXT, amount INTEGER)", "INSERT INTO sqltools_sales VALUES ('eu', 10), ('eu', 20), ('us', 5)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	defer db.Exec("DROP TABLE sqltools_sales") //nolint:errcheck
	s, err := Open(url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(t, s, "sqltools_sales")
}
