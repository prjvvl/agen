package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ProviderHold is a provider account (provider + key) that failed with an
// account error: the deployments using it take no new work until a probe
// task succeeds or an operator resumes it.
type ProviderHold struct {
	Account  string
	Reason   string
	Failures int
	HeldMs   int64
	// RetryMs is when one probe task may go through.
	RetryMs   int64
	ProbeTask string
}

const holdCols = "account, reason, failures, held_ms, retry_ms, probe_task"

func scanHold(r interface{ Scan(...any) error }) (ProviderHold, error) {
	var h ProviderHold
	err := r.Scan(&h.Account, &h.Reason, &h.Failures, &h.HeldMs, &h.RetryMs, &h.ProbeTask)
	return h, err
}

// HoldBackoff is the wait before the next probe after n account failures:
// 5 min, doubling, at most 1 h.
func HoldBackoff(n int) time.Duration {
	d := 5 * time.Minute
	for i := 1; i < n && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// HoldProvider records an account failure. It returns the hold and whether
// it is new (the account was working until now).
func (s *Store) HoldProvider(ctx context.Context, account, reason string) (ProviderHold, bool, error) {
	var out ProviderHold
	created := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := NowMs()
		h, err := scanHold(tx.QueryRowContext(ctx, "SELECT "+holdCols+" FROM provider_holds WHERE account = $1", account))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			created = true
			out = ProviderHold{Account: account, Reason: reason, Failures: 1, HeldMs: now, RetryMs: now + HoldBackoff(1).Milliseconds()}
			_, err = tx.ExecContext(ctx, "INSERT INTO provider_holds ("+holdCols+") VALUES ($1, $2, $3, $4, $5, '')",
				out.Account, out.Reason, out.Failures, out.HeldMs, out.RetryMs)
			return err
		case err != nil:
			return err
		}
		h.Failures++
		h.Reason, h.RetryMs, h.ProbeTask = reason, now+HoldBackoff(h.Failures).Milliseconds(), ""
		out = h
		_, err = tx.ExecContext(ctx, "UPDATE provider_holds SET reason = $1, failures = $2, retry_ms = $3, probe_task = '' WHERE account = $4",
			h.Reason, h.Failures, h.RetryMs, account)
		return err
	})
	return out, created, err
}

// GetProviderHold returns an account's hold, or ErrNotFound.
func (s *Store) GetProviderHold(ctx context.Context, account string) (ProviderHold, error) {
	h, err := scanHold(s.db.QueryRowContext(ctx, "SELECT "+holdCols+" FROM provider_holds WHERE account = $1", account))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// ListProviderHolds returns every held account.
func (s *Store) ListProviderHolds(ctx context.Context) ([]ProviderHold, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+holdCols+" FROM provider_holds ORDER BY held_ms")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderHold
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ClaimProbe lets one task through a hold once its retry time has come.
func (s *Store) ClaimProbe(ctx context.Context, account, taskID string) (bool, error) {
	// A probe whose task went back to the queue (released by its instance,
	// lease expired) or ended without clearing the hold no longer counts.
	res, err := s.db.ExecContext(ctx, "UPDATE provider_holds SET probe_task = $1 WHERE account = $2 AND retry_ms <= $3 AND "+
		"(probe_task = '' OR (probe_task NOT LIKE 'claim:%' AND NOT EXISTS "+
		"(SELECT 1 FROM tasks WHERE tasks.id = provider_holds.probe_task AND tasks.state IN ('leased', 'running'))))",
		taskID, account, NowMs())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseProbe makes a probe that did not run (released, cancelled, or failed
// for another reason) available again.
func (s *Store) ReleaseProbe(ctx context.Context, account, taskID string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE provider_holds SET probe_task = '' WHERE account = $1 AND probe_task = $2", account, taskID)
	return err
}

// ClearProviderHold ends a hold; it reports whether there was one.
func (s *Store) ClearProviderHold(ctx context.Context, account string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM provider_holds WHERE account = $1", account)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetProbeTask records which task a claimed probe became.
func (s *Store) SetProbeTask(ctx context.Context, account, claim, taskID string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE provider_holds SET probe_task = $1 WHERE account = $2 AND probe_task = $3", taskID, account, claim)
	return err
}

// RunSucceededSince reports whether a run of one of the deployments that
// started after sinceMs has succeeded.
func (s *Store) RunSucceededSince(ctx context.Context, deps [][2]string, sinceMs int64) (bool, error) {
	for _, d := range deps {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE namespace = $1 AND deployment = $2 AND status = 'succeeded' AND started_ms > $3",
			d[0], d[1], sinceMs).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}
