package hub

import (
	"context"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

func evalReportProto(e store.EvalReport) *agenv1.EvalReport {
	return &agenv1.EvalReport{Id: e.ID, Namespace: e.Namespace, Deployment: e.Deployment,
		DefinitionDigest: e.DefinitionDigest, ReportJson: e.ReportJSON, CreatedAt: ms(e.CreatedMs)}
}

// RecordEvalReport stores an `agen eval` report against a deployment
// (2.43, part: eval history), so a later `agen deploy --require-eval`
// can compare against it. The deployment need not exist yet (a report
// from the very first deploy records before CreateDeployment ever
// runs, same reasoning `agen deploy` itself uses to run eval first).
func (h *Hub) RecordEvalReport(ctx context.Context, req *connect.Request[agenv1.RecordEvalReportRequest]) (*connect.Response[agenv1.RecordEvalReportResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	name := req.Msg.GetRef().GetName()
	if name == "" {
		return nil, invalid("ref.name is required")
	}
	if req.Msg.ReportJson == "" {
		return nil, invalid("report_json is required")
	}
	e, err := h.Store.RecordEvalReport(ctx, store.EvalReport{Namespace: ns, Deployment: name,
		DefinitionDigest: req.Msg.DefinitionDigest, ReportJSON: req.Msg.ReportJson})
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.RecordEvalReportResponse{Report: evalReportProto(e)}), nil
}

// ListEvalReports lists a deployment's eval reports, newest first.
func (h *Hub) ListEvalReports(ctx context.Context, req *connect.Request[agenv1.ListEvalReportsRequest]) (*connect.Response[agenv1.ListEvalReportsResponse], error) {
	ns := nsOr(req.Msg.GetRef().GetNamespace())
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	name := req.Msg.GetRef().GetName()
	if name == "" {
		return nil, invalid("ref.name is required")
	}
	reports, err := h.Store.ListEvalReports(ctx, ns, name, int(req.Msg.Limit))
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.ListEvalReportsResponse{}
	for _, e := range reports {
		out.Reports = append(out.Reports, evalReportProto(e))
	}
	return connect.NewResponse(out), nil
}
