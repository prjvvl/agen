// Package manager is a Nest's execution authority (docs/architecture.md §3,
// §5): it pulls assignments from the Hub, starts and stops agen-host
// instances, leases tasks for them, dispatches them over HostService and
// reports status. The Hub never connects into a Nest.
package manager

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/definition"
	"github.com/prjvvl/agen/platform/internal/pki"
	"github.com/prjvvl/agen/platform/internal/store"
)

// Config configures a Manager.
type Config struct {
	HubURL string
	// JoinToken enrols a new Nest. NestID + NestToken reuse an enrolment.
	JoinToken string
	NestID    string
	NestToken string
	// mTLS (https Hub URL): CAHash pins the Hub CA until the Nest has its
	// certificate; CertPEM/KeyPEM/CAPEM are the saved enrolment.
	CAHash  string
	CertPEM string
	KeyPEM  string
	CAPEM   string

	Name     string
	Backend  string
	Labels   map[string]string
	Capacity int
	// GatewayURL is this Nest's Gateway, reported to the Hub.
	GatewayURL string

	// HostBin is the agen-host executable (native backend).
	HostBin string
	// Kube selects the Kubernetes backend: instances run as pods.
	Kube *KubeConfig
	// ManagerListen is where ManagerService (host callbacks) listens
	// (default 127.0.0.1:0); ManagerURL is how hosts reach it (default: the
	// listen address). Pods need a pod-network address.
	ManagerListen string
	ManagerURL    string
	// StoreURL is given to every instance (AGEN_STORE) for run data.
	StoreURL string
	// StoreURLs overrides StoreURL per namespace (e.g. a Store role limited
	// to that namespace's rows: agen store host-role --namespace).
	StoreURLs map[string]string
	// StoreStrict refuses instances of namespaces without a StoreURLs entry
	// (so none falls back to an unscoped StoreURL).
	StoreStrict bool
	// DataDir holds unpacked definitions.
	DataDir string
	// HostEnv is extra environment for instances (e.g. provider keys).
	HostEnv []string
	// HostMemoryLimit caps each native host's memory in bytes (0: none):
	// a Job Object limit on Windows, RLIMIT_AS on Linux. The Kubernetes
	// backend uses pod limits instead.
	HostMemoryLimit uint64

	Heartbeat    time.Duration // ReportStatus interval
	DispatchPoll time.Duration // lease / reconcile interval
	LeaseSeconds int32
	// DrainTimeout bounds how long a retiring instance may finish its tasks.
	DrainTimeout time.Duration
	// KillGrace is how long a stopped host may take to exit before it is
	// killed.
	KillGrace time.Duration
	// HubLostAfter: when the Hub has been unreachable this long, singleton
	// instances are killed, so a partitioned Nest cannot run a second copy
	// once the Hub reschedules it. Pools keep running. Must be shorter than
	// the Hub's nest_lost_after (minus the heartbeat interval).
	HubLostAfter time.Duration
	// CertRenewBefore: an mTLS Nest renews its certificate when less than
	// this remains (default 10 days).
	CertRenewBefore time.Duration
	// OnCredential persists the Nest's credential when it changes
	// (renewal); the Nest only switches to a renewed certificate once saved.
	OnCredential func(Credential) error
	// StartTimeout bounds how long a new instance may take to listen
	// (default 60s; 5 min on Kubernetes, where images may be pulled).
	StartTimeout time.Duration
	Log          *slog.Logger
}

// Manager runs one Nest.
type Manager struct {
	cfg     Config
	nestPtr atomic.Pointer[agenv1connect.NestServiceClient]
	// credMu guards the credential fields of cfg (renewed while running).
	credMu  sync.Mutex
	nestID  string
	backend backend

	mu          sync.Mutex
	assignments map[[2]string]*agenv1.Assignment
	haveAssign  bool
	instances   map[string]*instance
	backoff     map[[2]string]*restartBackoff
	lastHubOK   time.Time
	wg          sync.WaitGroup

	// ManagerService (host callbacks).
	managerURL     string
	managerSrv     *http.Server
	instanceTokens map[string]string         // token -> instance id
	resolved       map[string]*resolvedEntry // by "caller ns/dep -> ns/dep"
	tokenKeys      map[string]ed25519.PublicKey
	secrets        map[string]string // platform secrets, memory only
}

type restartBackoff struct {
	failures  int
	notBefore time.Time
}

type instance struct {
	id, ns, dep, digest string
	singleton           bool
	token               string // ManagerService credential
	hostToken           string // the Manager's credential for the host's API
	// oneShot instances (task kind) serve exactly one task, then exit.
	oneShot        bool
	served         bool
	maxConcurrency int
	proc           hostProc
	host           *HostClient
	endpoint       string
	state          string // starting | ready | busy | draining | stopped | failed
	message        string
	running        int
	started        time.Time
	exited         chan struct{}
	retiring       bool
	retireAt       time.Time
	terminated     bool
	terminatedAt   time.Time
	killed         bool
	unhealthy      int
}

// New validates the config and returns a Manager.
func New(cfg Config) (*Manager, error) {
	if cfg.HubURL == "" || (cfg.HostBin == "" && cfg.Kube == nil) || cfg.StoreURL == "" || cfg.DataDir == "" {
		return nil, errors.New("manager: HubURL, HostBin (or Kube), StoreURL and DataDir are required")
	}
	if cfg.Name == "" {
		h, _ := os.Hostname()
		cfg.Name = h
	}
	if cfg.Backend == "" {
		cfg.Backend = "native"
		if cfg.Kube != nil {
			cfg.Backend = "kubernetes"
		}
	}
	if k := cfg.Kube; k != nil {
		if k.Client == nil || k.Image == "" {
			return nil, errors.New("manager: the Kubernetes backend needs a client and an image")
		}
		if k.HostPort == 0 {
			k.HostPort = 7080
		}
		if k.Poll <= 0 {
			k.Poll = time.Second
		}
		if k.ImagePullPolicy == "" {
			k.ImagePullPolicy = "IfNotPresent"
		}
		if k.CPULimit == "" {
			k.CPULimit = "1"
		}
		if k.MemoryLimit == "" {
			k.MemoryLimit = "1Gi"
		}
		if cfg.StartTimeout <= 0 {
			cfg.StartTimeout = 5 * time.Minute // first image pulls are slow
		}
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 2 * time.Second
	}
	if cfg.DispatchPoll <= 0 {
		cfg.DispatchPoll = 250 * time.Millisecond
	}
	if cfg.LeaseSeconds <= 0 {
		cfg.LeaseSeconds = 30
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 5 * time.Minute
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 30 * time.Second
	}
	if cfg.HubLostAfter <= 0 {
		// Below the Hub's default nest_lost_after (15s), so a partitioned
		// singleton is gone before the Hub places it elsewhere.
		cfg.HubLostAfter = 10 * time.Second
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 60 * time.Second
	}
	if cfg.CertRenewBefore <= 0 {
		cfg.CertRenewBefore = 10 * 24 * time.Hour
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	m := &Manager{cfg: cfg, nestID: cfg.NestID, assignments: map[[2]string]*agenv1.Assignment{}, instances: map[string]*instance{},
		backoff: map[[2]string]*restartBackoff{}, instanceTokens: map[string]string{}, resolved: map[string]*resolvedEntry{}, secrets: map[string]string{}}
	m.setToken(cfg.NestToken)
	if cfg.Kube != nil {
		m.backend = &kubeBackend{m: m, cfg: *cfg.Kube}
	} else {
		m.backend = nativeBackend{m: m}
	}
	return m, nil
}

// bearerTransport adds the Nest token to every Hub call (unary and streams).
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(r)
}

// hub is the current Hub client (replaced when the certificate is renewed).
func (m *Manager) hub() agenv1connect.NestServiceClient { return *m.nestPtr.Load() }

func (m *Manager) setToken(token string) {
	m.credMu.Lock()
	defer m.credMu.Unlock()
	m.cfg.NestToken = token
	base := http.DefaultTransport
	if strings.HasPrefix(m.cfg.HubURL, "https://") {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ForceAttemptHTTP2 = true
		switch {
		case m.cfg.CAPEM != "":
			cfg, err := pki.ClientTLS(m.cfg.CAPEM, m.cfg.CertPEM, m.cfg.KeyPEM)
			if err != nil {
				m.cfg.Log.Error("bad saved nest certificate", "err", err)
			} else {
				tr.TLSClientConfig = cfg
			}
		case m.cfg.CAHash != "":
			tr.TLSClientConfig = pki.PinnedTLS(m.cfg.CAHash, pki.HostOf(m.cfg.HubURL))
		}
		base = tr
	}
	hc := &http.Client{Transport: &bearerTransport{token: token, base: base}}
	c := agenv1connect.NewNestServiceClient(hc, m.cfg.HubURL, connect.WithProtoJSON())
	m.nestPtr.Store(&c)
}

// Credential is what a Nest keeps to reconnect without a new join token.
type Credential struct {
	NestID, NestToken, CertPEM, KeyPEM, CAPEM string
}

// Credential returns the enrolment to persist.
func (m *Manager) Credential() Credential {
	m.credMu.Lock()
	defer m.credMu.Unlock()
	return Credential{NestID: m.nestID, NestToken: m.cfg.NestToken, CertPEM: m.cfg.CertPEM, KeyPEM: m.cfg.KeyPEM, CAPEM: m.cfg.CAPEM}
}

// NestID is the enrolled Nest id ("" before enrolment).
func (m *Manager) NestID() string { return m.nestID }

// NestToken is the Nest's Hub credential (persist it to restart without a
// new join token).
func (m *Manager) NestToken() string { return m.cfg.NestToken }

// Enroll exchanges the join token for a Nest identity (no-op if enrolled).
func (m *Manager) Enroll(ctx context.Context) error {
	if m.nestID != "" && (m.cfg.NestToken != "" || m.cfg.CertPEM != "") {
		// Check the saved credential. A Hub that no longer knows this nest
		// (new store, revoked token) gets a fresh enrolment if a join token
		// is at hand; otherwise fail loudly instead of running unheard.
		_, err := m.hub().ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: m.nestID,
			Capacity: int32(m.cfg.Capacity), GatewayUrl: m.cfg.GatewayURL}))
		switch connect.CodeOf(err) {
		case connect.CodeUnauthenticated, connect.CodeNotFound, connect.CodePermissionDenied:
			if m.cfg.JoinToken == "" {
				return fmt.Errorf("the hub rejected this nest's saved credential (%v); enrol again with a new join token", err)
			}
			m.cfg.Log.Warn("saved nest credential rejected by the hub; enrolling again", "nest", m.nestID, "err", err)
			m.nestID = ""
			m.setToken("")
		default:
			if err != nil {
				m.cfg.Log.Warn("hub unreachable at start; running with the saved identity", "err", err)
			}
			return nil
		}
	}
	req := &agenv1.EnrollRequest{JoinToken: m.cfg.JoinToken, Name: m.cfg.Name,
		Backend: m.cfg.Backend, Labels: m.cfg.Labels, Capacity: int32(m.cfg.Capacity), GatewayUrl: m.cfg.GatewayURL}
	var key string
	if strings.HasPrefix(m.cfg.HubURL, "https://") {
		// The key never leaves the Nest; the Hub signs the CSR.
		k, csr, err := pki.NewKeyAndCSR(m.cfg.Name)
		if err != nil {
			return err
		}
		key, req.CsrPem = k, csr
		m.cfg.CAPEM, m.cfg.CertPEM, m.cfg.KeyPEM = "", "", ""
		m.setToken("") // pinned-CA transport for the enrolment call
	}
	resp, err := m.hub().Enroll(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("enrol with hub: %w", err)
	}
	m.nestID = resp.Msg.NestId
	if resp.Msg.CertificatePem != "" {
		// The CA we will trust from now on must be the one we pinned.
		if fp, err := pki.PEMFingerprint(resp.Msg.CaPem); err != nil || (m.cfg.CAHash != "" && fp != m.cfg.CAHash) {
			return fmt.Errorf("enrol with hub: the returned CA does not match the pinned CA hash")
		}
		m.cfg.CertPEM, m.cfg.KeyPEM, m.cfg.CAPEM = resp.Msg.CertificatePem, key, resp.Msg.CaPem
	}
	m.setToken(resp.Msg.NestToken)
	m.cfg.Log.Info("nest enrolled", "nest", m.nestID, "name", m.cfg.Name)
	return nil
}

// Run enrols if needed and manages instances until ctx is done; then it
// drains and stops every instance.
func (m *Manager) Run(ctx context.Context) error {
	if err := m.Enroll(ctx); err != nil {
		return err
	}
	if err := m.backend.cleanup(ctx, m.nestID); err != nil {
		return fmt.Errorf("clean up old instances: %w", err)
	}
	if err := m.startManagerService(); err != nil {
		return err
	}
	defer m.managerSrv.Close()
	m.refreshTokenKeys(ctx)
	m.loadResolved()
	go m.watch(ctx)
	tick := time.NewTicker(m.cfg.DispatchPoll)
	defer tick.Stop()
	beat := time.NewTicker(m.cfg.Heartbeat)
	defer beat.Stop()
	beats := 0
	m.report(ctx)
	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return nil
		case <-beat.C:
			m.maybeRenewCert(ctx)
			m.checkHealth(ctx)
			m.report(ctx)
			if beats++; beats%30 == 0 {
				m.refreshTokenKeys(ctx)
				m.refreshResolved(ctx)
			}
		case <-tick.C:
			m.reconcile(ctx)
			m.dispatch(ctx)
		}
	}
}

// watch keeps the latest assignment set. When the Hub is unreachable the
// Manager keeps running its last assignments.
func (m *Manager) watch(ctx context.Context) {
	delay := 200 * time.Millisecond
	for ctx.Err() == nil {
		stream, err := m.hub().WatchAssignments(ctx, connect.NewRequest(&agenv1.WatchAssignmentsRequest{NestId: m.nestID}))
		if err == nil {
			for stream.Receive() {
				delay = 200 * time.Millisecond
				msg := stream.Msg()
				next := map[[2]string]*agenv1.Assignment{}
				for _, a := range msg.Assignments {
					next[[2]string{a.Namespace, a.Deployment}] = a
				}
				m.mu.Lock()
				m.assignments, m.haveAssign = next, true
				m.mu.Unlock()
			}
			err = stream.Err()
			stream.Close()
		}
		if ctx.Err() != nil {
			return
		}
		m.cfg.Log.Warn("assignment stream interrupted; keeping last assignments", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// reconcile starts and retires instances to match the assignments.
func (m *Manager) reconcile(ctx context.Context) {
	m.mu.Lock()
	if !m.haveAssign {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	hubLost := !m.lastHubOK.IsZero() && now.Sub(m.lastHubOK) > m.cfg.HubLostAfter
	live := map[[2]string][]*instance{}
	for _, in := range m.instances {
		select {
		case <-in.exited:
			m.forget(in, now)
			continue
		default:
		}
		k := [2]string{in.ns, in.dep}
		a := m.assignments[k]
		if !in.retiring && (a == nil || a.DefinitionDigest != in.digest) {
			m.retire(ctx, in, "unassigned or definition changed")
		}
		if in.singleton && hubLost && !in.terminated {
			// Stop at once (no drain): the Hub reschedules the singleton
			// elsewhere after its own lost-after, which is longer. The
			// unfinished run is resumed by the new owner (run fencing).
			m.retire(ctx, in, "hub unreachable: singleton may be rescheduled elsewhere")
			in.terminated, in.terminatedAt = true, now
			in.proc.Kill()
		}
		if !in.retiring {
			live[k] = append(live[k], in)
		}
		if in.retiring && !in.terminated && (in.running == 0 || now.After(in.retireAt)) {
			in.terminated, in.terminatedAt = true, now
			in.proc.Terminate()
		}
		// A host that ignores SIGTERM is killed after a grace period.
		if in.terminated && !in.killed && now.Sub(in.terminatedAt) > m.cfg.KillGrace {
			in.killed = true
			in.proc.Kill()
		}
	}
	var starts []*agenv1.Assignment
	for k, a := range m.assignments {
		have := live[k]
		if extra := len(have) - int(a.Count); extra > 0 {
			// Retire the least busy instances first.
			sort.Slice(have, func(i, j int) bool { return have[i].running < have[j].running })
			for _, in := range have[:extra] {
				m.retire(ctx, in, "scaled down")
			}
		}
		if b := m.backoff[k]; b != nil && now.Before(b.notBefore) {
			continue
		}
		if hubLost && a.Kind == "singleton" {
			continue
		}
		for i := len(have); i < int(a.Count); i++ {
			starts = append(starts, a)
		}
	}
	m.mu.Unlock()
	for _, a := range starts {
		if err := m.start(ctx, a); err != nil {
			m.cfg.Log.Error("start instance failed", "deployment", a.Namespace+"/"+a.Deployment, "err", err)
			m.mu.Lock()
			m.noteFailure([2]string{a.Namespace, a.Deployment}, now)
			m.mu.Unlock()
			break
		}
	}
}

// forget drops an exited instance; a crash soon after start backs off
// restarts of that deployment. Caller holds m.mu.
func (m *Manager) forget(in *instance, now time.Time) {
	delete(m.instances, in.id)
	delete(m.instanceTokens, in.token)
	k := [2]string{in.ns, in.dep}
	if in.retiring {
		return
	}
	m.cfg.Log.Warn("instance exited unexpectedly", "instance", in.id, "deployment", in.ns+"/"+in.dep, "message", in.message)
	if now.Sub(in.started) < 10*time.Second {
		m.noteFailure(k, now)
	} else {
		delete(m.backoff, k)
	}
}

func (m *Manager) noteFailure(k [2]string, now time.Time) {
	b := m.backoff[k]
	if b == nil {
		b = &restartBackoff{}
		m.backoff[k] = b
	}
	b.failures++
	d := time.Duration(1<<min(b.failures, 5)) * time.Second
	b.notBefore = now.Add(min(d, 30*time.Second))
}

// retire drains an instance; it is stopped once idle. Caller holds m.mu.
func (m *Manager) retire(ctx context.Context, in *instance, why string) {
	in.retiring, in.state, in.retireAt = true, "draining", time.Now().Add(m.cfg.DrainTimeout)
	m.cfg.Log.Info("retiring instance", "instance", in.id, "deployment", in.ns+"/"+in.dep, "why", why)
	if in.host != nil {
		go func(h *HostClient) {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = h.Drain(c)
		}(in.host)
	}
}

// fetchDefinition gets a definition's files from the Hub and checks them
// against the digest.
func (m *Manager) fetchDefinition(ctx context.Context, digest string) (map[string][]byte, error) {
	resp, err := m.hub().GetDefinition(ctx, connect.NewRequest(&agenv1.GetDefinitionRequest{Digest: digest}))
	if err != nil {
		return nil, fmt.Errorf("fetch definition %s: %w", digest, err)
	}
	files := resp.Msg.GetDefinition().GetFiles()
	if got := definition.Digest(files); got != digest {
		return nil, fmt.Errorf("definition %s: content digest is %s", digest, got)
	}
	return files, nil
}

// definitionDir unpacks a definition once per digest.
func (m *Manager) definitionDir(ctx context.Context, digest string) (string, error) {
	name := strings.NewReplacer(":", "-", "/", "-", "\\", "-").Replace(digest)
	dir := filepath.Join(m.cfg.DataDir, "definitions", name)
	if _, err := os.Stat(filepath.Join(dir, ".complete")); err == nil {
		return dir, nil
	}
	files, err := m.fetchDefinition(ctx, digest)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".unpack-")
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if tmp, err = os.MkdirTemp(filepath.Dir(dir), ".unpack-"); err != nil {
			return "", err
		}
	}
	defer os.RemoveAll(tmp)
	for rel, data := range files {
		p, err := safeJoin(tmp, rel)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		if bytes.HasPrefix(data, []byte("#!")) {
			mode = 0o755 // scripts run by hooks or stdio MCP servers
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, ".complete"), nil, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another start unpacked it concurrently.
		if _, statErr := os.Stat(filepath.Join(dir, ".complete")); statErr == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

// lineWriter calls line for each complete output line. Lines longer than
// 64 KiB are split so a runaway line cannot grow memory without bound.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	line func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) >= 64<<10 {
				w.line(string(w.buf))
				w.buf = w.buf[:0]
			}
			return len(p), nil
		}
		w.line(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
}

// safeJoin resolves a bundle-relative path inside root, rejecting absolute
// paths and "..".
func safeJoin(root, rel string) (string, error) {
	// A drive prefix ("C:x") is refused on every OS, so a bundle is valid or
	// not regardless of where its Nest runs.
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") || strings.Contains(rel, ":") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("definition has an invalid file path %q", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." || part == "" || part == "." {
			return "", fmt.Errorf("definition has an invalid file path %q", rel)
		}
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// storeFor is the Store URL an instance of namespace ns gets.
func (m *Manager) storeFor(ns string) string {
	if u, ok := m.cfg.StoreURLs[ns]; ok {
		return u
	}
	return m.cfg.StoreURL
}

// start launches one agen-host instance for an assignment.
func (m *Manager) start(ctx context.Context, a *agenv1.Assignment) error {
	if _, mapped := m.cfg.StoreURLs[a.Namespace]; m.cfg.StoreStrict && !mapped {
		return fmt.Errorf("namespace %s has no --store-for entry (strict store mapping)", a.Namespace)
	}
	conc := int(a.MaxConcurrency)
	if conc <= 0 {
		conc = 1
	}
	in := &instance{id: store.NewID(), ns: a.Namespace, dep: a.Deployment, digest: a.DefinitionDigest, singleton: a.Kind == "singleton",
		maxConcurrency: conc, state: "starting", started: time.Now(), exited: make(chan struct{})}
	in.oneShot = a.Kind == "task"
	if in.singleton || in.oneShot {
		in.maxConcurrency = 1
	}
	args := []string{"--max-concurrency", fmt.Sprint(in.maxConcurrency), "--namespace", in.ns, "--deployment", in.dep}
	if in.singleton {
		args = append(args, "--singleton")
	}
	m.mu.Lock()
	token := m.newInstanceToken(in.id)
	m.mu.Unlock()
	in.token = token
	in.hostToken = store.NewSecret("agen_host_")
	log := m.cfg.Log.With("instance", in.id, "deployment", in.ns+"/"+in.dep)
	endpoint := make(chan string, 1)
	proc, err := m.backend.start(ctx, hostSpec{id: in.id, ns: in.ns, dep: in.dep, digest: in.digest, args: args, log: log, endpoint: endpoint,
		env: []string{"AGEN_STORE=" + m.storeFor(in.ns), "AGEN_INSTANCE_ID=" + in.id, "AGEN_MANAGER_URL=" + m.managerURL, "AGEN_MANAGER_TOKEN=" + token, "AGEN_HOST_TOKEN=" + in.hostToken}})
	if err != nil {
		m.mu.Lock()
		delete(m.instanceTokens, token)
		m.mu.Unlock()
		return err
	}
	in.proc = proc
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := proc.Wait()
		m.mu.Lock()
		in.state = "stopped"
		if !in.retiring {
			in.state = "failed"
			in.message = fmt.Sprintf("host exited: %v", err)
		}
		m.mu.Unlock()
		close(in.exited)
	}()
	m.mu.Lock()
	m.instances[in.id] = in
	m.mu.Unlock()
	log.Info("instance starting", "host", proc.String())

	go func() {
		select {
		case url := <-endpoint:
			m.mu.Lock()
			in.endpoint = url
			in.host = &HostClient{BaseURL: url, Token: in.hostToken}
			m.mu.Unlock()
			m.checkInstance(ctx, in)
		case <-in.exited:
		case <-time.After(m.cfg.StartTimeout):
			log.Error("instance did not start listening in time")
			m.mu.Lock()
			in.message = "did not start listening in time"
			m.retire(ctx, in, "start timeout")
			m.mu.Unlock()
		}
	}()
	return nil
}

// checkInstance probes Health; an instance that stays unhealthy (e.g. a dead
// MCP server) is replaced.
func (m *Manager) checkInstance(ctx context.Context, in *instance) {
	m.mu.Lock()
	h := in.host
	m.mu.Unlock()
	if h == nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	hl, err := h.Health(c)
	m.mu.Lock()
	defer m.mu.Unlock()
	if in.retiring {
		return
	}
	if err == nil && hl.Ready {
		in.unhealthy = 0
		in.message = ""
		if in.running > 0 {
			in.state = "busy"
		} else {
			in.state = "ready"
		}
		return
	}
	in.unhealthy++
	if err != nil {
		in.message = err.Error()
	} else {
		in.message = "not ready: " + strings.Join(hl.Problems, "; ")
	}
	if in.state != "starting" {
		in.state = "failed"
	}
	if in.unhealthy >= 3 {
		m.retire(ctx, in, "unhealthy: "+in.message)
	}
}

func (m *Manager) checkHealth(ctx context.Context) {
	m.mu.Lock()
	list := make([]*instance, 0, len(m.instances))
	for _, in := range m.instances {
		if in.host != nil && !in.retiring {
			list = append(list, in)
		}
	}
	m.mu.Unlock()
	for _, in := range list {
		m.checkInstance(ctx, in)
	}
}

// report sends the heartbeat and actual instance state.
func (m *Manager) report(ctx context.Context) {
	states := map[string]agenv1.InstanceState{
		"starting": agenv1.InstanceState_INSTANCE_STATE_STARTING, "ready": agenv1.InstanceState_INSTANCE_STATE_READY,
		"busy": agenv1.InstanceState_INSTANCE_STATE_BUSY, "draining": agenv1.InstanceState_INSTANCE_STATE_DRAINING,
		"stopped": agenv1.InstanceState_INSTANCE_STATE_STOPPED, "failed": agenv1.InstanceState_INSTANCE_STATE_FAILED,
	}
	m.mu.Lock()
	req := &agenv1.ReportStatusRequest{NestId: m.nestID, Capacity: int32(m.cfg.Capacity), GatewayUrl: m.cfg.GatewayURL}
	for _, in := range m.instances {
		req.Instances = append(req.Instances, &agenv1.Instance{Id: in.id, Namespace: in.ns, Deployment: in.dep, NestId: m.nestID,
			DefinitionDigest: in.digest, State: states[in.state], Endpoint: in.endpoint, RunningTasks: int32(in.running),
			StartedAt: timestamppb.New(in.started), Message: in.message})
	}
	m.mu.Unlock()
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := m.hub().ReportStatus(c, connect.NewRequest(req)); err != nil {
		if ctx.Err() == nil {
			m.cfg.Log.Warn("report status failed", "err", err)
		}
		return
	}
	m.mu.Lock()
	m.lastHubOK = time.Now()
	m.mu.Unlock()
}

// dispatch leases tasks for instances with free slots.
func (m *Manager) dispatch(ctx context.Context) {
	type slotSet struct {
		ns, dep string
		free    int
	}
	m.mu.Lock()
	byDep := map[[2]string]*slotSet{}
	for _, in := range m.instances {
		if in.retiring || in.host == nil || (in.state != "ready" && in.state != "busy") {
			continue
		}
		k := [2]string{in.ns, in.dep}
		if byDep[k] == nil {
			byDep[k] = &slotSet{ns: in.ns, dep: in.dep}
		}
		byDep[k].free += in.maxConcurrency - in.running
	}
	m.mu.Unlock()
	for _, s := range byDep {
		if s.free <= 0 {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := m.hub().LeaseTasks(c, connect.NewRequest(&agenv1.LeaseTasksRequest{NestId: m.nestID,
			Ref: &agenv1.DeploymentRef{Namespace: s.ns, Name: s.dep}, Max: int32(s.free), LeaseSeconds: m.cfg.LeaseSeconds}))
		cancel()
		if err != nil {
			if ctx.Err() == nil && connect.CodeOf(err) != connect.CodeFailedPrecondition {
				m.cfg.Log.Warn("lease tasks failed", "deployment", s.ns+"/"+s.dep, "err", err)
			}
			continue
		}
		for _, t := range resp.Msg.Tasks {
			in := m.pick(s.ns, s.dep)
			if in == nil {
				// Slots vanished (instance retired); the lease expires and
				// the task is requeued.
				m.cfg.Log.Warn("no free instance for leased task", "task", t.Id)
				continue
			}
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				m.runTask(ctx, in, t)
			}()
		}
	}
}

// pick reserves a slot on the least loaded ready instance of a deployment.
func (m *Manager) pick(ns, dep string) *instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *instance
	for _, in := range m.instances {
		if in.ns != ns || in.dep != dep || in.retiring || in.served || in.host == nil || (in.state != "ready" && in.state != "busy") {
			continue
		}
		if in.running < in.maxConcurrency && (best == nil || in.running < best.running) {
			best = in
		}
	}
	if best != nil {
		best.running++
		best.state = "busy"
		best.served = best.oneShot
	}
	return best
}

func (m *Manager) release(in *instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in.running--
	if in.running == 0 && in.state == "busy" {
		in.state = "ready"
	}
	if in.oneShot && in.running == 0 && !in.retiring {
		// A task instance exits once its task is done; the next task gets a
		// fresh instance.
		m.retire(context.Background(), in, "task finished")
	}
}

// runTask runs one leased task on an instance, keeps its lease alive and
// reports the result with the lease's fencing id.
func (m *Manager) runTask(ctx context.Context, in *instance, t *agenv1.Task) {
	defer m.release(in)
	log := m.cfg.Log.With("task", t.Id, "instance", in.id)
	extend := func(c context.Context, instanceID string) error {
		_, err := m.hub().ExtendLease(c, connect.NewRequest(&agenv1.ExtendLeaseRequest{NestId: m.nestID, TaskId: t.Id,
			LeaseId: t.LeaseId, LeaseSeconds: m.cfg.LeaseSeconds, InstanceId: instanceID}))
		return err
	}
	if err := extend(ctx, in.id); err != nil {
		log.Warn("task no longer leased; skipping", "err", err)
		return
	}
	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	// Keep the lease alive. If the Hub says the lease is gone (task
	// cancelled or re-leased) cancel the run on the host.
	go func() {
		iv := time.Duration(m.cfg.LeaseSeconds) * time.Second / 3
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(iv):
			}
			c, cancel := context.WithTimeout(runCtx, iv)
			err := extend(c, "")
			cancel()
			code := connect.CodeOf(err)
			if err != nil && (code == connect.CodeFailedPrecondition || code == connect.CodeNotFound) {
				log.Info("lease lost; cancelling run on host", "err", err)
				cc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, _ = in.host.CancelTask(cc, t.Id)
				cancel()
				return
			}
		}
	}()
	res, err := in.host.RunTask(runCtx, HostTask{ID: t.Id, Input: t.Input, ParentRunID: t.ParentRunId, RootRunID: t.RootRunId, Traceparent: t.Traceparent,
		Depth: int(t.Depth)})
	stopRun()
	var he *HostError
	switch {
	case errors.As(err, &he) && (he.Code == "resource_exhausted" || he.Code == "unavailable" || he.Code == "aborted" || he.Code == "already_exists"):
		// Not run now: hand it back so another slot takes it.
		log.Info("host deferred task", "code", he.Code, "message", he.Message)
		m.releaseTask(ctx, t, true)
		return
	case errors.As(err, &he) && he.Code == "failed_precondition":
		log.Info("run was taken over elsewhere", "message", he.Message)
		return
	case errors.As(err, &he):
		res = HostRunResult{Success: false, Error: he.Message}
	case err != nil:
		// Host unreachable or died: requeue so the task is re-leased and its
		// run resumed (same task id) on another instance. The attempt counts,
		// so a task that keeps killing hosts eventually fails.
		log.Warn("host call failed; requeueing task", "err", err)
		m.releaseTask(ctx, t, false)
		return
	}
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := m.hub().CompleteTask(c, connect.NewRequest(&agenv1.CompleteTaskRequest{NestId: m.nestID, TaskId: t.Id, Success: res.Success,
		Output: res.Output, Error: res.Error, RunId: res.Run.ID, InstanceId: in.id, LeaseId: t.LeaseId})); err != nil {
		log.Warn("complete task rejected", "err", err)
	}
}

func (m *Manager) releaseTask(ctx context.Context, t *agenv1.Task, notStarted bool) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := m.hub().ReleaseTask(c, connect.NewRequest(&agenv1.ReleaseTaskRequest{NestId: m.nestID, TaskId: t.Id, LeaseId: t.LeaseId,
		NotStarted: notStarted})); err != nil {
		m.cfg.Log.Warn("release task failed; it is retried when its lease expires", "task", t.Id, "err", err)
	}
}

// Instances returns a snapshot of instance ids per "ns/deployment" (tests,
// diagnostics).
func (m *Manager) Instances() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for _, in := range m.instances {
		if !in.retiring {
			k := in.ns + "/" + in.dep
			out[k] = append(out[k], in.id)
		}
	}
	return out
}

// KillInstance kills an instance's process without draining (tests: crash).
func (m *Manager) KillInstance(id string) bool {
	m.mu.Lock()
	in := m.instances[id]
	m.mu.Unlock()
	if in == nil {
		return false
	}
	in.proc.Kill()
	return true
}

// stopAll drains and stops every instance and waits for them to exit.
func (m *Manager) stopAll() {
	m.mu.Lock()
	for _, in := range m.instances {
		if !in.retiring {
			m.retire(context.Background(), in, "manager stopping")
		}
	}
	list := make([]*instance, 0, len(m.instances))
	for _, in := range m.instances {
		list = append(list, in)
	}
	m.mu.Unlock()
	// All instances stop in parallel: at most 30s to finish running tasks,
	// 10s to exit after SIGTERM / pod deletion, then killed. A backend
	// that cannot confirm the kill (API server gone) reports the instance
	// exited after a bounded time of its own.
	deadline := time.Now().Add(30 * time.Second)
	var wg sync.WaitGroup
	for _, in := range list {
		wg.Add(1)
		go func(in *instance) {
			defer wg.Done()
			for {
				m.mu.Lock()
				busy := in.running > 0
				m.mu.Unlock()
				if !busy || time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			m.mu.Lock()
			stop := !in.terminated
			in.terminated = true
			m.mu.Unlock()
			if stop {
				in.proc.Terminate()
			}
			select {
			case <-in.exited:
			case <-time.After(10 * time.Second):
				in.proc.Kill()
				<-in.exited
			}
		}(in)
	}
	wg.Wait()
	m.wg.Wait()
}

// hostEnviron is the Manager's environment without its own AGEN_* settings
// (join/nest/admin tokens, store URL): hosts get only what the Manager sets
// for them, plus HostEnv.
func hostEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "AGEN_") {
			out = append(out, kv)
		}
	}
	return out
}
