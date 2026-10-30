package store

import (
	"context"
	"database/sql"
	"errors"
)

// Artifact is named, content-addressed text an agent saved during a run
// with the save_artifact tool (or read back with read_artifact).
type Artifact struct {
	Namespace string
	// ID: "sha256:<hex>" of Content.
	ID        string
	Name      string
	MediaType string
	// Size: byte length of Content.
	Size      int64
	Content   string
	TaskID    string
	RunID     string
	CreatedMs int64
}

const artifactCols = "namespace, id, name, media_type, size, content, task_id, run_id, created_ms"

func scanArtifact(r interface{ Scan(...any) error }) (Artifact, error) {
	var a Artifact
	err := r.Scan(&a.Namespace, &a.ID, &a.Name, &a.MediaType, &a.Size, &a.Content, &a.TaskID, &a.RunID, &a.CreatedMs)
	return a, err
}

// GetArtifact returns an artifact, or ErrNotFound if it does not exist.
func (s *Store) GetArtifact(ctx context.Context, namespace, id string) (Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx, "SELECT "+artifactCols+" FROM artifacts WHERE namespace = $1 AND id = $2", namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ListArtifacts lists a namespace's artifacts, newest first; "" lists
// every namespace's (an unrestricted caller with no namespace named).
// taskID ("") filters to one task's.
func (s *Store) ListArtifacts(ctx context.Context, namespace, taskID string) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+artifactCols+" FROM artifacts WHERE ($1 = '' OR namespace = $1) AND ($2 = '' OR task_id = $2) ORDER BY created_ms DESC, id DESC", namespace, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
