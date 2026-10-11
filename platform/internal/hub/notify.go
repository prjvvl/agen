package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/store"
)

// NotifyClient sends notifications.
var NotifyClient = &http.Client{Timeout: 10 * time.Second}

// postSigned POSTs body to target, signed with key when it is not empty,
// retrying network errors and 5xx answers.
func postSigned(ctx context.Context, target, event string, body []byte, key string) error {
	if u, err := url.Parse(target); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("the URL must be http(s)")
	}
	signature := ""
	if key != "" {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(body)
		signature = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	var last string
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agen-Event", event)
		if signature != "" {
			req.Header.Set("X-Agen-Signature", signature)
		}
		resp, err := NotifyClient.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		last = resp.Status
		if resp.StatusCode < 500 {
			break
		}
	}
	return errors.New(last)
}

// notifyApproval sends a new pending approval to the deployment's
// permissions.notify URL, if it has one, and to its namespace's targets.
// Failures are logged on the deployment; the approval itself is unaffected.
func (h *Hub) notifyApproval(ctx context.Context, a store.Approval) {
	approval := h.approvalProto(a, h.principalNames(ctx))
	h.notifyTargets(ctx, a.Namespace, a.Deployment, "approval.pending", "approval", approval)
	d, err := h.Store.GetDeployment(ctx, a.Namespace, a.Deployment)
	if err != nil {
		return
	}
	def, err := h.Store.GetDefinition(ctx, d.DefinitionDigest)
	if err != nil {
		return
	}
	var cfg struct {
		Permissions struct {
			Notify *bundle.Notify `json:"notify"`
		} `json:"permissions"`
	}
	if json.Unmarshal(def.Files["x-agen/config.json"], &cfg) != nil || cfg.Permissions.Notify == nil {
		return
	}
	n := cfg.Permissions.Notify
	fail := func(msg string) {
		_ = h.Store.AppendLog(ctx, "", a.Namespace, a.Deployment, "warn", fmt.Sprintf("approval %s: notification to %s failed: %s", a.ID, n.URL, msg))
	}
	raw, err := protojson.Marshal(approval)
	if err != nil {
		fail(err.Error())
		return
	}
	body, _ := json.Marshal(map[string]any{"type": "approval.pending", "approval": json.RawMessage(raw)})
	key := ""
	if n.Secret != "" {
		if key, err = h.deploymentSecret(ctx, d, n.Secret); err != nil {
			fail(err.Error())
			return
		}
	}
	if err := postSigned(ctx, n.URL, "approval.pending", body, key); err != nil {
		fail(err.Error())
	}
}

// notifyTargets sends an event to the namespace's notification targets that
// want it: {"type": event, field: <msg as protojson>}.
func (h *Hub) notifyTargets(ctx context.Context, ns, deployment, event, field string, msg proto.Message) {
	targets, err := h.Store.ListNotificationTargets(ctx, ns)
	if err != nil || len(targets) == 0 {
		return
	}
	raw, err := protojson.Marshal(msg)
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"type": event, field: json.RawMessage(raw)})
	var wg sync.WaitGroup
	for _, t := range targets {
		if len(t.Events) > 0 && !slices.Contains(t.Events, event) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := h.Store.NotificationSecret(ctx, t.Namespace, t.Name)
			if err == nil {
				err = postSigned(ctx, t.URL, event, body, key)
			}
			if err != nil {
				_ = h.Store.AppendLog(ctx, "", ns, deployment, "warn", fmt.Sprintf("%s: notification to %s (%s) failed: %s", event, t.Name, t.URL, err))
			}
		}()
	}
	wg.Wait()
}

// budgetNotified remembers the UTC day a deployment's exhausted budget was
// last announced, so it is announced once a day.
var budgetNotified sync.Map

// afterTaskEnded sends task.failed for a failed task and budget.exhausted
// when the deployment's daily budget just ran out.
func (h *Hub) afterTaskEnded(ctx context.Context, taskID string) {
	t, err := h.Store.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	if t.State == "failed" {
		h.notifyTargets(ctx, t.Namespace, t.Deployment, "task.failed", "task", TaskProto(t))
	}
	d, err := h.Store.GetDeployment(ctx, t.Namespace, t.Deployment)
	if err != nil {
		return
	}
	if _, exhausted, err := h.spentToday(ctx, d); err == nil && exhausted {
		day := dayStartMs()
		if prev, loaded := budgetNotified.Swap(d.Namespace+"/"+d.Name, day); !loaded || prev.(int64) != day {
			h.notifyTargets(ctx, d.Namespace, d.Name, "budget.exhausted", "deployment", h.withBudget(ctx, d, deploymentProto(d, h.ready(ctx, d.Namespace, d.Name))))
		}
	}
}
