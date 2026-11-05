package hub

import (
	"context"
	"sort"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

func artifactProto(a store.Artifact) *agenv1.Artifact {
	return &agenv1.Artifact{Namespace: a.Namespace, Id: a.ID, Name: a.Name, MediaType: a.MediaType, Size: a.Size,
		Content: a.Content, TaskId: a.TaskID, RunId: a.RunID, CreatedAt: ms(a.CreatedMs)}
}

// GetArtifact returns one artifact by id: named, content-addressed text an
// agent saved during a run with the save_artifact tool.
func (h *Hub) GetArtifact(ctx context.Context, req *connect.Request[agenv1.GetArtifactRequest]) (*connect.Response[agenv1.GetArtifactResponse], error) {
	ns := nsOr(req.Msg.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	if req.Msg.Id == "" {
		return nil, invalid("id is required")
	}
	a, err := h.Store.GetArtifact(ctx, ns, req.Msg.Id)
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.GetArtifactResponse{Artifact: artifactProto(a)}), nil
}

// ListArtifacts lists a namespace's artifacts, newest first; task_id
// filters to one task's.
func (h *Hub) ListArtifacts(ctx context.Context, req *connect.Request[agenv1.ListArtifactsRequest]) (*connect.Response[agenv1.ListArtifactsResponse], error) {
	nss, err := namespacesFor(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, err
	}
	var all []store.Artifact
	for _, ns := range nss {
		part, err := h.Store.ListArtifacts(ctx, ns, req.Msg.TaskId)
		if err != nil {
			return nil, connectErr(err)
		}
		all = append(all, part...)
	}
	if len(nss) > 1 {
		sort.Slice(all, func(i, j int) bool { return all[i].CreatedMs > all[j].CreatedMs })
	}
	out := &agenv1.ListArtifactsResponse{}
	for _, a := range all {
		out.Artifacts = append(out.Artifacts, artifactProto(a))
	}
	return connect.NewResponse(out), nil
}
