// Package gateway is a Nest's A2A front door (docs/architecture.md §6):
// `/a2a/<ns>/<deployment>` (JSON-RPC message/send, message/stream,
// tasks/get, tasks/cancel) and the agent card. Calls run directly on a ready
// instance in this Nest; the Hub is never on the data path. A deployment
// with no ready instance is woken (activator) and the call is held until an
// instance is ready or the wake timeout passes (503 + Retry-After).
package gateway

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/calltoken"
	"github.com/prjvvl/agen/platform/internal/manager"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Fleet is what the Gateway needs from its Nest's Manager.
type Fleet interface {
	Acquire(ns, dep string) *manager.Slot
	Assignment(ns, dep string) (*agenv1.Assignment, bool)
	Saturated(ns, dep string) bool
	ReportActivity(ctx context.Context, ns, dep string, saturated bool) error
	DefinitionDir(ctx context.Context, digest string) (string, error)
	// TokenKeys are the Hub's public keys for A2A call tokens.
	TokenKeys() map[string]ed25519.PublicKey
}

// Gateway serves A2A for the deployments of one Nest.
type Gateway struct {
	Fleet Fleet
	// BaseURL is this Gateway's public URL (used in agent cards).
	BaseURL string
	// WakeTimeout bounds how long a call waits for a sleeping deployment.
	WakeTimeout time.Duration
	Log         *slog.Logger
	// RunTimeout bounds one A2A call on an instance (default 1h), so a
	// host that never answers does not hold the call forever.
	RunTimeout time.Duration
	// RequireAuth refuses A2A calls without a valid call token (Hub-signed,
	// from Resolve). Off only for a Gateway on loopback (agen up): anonymous
	// callers are then allowed, but their lineage metadata is ignored.
	RequireAuth bool

	mu       sync.Mutex
	tasks    map[string]*Task // recent A2A tasks, for tasks/get
	order    []string
	running  map[string]*manager.Slot
	inflight map[string]chan struct{}
	touched  map[[2]string]time.Time
	// saturatedAt throttles scale-up requests per deployment.
	saturatedAt map[[2]string]time.Time
	// wokeAt throttles wake requests per deployment while calls wait.
	wokeAt   map[[2]string]time.Time
	maxTasks int
}

// New returns a Gateway over a Manager.
func New(f Fleet, baseURL string, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{Fleet: f, BaseURL: strings.TrimRight(baseURL, "/"), WakeTimeout: 60 * time.Second, RunTimeout: time.Hour, Log: log,
		tasks: map[string]*Task{}, running: map[string]*manager.Slot{}, inflight: map[string]chan struct{}{}, touched: map[[2]string]time.Time{}, saturatedAt: map[[2]string]time.Time{}, wokeAt: map[[2]string]time.Time{}, maxTasks: 1000}
}

// Handler serves the A2A routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /a2a/{ns}/{dep}", g.serveRPC)
	mux.HandleFunc("GET /a2a/{ns}/{dep}/.well-known/agent-card.json", g.serveCard)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

// ---- A2A wire types (A2A 0.3, JSON-RPC binding) ----

// Part is a message part; only text parts are used.
type Part struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

// Message is an A2A message.
type Message struct {
	Kind      string         `json:"kind"`
	Role      string         `json:"role"`
	Parts     []Part         `json:"parts"`
	MessageID string         `json:"messageId"`
	ContextID string         `json:"contextId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TaskStatus is an A2A task status.
type TaskStatus struct {
	State     string   `json:"state"`
	Message   *Message `json:"message,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

// Artifact is an A2A task output.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Parts      []Part `json:"parts"`
}

// Task is an A2A task.
type Task struct {
	Kind      string         `json:"kind"`
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	status  int
}

func (e *rpcError) Error() string { return e.Message }

// JSON-RPC / A2A error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeTaskNotFound   = -32001
	codeNotCancelable  = -32002
	// Agen: deployment unknown here, asleep past the wake timeout, or a
	// delegation limit.
	codeUnavailable     = -32050
	codeDepthExceeded   = -32051
	codeUnauthenticated = -32052
)

// caller is who made an A2A call, from its call token.
type caller struct {
	claims calltoken.Claims
	ok     bool   // a valid token was presented
	token  string // the token itself
}

// agent reports whether the caller is an agent, whose delegation lineage
// (parent/root run, depth) the Gateway may record.
func (c caller) agent() bool {
	_, _, ok := c.claims.Agent()
	return c.ok && ok
}

// sub is the caller's token subject ("" for anonymous callers).
func (c caller) sub() string {
	if !c.ok {
		return ""
	}
	return c.claims.Sub
}

// sameCaller reports whether a remembered task was created by this caller
// (anonymous callers only match anonymous tasks).
func sameCaller(t *Task, who caller) bool {
	sub, _ := t.Metadata["agen.caller"].(string)
	if !who.ok {
		return sub == ""
	}
	return sub == who.claims.Sub
}

// authenticate checks the bearer call token of a request for ns/dep.
func (g *Gateway) authenticate(r *http.Request, ns, dep string) (caller, *rpcError) {
	auth := r.Header.Get("Authorization")
	tok, hasBearer := strings.CutPrefix(auth, "Bearer ")
	if auth == "" || !hasBearer || strings.TrimSpace(tok) == "" {
		if g.RequireAuth {
			return caller{}, errf(codeUnauthenticated, http.StatusUnauthorized, "this gateway requires a call token for %s/%s (from the Hub's Resolve)", ns, dep)
		}
		return caller{}, nil
	}
	c, err := calltoken.Verify(strings.TrimSpace(tok), g.Fleet.TokenKeys(), ns+"/"+dep, time.Now())
	if err != nil {
		return caller{}, errf(codeUnauthenticated, http.StatusUnauthorized, "call token refused: %v", err)
	}
	return caller{claims: c, ok: true, token: strings.TrimSpace(tok)}, nil
}

func errf(code, status int, format string, a ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, a...), status: status}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, err *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	if err != nil {
		st := err.status
		if st == 0 {
			st = http.StatusOK
		}
		if st == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		writeJSON(w, st, map[string]any{"jsonrpc": "2.0", "id": id, "error": err})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (g *Gateway) serveRPC(w http.ResponseWriter, r *http.Request) {
	ns, dep := r.PathValue("ns"), r.PathValue("dep")
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeRPC(w, nil, nil, errf(codeParse, http.StatusBadRequest, "invalid JSON: %v", err))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, nil, errf(codeInvalidRequest, http.StatusBadRequest, "not a JSON-RPC 2.0 request"))
		return
	}
	who, aerr := g.authenticate(r, ns, dep)
	if aerr != nil {
		writeRPC(w, req.ID, nil, aerr)
		return
	}
	switch req.Method {
	case "message/send":
		t, err := g.send(r.Context(), ns, dep, req.Params, who, nil)
		writeRPC(w, req.ID, t, err)
	case "message/stream":
		g.stream(w, r, ns, dep, req, who)
	case "tasks/get":
		var p taskParams
		_ = json.Unmarshal(req.Params, &p)
		id := p.taskID(ns, dep, who)
		g.mu.Lock()
		t, ok := g.tasks[id]
		ok = ok && t.Metadata["agen.namespace"] == ns && t.Metadata["agen.deployment"] == dep && sameCaller(t, who)
		var cp Task
		if ok {
			cp = *t
		}
		g.mu.Unlock()
		if !ok {
			writeRPC(w, req.ID, nil, errf(codeTaskNotFound, http.StatusOK, "task %q not found", p.ID))
			return
		}
		writeRPC(w, req.ID, cp, nil)
	case "tasks/cancel":
		var p taskParams
		_ = json.Unmarshal(req.Params, &p)
		id := p.taskID(ns, dep, who)
		g.mu.Lock()
		slot := g.running[id]
		if t, ok := g.tasks[id]; !ok || t.Metadata["agen.namespace"] != ns || t.Metadata["agen.deployment"] != dep || !sameCaller(t, who) {
			slot = nil
		}
		g.mu.Unlock()
		if slot == nil {
			writeRPC(w, req.ID, nil, errf(codeNotCancelable, http.StatusOK, "task %q is not running here", p.ID))
			return
		}
		c, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if _, err := slot.Host.CancelTask(c, id); err != nil {
			writeRPC(w, req.ID, nil, errf(codeInternal, http.StatusOK, "cancel: %v", err))
			return
		}
		writeRPC(w, req.ID, map[string]any{"kind": "task", "id": id, "status": TaskStatus{State: "canceled"}}, nil)
	default:
		writeRPC(w, req.ID, nil, errf(codeMethodNotFound, http.StatusOK, "method %q not supported", req.Method))
	}
}

// taskParams names a task by id, or by the messageId that created it
// (params.metadata.messageId, an Agen extension used to cancel a call).
type taskParams struct {
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata"`
}

func (p taskParams) taskID(ns, dep string, who caller) string {
	if m, _ := p.Metadata["messageId"].(string); p.ID == "" && m != "" {
		return a2aTaskID(ns, dep, who.sub(), m)
	}
	return p.ID
}

// a2aTaskID scopes a caller-chosen message id to the deployment, so the same
// message id sent to two deployments are two tasks.
func a2aTaskID(ns, dep, caller, messageID string) string {
	// The caller is part of the id: another caller reusing a message id gets
	// its own task, on any Gateway and after restarts.
	h := sha256.Sum256([]byte(ns + "/" + dep + "/" + caller + "/" + messageID))
	return "a2a-" + hex.EncodeToString(h[:16])
}

type sendParams struct {
	Message  Message        `json:"message"`
	Metadata map[string]any `json:"metadata"`
}

func meta(p sendParams, key string) any {
	if v, ok := p.Metadata[key]; ok {
		return v
	}
	return p.Message.Metadata[key]
}

func metaString(p sendParams, key string) string {
	s, _ := meta(p, key).(string)
	return s
}

func metaInt(p sendParams, key string) int {
	switch v := meta(p, key).(type) {
	case float64:
		return int(v)
	case string:
		var n int
		fmt.Sscan(v, &n)
		return n
	}
	return 0
}

// send runs one A2A message as an idempotent task on a ready instance. The
// task id is derived from the messageId, so a retried send returns the same
// result instead of running twice. onWorking (optional) is called once the
// task has an instance.
func (g *Gateway) send(ctx context.Context, ns, dep string, raw json.RawMessage, who caller, onWorking func(Task)) (*Task, *rpcError) {
	var p sendParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errf(codeInvalidParams, http.StatusBadRequest, "invalid params: %v", err)
	}
	var text []string
	for _, part := range p.Message.Parts {
		if part.Kind == "text" || (part.Kind == "" && part.Text != "") {
			text = append(text, part.Text)
		}
	}
	if len(text) == 0 {
		return nil, errf(codeInvalidParams, http.StatusBadRequest, "message has no text parts")
	}
	// Lineage is recorded only for authenticated agent callers: anyone else
	// could otherwise attach a run to another trace or reset its depth.
	depth, parentRun, rootRun := 0, "", ""
	if who.agent() {
		depth, parentRun, rootRun = metaInt(p, "agen.depth"), metaString(p, "agen.parent_run_id"), metaString(p, "agen.root_run_id")
		// Without a parent run there is no lineage to check: a root claim
		// alone would attach the run to someone else's tree.
		if parentRun == "" {
			depth, rootRun = 0, ""
		}
	}
	if a, ok := g.Fleet.Assignment(ns, dep); ok && a.MaxDelegationDepth > 0 && depth > int(a.MaxDelegationDepth) {
		return nil, errf(codeDepthExceeded, http.StatusOK, "delegation depth %d exceeds max_delegation_depth %d of %s/%s", depth, a.MaxDelegationDepth, ns, dep)
	}
	msgID := p.Message.MessageID
	if msgID == "" {
		msgID = store.NewID()
	}
	taskID := a2aTaskID(ns, dep, who.sub(), msgID)
	contextID := p.Message.ContextID
	if contextID == "" {
		contextID = store.NewID()
	}
	// The same message again: a finished task is returned as is; a running
	// one is waited for (a second RunTask would fence the first).
	for {
		g.mu.Lock()
		if t, ok := g.tasks[taskID]; ok && !sameCaller(t, who) {
			// Another caller's task (message ids can be guessed): never
			// replay or join it.
			g.mu.Unlock()
			return nil, errf(codeInvalidParams, http.StatusConflict, "message id %q is already used by another caller", msgID)
		} else if ok && t.Status.State != "working" {
			cp := *t
			g.mu.Unlock()
			return &cp, nil
		}
		wait, busy := g.inflight[taskID]
		if !busy {
			done := make(chan struct{})
			g.inflight[taskID] = done
			g.mu.Unlock()
			defer func() {
				g.mu.Lock()
				delete(g.inflight, taskID)
				g.mu.Unlock()
				close(done)
			}()
			break
		}
		g.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, errf(codeUnavailable, http.StatusServiceUnavailable, "request cancelled while the same message was running")
		}
	}
	user := Message{Kind: "message", Role: "user", Parts: p.Message.Parts, MessageID: msgID, ContextID: contextID, TaskID: taskID}
	task := &Task{Kind: "task", ID: taskID, ContextID: contextID, Status: TaskStatus{State: "working", Timestamp: now()},
		History: []Message{user}, Metadata: map[string]any{"agen.namespace": ns, "agen.deployment": dep}}
	if who.ok {
		task.Metadata["agen.caller"] = who.claims.Sub
	}
	host := manager.HostTask{ID: taskID, Input: strings.Join(text, "\n"), ParentRunID: parentRun,
		RootRunID: rootRun, Traceparent: metaString(p, "traceparent"), Depth: depth}
	// A person (user token) asking directly: the run is theirs, so they
	// cannot approve its asks. The signed token itself is recorded, so the
	// Hub can check it (a host cannot forge a requester). Agent calls
	// inherit the root run's requester.
	if strings.HasPrefix(who.claims.Sub, "user:") && who.ok {
		host.RequestedBy = who.token
	}
	// An agent's lineage claims are checked by the host against the Store
	// (the parent run must be a run of the calling deployment).
	if who.agent() {
		host.Caller = who.claims.Sub
	}

	// An instance can die mid-call: the same task id resumes its run on
	// another instance.
	var res manager.HostRunResult
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		slot, rerr := g.acquire(ctx, ns, dep)
		if rerr != nil {
			return nil, rerr
		}
		g.mu.Lock()
		g.running[taskID] = slot
		g.remember(task)
		g.mu.Unlock()
		task.Metadata["agen.instance_id"] = slot.InstanceID
		if onWorking != nil && attempt == 0 {
			onWorking(*task)
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.RunTimeout)
		res, lastErr = slot.Host.RunTask(rctx, host)
		timedOut := rctx.Err() != nil
		cancel()
		if timedOut {
			// No answer in time: cancel it there and report it, instead of
			// running it again elsewhere.
			c, cc := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = slot.Host.CancelTask(c, taskID)
			cc()
			lastErr = &manager.HostError{Code: "deadline_exceeded", Message: fmt.Sprintf("no answer within %s", g.RunTimeout)}
		}
		slot.Release()
		g.mu.Lock()
		delete(g.running, taskID)
		g.mu.Unlock()
		var he *manager.HostError
		if lastErr == nil || (errors.As(lastErr, &he) && he.Code != "resource_exhausted" && he.Code != "unavailable") {
			break
		}
		// Transport errors (the instance died) and deferrals: try another
		// instance; the same task id resumes the run there.
		g.Log.Warn("a2a call failed on instance; retrying", "task", taskID, "instance", slot.InstanceID, "err", lastErr)
		time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
	}
	var he *manager.HostError
	deferred := errors.As(lastErr, &he) && (he.Code == "resource_exhausted" || he.Code == "unavailable")
	if lastErr != nil && (!errors.As(lastErr, &he) || deferred) {
		// Never answered by any instance: not a result. Forget it so a retry
		// of the same message runs (and resumes) it instead of reading a
		// cached failure.
		g.mu.Lock()
		g.forget(taskID)
		g.mu.Unlock()
		return nil, errf(codeUnavailable, http.StatusServiceUnavailable, "%s/%s: no instance could run the call: %v", ns, dep, lastErr)
	}
	switch {
	case lastErr != nil:
		task.Status = TaskStatus{State: "failed", Timestamp: now(), Message: agentText(contextID, taskID, lastErr.Error())}
	case res.Success:
		task.Status = TaskStatus{State: "completed", Timestamp: now()}
		task.Artifacts = []Artifact{{ArtifactID: "output", Parts: []Part{{Kind: "text", Text: res.Output}}}}
		task.History = append(task.History, *agentText(contextID, taskID, res.Output))
	case res.Run.Status == "cancelled":
		task.Status = TaskStatus{State: "canceled", Timestamp: now()}
	default:
		task.Status = TaskStatus{State: "failed", Timestamp: now(), Message: agentText(contextID, taskID, res.Error)}
	}
	if res.Run.ID != "" {
		task.Metadata["agen.run_id"] = res.Run.ID
		task.Metadata["agen.trace_id"] = res.Run.TraceID
	}
	g.mu.Lock()
	g.remember(task)
	cp := *task
	g.mu.Unlock()
	return &cp, nil
}

func agentText(contextID, taskID, text string) *Message {
	return &Message{Kind: "message", Role: "agent", Parts: []Part{{Kind: "text", Text: text}}, MessageID: store.NewID(), ContextID: contextID, TaskID: taskID}
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// remember keeps a bounded set of recent tasks. Caller holds g.mu.
// forget drops a task from the cache and its eviction order. Caller holds g.mu.
func (g *Gateway) forget(id string) {
	if _, ok := g.tasks[id]; !ok {
		return
	}
	delete(g.tasks, id)
	for i, o := range g.order {
		if o == id {
			g.order = append(g.order[:i], g.order[i+1:]...)
			break
		}
	}
}

func (g *Gateway) remember(t *Task) {
	if _, ok := g.tasks[t.ID]; !ok {
		g.order = append(g.order, t.ID)
		for len(g.order) > g.maxTasks {
			delete(g.tasks, g.order[0])
			g.order = g.order[1:]
		}
	}
	// A deep enough copy: the caller keeps changing its task (metadata,
	// history) without g.mu while other requests read the cached one.
	cp := *t
	cp.Metadata = maps.Clone(t.Metadata)
	cp.History = slices.Clone(t.History)
	cp.Artifacts = slices.Clone(t.Artifacts)
	g.tasks[t.ID] = &cp
}

// acquire reserves a slot, waking the deployment if nothing is ready.
func (g *Gateway) acquire(ctx context.Context, ns, dep string) (*manager.Slot, *rpcError) {
	deadline := time.Now().Add(g.WakeTimeout)
	first, woke := true, false
	for {
		if s := g.Fleet.Acquire(ns, dep); s != nil {
			g.touch(ctx, ns, dep)
			return s, nil
		}
		// Keep waking while calls wait (at most once a second per
		// deployment): a waiting call is activity, so a deployment whose
		// instance is slow to start is not scaled back to zero under it, and a
		// failed wake is retried.
		if first || g.wakeDue(ns, dep) {
			first = false
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := g.Fleet.ReportActivity(c, ns, dep, false)
			cancel()
			switch connect.CodeOf(err) {
			case connect.CodeNotFound, connect.CodePermissionDenied:
				return nil, errf(codeUnavailable, http.StatusNotFound, "%s/%s is not served by this gateway: %v", ns, dep, err)
			case connect.CodeFailedPrecondition: // paused
				return nil, errf(codeUnavailable, http.StatusServiceUnavailable, "%s/%s is stopped: %v", ns, dep, err)
			case connect.CodeResourceExhausted: // daily budget used up
				return nil, errf(codeUnavailable, http.StatusServiceUnavailable, "%v", err)
			}
			if err != nil {
				g.Log.Warn("wake request failed; waiting for an instance", "deployment", ns+"/"+dep, "err", err)
			} else if !woke {
				woke = true
				g.Log.Info("woke deployment for a2a call", "deployment", ns+"/"+dep)
			}
		}
		if g.Fleet.Saturated(ns, dep) {
			g.saturated(ctx, ns, dep)
		}
		if time.Now().After(deadline) {
			return nil, errf(codeUnavailable, http.StatusServiceUnavailable, "%s/%s has no ready instance yet; retry later", ns, dep)
		}
		select {
		case <-ctx.Done():
			return nil, errf(codeUnavailable, http.StatusServiceUnavailable, "request cancelled while waiting for an instance")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// wakeDue reports whether a waiting call should wake ns/dep again: at most
// once a second per deployment, however many calls wait.
func (g *Gateway) wakeDue(ns, dep string) bool {
	k := [2]string{ns, dep}
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.wokeAt[k]) < time.Second {
		return false
	}
	g.wokeAt[k] = time.Now()
	return true
}

// saturated tells the Hub, at most every 2s per deployment, that calls are
// waiting on busy instances, so it scales the deployment up.
func (g *Gateway) saturated(ctx context.Context, ns, dep string) {
	k := [2]string{ns, dep}
	g.mu.Lock()
	if time.Since(g.saturatedAt[k]) < 2*time.Second {
		g.mu.Unlock()
		return
	}
	g.saturatedAt[k] = time.Now()
	g.mu.Unlock()
	go func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = g.Fleet.ReportActivity(c, ns, dep, true)
	}()
}

// touch reports traffic at most every 10s per deployment so the autoscaler
// keeps a deployment with A2A traffic awake.
func (g *Gateway) touch(ctx context.Context, ns, dep string) {
	k := [2]string{ns, dep}
	g.mu.Lock()
	last := g.touched[k]
	if time.Since(last) < 10*time.Second {
		g.mu.Unlock()
		return
	}
	g.touched[k] = time.Now()
	g.mu.Unlock()
	go func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = g.Fleet.ReportActivity(c, ns, dep, false)
	}()
}

// stream answers message/stream with server-sent events: the task (working),
// the output artifact, then the final status.
func (g *Gateway) stream(w http.ResponseWriter, r *http.Request, ns, dep string, req rpcRequest, who caller) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRPC(w, req.ID, nil, errf(codeInternal, http.StatusInternalServerError, "streaming unsupported"))
		return
	}
	started := false
	event := func(v any) {
		if !started {
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": v})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	t, err := g.send(r.Context(), ns, dep, req.Params, who, func(t Task) { event(t) })
	if err != nil {
		if !started {
			writeRPC(w, req.ID, nil, err)
			return
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": err})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		return
	}
	for _, a := range t.Artifacts {
		event(map[string]any{"kind": "artifact-update", "taskId": t.ID, "contextId": t.ContextID, "artifact": a, "lastChunk": true})
	}
	event(map[string]any{"kind": "status-update", "taskId": t.ID, "contextId": t.ContextID, "status": t.Status, "final": true, "metadata": t.Metadata})
}

// ---- agent card ----

func (g *Gateway) serveCard(w http.ResponseWriter, r *http.Request) {
	ns, dep := r.PathValue("ns"), r.PathValue("dep")
	a, ok := g.Fleet.Assignment(ns, dep)
	if !ok {
		// Asleep here: waking costs resources, so on a Gateway that requires
		// call tokens only an authenticated caller may wake it for a card.
		if _, aerr := g.authenticate(r, ns, dep); aerr != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": aerr.Message})
			return
		}
		// Wake it and wait for the assignment (not an instance).
		if err := g.Fleet.ReportActivity(r.Context(), ns, dep, false); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("%s/%s is not served by this gateway", ns, dep)})
			return
		}
		deadline := time.Now().Add(g.WakeTimeout)
		for !ok && time.Now().Before(deadline) && r.Context().Err() == nil {
			time.Sleep(100 * time.Millisecond)
			a, ok = g.Fleet.Assignment(ns, dep)
		}
		if !ok {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "deployment is waking up; retry later"})
			return
		}
	}
	dir, err := g.Fleet.DefinitionDir(r.Context(), a.DefinitionDigest)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, agentCard(dir, g.BaseURL+"/a2a/"+ns+"/"+dep, ns, dep))
}

// agentCard builds an A2A agent card from an unpacked bundle.
func agentCard(dir, url, ns, dep string) map[string]any {
	var plugin struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     string `json:"version"`
	}
	for _, p := range []string{filepath.Join(dir, ".claude-plugin", "plugin.json"), filepath.Join(dir, "plugin.json")} {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &plugin)
			break
		}
	}
	if plugin.Version == "" {
		plugin.Version = "0.0.0"
	}
	skills := []map[string]any{}
	entries, _ := os.ReadDir(filepath.Join(dir, "skills"))
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fm := frontmatter(filepath.Join(dir, "skills", n, "SKILL.md"))
		name := fm["name"]
		if name == "" {
			name = n
		}
		skills = append(skills, map[string]any{"id": n, "name": name, "description": fm["description"], "tags": []string{}})
	}
	return map[string]any{
		"protocolVersion":    "0.3.0",
		"name":               plugin.Name,
		"description":        plugin.Description,
		"version":            plugin.Version,
		"url":                url,
		"preferredTransport": "JSONRPC",
		"capabilities":       map[string]any{"streaming": true, "pushNotifications": false},
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain"},
		"skills":             skills,
		"metadata":           map[string]any{"agen.namespace": ns, "agen.deployment": dep},
	}
}

// frontmatter reads simple "key: value" lines between leading --- markers.
func frontmatter(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return out
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}
