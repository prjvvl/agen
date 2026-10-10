package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Nest is an enrolled execution environment.
type Nest struct {
	ID              string
	Name            string
	Backend         string
	Labels          map[string]string
	Capacity        int
	GatewayURL      string
	State           string // active | lost | draining
	CertFingerprint string
	LastHeartbeatMs int64
	CreatedMs       int64
}

const nestCols = "id, name, backend, labels, capacity, gateway_url, state, cert_fingerprint, last_heartbeat_ms, created_ms"

func scanNest(r interface{ Scan(...any) error }) (Nest, error) {
	var n Nest
	var labels string
	err := r.Scan(&n.ID, &n.Name, &n.Backend, &labels, &n.Capacity, &n.GatewayURL, &n.State, &n.CertFingerprint, &n.LastHeartbeatMs, &n.CreatedMs)
	if err == nil {
		err = json.Unmarshal([]byte(labels), &n.Labels)
	}
	return n, err
}

// UpsertNest registers a nest (or updates it on re-enrolment) as active.
func (s *Store) UpsertNest(ctx context.Context, n Nest) (Nest, error) {
	if n.ID == "" {
		n.ID = NewID()
	}
	labels, _ := json.Marshal(n.Labels)
	now := NowMs()
	_, err := s.db.ExecContext(ctx, "INSERT INTO nests ("+nestCols+") VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9) "+
		"ON CONFLICT (id) DO UPDATE SET name = excluded.name, backend = excluded.backend, labels = excluded.labels, capacity = excluded.capacity, "+
		"gateway_url = excluded.gateway_url, state = 'active', cert_fingerprint = excluded.cert_fingerprint, last_heartbeat_ms = excluded.last_heartbeat_ms",
		n.ID, n.Name, n.Backend, string(labels), n.Capacity, n.GatewayURL, n.CertFingerprint, now, now)
	if err != nil {
		return n, err
	}
	return s.GetNest(ctx, n.ID)
}

// SetNestCert records the fingerprint of a nest's client certificate.
func (s *Store) SetNestCert(ctx context.Context, id, fingerprint string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE nests SET cert_fingerprint = $1 WHERE id = $2", fingerprint, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// NestByCert returns the nest a client certificate was issued to.
func (s *Store) NestByCert(ctx context.Context, fingerprint string) (Nest, error) {
	if fingerprint == "" {
		return Nest{}, ErrNotFound
	}
	// The previous certificate stays valid until the renewed one is first
	// used (a lost renewal reply must not lock the Nest out). Revoked Nests
	// never match.
	n, err := scanNest(s.db.QueryRowContext(ctx, "SELECT "+nestCols+" FROM nests WHERE (cert_fingerprint = $1 OR prev_cert_fingerprint = $1) AND state <> 'revoked'",
		fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	if err == nil && n.CertFingerprint == fingerprint {
		// The current certificate is in use: retire the previous one.
		_, _ = s.db.ExecContext(ctx, "UPDATE nests SET prev_cert_fingerprint = '' WHERE id = $1 AND prev_cert_fingerprint <> ''", n.ID)
	}
	return n, err
}

// RotateNestCert installs a renewed certificate for a Nest authenticated by
// presented (its current or, after a lost reply, previous certificate). It
// fails for revoked Nests or when presented is neither.
func (s *Store) RotateNestCert(ctx context.Context, id, presented, next string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE nests SET cert_fingerprint = $1, prev_cert_fingerprint = $2 WHERE id = $3 AND state <> 'revoked' AND (cert_fingerprint = $2 OR prev_cert_fingerprint = $2)",
		next, presented, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// GetNest returns one nest.
func (s *Store) GetNest(ctx context.Context, id string) (Nest, error) {
	n, err := scanNest(s.db.QueryRowContext(ctx, "SELECT "+nestCols+" FROM nests WHERE id = $1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

// ListNests lists all nests.
func (s *Store) ListNests(ctx context.Context) ([]Nest, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+nestCols+" FROM nests ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Nest
	for rows.Next() {
		n, err := scanNest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Heartbeat records that a nest is alive (and revives a lost one).
func (s *Store) Heartbeat(ctx context.Context, id string, capacity int, gatewayURL string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE nests SET last_heartbeat_ms = $1, capacity = $2, gateway_url = $3, state = CASE WHEN state = 'lost' THEN 'active' ELSE state END WHERE id = $4",
		NowMs(), capacity, gatewayURL, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeNest cuts a Nest off: its bearer token is revoked, its certificate
// no longer maps to it, it is marked revoked (never revived by a heartbeat)
// and its instances are dropped so the scheduler places them elsewhere.
func (s *Store) RevokeNest(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE nests SET state = 'revoked', cert_fingerprint = '', prev_cert_fingerprint = '' WHERE id = $1", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, "UPDATE api_tokens SET revoked = 1 WHERE name = $1", "nest:"+id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM instances WHERE nest_id = $1", id)
		return err
	})
}

// MarkLostNests marks active nests silent since before cutoffMs as lost and
// returns their ids.
func (s *Store) MarkLostNests(ctx context.Context, cutoffMs int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "UPDATE nests SET state = 'lost' WHERE state = 'active' AND last_heartbeat_ms < $1 RETURNING id", cutoffMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Assignment: run Count instances of a deployment in a nest.
type Assignment struct {
	Namespace        string
	Deployment       string
	NestID           string
	Count            int
	DefinitionDigest string
	Generation       int64
	UpdatedMs        int64
}

// SetAssignments replaces a deployment's assignments. Only the current Hub
// leader may write: epoch must match the leader lease, else ErrFenced.
func (s *Store) SetAssignments(ctx context.Context, leaderEpoch int64, ns, deployment string, as []Assignment) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := checkLeaderEpoch(ctx, tx, s.leader, leaderEpoch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM assignments WHERE namespace = $1 AND deployment = $2", ns, deployment); err != nil {
			return err
		}
		now := NowMs()
		for _, a := range as {
			if a.Count <= 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO assignments (namespace, deployment, nest_id, count, definition_digest, generation, updated_ms) VALUES ($1, $2, $3, $4, $5, $6, $7)",
				ns, deployment, a.NestID, a.Count, a.DefinitionDigest, a.Generation, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func scanAssignments(rows *sql.Rows) ([]Assignment, error) {
	defer rows.Close()
	var out []Assignment
	for rows.Next() {
		var a Assignment
		if err := rows.Scan(&a.Namespace, &a.Deployment, &a.NestID, &a.Count, &a.DefinitionDigest, &a.Generation, &a.UpdatedMs); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const assignmentCols = "namespace, deployment, nest_id, count, definition_digest, generation, updated_ms"

// AssignmentsForNest lists a nest's assignments.
func (s *Store) AssignmentsForNest(ctx context.Context, nestID string) ([]Assignment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+assignmentCols+" FROM assignments WHERE nest_id = $1 ORDER BY namespace, deployment", nestID)
	if err != nil {
		return nil, err
	}
	return scanAssignments(rows)
}

// AllAssignments lists every assignment.
func (s *Store) AllAssignments(ctx context.Context) ([]Assignment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+assignmentCols+" FROM assignments ORDER BY namespace, deployment, nest_id")
	if err != nil {
		return nil, err
	}
	return scanAssignments(rows)
}

// Instance is one running agent-host process, as reported by its nest.
type Instance struct {
	ID               string
	Namespace        string
	Deployment       string
	NestID           string
	DefinitionDigest string
	State            string // starting | ready | busy | draining | stopped | failed
	Endpoint         string
	RunningTasks     int
	Message          string
	StartedMs        int64
	LastSeenMs       int64
	Tools            InstanceTools
}

// InstanceTools is what an instance reported loading.
type InstanceTools struct {
	Tools   []string     `json:"tools,omitempty"`
	Servers []ToolServer `json:"servers,omitempty"`
}

// ToolServer is the state of one of an instance's tool servers.
type ToolServer struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	ToolCount int    `json:"toolCount"`
}

const instanceCols = "id, namespace, deployment, nest_id, definition_digest, state, endpoint, running_tasks, message, started_ms, last_seen_ms, tools"

// ReportInstances replaces a nest's instance set with what it reports, and
// logs instances that started, failed or went away.
func (s *Store) ReportInstances(ctx context.Context, nestID string, list []Instance) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		before := map[string]Instance{}
		rows, err := tx.QueryContext(ctx, "SELECT id, namespace, deployment, state, message FROM instances WHERE nest_id = $1", nestID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var in Instance
			if err := rows.Scan(&in.ID, &in.Namespace, &in.Deployment, &in.State, &in.Message); err != nil {
				rows.Close()
				return err
			}
			before[in.ID] = in
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, in := range list {
			prev, known := before[in.ID]
			delete(before, in.ID)
			msg, level := "", "info"
			switch {
			case !known:
				msg = "instance " + in.ID + " started on nest " + nestID
			case (in.State == "failed" || in.State == "stopped") && prev.State != in.State:
				msg = "instance " + in.ID + " " + in.State
				if in.State == "failed" {
					level = "error"
				}
				if in.Message != "" {
					msg += ": " + in.Message
				}
			default:
				continue
			}
			if err := appendLog(ctx, tx, in.ID, in.Namespace, in.Deployment, level, msg); err != nil {
				return err
			}
		}
		for _, gone := range before {
			if err := appendLog(ctx, tx, gone.ID, gone.Namespace, gone.Deployment, "info", "instance "+gone.ID+" exited (no longer reported by nest "+nestID+")"); err != nil {
				return err
			}
		}
		ids := make([]any, 0, len(list)+1)
		ids = append(ids, nestID)
		ph := []string{}
		now := NowMs()
		for i, in := range list {
			ph = append(ph, "$"+itoa(i+2))
			ids = append(ids, in.ID)
			if in.StartedMs == 0 {
				in.StartedMs = now
			}
			tools, err := json.Marshal(in.Tools)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO instances ("+instanceCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) "+
				"ON CONFLICT (id) DO UPDATE SET state = excluded.state, endpoint = excluded.endpoint, running_tasks = excluded.running_tasks, "+
				"message = excluded.message, definition_digest = excluded.definition_digest, last_seen_ms = excluded.last_seen_ms, tools = excluded.tools "+
				// A nest can only update its own instances.
				"WHERE instances.nest_id = excluded.nest_id",
				in.ID, in.Namespace, in.Deployment, nestID, in.DefinitionDigest, in.State, in.Endpoint, in.RunningTasks, in.Message, in.StartedMs, now, string(tools)); err != nil {
				return err
			}
		}
		q := "DELETE FROM instances WHERE nest_id = $1"
		if len(ph) > 0 {
			q += " AND id NOT IN (" + strings.Join(ph, ", ") + ")"
		}
		_, err = tx.ExecContext(ctx, q, ids...)
		return err
	})
}

// ListInstances filters by namespace, deployment and nest ("" = any).
func (s *Store) ListInstances(ctx context.Context, ns, deployment, nestID string) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+instanceCols+" FROM instances WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR deployment = $2) AND ($3 = '' OR nest_id = $3) ORDER BY namespace, deployment, id",
		ns, deployment, nestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		var in Instance
		var tools string
		if err := rows.Scan(&in.ID, &in.Namespace, &in.Deployment, &in.NestID, &in.DefinitionDigest, &in.State, &in.Endpoint, &in.RunningTasks, &in.Message, &in.StartedMs, &in.LastSeenMs, &tools); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tools), &in.Tools)
		out = append(out, in)
	}
	return out, rows.Err()
}

// AppendLog writes a platform log line for a deployment.
func (s *Store) AppendLog(ctx context.Context, instanceID, ns, deployment, level, msg string) error {
	return appendLog(ctx, s.db, instanceID, ns, deployment, level, msg)
}

// appendLog writes a platform log line for a deployment (shown with the
// instances' own lines by GetLogs).
func appendLog(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, instanceID, ns, deployment, level, msg string) error {
	_, err := q.ExecContext(ctx, "INSERT INTO logs (instance_id, namespace, deployment, time_ms, level, message) VALUES ($1, $2, $3, $4, $5, $6)",
		instanceID, ns, deployment, NowMs(), level, msg)
	return err
}

// DeleteInstancesOfNest forgets every instance of a (lost) nest.
func (s *Store) DeleteInstancesOfNest(ctx context.Context, nestID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM instances WHERE nest_id = $1", nestID)
	return err
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return digits[i : i+1]
	}
	return itoa(i/10) + digits[i%10:i%10+1]
}
