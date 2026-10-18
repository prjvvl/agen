// Package sqltools is a read-only SQL MCP server (stdio) for analyst agents:
// list tables, describe one, run a query. Postgres queries run in a READ ONLY
// transaction with a statement timeout; SQLite databases open read-only. The
// connection string comes from an environment variable (a bundle secret), so
// it never appears on a command line.
package sqltools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver "pgx"
	_ "modernc.org/sqlite"             // sqlite driver "sqlite"

	"github.com/prjvvl/agen/platform/internal/mcpstdio"
)

const (
	defaultRows = 100
	maxRows     = 1000
	maxCell     = 300
	maxOutput   = 30_000
)

// Server answers read-only queries on one database.
type Server struct {
	db       *sql.DB
	postgres bool
	Timeout  time.Duration
}

// Open connects read-only: postgres:// or postgresql:// URLs, otherwise a
// SQLite file path (optionally "sqlite:" prefixed).
func Open(dsn string) (*Server, error) {
	if dsn == "" {
		return nil, errors.New("no connection string")
	}
	s := &Server{Timeout: 30 * time.Second}
	var err error
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		s.postgres = true
		s.db, err = sql.Open("pgx", dsn)
	default:
		path := strings.TrimPrefix(dsn, "sqlite:")
		s.db, err = sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=query_only(1)")
	}
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("cannot connect: %v", err)
	}
	return s, nil
}

// Close closes the connection pool.
func (s *Server) Close() error { return s.db.Close() }

func tool(name, desc string, schema map[string]any) mcpstdio.Tool {
	return mcpstdio.Tool{Name: name, Description: desc, InputSchema: schema, Annotations: mcpstdio.ReadOnly}
}

// Tools lists the SQL tools.
func (s *Server) Tools() []mcpstdio.Tool {
	dialect := "SQLite"
	if s.postgres {
		dialect = "PostgreSQL"
	}
	return []mcpstdio.Tool{
		tool("tables", "List the tables and views (with schema, for "+dialect+").", mcpstdio.Object(map[string]any{})),
		tool("describe", "Columns of a table: name, type, nullable.", mcpstdio.Object(map[string]any{"table": mcpstdio.Str("Table name (schema.table allowed)")}, "table")),
		tool("query", "Run one read-only "+dialect+" query (SELECT / WITH). Returns up to limit rows as a table; writes are refused.",
			mcpstdio.Object(map[string]any{"sql": mcpstdio.Str("The query"), "limit": mcpstdio.Int(fmt.Sprintf("Rows to return (default %d, at most %d)", defaultRows, maxRows))}, "sql")),
	}
}

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// Call runs one tool.
func (s *Server) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var a struct {
		Table string `json:"table"`
		SQL   string `json:"sql"`
		Limit int    `json:"limit"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return "", fmt.Errorf("arguments: %v", err)
		}
	}
	switch name {
	case "tables":
		if s.postgres {
			return s.run(ctx, "SELECT table_schema, table_name, table_type FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog', 'information_schema') ORDER BY 1, 2", maxRows)
		}
		return s.run(ctx, "SELECT name, type FROM sqlite_master WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY name", maxRows)
	case "describe":
		if !ident.MatchString(a.Table) {
			return "", fmt.Errorf("table %q: use a plain name or schema.name", a.Table)
		}
		if s.postgres {
			schema, table := "public", a.Table
			if i := strings.IndexByte(a.Table, '.'); i >= 0 {
				schema, table = a.Table[:i], a.Table[i+1:]
			}
			return s.run(ctx, "SELECT column_name, data_type, is_nullable FROM information_schema.columns WHERE table_schema = '"+schema+"' AND table_name = '"+table+"' ORDER BY ordinal_position", maxRows)
		}
		return s.run(ctx, "SELECT name, type, CASE WHEN \"notnull\" = 1 THEN 'NO' ELSE 'YES' END AS nullable FROM pragma_table_info('"+a.Table+"')", maxRows)
	case "query":
		if strings.TrimSpace(a.SQL) == "" {
			return "", errors.New("sql is empty")
		}
		limit := a.Limit
		if limit <= 0 {
			limit = defaultRows
		}
		return s.run(ctx, a.SQL, min(limit, maxRows))
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// run executes one query read-only and formats the rows as a table.
func (s *Server) run(ctx context.Context, q string, limit int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // read-only: nothing to keep
	if s.postgres {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", s.Timeout.Milliseconds())); err != nil {
			return "", err
		}
	}
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(strings.Join(cols, "\t") + "\n")
	n, more := 0, false
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if n == limit {
			more = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		cells := make([]string, len(cols))
		for i, v := range vals {
			c := cell(v)
			if len(c) > maxCell {
				c = c[:maxCell] + "…"
			}
			cells[i] = c
		}
		b.WriteString(strings.Join(cells, "\t") + "\n")
		n++
		if b.Len() > maxOutput {
			more = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if more {
		fmt.Fprintf(&b, "… more rows; showing %d (add a WHERE, an aggregate or a LIMIT)\n", n)
	} else {
		fmt.Fprintf(&b, "(%d rows)\n", n)
	}
	return b.String(), nil
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return strings.NewReplacer("\t", " ", "\n", " ").Replace(fmt.Sprint(x))
	}
}

// Serve answers MCP requests on r with responses on w until r ends.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return mcpstdio.Serve(ctx, s, "agen-sql", "Read-only SQL: list tables, describe one, query.", r, w)
}
