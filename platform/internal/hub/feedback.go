package hub

import (
	"context"
	"sort"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

func feedbackProto(f store.Feedback) *agenv1.Feedback {
	return &agenv1.Feedback{Id: f.ID, Namespace: f.Namespace, Deployment: f.Deployment, TaskId: f.TaskID,
		Score: f.Score, Labels: f.Labels, Note: f.Note, By: f.By, CreatedAt: ms(f.CreatedMs)}
}

// RecordFeedback records a judgement on how a task went: a person, a check,
// or a judge model. Its namespace and deployment come from the task.
func (h *Hub) RecordFeedback(ctx context.Context, req *connect.Request[agenv1.RecordFeedbackRequest]) (*connect.Response[agenv1.RecordFeedbackResponse], error) {
	if req.Msg.TaskId == "" {
		return nil, invalid("task_id is required")
	}
	if req.Msg.Score < 0 || req.Msg.Score > 1 {
		return nil, invalid("score must be between 0.0 and 1.0")
	}
	t, err := h.Store.GetTask(ctx, req.Msg.TaskId)
	if err != nil {
		return nil, connectErr(err)
	}
	if err := requireNamespace(ctx, t.Namespace); err != nil {
		return nil, err
	}
	if err := store.CheckLabels(req.Msg.Labels); err != nil {
		return nil, invalid(err.Error())
	}
	f, err := h.Store.RecordFeedback(ctx, store.Feedback{Namespace: t.Namespace, Deployment: t.Deployment, TaskID: t.ID,
		Score: req.Msg.Score, Labels: req.Msg.Labels, Note: req.Msg.Note, By: PrincipalFrom(ctx).ID})
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.RecordFeedbackResponse{Feedback: feedbackProto(f)}), nil
}

// ListFeedback lists feedback, newest first, filtered by deployment, labels
// and a since cutoff.
func (h *Hub) ListFeedback(ctx context.Context, req *connect.Request[agenv1.ListFeedbackRequest]) (*connect.Response[agenv1.ListFeedbackResponse], error) {
	nss, err := namespacesFor(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, err
	}
	since := int64(0)
	if req.Msg.Since != nil {
		since = req.Msg.Since.AsTime().UnixMilli()
	}
	limit := listLimit(req.Msg.Limit, 100)
	var all []store.Feedback
	for _, ns := range nss {
		part, err := h.Store.ListFeedback(ctx, ns, req.Msg.Deployment, req.Msg.Labels, since, limit)
		if err != nil {
			return nil, connectErr(err)
		}
		all = append(all, part...)
	}
	if len(nss) > 1 {
		sort.Slice(all, func(i, j int) bool { return all[i].CreatedMs > all[j].CreatedMs })
		if len(all) > limit {
			all = all[:limit]
		}
	}
	out := &agenv1.ListFeedbackResponse{}
	for _, f := range all {
		out.Feedback = append(out.Feedback, feedbackProto(f))
	}
	return connect.NewResponse(out), nil
}
