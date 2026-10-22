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
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/store"
)

// NotifyClient sends notifications.
var NotifyClient = &http.Client{Timeout: 10 * time.Second}

// postSigned POSTs body to target, signed with key when it is not empty,
// trying up to attempts times on network errors and 5xx answers.
func postSigned(ctx context.Context, target, event string, body []byte, key string, attempts int) error {
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
	for attempt := 0; attempt < attempts; attempt++ {
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
// permissions.notify URL, if it has one (namespace notification targets get
// it from the event log). Failures are logged on the deployment; the
// approval itself is unaffected.
func (h *Hub) notifyApproval(ctx context.Context, a store.Approval) {
	approval := h.approvalProto(a, h.principalNames(ctx))
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
	if err := postSigned(ctx, n.URL, "approval.pending", body, key, 3); err != nil {
		fail(err.Error())
	}
}

// afterTaskEnded handles what follows a task's end: provider holds, the
// reply to the agent that started it, and the daily budget thresholds.
func (h *Hub) afterTaskEnded(ctx context.Context, taskID string) {
	t, err := h.Store.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	h.providerTaskEnded(ctx, t)
	h.replyResult(ctx, t)
	if d, err := h.Store.GetDeployment(ctx, t.Namespace, t.Deployment); err == nil {
		h.budgetThresholds(ctx, d)
	}
}
