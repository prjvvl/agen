// Package kube is the small slice of the Kubernetes API the Kubernetes Nest
// backend needs (pods, config maps, secrets), over plain REST with the
// in-cluster service account. It avoids client-go on purpose: the backend
// needs a handful of calls and the agen binary stays small.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// ErrNotFound is returned for a missing object; ErrExists for a create of an
// existing one.
var (
	ErrNotFound = errors.New("kube: not found")
	ErrExists   = errors.New("kube: already exists")
)

// Client calls one namespace of a Kubernetes API server.
type Client struct {
	BaseURL   string
	Namespace string
	HTTP      *http.Client
	// Token is read on every call (the projected token is rotated).
	Token func() string
}

// InCluster returns a client for the pod's own namespace (or namespace, when
// set) using its service account.
func InCluster(namespace string) (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("kube: not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST unset)")
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("kube: bad service account CA")
	}
	if namespace == "" {
		b, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, err
		}
		namespace = strings.TrimSpace(string(b))
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &Client{
		BaseURL:   "https://" + net.JoinHostPort(host, port),
		Namespace: namespace,
		HTTP:      &http.Client{Transport: tr, Timeout: 30 * time.Second},
		Token: func() string {
			b, _ := os.ReadFile(saDir + "/token")
			return strings.TrimSpace(string(b))
		},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != nil {
		if t := c.Token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusConflict:
		return ErrExists
	case resp.StatusCode >= 300:
		var st struct{ Message string }
		_ = json.Unmarshal(data, &st)
		if st.Message == "" {
			st.Message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("kube: %s %s: %s: %s", method, path, resp.Status, st.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) path(kind, name string) string {
	p := "/api/v1/namespaces/" + url.PathEscape(c.Namespace) + "/" + kind
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

// OwnerRef points an object at its owner, so Kubernetes deletes it with the
// owner.
type OwnerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

// Meta is object metadata.
type Meta struct {
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels,omitempty"`
	OwnerReferences   []OwnerRef        `json:"ownerReferences,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
	UID               string            `json:"uid,omitempty"`
}

// Pod is the part of a pod the backend reads back.
type Pod struct {
	Metadata Meta `json:"metadata"`
	Status   struct {
		Phase             string `json:"phase"`
		PodIP             string `json:"podIP"`
		Reason            string `json:"reason"`
		Message           string `json:"message"`
		ContainerStatuses []struct {
			State struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					ExitCode int    `json:"exitCode"`
					Reason   string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// CreatePod creates a pod from a manifest (a map in API JSON shape).
func (c *Client) CreatePod(ctx context.Context, manifest map[string]any) error {
	return c.do(ctx, http.MethodPost, c.path("pods", ""), manifest, nil)
}

// GetPod reads a pod.
func (c *Client) GetPod(ctx context.Context, name string) (Pod, error) {
	var p Pod
	err := c.do(ctx, http.MethodGet, c.path("pods", name), nil, &p)
	return p, err
}

// ListPods lists pods matching a label selector ("k=v,k2=v2").
func (c *Client) ListPods(ctx context.Context, selector string) ([]Pod, error) {
	var out struct{ Items []Pod }
	err := c.do(ctx, http.MethodGet, c.path("pods", "")+"?labelSelector="+url.QueryEscape(selector), nil, &out)
	return out.Items, err
}

// DeletePod deletes a pod with a grace period (0 = kill now). A missing pod
// is not an error.
func (c *Client) DeletePod(ctx context.Context, name string, grace time.Duration) error {
	secs := int64(grace / time.Second)
	err := c.do(ctx, http.MethodDelete, c.path("pods", name), map[string]any{"gracePeriodSeconds": secs, "propagationPolicy": "Background"}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// CreateConfigMap creates a config map; ErrExists when it is already there.
func (c *Client) CreateConfigMap(ctx context.Context, meta Meta, data map[string]string, binary map[string][]byte) error {
	return c.do(ctx, http.MethodPost, c.path("configmaps", ""), map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": data, "binaryData": binary, "immutable": true}, nil)
}

// CreateSecret creates an Opaque secret.
func (c *Client) CreateSecret(ctx context.Context, meta Meta, data map[string]string) error {
	return c.do(ctx, http.MethodPost, c.path("secrets", ""), map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta, "stringData": data}, nil)
}

// DeleteSecret deletes a secret; a missing one is not an error.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, c.path("secrets", name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
