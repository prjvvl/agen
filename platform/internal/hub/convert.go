package hub

import (
	"encoding/json"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Deployment policy is stored as protojson of the proto messages.
var pj = protojson.MarshalOptions{EmitUnpopulated: false}

func toJSON(m proto.Message) json.RawMessage {
	if m == nil {
		return json.RawMessage("{}")
	}
	b, err := pj.Marshal(m)
	if err != nil || len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

func fromJSON(raw json.RawMessage, m proto.Message) {
	if len(raw) > 0 {
		_ = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, m)
	}
}

func triggersJSON(ts []*agenv1.Trigger) json.RawMessage {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, string(toJSON(t)))
	}
	return json.RawMessage("[" + strings.Join(parts, ",") + "]")
}

func triggersFrom(raw json.RawMessage) []*agenv1.Trigger {
	var items []json.RawMessage
	_ = json.Unmarshal(raw, &items)
	out := make([]*agenv1.Trigger, 0, len(items))
	for _, it := range items {
		t := &agenv1.Trigger{}
		fromJSON(it, t)
		out = append(out, t)
	}
	return out
}

var kindNames = map[string]agenv1.DeploymentKind{
	"singleton": agenv1.DeploymentKind_DEPLOYMENT_KIND_SINGLETON,
	"pool":      agenv1.DeploymentKind_DEPLOYMENT_KIND_POOL,
	"task":      agenv1.DeploymentKind_DEPLOYMENT_KIND_TASK,
}

func kindString(k agenv1.DeploymentKind) string {
	for s, v := range kindNames {
		if v == k {
			return s
		}
	}
	return ""
}

// Policy is a deployment's decoded policy.
type Policy struct {
	Scale     *agenv1.ScalePolicy
	Budget    *agenv1.Budget
	Limits    *agenv1.Limits
	Placement *agenv1.Placement
	Triggers  []*agenv1.Trigger
}

// PolicyOf decodes the stored policy of a deployment.
func PolicyOf(d store.Deployment) Policy {
	p := Policy{Scale: &agenv1.ScalePolicy{}, Budget: &agenv1.Budget{}, Limits: &agenv1.Limits{}, Placement: &agenv1.Placement{}}
	fromJSON(d.Scale, p.Scale)
	fromJSON(d.Budget, p.Budget)
	fromJSON(d.Limits, p.Limits)
	fromJSON(d.Placement, p.Placement)
	p.Triggers = triggersFrom(d.Triggers)
	return p
}

// policyFromBundle converts config.json policy to proto messages.
func policyFromBundle(b *bundle.Bundle) Policy {
	p := Policy{
		Scale: &agenv1.ScalePolicy{
			Min:                    int32(b.Scale.Min),
			Max:                    int32(b.Scale.Max),
			TargetQueuePerInstance: int32(b.Scale.TargetQueuePerInstance),
			IdleTimeoutSeconds:     int32(b.Scale.IdleSeconds()),
			MaxConcurrency:         int32(b.Scale.MaxConcurrency),
		},
		Budget:    &agenv1.Budget{},
		Limits:    &agenv1.Limits{},
		Placement: &agenv1.Placement{},
	}
	var budget struct {
		MaxTokensPerRun int64   `json:"maxTokensPerRun"`
		MaxUsdPerRun    float64 `json:"maxUsdPerRun"`
		MaxUsdPerDay    float64 `json:"maxUsdPerDay"`
	}
	_ = json.Unmarshal(b.Budget, &budget)
	p.Budget = &agenv1.Budget{MaxTokensPerRun: budget.MaxTokensPerRun, MaxUsdPerRun: budget.MaxUsdPerRun, MaxUsdPerDay: budget.MaxUsdPerDay}
	var limits struct {
		MaxDelegationDepth  int32 `json:"maxDelegationDepth"`
		MaxFanOut           int32 `json:"maxFanOut"`
		MaxTotalDelegations int32 `json:"maxTotalDelegations"`
	}
	_ = json.Unmarshal(b.Limits, &limits)
	p.Limits = &agenv1.Limits{MaxDelegationDepth: limits.MaxDelegationDepth, MaxFanOut: limits.MaxFanOut, MaxTotalDelegations: limits.MaxTotalDelegations}
	for _, t := range b.Triggers {
		p.Triggers = append(p.Triggers, &agenv1.Trigger{Type: t.Type, Name: t.Name, Schedule: t.Schedule, Input: t.Input, CatchUp: t.CatchUp})
	}
	return p
}

func (p Policy) apply(d *store.Deployment) {
	d.Scale, d.Budget, d.Limits, d.Placement = toJSON(p.Scale), toJSON(p.Budget), toJSON(p.Limits), toJSON(p.Placement)
	d.Triggers = triggersJSON(p.Triggers)
}

func ms(t int64) *timestamppb.Timestamp {
	if t == 0 {
		return nil
	}
	return timestamppb.New(msTime(t))
}

func deploymentProto(d store.Deployment, ready int) *agenv1.Deployment {
	p := PolicyOf(d)
	return &agenv1.Deployment{
		Namespace:        d.Namespace,
		Name:             d.Name,
		DefinitionDigest: d.DefinitionDigest,
		Kind:             kindNames[d.Kind],
		Scale:            p.Scale,
		Budget:           p.Budget,
		Limits:           p.Limits,
		Placement:        p.Placement,
		Triggers:         p.Triggers,
		Paused:           d.Paused,
		Desired:          int32(d.Desired),
		Ready:            int32(ready),
		Generation:       d.Generation,
		CreatedAt:        ms(d.CreatedMs),
		UpdatedAt:        ms(d.UpdatedMs),
		LastActivityUnix: d.LastActivityMs / 1000,
	}
}

var instanceStates = map[string]agenv1.InstanceState{
	"starting": agenv1.InstanceState_INSTANCE_STATE_STARTING,
	"ready":    agenv1.InstanceState_INSTANCE_STATE_READY,
	"busy":     agenv1.InstanceState_INSTANCE_STATE_BUSY,
	"draining": agenv1.InstanceState_INSTANCE_STATE_DRAINING,
	"stopped":  agenv1.InstanceState_INSTANCE_STATE_STOPPED,
	"failed":   agenv1.InstanceState_INSTANCE_STATE_FAILED,
}

// InstanceStateName converts a proto state to the store's string.
func InstanceStateName(s agenv1.InstanceState) string {
	for k, v := range instanceStates {
		if v == s {
			return k
		}
	}
	return "starting"
}

func instanceProto(i store.Instance) *agenv1.Instance {
	return &agenv1.Instance{
		Id: i.ID, Namespace: i.Namespace, Deployment: i.Deployment, NestId: i.NestID, DefinitionDigest: i.DefinitionDigest,
		State: instanceStates[i.State], Endpoint: i.Endpoint, RunningTasks: int32(i.RunningTasks), StartedAt: ms(i.StartedMs),
		LastSeen: ms(i.LastSeenMs), Message: i.Message,
	}
}

func nestProto(n store.Nest, used int) *agenv1.Nest {
	states := map[string]agenv1.NestState{"active": agenv1.NestState_NEST_STATE_ACTIVE, "lost": agenv1.NestState_NEST_STATE_LOST,
		"draining": agenv1.NestState_NEST_STATE_DRAINING, "revoked": agenv1.NestState_NEST_STATE_REVOKED}
	return &agenv1.Nest{Id: n.ID, Name: n.Name, Backend: n.Backend, Labels: n.Labels, Capacity: int32(n.Capacity), Used: int32(used),
		GatewayUrl: n.GatewayURL, State: states[n.State], LastHeartbeat: ms(n.LastHeartbeatMs)}
}

var taskStates = map[string]agenv1.TaskState{
	"queued": agenv1.TaskState_TASK_STATE_QUEUED, "leased": agenv1.TaskState_TASK_STATE_LEASED,
	"running": agenv1.TaskState_TASK_STATE_RUNNING, "succeeded": agenv1.TaskState_TASK_STATE_SUCCEEDED,
	"failed": agenv1.TaskState_TASK_STATE_FAILED, "cancelled": agenv1.TaskState_TASK_STATE_CANCELLED,
}

func taskStateName(s agenv1.TaskState) string {
	for k, v := range taskStates {
		if v == s {
			return k
		}
	}
	return ""
}

// TaskProto converts a stored task.
func TaskProto(t store.Task) *agenv1.Task {
	out := &agenv1.Task{
		Id: t.ID, Namespace: t.Namespace, Deployment: t.Deployment, Input: t.Input, State: taskStates[t.State], Output: t.Output,
		Error: t.Error, InstanceId: t.InstanceID, RunId: t.RunID, Source: t.Source, Attempts: int32(t.Attempts),
		CreatedAt: ms(t.CreatedMs), UpdatedAt: ms(t.UpdatedMs), LeaseExpiresAt: ms(t.LeaseExpiresMs), LeaseId: t.LeaseID,
		ParentTaskId: t.ParentTaskID, ParentRunId: t.ParentRunID, RootRunId: t.RootRunID, Depth: int32(t.Depth), Traceparent: t.Traceparent,
	}
	if tp := t.Traceparent; len(tp) > 35 {
		out.TraceId = tp[3:35]
	}
	return out
}

var approvalStates = map[string]agenv1.ApprovalState{
	"pending": agenv1.ApprovalState_APPROVAL_STATE_PENDING, "approved": agenv1.ApprovalState_APPROVAL_STATE_APPROVED,
	"denied": agenv1.ApprovalState_APPROVAL_STATE_DENIED, "expired": agenv1.ApprovalState_APPROVAL_STATE_EXPIRED,
}

// ApprovalProto converts a stored approval.
func ApprovalProto(a store.Approval) *agenv1.Approval {
	args := &structpb.Struct{}
	_ = args.UnmarshalJSON(a.Arguments)
	return &agenv1.Approval{Id: a.ID, Namespace: a.Namespace, Deployment: a.Deployment, RunId: a.RunID, Tool: a.Tool, Arguments: args,
		State: approvalStates[a.State], RequestedBy: a.RequestedBy, DecidedBy: a.DecidedBy, CreatedAt: ms(a.CreatedMs), ExpiresAt: ms(a.ExpiresMs)}
}

func runProto(r store.RunRow) *agenv1.Run {
	return &agenv1.Run{Id: r.ID, SessionId: r.SessionID, ConversationId: r.ConversationID, Namespace: r.Namespace, Deployment: r.Deployment,
		Status: r.Status, Input: r.Input, Output: r.Output, Usage: &agenv1.Usage{InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CostUsd: r.CostUSD},
		TraceId: r.TraceID, StartedAt: ms(r.StartedMs), EndedAt: ms(r.EndedMs), ParentRunId: r.ParentRunID, RootRunId: r.RootRunID,
		DefinitionDigest: r.DefinitionDigest}
}

func spanProto(s store.SpanRow) *agenv1.Span {
	attrs := &structpb.Struct{}
	_ = attrs.UnmarshalJSON(s.Attributes)
	return &agenv1.Span{TraceId: s.TraceID, SpanId: s.SpanID, ParentSpanId: s.ParentSpanID, Name: s.Name, RunId: s.RunID,
		Start: ms(s.StartMs), End: ms(s.EndMs), Attributes: attrs, Status: s.Status}
}

// connectErr maps store and bundle errors to Connect codes.
func connectErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	var be *bundle.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.As(err, &be):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrForbidden):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, store.ErrFenced):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func invalid(msg string) error { return connect.NewError(connect.CodeInvalidArgument, errors.New(msg)) }
