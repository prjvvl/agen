package store

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// HostTables are the run-data tables agen-host (the engine) reads and
// writes. A host role gets these and nothing else: not the Hub's keys
// (hub_ca, hub_token_key), tokens, tasks or fleet state.
var HostTables = []string{"sessions", "conversations", "messages", "runs", "spans", "effects", "delegation_calls", "delegations", "logs"}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// scramVerifier computes a Postgres SCRAM-SHA-256 password verifier
// ("SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>", RFC 5802 /
// 7677), which CREATE/ALTER ROLE ... PASSWORD stores as is.
func scramVerifier(password string) (string, error) {
	const iterations = 4096
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(mac(salted, "Server Key"))), nil
}

// hostPolicies scope each host table by namespace (row-level security).
// Tables without a reliable namespace column follow their parent row; the
// subqueries are themselves filtered by the parent table's policy.
var hostPolicies = map[string]string{
	"sessions":         "namespace IN (%s)",
	"runs":             "namespace IN (%s)",
	"logs":             "namespace IN (%s)",
	"conversations":    "EXISTS (SELECT 1 FROM sessions p WHERE p.id = conversations.session_id)",
	"messages":         "EXISTS (SELECT 1 FROM conversations p WHERE p.id = messages.conversation_id)",
	"spans":            "EXISTS (SELECT 1 FROM runs p WHERE p.id = spans.run_id)",
	"effects":          "EXISTS (SELECT 1 FROM runs p WHERE p.id = effects.run_id)",
	"delegation_calls": "EXISTS (SELECT 1 FROM runs p WHERE p.id = delegation_calls.run_id)",
	"delegations":      "EXISTS (SELECT 1 FROM runs p WHERE p.id = delegations.root_run_id)",
}

var namespaceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// CreateHostRole creates (or updates) a Postgres login role limited to
// HostTables plus reading schema_migrations, for the Store DSN given to
// Nests and their hosts. With namespaces, row-level security limits the role
// to those namespaces' rows (reads and writes); without, it sees all
// namespaces. It needs a Store opened by the tables' owner (row-level
// security does not apply to the owner, so the Hub keeps seeing everything).
func (s *Store) CreateHostRole(ctx context.Context, role, password string, namespaces []string) error {
	if s.dialect != Postgres {
		return errors.New("host roles need Postgres (a SQLite store is a single local file)")
	}
	if !roleName.MatchString(role) {
		return fmt.Errorf("invalid role name %q (lowercase letters, digits, _)", role)
	}
	if len(password) < 16 {
		return errors.New("use a password of at least 16 characters")
	}
	// Send a SCRAM-SHA-256 verifier, not the password: statement logs
	// (log_statement) never see it.
	verifier, err := scramVerifier(password)
	if err != nil {
		return err
	}
	lit := "'" + verifier + "'"
	var exists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return err
	}
	stmts := []string{}
	if exists {
		stmts = append(stmts, "ALTER ROLE "+role+" LOGIN PASSWORD "+lit)
	} else {
		stmts = append(stmts, "CREATE ROLE "+role+" LOGIN PASSWORD "+lit)
	}
	stmts = append(stmts,
		"REVOKE ALL ON ALL TABLES IN SCHEMA public FROM "+role,
		"GRANT USAGE ON SCHEMA public TO "+role,
		"GRANT SELECT ON schema_migrations TO "+role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON "+strings.Join(HostTables, ", ")+" TO "+role,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO "+role,
	)
	quoted := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if !namespaceName.MatchString(ns) {
			return fmt.Errorf("invalid namespace %q", ns)
		}
		quoted = append(quoted, "'"+ns+"'")
	}
	policy := "agen_" + role
	for _, t := range HostTables {
		pred := "true"
		if len(quoted) > 0 {
			pred = hostPolicies[t]
			if strings.Contains(pred, "%s") {
				pred = fmt.Sprintf(pred, strings.Join(quoted, ", "))
			}
		}
		stmts = append(stmts,
			"ALTER TABLE "+t+" ENABLE ROW LEVEL SECURITY",
			"DROP POLICY IF EXISTS "+policy+" ON "+t,
			"CREATE POLICY "+policy+" ON "+t+" TO "+role+" USING ("+pred+") WITH CHECK ("+pred+")",
		)
	}
	// All or nothing: a failure part-way must not leave half the grants and
	// policies in place.
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				// Never echo the statement: it contains the password verifier.
				return fmt.Errorf("host role: %s: %w", strings.Fields(q)[0]+" "+strings.Fields(q)[1], err)
			}
		}
		return nil
	})
}
