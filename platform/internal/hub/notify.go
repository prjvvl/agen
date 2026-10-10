package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/prjvvl/agen/platform/internal/bundle"
	"github.com/prjvvl/agen/platform/internal/store"
)

// NotifyClient sends approval notifications.
var NotifyClient = &http.Client{Timeout: 10 * time.Second}

// notifyApproval POSTs a new pending approval to the deployment's
// permissions.notify URL, if it has one. Failures are logged on the
// deployment; the approval itself is unaffected.
func (h *Hub) notifyApproval(ctx context.Context, a store.Approval) {
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
	if u, err := url.Parse(n.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		fail("the URL must be http(s)")
		return
	}
	approval, err := protojson.Marshal(h.approvalProto(a, h.principalNames(ctx)))
	if err != nil {
		fail(err.Error())
		return
	}
	body, _ := json.Marshal(map[string]any{"type": "approval.pending", "approval": json.RawMessage(approval)})
	signature := ""
	if n.Secret != "" {
		key, err := h.deploymentSecret(ctx, d, n.Secret)
		if err != nil {
			fail(err.Error())
			return
		}
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(body)
		signature = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	var last string
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
		if err != nil {
			fail(err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agen-Event", "approval.pending")
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
			return
		}
		last = resp.Status
		if resp.StatusCode < 500 {
			break
		}
	}
	fail(last)
}
