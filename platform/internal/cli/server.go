package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/prjvvl/agen/platform/internal/gateway"
	"github.com/prjvvl/agen/platform/internal/hub"
	"github.com/prjvvl/agen/platform/internal/kube"
	"github.com/prjvvl/agen/platform/internal/manager"
	"github.com/prjvvl/agen/platform/internal/mcpserver"
	"github.com/prjvvl/agen/platform/internal/store"
	"github.com/prjvvl/agen/platform/internal/ui"
)

// hubServer serves HubService + NestService and runs the scheduler until ctx
// is done.
type hubServer struct {
	URL  string
	Hub  *hub.Hub
	done chan struct{}
}

// defaultRetention keeps trace data for 30 days.
const defaultRetention = 30 * 24 * time.Hour

// hubOptions tunes startHub.
type hubOptions struct {
	// tlsHosts enables TLS (and Nest mTLS) with a server certificate for
	// these names; nil serves plain HTTP (local mode).
	tlsHosts []string
	extra    []func(*http.ServeMux)
	// nestCertLifetime of Nest certificates (0: pki default).
	nestCertLifetime time.Duration
	// retention of spans, log lines and trigger events (0 keeps them).
	retention time.Duration
}

func startHub(ctx context.Context, st *store.Store, adminToken, listen string, log *slog.Logger, opt hubOptions) (*hubServer, error) {
	h := hub.New(st, adminToken)
	extra := opt.extra
	mux := http.NewServeMux()
	for _, f := range extra {
		f(mux)
	}
	p, hh := h.Handler()
	mux.Handle(p, hh)
	p, nh := h.Nest().Handler()
	mux.Handle(p, nh)
	p, wh := h.WebhookHandler()
	mux.Handle(p, wh)
	p, eh := h.EventsHandler()
	mux.Handle(p, eh)
	// MCP: every HubService RPC as a tool, executed by the same handler.
	mcpH, err := mcpserver.Handler(hh)
	if err != nil {
		return nil, err
	}
	mux.Handle("/mcp", mcpH)
	// The web UI (a client of the same API) at "/".
	mux.Handle("/", ui.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true) // gRPC clients without TLS
	srv := &http.Server{Handler: mux, Protocols: &protocols, ReadHeaderTimeout: 10 * time.Second}
	scheme := "http"
	if opt.tlsHosts != nil {
		tlsCfg, err := h.EnableTLS(ctx, opt.tlsHosts)
		if err != nil {
			ln.Close()
			return nil, err
		}
		srv.TLSConfig = tlsCfg
		h.CA.NestLifetime = opt.nestCertLifetime
		protocols.SetHTTP2(true)
		scheme = "https"
	}
	host, _ := os.Hostname()
	sched := h.NewScheduler(fmt.Sprintf("%s/%d/%s", host, os.Getpid(), store.NewID()))
	sched.Log = log
	sched.Retention = opt.retention
	hs := &hubServer{URL: scheme + "://" + ln.Addr().String(), Hub: h, done: make(chan struct{})}
	schedDone := make(chan struct{})
	go func() { sched.Run(ctx); close(schedDone) }()
	go func() {
		var err error
		if srv.TLSConfig != nil {
			err = srv.ServeTLS(ln, "", "") // certificates are in TLSConfig
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("hub server failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		<-schedDone
		close(hs.done)
	}()
	return hs, nil
}

// nestCredential persists a Nest's enrolment so a restart reuses it.
type nestCredential struct {
	Hub     string `json:"hub"`
	NestID  string `json:"nest_id"`
	Token   string `json:"token,omitempty"`
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
	CAPEM   string `json:"ca_pem,omitempty"`
}

func loadNestCredential(dir, hubURL string) (nestCredential, bool) {
	var c nestCredential
	b, err := os.ReadFile(filepath.Join(dir, "nest.json"))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Hub != hubURL || c.NestID == "" || (c.Token == "" && c.CertPEM == "") {
		return nestCredential{}, false
	}
	return c, true
}

func saveNestCredential(dir string, c nestCredential) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(filepath.Join(dir, "nest.json"), b, 0o600)
}

// findHostBin locates agen-host: explicit flag, AGEN_HOST_BIN, next to this
// executable, then PATH.
func findHostBin(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := os.Getenv("AGEN_HOST_BIN"); v != "" {
		return v, nil
	}
	name := "agen-host"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("agen-host"); err == nil {
		return p, nil
	}
	return "", errors.New("agen-host not found: install it next to agen, put it on PATH, or pass --host-bin / AGEN_HOST_BIN")
}

// runNest runs a Manager and its Gateway, enrolling with joinToken unless
// dataDir already holds a credential for this Hub. The Gateway listens on
// gatewayListen; its public URL is cfg.GatewayURL, or the listen address.
// runNest runs a Nest. requireAuth makes its Gateway refuse A2A calls
// without a Hub-signed call token; it may only be off on loopback.
func runNest(ctx context.Context, cfg manager.Config, joinToken, gatewayListen string, requireAuth bool) error {
	if !requireAuth && !loopback(gatewayListen) {
		return fmt.Errorf("the gateway on %s must authenticate A2A callers (--gateway-auth required)", gatewayListen)
	}
	if c, ok := loadNestCredential(cfg.DataDir, cfg.HubURL); ok {
		cfg.NestID, cfg.NestToken, cfg.CertPEM, cfg.KeyPEM, cfg.CAPEM = c.NestID, c.Token, c.CertPEM, c.KeyPEM, c.CAPEM
	}
	// Kept even with a saved credential: used if the Hub rejects it.
	cfg.JoinToken = joinToken
	ln, err := net.Listen("tcp", gatewayListen)
	if err != nil {
		return fmt.Errorf("gateway listen: %w", err)
	}
	if cfg.GatewayURL == "" {
		cfg.GatewayURL = "http://" + ln.Addr().String()
	}
	// A renewed certificate is saved like the enrolment.
	cfg.OnCredential = func(c manager.Credential) error {
		return saveNestCredential(cfg.DataDir, nestCredential{Hub: cfg.HubURL, NestID: c.NestID, Token: c.NestToken,
			CertPEM: c.CertPEM, KeyPEM: c.KeyPEM, CAPEM: c.CAPEM})
	}
	m, err := manager.New(cfg)
	if err != nil {
		ln.Close()
		return err
	}
	if err := m.Enroll(ctx); err != nil {
		ln.Close()
		return err
	}
	cred := m.Credential()
	if err := saveNestCredential(cfg.DataDir, nestCredential{Hub: cfg.HubURL, NestID: cred.NestID, Token: cred.NestToken,
		CertPEM: cred.CertPEM, KeyPEM: cred.KeyPEM, CAPEM: cred.CAPEM}); err != nil {
		ln.Close()
		return err
	}
	gw := gateway.New(m, cfg.GatewayURL, cfg.Log)
	gw.RequireAuth = requireAuth
	srv := &http.Server{Handler: gw.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cfg.Log.Error("gateway failed", "err", err)
		}
	}()
	cfg.Log.Info("gateway listening", "url", cfg.GatewayURL, "require_call_tokens", requireAuth)
	err = m.Run(ctx)
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(c)
	return err
}

type labelsFlag map[string]string

func (l labelsFlag) String() string { return fmt.Sprint(map[string]string(l)) }
func (l labelsFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("label must be key=value, got %q", v)
	}
	l[k] = val
	return nil
}

// parseBytes reads sizes like 512MiB, 2GiB, 1G or 1048576.
func parseBytes(s string) (uint64, error) {
	units := []struct {
		suffix string
		mult   uint64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}}
	mult := uint64(1)
	for _, u := range units {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = v, u.mult
			break
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("not a size: %q", s)
	}
	return n * mult, nil
}

// loopback reports whether a listen address only accepts local connections.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func defaultStoreURL(home string) string {
	return "sqlite:" + filepath.ToSlash(filepath.Join(home, "agen.db"))
}

func (e *env) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// cmdHub: agen hub serve
func (e *env) cmdHub(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "rotate-kek" {
		return e.cmdRotateKEK(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "rotate-token-key" {
		return e.cmdRotateTokenKey(ctx, args[1:])
	}
	if len(args) == 0 || args[0] != "serve" {
		return usageErr("agen hub serve [--listen ADDR] [--store URL] [--tls [--tls-host H]...]  (admin token from AGEN_ADMIN_TOKEN) | agen hub rotate-kek --store URL | agen hub rotate-token-key --store URL")
	}
	fs := flag.NewFlagSet("hub serve", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	listen := fs.String("listen", "127.0.0.1:7070", "listen address")
	storeURL := fs.String("store", os.Getenv("AGEN_STORE"), "store URL (sqlite:<path> or postgres://...)")
	useTLS := fs.Bool("tls", false, "serve HTTPS with the Hub CA; Nests enrol with certificates (mTLS)")
	var hosts listFlag
	fs.Var(&hosts, "tls-host", "extra host name or IP for the Hub certificate (repeatable)")
	nestCertLifetime := fs.Duration("nest-cert-lifetime", 0, "lifetime of Nest client certificates (default 30 days; Nests renew before expiry)")
	retention := fs.Duration("retention", defaultRetention, "delete spans, log lines and trigger events older than this (0 keeps them)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	admin := os.Getenv("AGEN_ADMIN_TOKEN")
	if admin == "" {
		return errors.New("set AGEN_ADMIN_TOKEN (the admin bearer token)")
	}
	if !*useTLS && !loopback(*listen) && os.Getenv("AGEN_INSECURE") != "1" {
		return errors.New("refusing to serve bearer tokens over plain HTTP on " + *listen + ": use --tls (or AGEN_INSECURE=1 behind a TLS-terminating proxy)")
	}
	if *storeURL == "" {
		if err := os.MkdirAll(e.home, 0o700); err != nil {
			return err
		}
		*storeURL = defaultStoreURL(e.home)
	}
	st, err := store.Open(ctx, *storeURL)
	if err != nil {
		return err
	}
	defer st.Close()
	// Hub secrets in the Store are sealed with a key only Hubs hold; the
	// previous KEK still opens values during a rotation.
	st.SetKEK(os.Getenv("AGEN_HUB_KEK"))
	st.SetPreviousKEK(os.Getenv("AGEN_HUB_KEK_PREVIOUS"))
	var tlsHosts []string
	if *useTLS {
		tlsHosts = append([]string{"localhost", "127.0.0.1", "::1"}, hosts...)
		if hn, err := os.Hostname(); err == nil {
			tlsHosts = append(tlsHosts, hn)
		}
	}
	if *useTLS && os.Getenv("AGEN_HUB_KEK") == "" {
		e.logger().Warn("AGEN_HUB_KEK is not set: the Hub's CA and call-token keys are stored unencrypted in the Store")
	}
	hs, err := startHub(ctx, st, admin, *listen, e.logger(), hubOptions{tlsHosts: tlsHosts, nestCertLifetime: *nestCertLifetime, retention: *retention})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "hub listening %s\n", hs.URL)
	if hs.Hub.CA != nil {
		fmt.Fprintf(e.stdout, "CA %s\n", hs.Hub.CA.Hash())
	}
	<-hs.done
	return nil
}

// cmdNest: agen nest run
func (e *env) cmdNest(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return usageErr("agen nest run --hub URL [--join-token T] [--name N] [--capacity C] [--label k=v] [--store URL] [--data-dir D] [--backend native|kubernetes]")
	}
	fs := flag.NewFlagSet("nest run", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	hubURL := fs.String("hub", os.Getenv("AGEN_HUB"), "Hub URL")
	join := fs.String("join-token", os.Getenv("AGEN_JOIN_TOKEN"), "one-time join token (first start only)")
	name := fs.String("name", "", "nest name (default: hostname)")
	capacity := fs.Int("capacity", 8, "maximum instances (0 = unlimited)")
	labels := labelsFlag{}
	fs.Var(labels, "label", "nest label key=value (repeatable)")
	storeURL := fs.String("store", os.Getenv("AGEN_STORE"), "store URL given to instances for run data")
	storeFor := labelsFlag{}
	fs.Var(storeFor, "store-for", "NAMESPACE=URL: store URL for that namespace's instances, e.g. a role limited to it (repeatable)")
	hostMem := fs.String("host-memory-limit", "", "native backend: cap each agent host's memory, e.g. 1GiB (Windows job limit, Linux RLIMIT_AS)")
	storeStrict := fs.Bool("store-for-only", false, "run only namespaces that have a --store-for entry (no fallback to --store)")
	dataDir := fs.String("data-dir", "", "nest state directory (default: AGEN_HOME/nests/<name>)")
	hostBin := fs.String("host-bin", "", "agen-host executable")
	gatewayURL := fs.String("gateway-url", "", "public URL of this nest's Gateway (default: from --gateway-listen)")
	gatewayListen := fs.String("gateway-listen", "127.0.0.1:7071", "Gateway listen address")
	caHash := fs.String("ca-hash", os.Getenv("AGEN_CA_HASH"), "pin the Hub CA (from 'agen join-token') for an https Hub")
	backend := fs.String("backend", "native", "where instances run: native (child processes) or kubernetes (pods; run the nest in a pod)")
	kubeImage := fs.String("kube-image", os.Getenv("AGEN_KUBE_IMAGE"), "kubernetes: image with agen-host for instance pods")
	kubeNamespace := fs.String("kube-namespace", "", "kubernetes: namespace for instance pods (default: the nest pod's)")
	managerListen := fs.String("manager-listen", "", "ManagerService listen address for host callbacks (kubernetes default 0.0.0.0:7072)")
	managerURL := fs.String("manager-url", "", "URL hosts use to reach ManagerService (kubernetes default http://$POD_IP:<port>)")
	var hostEnv listFlag
	fs.Var(&hostEnv, "host-env", "pass this variable from the nest's environment to every instance, e.g. a provider key (repeatable)")
	certRenewBefore := fs.Duration("cert-renew-before", 0, "mTLS: renew the Nest certificate when less than this remains (default 10 days)")
	gatewayAuth := fs.String("gateway-auth", "required", "required: A2A callers need a Hub-signed call token; optional: anonymous callers allowed (loopback gateways only)")
	hostEnvDir := fs.String("host-env-dir", "", "pass each file in this directory (name = variable, content = value; e.g. a mounted secret) to every instance")
	kubeCPU := fs.String("kube-cpu-limit", "1", "kubernetes: CPU limit per instance pod")
	kubeMemory := fs.String("kube-memory-limit", "1Gi", "kubernetes: memory limit per instance pod")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var extraEnv []string
	for _, name := range hostEnv {
		if strings.HasPrefix(strings.ToUpper(name), "AGEN_") {
			return fmt.Errorf("--host-env %s: AGEN_* settings are the nest's own and are never passed on", name)
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return fmt.Errorf("--host-env %s: not set in the nest's environment", name)
		}
		extraEnv = append(extraEnv, name+"="+v)
	}
	if *hostEnvDir != "" {
		fromDir, err := readHostEnvDir(*hostEnvDir)
		if err != nil {
			return err
		}
		extraEnv = append(extraEnv, fromDir...)
	}
	if *hubURL == "" || *storeURL == "" {
		return errors.New("--hub and --store (or AGEN_HUB / AGEN_STORE) are required")
	}
	if *name == "" {
		*name, _ = os.Hostname()
	}
	if *dataDir == "" {
		// Separate from agen up's local nest (AGEN_HOME/nest), so two nests
		// on one machine never share an identity.
		*dataDir = filepath.Join(e.home, "nests", *name)
	}
	cfg := manager.Config{HubURL: strings.TrimRight(*hubURL, "/"), Name: *name, Capacity: *capacity, Labels: labels,
		StoreURL: *storeURL, DataDir: *dataDir, GatewayURL: *gatewayURL, CAHash: *caHash, Log: e.logger(),
		ManagerListen: *managerListen, ManagerURL: *managerURL, HostEnv: extraEnv, StoreURLs: storeFor, StoreStrict: *storeStrict, CertRenewBefore: *certRenewBefore}
	if *hostMem != "" {
		n, err := parseBytes(*hostMem)
		if err != nil {
			return fmt.Errorf("--host-memory-limit: %w", err)
		}
		cfg.HostMemoryLimit = n
	}
	switch *backend {
	case "native":
		bin, err := findHostBin(*hostBin)
		if err != nil {
			return err
		}
		cfg.HostBin = bin
	case "kubernetes":
		if *hostMem != "" {
			return errors.New("--host-memory-limit is for the native backend; use --kube-memory-limit with kubernetes")
		}
		kc, err := kube.InCluster(*kubeNamespace)
		if err != nil {
			return err
		}
		if *kubeImage == "" {
			return errors.New("--kube-image (or AGEN_KUBE_IMAGE) is required for the kubernetes backend")
		}
		// POD_NAME / POD_UID / POD_IP come from the downward API.
		cfg.Kube = &manager.KubeConfig{Client: kc, Image: *kubeImage, OwnerPod: os.Getenv("POD_NAME"), OwnerUID: os.Getenv("POD_UID"),
			CPULimit: *kubeCPU, MemoryLimit: *kubeMemory}
		if cfg.ManagerListen == "" {
			cfg.ManagerListen = "0.0.0.0:7072"
		}
		if cfg.ManagerURL == "" {
			ip := os.Getenv("POD_IP")
			if ip == "" {
				return errors.New("set POD_IP (downward API) or --manager-url for the kubernetes backend")
			}
			_, port, _ := net.SplitHostPort(cfg.ManagerListen)
			cfg.ManagerURL = "http://" + net.JoinHostPort(ip, port)
		}
	default:
		return fmt.Errorf("unknown --backend %q (native or kubernetes)", *backend)
	}
	if *gatewayAuth != "required" && *gatewayAuth != "optional" {
		return fmt.Errorf("--gateway-auth must be required or optional")
	}
	return runNest(ctx, cfg, *join, *gatewayListen, *gatewayAuth == "required")
}

// readHostEnvDir reads NAME=value pairs from the files of a directory (a
// mounted Kubernetes secret: its "..data" entries are skipped). A missing
// directory passes nothing.
func readHostEnvDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(name), "AGEN_") {
			return nil, fmt.Errorf("--host-env-dir: %s: AGEN_* settings are never passed to instances", name)
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if st, serr := os.Stat(filepath.Join(dir, name)); serr == nil && st.IsDir() {
				continue
			}
			return nil, err
		}
		out = append(out, name+"="+strings.TrimRight(string(b), "\r\n"))
	}
	return out, nil
}

// cmdDown stops the `agen up` whose credentials are in AGEN_HOME and waits
// until its Hub is gone.
func (e *env) cmdDown(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usageErr("agen down")
	}
	lc, err := loadLocalConfig(e.home)
	if err != nil {
		return errors.New("no local agen found (run 'agen up' first)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lc.Hub+"/local/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+lc.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(e.stdout, "agen is not running")
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("shutdown refused: %s", resp.Status)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if r, err := http.Get(lc.Hub + "/healthz"); err != nil {
			fmt.Fprintln(e.stdout, "agen is down")
			return nil
		} else {
			r.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("agen did not stop within 2 minutes")
}

// cmdUp: agen up — Hub + one Nest in this process with SQLite.
func (e *env) cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	listen := fs.String("listen", "127.0.0.1:7070", "Hub listen address")
	storeURL := fs.String("store", "", "store URL (default: sqlite in AGEN_HOME)")
	capacity := fs.Int("capacity", 8, "local nest capacity (0 = unlimited)")
	hostBin := fs.String("host-bin", "", "agen-host executable")
	gatewayListen := fs.String("gateway-listen", "127.0.0.1:7071", "local Gateway (A2A) listen address")
	retention := fs.Duration("retention", defaultRetention, "delete spans, log lines and trigger events older than this (0 keeps them)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	bin, err := findHostBin(*hostBin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.home, 0o700); err != nil {
		return err
	}
	if *storeURL == "" {
		*storeURL = defaultStoreURL(e.home)
	}
	st, err := store.Open(ctx, *storeURL)
	if err != nil {
		return err
	}
	defer st.Close()
	// Hub secrets in the Store are sealed with a key only Hubs hold; the
	// previous KEK still opens values during a rotation.
	st.SetKEK(os.Getenv("AGEN_HUB_KEK"))
	st.SetPreviousKEK(os.Getenv("AGEN_HUB_KEK_PREVIOUS"))
	// Reuse the local admin token across restarts.
	cfg, _ := loadLocalConfig(e.home)
	if cfg.Token == "" {
		cfg.Token = store.NewSecret("agen_admin_")
	}
	log := e.logger()
	// `agen down` asks this process to stop (a portable alternative to
	// signals); admin token required.
	shutdown := func(mux *http.ServeMux) {
		mux.HandleFunc("POST /local/shutdown", func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+cfg.Token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			cancel()
		})
	}
	hs, err := startHub(ctx, st, cfg.Token, *listen, log, hubOptions{extra: []func(*http.ServeMux){shutdown}, retention: *retention})
	if err != nil {
		return err
	}
	cfg.Hub = hs.URL
	if err := saveLocalConfig(e.home, cfg); err != nil {
		return err
	}
	nestDir := filepath.Join(e.home, "nest")
	// Always at hand, so a stale saved nest credential (e.g. a new store) is
	// replaced by a fresh enrolment instead of a silent, unheard nest.
	join, err := st.CreateJoinToken(ctx, int64(time.Minute/time.Millisecond))
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "agen is up: hub %s (credentials in %s)\n", hs.URL, filepath.Join(e.home, "local.json"))
	fmt.Fprintf(e.stdout, "web UI: %s/ (run 'agen ui' for a sign-in link)\n", hs.URL)
	nestErr := make(chan error, 1)
	go func() {
		nestErr <- runNest(ctx, manager.Config{HubURL: hs.URL, Name: "local", Capacity: *capacity, HostBin: bin, StoreURL: *storeURL,
			DataDir: nestDir, Log: log}, join, *gatewayListen, !loopback(*gatewayListen))
	}()
	var runErr error
	select {
	case <-ctx.Done():
		runErr = <-nestErr
	case runErr = <-nestErr:
	}
	cancel()
	<-hs.done
	return runErr
}

// cmdMigrate copies a store (by default the local SQLite store of `agen up`)
// into an empty target store (usually Postgres for a distributed fleet).
func (e *env) cmdMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	from := fs.String("from", "", "source store URL (default: the local agen up store)")
	to := fs.String("to", "", "target store URL, empty (e.g. postgres://user:pass@host/db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		return usageErr("agen migrate --to postgres://... [--from sqlite:<path>]")
	}
	if *from == "" {
		*from = defaultStoreURL(e.home)
		// A running local fleet would keep writing to the source.
		if lc, err := loadLocalConfig(e.home); err == nil {
			c := &http.Client{Timeout: 2 * time.Second}
			if r, err := c.Get(lc.Hub + "/healthz"); err == nil {
				r.Body.Close()
				return errors.New("the local agen is running: stop it with 'agen down' first")
			}
		}
	}
	src, err := store.Open(ctx, *from)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer src.Close()
	dst, err := store.Open(ctx, *to)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	defer dst.Close()
	rep, err := src.CopyTo(ctx, dst)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TABLE\tROWS")
	for _, t := range store.CopyTables {
		fmt.Fprintf(w, "%s\t%d\n", t, rep[t])
	}
	w.Flush()
	fmt.Fprintln(e.stdout, "migrated; start the Hub with the new store, e.g. agen up --store <target> or agen hub serve --store <target>")
	return nil
}

// cmdRotateKEK: agen hub rotate-kek — re-seal the Hub's keys and platform
// secrets from AGEN_HUB_KEK to AGEN_HUB_KEK_NEW (both from the environment,
// never the command line). Then restart every Hub with the new KEK.
func (e *env) cmdRotateKEK(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hub rotate-kek", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	storeURL := fs.String("store", os.Getenv("AGEN_STORE"), "store URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	next := os.Getenv("AGEN_HUB_KEK_NEW")
	if *storeURL == "" || next == "" {
		return errors.New("set --store (or AGEN_STORE), AGEN_HUB_KEK (current, if any) and AGEN_HUB_KEK_NEW")
	}
	st, err := store.Open(ctx, *storeURL)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetKEK(os.Getenv("AGEN_HUB_KEK"))
	n, err := st.RotateKEK(ctx, next)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "re-sealed %d secrets: restart every Hub with AGEN_HUB_KEK set to the new value\n", n)
	return nil
}

// cmdRotateTokenKey: agen hub rotate-token-key — add a new call-token key.
// Hubs publish it at once and sign with it after a short grace; tokens
// signed with the previous key stay valid until they expire.
func (e *env) cmdRotateTokenKey(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hub rotate-token-key", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	storeURL := fs.String("store", os.Getenv("AGEN_STORE"), "store URL")
	force := fs.Bool("force", false, "rotate even if the current key is younger than an agent token's lifetime plus grace (live tokens may stop verifying)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storeURL == "" {
		return errors.New("set --store (or AGEN_STORE), and AGEN_HUB_KEK if the Hubs use one")
	}
	st, err := store.Open(ctx, *storeURL)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetKEK(os.Getenv("AGEN_HUB_KEK"))
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	minAge := hub.MinTokenKeyRotationAge()
	if *force {
		minAge = 0
	}
	if err := st.RotateTokenKey(ctx, base64.StdEncoding.EncodeToString(seed), minAge); err != nil {
		if errors.Is(err, store.ErrTooSoon) {
			return fmt.Errorf("the current call-token key is younger than %s: rotating now could invalidate live tokens (use --force to do it anyway)", minAge)
		}
		return err
	}
	fmt.Fprintln(e.stdout, "new call-token key added: Hubs sign with it within a few minutes; the previous key still verifies")
	return nil
}
