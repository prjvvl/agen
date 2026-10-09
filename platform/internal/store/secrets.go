package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// PlatformSecret is a secret's metadata (never its value).
type PlatformSecret struct {
	Namespace, Name string
	UpdatedMs       int64
	// Deployments allowed to read it.
	Deployments []string
}

// SetPlatformSecret stores (or replaces) a namespace's secret, sealed with
// the KEK when one is set.
// The deployments allowed to read it are given separately: none by default.
func (s *Store) SetPlatformSecret(ctx context.Context, ns, name, value string, deployments ...string) error {
	sealed, err := s.seal(value)
	if err != nil {
		return err
	}
	if deployments == nil {
		deployments = []string{}
	}
	deps, _ := json.Marshal(deployments)
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO platform_secrets (namespace, name, value, updated_ms, deployments) VALUES ($1, $2, $3, $4, $5) "+
			"ON CONFLICT (namespace, name) DO UPDATE SET value = excluded.value, updated_ms = excluded.updated_ms, deployments = excluded.deployments",
		ns, name, sealed, NowMs(), string(deps))
	return err
}

// PlatformSecretDeployments returns the deployments allowed to read a secret.
func (s *Store) PlatformSecretDeployments(ctx context.Context, ns, name string) ([]string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT deployments FROM platform_secrets WHERE namespace = $1 AND name = $2", ns, name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var deps []string
	err = json.Unmarshal([]byte(raw), &deps)
	return deps, err
}

// GetPlatformSecret returns a secret's value.
func (s *Store) GetPlatformSecret(ctx context.Context, ns, name string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM platform_secrets WHERE namespace = $1 AND name = $2", ns, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return s.unseal(v)
}

// ListPlatformSecrets lists a namespace's secrets (names only).
func (s *Store) ListPlatformSecrets(ctx context.Context, ns string) ([]PlatformSecret, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT namespace, name, updated_ms, deployments FROM platform_secrets WHERE namespace = $1 ORDER BY name", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformSecret
	for rows.Next() {
		var p PlatformSecret
		var deps string
		if err := rows.Scan(&p.Namespace, &p.Name, &p.UpdatedMs, &deps); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(deps), &p.Deployments)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePlatformSecret removes a secret.
func (s *Store) DeletePlatformSecret(ctx context.Context, ns, name string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM platform_secrets WHERE namespace = $1 AND name = $2", ns, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
