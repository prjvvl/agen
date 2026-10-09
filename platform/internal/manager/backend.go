package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prjvvl/agen/platform/internal/kube"
)

// hostProc is one running agen-host: a local process or a pod.
type hostProc interface {
	// Wait blocks until the host has exited and says why (nil: clean exit).
	Wait() error
	// Terminate asks the host to stop (SIGTERM / pod delete with grace).
	Terminate()
	// Kill stops it at once.
	Kill()
	String() string
}

// hostSpec is what a backend needs to start one instance.
type hostSpec struct {
	id, ns, dep, digest string
	// args follow "serve <definition dir> --listen <addr>".
	args []string
	// env is the host's own AGEN_* settings (store, instance id, manager).
	env []string
	log *slog.Logger
	// endpoint receives the host's base URL once it listens.
	endpoint chan<- string
}

// backend starts instances on some substrate (architecture §5: native
// processes or Kubernetes pods; the Manager logic is the same).
type backend interface {
	start(ctx context.Context, s hostSpec) (hostProc, error)
	// cleanup removes instances left behind by an earlier run of this Nest.
	cleanup(ctx context.Context, nestID string) error
}

// ---- native: agen-host child processes ----

type nativeBackend struct{ m *Manager }

func (b nativeBackend) cleanup(context.Context, string) error { return nil } // children die with the Manager

func (b nativeBackend) start(ctx context.Context, s hostSpec) (hostProc, error) {
	dir, err := b.m.definitionDir(ctx, s.digest)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(b.m.cfg.HostBin, append([]string{"serve", dir, "--listen", "127.0.0.1:0"}, s.args...)...)
	// The store URL and instance id go through the environment so the
	// connection string never appears in process listings.
	cmd.Env = append(append(hostEnviron(), b.m.cfg.HostEnv...), s.env...)
	prepareCmd(cmd)
	// Writers (not pipes) so output is always drained, however long a line
	// is; WaitDelay bounds Wait if a grandchild keeps the handles open.
	cmd.Stdout = &lineWriter{line: func(line string) {
		if url, ok := strings.CutPrefix(line, "listening "); ok {
			select {
			case s.endpoint <- strings.TrimSpace(url):
			default:
			}
			return
		}
		s.log.Debug("host stdout", "line", line)
	}}
	cmd.Stderr = &lineWriter{line: func(line string) { s.log.Info("host", "line", line) }}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if !afterStart(cmd, b.m.cfg.HostMemoryLimit) {
		// A configured limit that cannot be enforced: fail closed.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("could not apply the host memory limit (%d bytes) on this platform", b.m.cfg.HostMemoryLimit)
	}
	return nativeProc{cmd}, nil
}

type nativeProc struct{ cmd *exec.Cmd }

func (p nativeProc) Wait() error    { return p.cmd.Wait() }
func (p nativeProc) Terminate()     { terminate(p.cmd) }
func (p nativeProc) Kill()          { _ = p.cmd.Process.Kill() }
func (p nativeProc) String() string { return fmt.Sprintf("pid %d", p.cmd.Process.Pid) }

// ---- Kubernetes: one pod per instance ----

// KubeConfig selects the Kubernetes backend: the Manager runs in a pod and
// starts each instance as a pod in its namespace.
type KubeConfig struct {
	Client *kube.Client
	// Image runs agen-host (and any stdio MCP servers bundles use).
	Image           string
	ImagePullPolicy string
	// OwnerPod/OwnerUID: the Nest's own pod. Instance pods and their
	// secrets are owned by it, so Kubernetes deletes them with the Nest.
	OwnerPod, OwnerUID string
	// HostPort is the port agen-host listens on inside its pod.
	HostPort int
	// CPULimit / MemoryLimit bound each instance pod (defaults 1 CPU,
	// 1Gi); requests are a tenth of the limits.
	CPULimit, MemoryLimit string
	// Poll is how often pod state is read.
	Poll time.Duration
}

type kubeBackend struct {
	m   *Manager
	cfg KubeConfig
}

const (
	labelManaged = "app.kubernetes.io/managed-by"
	labelNest    = "agen.dev/nest"
	labelInst    = "agen.dev/instance"
	// Namespace/deployment labels are set when the name is a valid label
	// value (they are for selection only; the instance label is the key).
	labelNamespace  = "agen.dev/namespace"
	labelDeployment = "agen.dev/deployment"
)

// maxConfigMapBytes leaves headroom under Kubernetes' 1 MiB object limit
// for keys, metadata and base64 of binary files.
const maxConfigMapBytes = 700 << 10

var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

func kubeName(prefix, id string) string { return prefix + strings.ToLower(id) }

func (b *kubeBackend) owner() []kube.OwnerRef {
	if b.cfg.OwnerPod == "" || b.cfg.OwnerUID == "" {
		return nil
	}
	return []kube.OwnerRef{{APIVersion: "v1", Kind: "Pod", Name: b.cfg.OwnerPod, UID: b.cfg.OwnerUID}}
}

func (b *kubeBackend) cleanup(ctx context.Context, nestID string) error {
	pods, err := b.cfg.Client.ListPods(ctx, labelManaged+"=agen,"+labelNest+"="+strings.ToLower(nestID))
	if err != nil {
		return err
	}
	for _, p := range pods {
		b.m.cfg.Log.Info("removing instance pod left by an earlier run", "pod", p.Metadata.Name)
		if err := b.cfg.Client.DeletePod(ctx, p.Metadata.Name, 0); err != nil {
			return err
		}
		_ = b.cfg.Client.DeleteSecret(ctx, p.Metadata.Name)
	}
	return nil
}

// definitionConfigMap publishes a definition once per digest as an immutable
// config map; pods mount it read-only as the bundle directory.
func (b *kubeBackend) definitionConfigMap(ctx context.Context, digest string) (string, []map[string]any, error) {
	files, err := b.m.fetchDefinition(ctx, digest)
	if err != nil {
		return "", nil, err
	}
	// One config map per digest and Nest pod, owned by that pod: a
	// restarted Nest (new pod) publishes its own copies, and Kubernetes
	// removes the old ones with the old pod.
	owner := "local"
	if b.cfg.OwnerUID != "" {
		owner = strings.ToLower(b.cfg.OwnerUID)
		if len(owner) > 8 {
			owner = owner[:8]
		}
	}
	name := "agen-def-" + owner + "-" + strings.ToLower(strings.NewReplacer(":", "-", "/", "-").Replace(digest))
	if len(name) > 253 {
		name = name[:253]
	}
	total := 0
	for _, c := range files {
		total += len(c)
	}
	if total > maxConfigMapBytes {
		return "", nil, fmt.Errorf("definition %s is %d bytes: the kubernetes backend publishes definitions as config maps, limited to %d bytes", digest, total, maxConfigMapBytes)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		if _, err := safeJoin("/", p); err != nil {
			return "", nil, err
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	data, binary := map[string]string{}, map[string][]byte{}
	var items []map[string]any
	for i, p := range paths {
		key := fmt.Sprintf("f%d", i)
		content := files[p]
		if utf8.Valid(content) {
			data[key] = string(content)
		} else {
			binary[key] = content
		}
		mode := 0o644
		if strings.HasPrefix(string(content), "#!") {
			mode = 0o755 // scripts run by hooks or stdio MCP servers
		}
		items = append(items, map[string]any{"key": key, "path": p, "mode": mode})
	}
	err = b.cfg.Client.CreateConfigMap(ctx, kube.Meta{Name: name, Labels: map[string]string{labelManaged: "agen"}, OwnerReferences: b.owner()}, data, binary)
	if err != nil && !errors.Is(err, kube.ErrExists) {
		return "", nil, fmt.Errorf("publish definition %s: %w", digest, err)
	}
	return name, items, nil
}

func (b *kubeBackend) start(ctx context.Context, s hostSpec) (hostProc, error) {
	cm, items, err := b.definitionConfigMap(ctx, s.digest)
	if err != nil {
		return nil, err
	}
	name := kubeName("agen-", s.id)
	labels := map[string]string{labelManaged: "agen", labelNest: strings.ToLower(b.m.nestID), labelInst: strings.ToLower(s.id)}
	for k, v := range map[string]string{labelNamespace: s.ns, labelDeployment: s.dep} {
		if labelValue.MatchString(v) {
			labels[k] = v
		}
	}
	// The host's settings (store DSN, tokens, provider keys) go in a secret,
	// never in the pod spec.
	env := map[string]string{}
	for _, kv := range append(append([]string{}, b.m.cfg.HostEnv...), s.env...) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	if err := b.cfg.Client.CreateSecret(ctx, kube.Meta{Name: name, Labels: labels, OwnerReferences: b.owner()}, env); err != nil {
		return nil, fmt.Errorf("instance secret: %w", err)
	}
	grace := int64(b.m.cfg.KillGrace / time.Second)
	port := b.cfg.HostPort
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": kube.Meta{Name: name, Labels: labels, OwnerReferences: b.owner()},
		"spec": map[string]any{
			"restartPolicy":                 "Never", // a failed host is replaced by the Manager, like a process
			"terminationGracePeriodSeconds": grace,
			"automountServiceAccountToken":  false,
			"enableServiceLinks":            false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 10001,
				"seccompProfile": map[string]string{"type": "RuntimeDefault"}},
			"containers": []map[string]any{{
				"name":            "host",
				"image":           b.cfg.Image,
				"imagePullPolicy": b.cfg.ImagePullPolicy,
				"command":         []string{"agen-host"},
				"args":            append([]string{"serve", "/agen/definition", "--listen", fmt.Sprintf("0.0.0.0:%d", port)}, s.args...),
				"envFrom":         []map[string]any{{"secretRef": map[string]string{"name": name}}},
				"ports":           []map[string]any{{"name": "host", "containerPort": port}},
				"volumeMounts":    []map[string]any{{"name": "definition", "mountPath": "/agen/definition", "readOnly": true}},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 10001,
					"capabilities": map[string]any{"drop": []string{"ALL"}}, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
				"resources": b.resources(),
			}},
			"volumes": []map[string]any{{"name": "definition", "configMap": map[string]any{"name": cm, "items": items}}},
		},
	}
	if err := b.cfg.Client.CreatePod(ctx, pod); err != nil {
		_ = b.cfg.Client.DeleteSecret(context.WithoutCancel(ctx), name)
		return nil, fmt.Errorf("create pod: %w", err)
	}
	p := &kubeProc{b: b, name: name, done: make(chan struct{})}
	go p.watch(s)
	return p, nil
}

type kubeProc struct {
	b    *kubeBackend
	name string
	done chan struct{}
	once sync.Once
	err  error

	mu       sync.Mutex
	stopAt   time.Time // when Terminate/Kill was first asked
	deadline time.Duration
}

// giveUpAfter bounds how long a stopping instance may stay unconfirmed when
// the API server cannot be reached; the Manager then treats it as gone
// (the pod is owned by the Nest pod, so Kubernetes still removes it).
var giveUpAfter = 90 * time.Second

func (p *kubeProc) String() string { return "pod " + p.name }

func (p *kubeProc) Wait() error {
	<-p.done
	return p.err
}

// delete asks for the pod's deletion, retrying until the API server
// accepts it, the pod has exited, or giveUpAfter has passed.
func (p *kubeProc) delete(grace time.Duration) {
	p.mu.Lock()
	if p.stopAt.IsZero() {
		p.stopAt = time.Now()
	}
	p.mu.Unlock()
	for wait := time.Second; ; wait = min(wait*2, 10*time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := p.b.cfg.Client.DeletePod(ctx, p.name, grace)
		cancel()
		if err == nil || p.stopping() > giveUpAfter {
			if err != nil {
				p.b.m.cfg.Log.Warn("delete pod failed; giving up", "pod", p.name, "err", err)
			}
			return
		}
		p.b.m.cfg.Log.Warn("delete pod failed; retrying", "pod", p.name, "err", err)
		select {
		case <-p.done:
			return
		case <-time.After(wait):
		}
	}
}

// stopping is how long ago the instance was asked to stop (0: not asked).
func (p *kubeProc) stopping() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopAt.IsZero() {
		return 0
	}
	return time.Since(p.stopAt)
}

func (p *kubeProc) Terminate() { go p.delete(p.b.m.cfg.KillGrace) }
func (p *kubeProc) Kill()      { go p.delete(0) }

func (p *kubeProc) exit(err error) {
	p.once.Do(func() {
		p.err = err
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// A finished pod is removed with its secret; the Manager decides
		// whether to start a replacement.
		_ = p.b.cfg.Client.DeletePod(ctx, p.name, 0)
		_ = p.b.cfg.Client.DeleteSecret(ctx, p.name)
		close(p.done)
	})
}

// watch follows the pod: it reports the endpoint once the pod runs and
// ends the instance when the pod finishes or disappears.
func (p *kubeProc) watch(s hostSpec) {
	sent := false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		pod, err := p.b.cfg.Client.GetPod(ctx, p.name)
		cancel()
		switch {
		case errors.Is(err, kube.ErrNotFound):
			p.exit(errors.New("pod deleted"))
			return
		case err != nil:
			s.log.Warn("read pod failed", "pod", p.name, "err", err)
			if p.stopping() > giveUpAfter {
				p.exit(fmt.Errorf("stopped; pod state unknown: %w", err))
				return
			}
		case pod.Status.Phase == "Succeeded":
			p.exit(nil)
			return
		case pod.Status.Phase == "Failed":
			p.exit(fmt.Errorf("pod failed: %s", podReason(pod)))
			return
		case pod.Metadata.DeletionTimestamp != "" && pod.Status.Phase != "Running":
			p.exit(errors.New("pod deleted"))
			return
		case !sent && pod.Status.Phase == "Running" && pod.Status.PodIP != "":
			sent = true
			select {
			case s.endpoint <- fmt.Sprintf("http://%s:%d", pod.Status.PodIP, p.b.cfg.HostPort):
			default:
			}
		case !sent:
			if r := podReason(pod); r != "" {
				s.log.Debug("pod not running yet", "pod", p.name, "state", r)
			}
		}
		time.Sleep(p.b.cfg.Poll)
	}
}

func (b *kubeBackend) resources() map[string]any {
	cpu, mem := b.cfg.CPULimit, b.cfg.MemoryLimit
	return map[string]any{
		"limits":   map[string]string{"cpu": cpu, "memory": mem},
		"requests": map[string]string{"cpu": fraction(cpu), "memory": fraction(mem)},
	}
}

// fraction returns a tenth of a simple quantity ("1" -> "100m", "1Gi" ->
// "102Mi"); anything it does not understand is requested in full.
func fraction(q string) string {
	var n int
	var unit string
	if _, err := fmt.Sscanf(q, "%d%s", &n, &unit); err != nil && unit == "" {
		if _, err := fmt.Sscanf(q, "%d", &n); err != nil {
			return q
		}
	}
	switch unit {
	case "":
		return fmt.Sprintf("%dm", n*100)
	case "m":
		return fmt.Sprintf("%dm", max(n/10, 1))
	case "Gi":
		return fmt.Sprintf("%dMi", max(n*1024/10, 1))
	case "Mi":
		return fmt.Sprintf("%dMi", max(n/10, 1))
	}
	return q
}

func podReason(p kube.Pod) string {
	parts := []string{}
	if p.Status.Reason != "" {
		parts = append(parts, p.Status.Reason)
	}
	for _, c := range p.Status.ContainerStatuses {
		if t := c.State.Terminated; t != nil {
			parts = append(parts, fmt.Sprintf("exit %d %s", t.ExitCode, t.Reason))
		}
		if w := c.State.Waiting; w != nil {
			parts = append(parts, w.Reason)
		}
	}
	return strings.Join(parts, "; ")
}
