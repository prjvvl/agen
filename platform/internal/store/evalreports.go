package store

import "context"

// EvalReport is one `agen eval` report recorded against a deployment
// (2.43, part: eval history), so a later deploy can compare against the
// last one. ReportJSON is agen eval's own report shape, opaque here.
type EvalReport struct {
	ID               string
	Namespace        string
	Deployment       string
	DefinitionDigest string
	ReportJSON       string
	CreatedMs        int64
}

const evalReportCols = "id, namespace, deployment, definition_digest, report_json, created_ms"

func scanEvalReport(r interface{ Scan(...any) error }) (EvalReport, error) {
	var e EvalReport
	err := r.Scan(&e.ID, &e.Namespace, &e.Deployment, &e.DefinitionDigest, &e.ReportJSON, &e.CreatedMs)
	return e, err
}

// RecordEvalReport stores one eval report against a deployment.
func (s *Store) RecordEvalReport(ctx context.Context, e EvalReport) (EvalReport, error) {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.CreatedMs == 0 {
		e.CreatedMs = NowMs()
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO eval_reports ("+evalReportCols+") VALUES ($1, $2, $3, $4, $5, $6)",
		e.ID, e.Namespace, e.Deployment, e.DefinitionDigest, e.ReportJSON, e.CreatedMs)
	return e, err
}

// ListEvalReports lists a deployment's eval reports, newest first.
func (s *Store) ListEvalReports(ctx context.Context, ns, deployment string, limit int) ([]EvalReport, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+evalReportCols+" FROM eval_reports WHERE namespace = $1 AND deployment = $2 "+
		"ORDER BY created_ms DESC, id DESC LIMIT $3", ns, deployment, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvalReport
	for rows.Next() {
		e, err := scanEvalReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
