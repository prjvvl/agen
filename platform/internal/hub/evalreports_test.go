package hub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// RecordEvalReport stores a report against a deployment that need not
// exist yet; ListEvalReports returns them newest first, scoped to that
// one deployment only (2.43, part: eval history).
// features: eval
func TestRecordAndListEvalReports(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(admin)

	if _, err := c.RecordEvalReport(ctx, connect.NewRequest(&agenv1.RecordEvalReportRequest{
		Ref: ref("", "not-deployed-yet"), DefinitionDigest: "sha256:1", ReportJson: `[{"scenario":"a","passK":true}]`})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordEvalReport(ctx, connect.NewRequest(&agenv1.RecordEvalReportRequest{
		Ref: ref("", "not-deployed-yet"), DefinitionDigest: "sha256:2", ReportJson: `[{"scenario":"a","passK":false}]`})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RecordEvalReport(ctx, connect.NewRequest(&agenv1.RecordEvalReportRequest{
		Ref: ref("", "other"), DefinitionDigest: "sha256:3", ReportJson: `[]`})); err != nil {
		t.Fatal(err)
	}

	got, err := c.ListEvalReports(ctx, connect.NewRequest(&agenv1.ListEvalReportsRequest{Ref: ref("", "not-deployed-yet")}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Msg.Reports) != 2 || got.Msg.Reports[0].DefinitionDigest != "sha256:2" {
		t.Fatalf("newest first, scoped to this deployment: %+v", got.Msg.Reports)
	}

	if _, err := c.RecordEvalReport(ctx, connect.NewRequest(&agenv1.RecordEvalReportRequest{
		Ref: ref("", ""), ReportJson: `[]`})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("an empty ref.name should be rejected: %v", err)
	}
	if _, err := c.RecordEvalReport(ctx, connect.NewRequest(&agenv1.RecordEvalReportRequest{
		Ref: ref("", "x")})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("an empty report_json should be rejected: %v", err)
	}
}
