package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientCalls(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"message":"no token"}`, http.StatusUnauthorized)
			return
		}
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/agen/pods/missing":
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/agen/pods/p1":
			w.Write([]byte(`{"metadata":{"name":"p1"},"status":{"phase":"Running","podIP":"10.0.0.7"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/agen/pods":
			w.Write([]byte(`{"items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/agen/configmaps":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["metadata"].(map[string]any)["name"] == "exists" {
				http.Error(w, `{"message":"exists"}`, http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodDelete:
			http.Error(w, `{"message":"gone"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"forbidden by RBAC"}`, http.StatusForbidden)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Namespace: "agen", HTTP: srv.Client(), Token: func() string { return "tok" }}
	ctx := context.Background()

	if p, err := c.GetPod(ctx, "p1"); err != nil || p.Status.PodIP != "10.0.0.7" || p.Status.Phase != "Running" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := c.GetPod(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	pods, err := c.ListPods(ctx, "a=b,c=d")
	if err != nil || len(pods) != 2 {
		t.Fatalf("%v %v", pods, err)
	}
	if seen[len(seen)-1] != "GET /api/v1/namespaces/agen/pods?labelSelector=a%3Db%2Cc%3Dd" {
		t.Fatal(seen[len(seen)-1])
	}
	if err := c.CreateConfigMap(ctx, Meta{Name: "new"}, map[string]string{"f0": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateConfigMap(ctx, Meta{Name: "exists"}, nil, nil); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	// Deleting something already gone is fine.
	if err := c.DeletePod(ctx, "gone", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSecret(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	// Other API errors carry the server's message.
	if err := c.CreateSecret(ctx, Meta{Name: "s"}, map[string]string{"K": "v"}); err == nil || !strings.Contains(err.Error(), "forbidden by RBAC") {
		t.Fatal(err)
	}
}
