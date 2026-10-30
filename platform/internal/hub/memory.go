package hub

import (
	"context"
	"sort"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/store"
)

func memoryBlockProto(b store.MemoryBlock) *agenv1.MemoryBlock {
	return &agenv1.MemoryBlock{Namespace: b.Namespace, Id: b.ID, Label: b.Label, Description: b.Description,
		Value: b.Value, LimitChars: b.LimitChars, Version: b.Version, UpdatedAt: ms(b.UpdatedMs)}
}

// SetMemoryBlock replaces a memory block's value outright, creating it if
// it does not exist yet. Agents change one through the memory_update tool
// instead, which compares versions first (compare-and-set).
func (h *Hub) SetMemoryBlock(ctx context.Context, req *connect.Request[agenv1.SetMemoryBlockRequest]) (*connect.Response[agenv1.SetMemoryBlockResponse], error) {
	ns := nsOr(req.Msg.Namespace)
	if err := requireNamespace(ctx, ns); err != nil {
		return nil, err
	}
	if req.Msg.Id == "" {
		return nil, invalid("id is required")
	}
	if req.Msg.LimitChars < 0 {
		return nil, invalid("limit_chars must not be negative")
	}
	if req.Msg.LimitChars > 0 && int64(len(req.Msg.Value)) > req.Msg.LimitChars {
		return nil, invalid("value is longer than limit_chars")
	}
	b, err := h.Store.SetMemoryBlock(ctx, store.MemoryBlock{Namespace: ns, ID: req.Msg.Id, Label: req.Msg.Label,
		Description: req.Msg.Description, Value: req.Msg.Value, LimitChars: req.Msg.LimitChars})
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&agenv1.SetMemoryBlockResponse{Block: memoryBlockProto(b)}), nil
}

// ListMemoryBlocks lists a namespace's memory blocks, newest-updated first.
func (h *Hub) ListMemoryBlocks(ctx context.Context, req *connect.Request[agenv1.ListMemoryBlocksRequest]) (*connect.Response[agenv1.ListMemoryBlocksResponse], error) {
	nss, err := namespacesFor(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, err
	}
	var all []store.MemoryBlock
	for _, ns := range nss {
		part, err := h.Store.ListMemoryBlocks(ctx, ns)
		if err != nil {
			return nil, connectErr(err)
		}
		all = append(all, part...)
	}
	if len(nss) > 1 {
		sort.Slice(all, func(i, j int) bool { return all[i].UpdatedMs > all[j].UpdatedMs })
	}
	out := &agenv1.ListMemoryBlocksResponse{}
	for _, b := range all {
		out.Blocks = append(out.Blocks, memoryBlockProto(b))
	}
	return connect.NewResponse(out), nil
}
