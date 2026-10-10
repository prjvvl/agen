package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// ---- leases (Hub leader election) ----

// LeaderLease is the name of the Hub leader lease row.
const LeaderLease = "hub-leader"

// AcquireLease takes or renews a named lease for holder. A lease that expired
// is taken over with epoch+1; a live lease held by someone else returns
// ErrConflict. Returns the epoch to use for fenced writes.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttlMs int64) (int64, error) {
	var epoch int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := NowMs()
		var cur string
		var curEpoch, expires int64
		err := tx.QueryRowContext(ctx, "SELECT holder, epoch, expires_ms FROM leases WHERE name = $1", name).Scan(&cur, &curEpoch, &expires)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			epoch = 1
			_, err = tx.ExecContext(ctx, "INSERT INTO leases (name, holder, epoch, expires_ms) VALUES ($1, $2, 1, $3)", name, holder, now+ttlMs)
			if isUnique(err) {
				return ErrConflict
			}
			return err
		case err != nil:
			return err
		case cur == holder:
			epoch = curEpoch
		case expires >= now:
			return ErrConflict
		default:
			epoch = curEpoch + 1
		}
		// Guard on the observed epoch so two contenders cannot both win.
		res, err := tx.ExecContext(ctx, "UPDATE leases SET holder = $1, epoch = $2, expires_ms = $3 WHERE name = $4 AND epoch = $5",
			holder, epoch, now+ttlMs, name, curEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrConflict
		}
		return nil
	})
	return epoch, err
}

// ReleaseLease gives up a lease early (it expires immediately).
func (s *Store) ReleaseLease(ctx context.Context, name, holder string, epoch int64) error {
	res, err := s.db.ExecContext(ctx, "UPDATE leases SET expires_ms = 0 WHERE name = $1 AND holder = $2 AND epoch = $3", name, holder, epoch)
	return fenced(res, err)
}

func checkLeaderEpoch(ctx context.Context, tx *sql.Tx, lease string, epoch int64) error {
	var cur, expires int64
	err := tx.QueryRowContext(ctx, "SELECT epoch, expires_ms FROM leases WHERE name = $1", lease).Scan(&cur, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (cur != epoch || expires < NowMs())) {
		return ErrFenced
	}
	return err
}

// ---- approvals ----

// Approval is a durable "ask" permission request.
type Approval struct {
	ID          string
	Namespace   string
	Deployment  string
	RunID       string
	Tool        string
	Arguments   json.RawMessage // already redacted by the engine
	State       string          // pending | approved | denied | expired
	RequestedBy string
	DecidedBy   string
	CreatedMs   int64
	ExpiresMs   int64
	// DecidedMs is when it was approved, denied or expired (0 while pending).
	DecidedMs int64
	// TaskID is the task of the run that asked ("" if it is not a Hub task).
	TaskID string
}

const approvalCols = "id, namespace, deployment, run_id, tool, arguments, state, requested_by, decided_by, created_ms, expires_ms"

// approvalSelect is approvalCols plus derived columns, for reads.
const approvalSelect = approvalCols + ", decided_ms, COALESCE((SELECT r.task_id FROM runs r WHERE r.id = approvals.run_id), '')"

func scanApproval(r interface{ Scan(...any) error }) (Approval, error) {
	var a Approval
	var args string
	err := r.Scan(&a.ID, &a.Namespace, &a.Deployment, &a.RunID, &a.Tool, &args, &a.State, &a.RequestedBy, &a.DecidedBy, &a.CreatedMs, &a.ExpiresMs,
		&a.DecidedMs, &a.TaskID)
	a.Arguments = json.RawMessage(args)
	return a, err
}

// CreateApproval stores a pending approval that expires after ttlMs.
func (s *Store) CreateApproval(ctx context.Context, a Approval, ttlMs int64) (Approval, error) {
	if a.ID == "" {
		a.ID = NewID()
	}
	for attempt := 0; ; attempt++ {
		now := NowMs()
		_, err := s.db.ExecContext(ctx, "INSERT INTO approvals ("+approvalCols+") VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, '', $8, $9)",
			a.ID, a.Namespace, a.Deployment, a.RunID, a.Tool, orEmpty(a.Arguments, "{}"), a.RequestedBy, now, now+ttlMs)
		if !isUnique(err) {
			if err != nil {
				return a, err
			}
			return s.GetApproval(ctx, a.ID)
		}
		// The same ask is already pending (a concurrent request won)...
		prev, err := s.PendingApproval(ctx, a.RunID, a.Tool, json.RawMessage(orEmpty(a.Arguments, "{}")))
		// ...unless it expired just now: then this ask can be the new one.
		if !errors.Is(err, ErrNotFound) || attempt == 2 {
			return prev, err
		}
	}
}

// RunOwner returns the instance that currently owns a run.
func (s *Store) RunOwner(ctx context.Context, runID string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, "SELECT owner FROM runs WHERE id = $1", runID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return owner, err
}

// PendingApproval returns the pending approval a run already raised for the
// same tool and arguments (a resumed run asking again), or ErrNotFound.
func (s *Store) PendingApproval(ctx context.Context, runID, tool string, args json.RawMessage) (Approval, error) {
	if _, err := s.ExpireApprovals(ctx); err != nil {
		return Approval{}, err
	}
	a, err := scanApproval(s.db.QueryRowContext(ctx,
		"SELECT "+approvalSelect+" FROM approvals WHERE run_id = $1 AND tool = $2 AND arguments = $3 AND state = 'pending' ORDER BY created_ms LIMIT 1",
		runID, tool, string(args)))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// GetApproval returns one approval (expiring it first if due).
func (s *Store) GetApproval(ctx context.Context, id string) (Approval, error) {
	if _, err := s.ExpireApprovals(ctx); err != nil {
		return Approval{}, err
	}
	a, err := scanApproval(s.db.QueryRowContext(ctx, "SELECT "+approvalSelect+" FROM approvals WHERE id = $1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ListApprovals filters by namespace and state ("" = any), newest first.
func (s *Store) ListApprovals(ctx context.Context, ns, state string) ([]Approval, error) {
	if _, err := s.ExpireApprovals(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+approvalSelect+" FROM approvals WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR state = $2) ORDER BY created_ms DESC, id DESC", ns, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideApproval approves or denies a pending approval. The principal who
// caused the request may not decide it (ErrForbidden); a decided or expired
// approval cannot be changed (ErrConflict).
func (s *Store) DecideApproval(ctx context.Context, id string, approve bool, by string) (Approval, error) {
	a, err := s.GetApproval(ctx, id)
	if err != nil {
		return a, err
	}
	if by == "" || by == a.RequestedBy {
		return a, ErrForbidden
	}
	state := "denied"
	if approve {
		state = "approved"
	}
	now := NowMs()
	res, err := s.db.ExecContext(ctx, "UPDATE approvals SET state = $1, decided_by = $2, decided_ms = $3 WHERE id = $4 AND state = 'pending' AND expires_ms >= $3",
		state, by, now, id)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, ErrConflict
	}
	return s.GetApproval(ctx, id)
}

// ExpireApprovals marks overdue pending approvals expired.
func (s *Store) ExpireApprovals(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE approvals SET state = 'expired', decided_ms = $1 WHERE state = 'pending' AND expires_ms < $1", NowMs())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- trigger events ----

// TriggerEvent records a trigger firing (or a missed firing).
type TriggerEvent struct {
	ID         string
	Namespace  string
	Deployment string
	Trigger    string
	State      string // fired | missed | rejected
	TaskID     string
	DueMs      int64
	RecordedMs int64
	Message    string
}

// RecordTriggerEvent stores an event; a second event for the same trigger and
// due time is ignored (returns false), so a firing is recorded exactly once.
func (s *Store) RecordTriggerEvent(ctx context.Context, e TriggerEvent) (bool, error) {
	if e.ID == "" {
		e.ID = NewID()
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO trigger_events (id, namespace, deployment, trigger_name, state, task_id, due_ms, recorded_ms, message) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) "+
			"ON CONFLICT (namespace, deployment, trigger_name, due_ms) DO NOTHING",
		e.ID, e.Namespace, e.Deployment, e.Trigger, e.State, e.TaskID, e.DueMs, NowMs(), e.Message)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ListTriggerEvents filters by deployment and state, newest first.
func (s *Store) ListTriggerEvents(ctx context.Context, ns, deployment, state string, limit int) ([]TriggerEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, namespace, deployment, trigger_name, state, task_id, due_ms, recorded_ms, message FROM trigger_events "+
			"WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR deployment = $2) AND ($3 = '' OR state = $3) ORDER BY due_ms DESC, id DESC LIMIT $4",
		ns, deployment, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriggerEvent
	for rows.Next() {
		var e TriggerEvent
		if err := rows.Scan(&e.ID, &e.Namespace, &e.Deployment, &e.Trigger, &e.State, &e.TaskID, &e.DueMs, &e.RecordedMs, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastTriggerDue returns the latest recorded due time for a trigger (0 if none).
func (s *Store) LastTriggerDue(ctx context.Context, ns, deployment, trigger string) (int64, error) {
	var due int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(due_ms), 0) FROM trigger_events WHERE namespace = $1 AND deployment = $2 AND trigger_name = $3",
		ns, deployment, trigger).Scan(&due)
	return due, err
}

// SetWebhookSecret creates or rotates a webhook trigger's secret and returns
// it (only its hash is stored).
func (s *Store) SetWebhookSecret(ctx context.Context, ns, deployment, trigger string) (string, error) {
	secret := NewSecret("agen_hook_")
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO webhook_secrets (namespace, deployment, trigger_name, secret_hash, created_ms) VALUES ($1, $2, $3, $4, $5) "+
			"ON CONFLICT (namespace, deployment, trigger_name) DO UPDATE SET secret_hash = excluded.secret_hash, created_ms = excluded.created_ms",
		ns, deployment, trigger, hashSecret(secret), NowMs())
	return secret, err
}

// CheckWebhookSecret reports whether secret is the trigger's current secret.
func (s *Store) CheckWebhookSecret(ctx context.Context, ns, deployment, trigger, secret string) (bool, error) {
	var h string
	err := s.db.QueryRowContext(ctx, "SELECT secret_hash FROM webhook_secrets WHERE namespace = $1 AND deployment = $2 AND trigger_name = $3",
		ns, deployment, trigger).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return secret != "" && subtle.ConstantTimeCompare([]byte(h), []byte(hashSecret(secret))) == 1, nil
}

// ---- tokens ----

// APIToken grants scoped access to the Hub API.
type APIToken struct {
	ID         string
	Name       string
	Scopes     []string
	Namespaces []string
	CreatedMs  int64
	ExpiresMs  int64
	Revoked    bool
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// NewSecret returns a random token secret with a readable prefix.
func NewSecret(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// CreateAPIToken stores a token and returns it with its secret (shown once).
func (s *Store) CreateAPIToken(ctx context.Context, name string, scopes, namespaces []string, ttlMs int64) (APIToken, string, error) {
	t := APIToken{ID: NewID(), Name: name, Scopes: scopes, Namespaces: namespaces, CreatedMs: NowMs()}
	if ttlMs > 0 {
		t.ExpiresMs = t.CreatedMs + ttlMs
	}
	secret := NewSecret("agen_")
	sc, _ := json.Marshal(scopes)
	nss, _ := json.Marshal(namespaces)
	_, err := s.db.ExecContext(ctx, "INSERT INTO api_tokens (id, name, secret_hash, scopes, namespaces, created_ms, expires_ms, revoked) VALUES ($1, $2, $3, $4, $5, $6, $7, 0)",
		t.ID, name, hashSecret(secret), string(sc), string(nss), t.CreatedMs, t.ExpiresMs)
	return t, secret, err
}

func scanToken(r interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var sc, nss string
	var revoked int
	err := r.Scan(&t.ID, &t.Name, &sc, &nss, &t.CreatedMs, &t.ExpiresMs, &revoked)
	if err == nil {
		_ = json.Unmarshal([]byte(sc), &t.Scopes)
		_ = json.Unmarshal([]byte(nss), &t.Namespaces)
		t.Revoked = revoked != 0
	}
	return t, err
}

// LookupAPIToken returns the valid (unrevoked, unexpired) token for a secret.
func (s *Store) LookupAPIToken(ctx context.Context, secret string) (APIToken, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx,
		"SELECT id, name, scopes, namespaces, created_ms, expires_ms, revoked FROM api_tokens WHERE secret_hash = $1", hashSecret(secret)))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if t.Revoked || (t.ExpiresMs > 0 && t.ExpiresMs < NowMs()) {
		return t, ErrForbidden
	}
	return t, nil
}

// ListAPITokens lists tokens (never secrets).
func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, scopes, namespaces, created_ms, expires_ms, revoked FROM api_tokens ORDER BY created_ms, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken revokes a token.
func (s *Store) RevokeAPIToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE api_tokens SET revoked = 1 WHERE id = $1", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateJoinToken returns a one-time nest enrolment secret.
func (s *Store) CreateJoinToken(ctx context.Context, ttlMs int64) (string, error) {
	secret := NewSecret("join_")
	_, err := s.db.ExecContext(ctx, "INSERT INTO join_tokens (token_hash, expires_ms, used_by) VALUES ($1, $2, '')", hashSecret(secret), NowMs()+ttlMs)
	return secret, err
}

// EnrollNest consumes a join token, registers the nest and stores its bearer
// token (name "nest:<id>", scope "nest") in one transaction. It returns the
// nest and the token secret.
//
// With sign (an mTLS Hub), the Nest authenticates with a certificate instead
// of a bearer token: sign is called inside the transaction with the new id
// and returns the certificate fingerprint, so a signing failure leaves no
// Nest and does not use up the join token. No bearer token is minted then.
func (s *Store) EnrollNest(ctx context.Context, joinSecret string, n Nest, sign func(nestID string) (string, error)) (Nest, string, error) {
	n.ID = NewID()
	secret := ""
	if sign == nil {
		secret = NewSecret("agen_nest_")
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := NowMs()
		res, err := tx.ExecContext(ctx, "UPDATE join_tokens SET used_by = $1 WHERE token_hash = $2 AND used_by = '' AND expires_ms >= $3",
			n.ID, hashSecret(joinSecret), now)
		if err != nil {
			return err
		}
		if c, _ := res.RowsAffected(); c == 0 {
			return ErrForbidden
		}
		fp := ""
		if sign != nil {
			if fp, err = sign(n.ID); err != nil {
				return err
			}
		}
		labels, _ := json.Marshal(n.Labels)
		if _, err := tx.ExecContext(ctx, "INSERT INTO nests ("+nestCols+") VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9)",
			n.ID, n.Name, n.Backend, string(labels), n.Capacity, n.GatewayURL, fp, now, now); err != nil {
			return err
		}
		if sign != nil {
			return nil
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO api_tokens (id, name, secret_hash, scopes, namespaces, created_ms, expires_ms, revoked) VALUES ($1, $2, $3, $4, '[]', $5, 0, 0)",
			NewID(), "nest:"+n.ID, hashSecret(secret), `["nest"]`, now)
		return err
	})
	if err != nil {
		return n, "", err
	}
	n, err = s.GetNest(ctx, n.ID)
	return n, secret, err
}

// ConsumeJoinToken marks a join token used by nestID; it works once.
func (s *Store) ConsumeJoinToken(ctx context.Context, secret, nestID string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE join_tokens SET used_by = $1 WHERE token_hash = $2 AND used_by = '' AND expires_ms >= $3",
		nestID, hashSecret(secret), NowMs())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrForbidden
	}
	return nil
}

// EnsureTokenKey returns the Hub's call-token signing key (an Ed25519 seed,
// base64), creating it with create() if none exists; Hub replicas racing to
// create it all end up with the same (first) one.
func (s *Store) EnsureTokenKey(ctx context.Context, create func() (string, error)) (string, error) {
	var key string
	// The newest key (rotation adds rows); the first one is created here.
	const newest = "SELECT private_key FROM hub_token_key ORDER BY id DESC LIMIT 1"
	err := s.db.QueryRowContext(ctx, newest).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		fresh, cerr := create()
		if cerr != nil {
			return "", cerr
		}
		sealed, serr := s.seal(fresh)
		if serr != nil {
			return "", serr
		}
		if _, err := s.db.ExecContext(ctx, "INSERT INTO hub_token_key (id, private_key, created_ms) VALUES (1, $1, $2) ON CONFLICT (id) DO NOTHING",
			sealed, NowMs()); err != nil {
			return "", err
		}
		err = s.db.QueryRowContext(ctx, newest).Scan(&key)
	}
	if err != nil {
		return "", err
	}
	s.sealInPlace(ctx, key, "UPDATE hub_token_key SET private_key = $1 WHERE private_key = $2")
	return s.unseal(key)
}

// TokenKey is one call-token signing key (an Ed25519 seed, base64).
// Retired keys (RetiredMs > 0) only verify stored requester records.
type TokenKey struct {
	ID        int64
	Seed      string
	CreatedMs int64
	RetiredMs int64
}

// TokenKeys returns the call-token keys, oldest first.
func (s *Store) TokenKeys(ctx context.Context) ([]TokenKey, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, private_key, created_ms, retired_ms FROM hub_token_key ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenKey
	for rows.Next() {
		var k TokenKey
		if err := rows.Scan(&k.ID, &k.Seed, &k.CreatedMs, &k.RetiredMs); err != nil {
			return nil, err
		}
		if k.Seed, err = s.unseal(k.Seed); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ErrTooSoon: the newest call-token key is younger than the rotation
// minimum (tokens signed by the key it would retire may still be live).
var ErrTooSoon = errors.New("store: the current call-token key is too new to rotate again")

// RotateTokenKey adds a new call-token key (seed, base64). The previous key
// keeps verifying live tokens; keys before it are retired (kept only to
// verify stored requester records). It refuses while the newest key is
// younger than minAge, so no key still signing or verifying live tokens is
// retired.
func (s *Store) RotateTokenKey(ctx context.Context, seed string, minAge time.Duration) error {
	sealed, err := s.seal(seed)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var maxID, newest int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0), COALESCE(MAX(created_ms), 0) FROM hub_token_key").Scan(&maxID, &newest); err != nil {
			return err
		}
		if maxID > 0 && NowMs()-newest < minAge.Milliseconds() {
			return ErrTooSoon
		}
		now := NowMs()
		if _, err := tx.ExecContext(ctx, "INSERT INTO hub_token_key (id, private_key, created_ms) VALUES ($1, $2, $3)", maxID+1, sealed, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE hub_token_key SET retired_ms = $1 WHERE id < $2 AND retired_ms = 0", now, maxID)
		return err
	})
}

// CA is the Hub certificate authority as stored.
type CA struct {
	CertPEM, KeyPEM string
}

// EnsureCA returns the Hub CA, creating it with create() if none exists. Hub
// replicas racing to create it all end up with the same (first) one.
func (s *Store) EnsureCA(ctx context.Context, create func() (CA, error)) (CA, error) {
	var ca CA
	err := s.db.QueryRowContext(ctx, "SELECT cert_pem, key_pem FROM hub_ca WHERE id = 1").Scan(&ca.CertPEM, &ca.KeyPEM)
	if errors.Is(err, sql.ErrNoRows) {
		fresh, cerr := create()
		if cerr != nil {
			return ca, cerr
		}
		sealed, serr := s.seal(fresh.KeyPEM)
		if serr != nil {
			return ca, serr
		}
		if _, err := s.db.ExecContext(ctx, "INSERT INTO hub_ca (id, cert_pem, key_pem, created_ms) VALUES (1, $1, $2, $3) ON CONFLICT (id) DO NOTHING",
			fresh.CertPEM, sealed, NowMs()); err != nil {
			return ca, err
		}
		err = s.db.QueryRowContext(ctx, "SELECT cert_pem, key_pem FROM hub_ca WHERE id = 1").Scan(&ca.CertPEM, &ca.KeyPEM)
	}
	if err != nil {
		return ca, err
	}
	s.sealInPlace(ctx, ca.KeyPEM, "UPDATE hub_ca SET key_pem = $1 WHERE id = 1 AND key_pem = $2")
	ca.KeyPEM, err = s.unseal(ca.KeyPEM)
	return ca, err
}
