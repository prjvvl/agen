package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Definition is an immutable, content-addressed bundle version.
type Definition struct {
	Digest    string
	Name      string
	Files     map[string][]byte
	CreatedMs int64
}

// PutDefinition stores a definition; storing the same digest again is a no-op.
func (s *Store) PutDefinition(ctx context.Context, d Definition) error {
	files, err := json.Marshal(d.Files) // []byte values encode as base64
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO definitions (digest, name, files, created_ms) VALUES ($1, $2, $3, $4) ON CONFLICT (digest) DO NOTHING",
		d.Digest, d.Name, string(files), NowMs())
	return err
}

// GetDefinition returns a definition with its files.
func (s *Store) GetDefinition(ctx context.Context, digest string) (Definition, error) {
	var d Definition
	var files string
	err := s.db.QueryRowContext(ctx, "SELECT digest, name, files, created_ms FROM definitions WHERE digest = $1", digest).
		Scan(&d.Digest, &d.Name, &files, &d.CreatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal([]byte(files), &d.Files)
}

// ListDefinitions lists definitions (without files), newest first.
func (s *Store) ListDefinitions(ctx context.Context, name string) ([]Definition, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT digest, name, created_ms FROM definitions WHERE ($1 = '' OR name = $1) ORDER BY created_ms DESC", name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Definition
	for rows.Next() {
		var d Definition
		if err := rows.Scan(&d.Digest, &d.Name, &d.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Deployment is desired state for one agent in a namespace. Policy fields are
// opaque JSON owned by the Hub (scale, budget, limits, triggers, placement).
type Deployment struct {
	Namespace        string
	Name             string
	DefinitionDigest string
	Kind             string // singleton | pool | task
	Scale            json.RawMessage
	Budget           json.RawMessage
	Limits           json.RawMessage
	Triggers         json.RawMessage
	Placement        json.RawMessage
	Desired          int
	Paused           bool
	Generation       int64
	LastActivityMs   int64
	CreatedMs        int64
	UpdatedMs        int64
}

func orEmpty(j json.RawMessage, def string) string {
	if len(j) == 0 {
		return def
	}
	return string(j)
}

const deploymentCols = "namespace, name, definition_digest, kind, scale, budget, limits, triggers, placement, desired, generation, last_activity_ms, created_ms, updated_ms, paused"

func scanDeployment(r interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var scale, budget, limits, triggers, placement string
	var paused int
	err := r.Scan(&d.Namespace, &d.Name, &d.DefinitionDigest, &d.Kind, &scale, &budget, &limits, &triggers, &placement,
		&d.Desired, &d.Generation, &d.LastActivityMs, &d.CreatedMs, &d.UpdatedMs, &paused)
	d.Paused = paused != 0
	d.Scale, d.Budget, d.Limits, d.Triggers, d.Placement = json.RawMessage(scale), json.RawMessage(budget),
		json.RawMessage(limits), json.RawMessage(triggers), json.RawMessage(placement)
	return d, err
}

// CreateDeployment inserts a deployment; ErrConflict if it exists.
func (s *Store) CreateDeployment(ctx context.Context, d Deployment) (Deployment, error) {
	now := NowMs()
	_, err := s.db.ExecContext(ctx, "INSERT INTO deployments ("+deploymentCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1, $11, $12, $13, 0)",
		d.Namespace, d.Name, d.DefinitionDigest, d.Kind, orEmpty(d.Scale, "{}"), orEmpty(d.Budget, "{}"),
		orEmpty(d.Limits, "{}"), orEmpty(d.Triggers, "[]"), orEmpty(d.Placement, "{}"), d.Desired, now, now, now)
	if isUnique(err) {
		return d, ErrConflict
	}
	if err != nil {
		return d, err
	}
	return s.GetDeployment(ctx, d.Namespace, d.Name)
}

// GetDeployment returns one deployment.
func (s *Store) GetDeployment(ctx context.Context, ns, name string) (Deployment, error) {
	d, err := scanDeployment(s.db.QueryRowContext(ctx, "SELECT "+deploymentCols+" FROM deployments WHERE namespace = $1 AND name = $2", ns, name))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// ListDeployments lists deployments in a namespace ("" = all).
func (s *Store) ListDeployments(ctx context.Context, ns string) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+deploymentCols+" FROM deployments WHERE ($1 = '' OR namespace = $1) ORDER BY namespace, name", ns)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDeployment applies mutate to the current row and saves it, bumping the
// generation. Concurrent updates are serialised by an optimistic generation
// check: mutate runs again on the fresh row after a conflict.
func (s *Store) UpdateDeployment(ctx context.Context, ns, name string, mutate func(*Deployment) error) (Deployment, error) {
	return s.updateDeployment(ctx, 0, ns, name, mutate)
}

// UpdateDeploymentFenced is UpdateDeployment for the Hub leader: the write
// fails with ErrFenced unless leaderEpoch is the current leader lease epoch.
func (s *Store) UpdateDeploymentFenced(ctx context.Context, leaderEpoch int64, ns, name string, mutate func(*Deployment) error) (Deployment, error) {
	return s.updateDeployment(ctx, leaderEpoch, ns, name, mutate)
}

func (s *Store) updateDeployment(ctx context.Context, leaderEpoch int64, ns, name string, mutate func(*Deployment) error) (Deployment, error) {
	for attempt := 0; attempt < 5; attempt++ {
		d, err := s.GetDeployment(ctx, ns, name)
		if err != nil {
			return d, err
		}
		gen := d.Generation
		if err := mutate(&d); err != nil {
			return d, err
		}
		paused := 0
		if d.Paused {
			paused = 1
		}
		var updated bool
		err = s.inTx(ctx, func(tx *sql.Tx) error {
			if leaderEpoch > 0 {
				if err := checkLeaderEpoch(ctx, tx, s.leader, leaderEpoch); err != nil {
					return err
				}
			}
			res, err := tx.ExecContext(ctx,
				"UPDATE deployments SET definition_digest = $1, kind = $2, scale = $3, budget = $4, limits = $5, triggers = $6, placement = $7, desired = $8, "+
					"paused = $9, last_activity_ms = $10, generation = generation + 1, updated_ms = $11 WHERE namespace = $12 AND name = $13 AND generation = $14",
				d.DefinitionDigest, d.Kind, orEmpty(d.Scale, "{}"), orEmpty(d.Budget, "{}"), orEmpty(d.Limits, "{}"),
				orEmpty(d.Triggers, "[]"), orEmpty(d.Placement, "{}"), d.Desired, paused, d.LastActivityMs, NowMs(), ns, name, gen)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			updated = n == 1
			return nil
		})
		if err != nil {
			return d, err
		}
		if updated {
			return s.GetDeployment(ctx, ns, name)
		}
	}
	return Deployment{}, ErrConflict
}

// SetDesired sets the desired instance count.
func (s *Store) SetDesired(ctx context.Context, ns, name string, desired int) (Deployment, error) {
	return s.UpdateDeployment(ctx, ns, name, func(d *Deployment) error { d.Desired = desired; return nil })
}

// TouchActivity records task/A2A activity (keeps a deployment awake).
func (s *Store) TouchActivity(ctx context.Context, ns, name string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE deployments SET last_activity_ms = $1 WHERE namespace = $2 AND name = $3", NowMs(), ns, name)
	return err
}

// DeleteDeployment removes a deployment, its assignments and queued tasks.
func (s *Store) DeleteDeployment(ctx context.Context, ns, name string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM deployments WHERE namespace = $1 AND name = $2", ns, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM assignments WHERE namespace = $1 AND deployment = $2", ns, name); err != nil {
			return err
		}
		// A re-created deployment of the same name must not accept old secrets.
		if _, err := tx.ExecContext(ctx, "DELETE FROM webhook_secrets WHERE namespace = $1 AND deployment = $2", ns, name); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE tasks SET state = 'cancelled', updated_ms = $1 WHERE namespace = $2 AND deployment = $3 AND state IN ('queued', 'leased', 'running')", NowMs(), ns, name)
		return err
	})
}
