package store

import (
	"context"
	"encoding/json"
)

// Feedback is a judgement on how a task went: a person, a check, or a judge
// model. Approval decisions are recorded this way too (By = "approval").
type Feedback struct {
	ID         string
	Namespace  string
	Deployment string
	TaskID     string
	// Score: 0.0 (worst) to 1.0 (best).
	Score     float64
	Labels    map[string]string
	Note      string
	By        string
	CreatedMs int64
}

const feedbackCols = "id, namespace, deployment, task_id, score, labels, note, by, created_ms"

func scanFeedback(r interface{ Scan(...any) error }) (Feedback, error) {
	var f Feedback
	var labels string
	err := r.Scan(&f.ID, &f.Namespace, &f.Deployment, &f.TaskID, &f.Score, &labels, &f.Note, &f.By, &f.CreatedMs)
	if err == nil {
		err = json.Unmarshal([]byte(labels), &f.Labels)
	}
	return f, err
}

// RecordFeedback stores one piece of feedback.
func (s *Store) RecordFeedback(ctx context.Context, f Feedback) (Feedback, error) {
	if f.ID == "" {
		f.ID = NewID()
	}
	if f.CreatedMs == 0 {
		f.CreatedMs = NowMs()
	}
	labels, err := json.Marshal(f.Labels)
	if err != nil {
		return f, err
	}
	if f.Labels == nil {
		labels = []byte("{}")
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO feedback ("+feedbackCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
		f.ID, f.Namespace, f.Deployment, f.TaskID, f.Score, string(labels), f.Note, f.By, f.CreatedMs)
	return f, err
}

// ListFeedback filters by namespace (""= any the caller passed), deployment
// ("" = any), labels (must all match) and a since cutoff, newest first.
func (s *Store) ListFeedback(ctx context.Context, ns, deployment string, labels map[string]string, sinceMs int64, limit int) ([]Feedback, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+feedbackCols+" FROM feedback WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR deployment = $2) AND created_ms >= $3 "+
		"ORDER BY created_ms DESC, id DESC LIMIT $4", ns, deployment, sinceMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feedback
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, err
		}
		match := true
		for k, v := range labels {
			if f.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, f)
		}
	}
	return out, rows.Err()
}
