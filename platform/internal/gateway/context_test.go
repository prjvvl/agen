package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/prjvvl/agen/platform/internal/testutil"
)

func sendInContext(t *testing.T, url, messageID, contextID string, labels map[string]string) map[string]any {
	t.Helper()
	msg := map[string]any{"kind": "message", "role": "user", "messageId": messageID, "parts": []map[string]string{{"kind": "text", "text": "hi " + messageID}}}
	if contextID != "" {
		msg["contextId"] = contextID
	}
	params := map[string]any{"message": msg}
	if labels != nil {
		params["metadata"] = map[string]any{"agen.labels": labels}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": params})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	res, _ := out["result"].(map[string]any)
	return res
}

// An A2A context continues one conversation; messages of one context run
// one at a time; labels in the message metadata reach the run.
func TestA2AContextsAreConversations(t *testing.T) {
	k := startStack(t)
	ctx := context.Background()
	files := testutil.BundleFiles(t, "hello")
	files["x-agen/config.json"] = []byte(`{"kind":"pool","scale":{"min":1,"max":1,"maxConcurrency":4}}`)
	files["x-agen/fake-script.json"] = []byte(`{"cycle":true,"responses":[{"text":"ok","delayMs":200}]}`)
	k.deploy(t, "chat", files, 1)
	k.waitReady(t, 1)
	url := k.gwURL + "/a2a/default/chat"

	conversation := func(res map[string]any) string {
		meta, _ := res["metadata"].(map[string]any)
		var id string
		k.st.DB().QueryRowContext(ctx, "SELECT conversation_id FROM runs WHERE id = $1", meta["agen.run_id"]).Scan(&id)
		return id
	}
	first := sendInContext(t, url, "m1", "ctx-1", map[string]string{"project": "apollo"})
	if first == nil || first["contextId"] != "ctx-1" {
		t.Fatalf("context not echoed: %v", first)
	}
	var wg sync.WaitGroup
	same := make([]map[string]any, 2)
	for i := range same {
		wg.Add(1)
		go func(i int) { defer wg.Done(); same[i] = sendInContext(t, url, []string{"m2", "m3"}[i], "ctx-1", nil) }(i)
	}
	wg.Wait()
	other := sendInContext(t, url, "m4", "ctx-2", nil)
	generated := sendInContext(t, url, "m5", "", nil)

	c1 := conversation(first)
	for _, r := range same {
		if st, _ := r["status"].(map[string]any); st["state"] != "completed" {
			t.Fatalf("concurrent message in one context: %v", r)
		}
		if conversation(r) != c1 {
			t.Fatalf("same context, different conversation")
		}
	}
	if c := conversation(other); c == "" || c == c1 {
		t.Fatalf("other context shares the conversation: %q", c)
	}
	if generated["contextId"] == "" || conversation(generated) == c1 {
		t.Fatalf("message without a context: %v", generated)
	}
	var labels string
	meta, _ := first["metadata"].(map[string]any)
	k.st.DB().QueryRowContext(ctx, "SELECT labels FROM runs WHERE id = $1", meta["agen.run_id"]).Scan(&labels)
	if labels != `{"project":"apollo"}` {
		t.Fatalf("labels: %s", labels)
	}
	var n int
	k.st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE conversation_id = $1", c1).Scan(&n)
	if n < 6 {
		t.Fatalf("conversation holds %d messages, want the 3 exchanges", n)
	}
	if bad := sendInContext(t, url, "m6", "", map[string]string{"bad key!": "x"}); bad != nil {
		t.Fatalf("invalid labels accepted: %v", bad)
	}
}
