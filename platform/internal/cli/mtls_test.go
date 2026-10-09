package cli

import (
	"context"
	"crypto/tls"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/gen/agen/v1/agenv1connect"
	"github.com/prjvvl/agen/platform/internal/pki"
	"github.com/prjvvl/agen/platform/internal/store"
)

// A TLS Hub issues Nest certificates at enrolment; NestService accepts
// only those certificates; unenrolled or foreign Nests are rejected.
func TestNestEnrolmentWithHubIssuedCertificates(t *testing.T) {
	bin := hostBin(t)
	dir := t.TempDir()
	storeURL := "sqlite:" + filepath.ToSlash(filepath.Join(dir, "agen.db"))
	const admin = "admin-token-for-mtls-test"
	t.Setenv("AGEN_ADMIN_TOKEN", admin)
	t.Setenv("AGEN_HOME", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hubOut := &safeBuf{}
	hubDone := make(chan int, 1)
	go func() {
		hubDone <- Main(ctx, []string{"hub", "serve", "--tls", "--listen", "127.0.0.1:0", "--store", storeURL}, hubOut, &safeBuf{})
	}()
	waitUntil(t, "hub", 30*time.Second, func() bool { return strings.Contains(hubOut.String(), "CA sha256:") })
	hubURL := regexp.MustCompile(`https://\S+`).FindString(hubOut.String())
	caHash := regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindString(hubOut.String())
	cli := func(code int, args ...string) string {
		return agen(t, code, append(args, "--hub", hubURL, "--token", admin, "--ca-hash", caHash)...)
	}
	joinOut := cli(0, "join-token")
	join := strings.SplitN(joinOut, "\n", 2)[0]
	if !strings.Contains(joinOut, "--ca-hash "+caHash) {
		t.Fatalf("join-token should print the CA hash: %s", joinOut)
	}

	// Raw NestService clients for the negative cases.
	nestClient := func(cfg *tls.Config) agenv1connect.NestServiceClient {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig, tr.ForceAttemptHTTP2 = cfg, true
		return agenv1connect.NewNestServiceClient(&http.Client{Transport: tr, Timeout: 10 * time.Second}, hubURL, connect.WithProtoJSON())
	}
	// Enrolling without a CSR is refused, and does not use up the token.
	if _, err := nestClient(pki.PinnedTLS(caHash, pki.HostOf(hubURL))).Enroll(ctx, connect.NewRequest(&agenv1.EnrollRequest{JoinToken: join, Name: "nocsr"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("enrol without CSR: %v", err)
	}
	// A wrong CA pin: the Nest refuses to talk to this Hub.
	var bad safeBuf
	wrong := "sha256:" + strings.Repeat("0", 64)
	if code := Main(ctx, []string{"nest", "run", "--hub", hubURL, "--join-token", join, "--ca-hash", wrong, "--store", storeURL,
		"--data-dir", filepath.Join(dir, "wrong"), "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, &safeBuf{}, &bad); code != 1 || !strings.Contains(bad.String(), "pinned CA") {
		t.Fatalf("wrong CA pin: exit %d %s", code, bad.String())
	}

	// The real Nest enrols (key stays local, the Hub signs its CSR) and runs work.
	nestDone := make(chan int, 1)
	nestErr := &safeBuf{}
	go func() {
		nestDone <- Main(ctx, []string{"nest", "run", "--hub", hubURL, "--join-token", join, "--ca-hash", caHash, "--name", "secure",
			"--store", storeURL, "--data-dir", filepath.Join(dir, "nest"), "--gateway-listen", "127.0.0.1:0", "--host-bin", bin}, &safeBuf{}, nestErr)
	}()
	waitUntil(t, "nest enrolled", 30*time.Second, func() bool { return strings.Contains(cli(0, "nests"), "secure") })
	cli(0, "deploy", filepath.Join(repoRoot(), "examples", "bundles", "hello"), "--replicas", "1")
	if out := cli(0, "run", "hello", "hi over mTLS"); strings.TrimSpace(out) != "Hello! Nice to meet you." {
		t.Fatalf("run: %q\n%s", out, nestErr.String())
	}

	// No client certificate (even with the admin token): refused.
	pinned := nestClient(pki.PinnedTLS(caHash, pki.HostOf(hubURL)))
	req := connect.NewRequest(&agenv1.ReportStatusRequest{NestId: "x"})
	req.Header().Set("Authorization", "Bearer "+admin)
	if _, err := pinned.ReportStatus(ctx, req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no client cert: %v", err)
	}
	// A certificate from another CA fails the TLS handshake.
	otherCA, otherKey, _ := pki.NewCA()
	other, _ := pki.Load(otherCA, otherKey)
	key, csr, _ := pki.NewKeyAndCSR("intruder")
	certPEM, _, _ := other.SignNestCSR(csr, "intruder")
	cfg := pki.PinnedTLS(caHash, pki.HostOf(hubURL))
	pair, _ := tls.X509KeyPair([]byte(certPEM), []byte(key))
	cfg.Certificates = []tls.Certificate{pair}
	if _, err := nestClient(cfg).ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: "x"})); err == nil {
		t.Fatal("foreign certificate accepted")
	}
	// A certificate from the right CA for a Nest that never enrolled.
	st, err := store.Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stored, _ := st.EnsureCA(ctx, nil)
	ca, _ := pki.Load(stored.CertPEM, stored.KeyPEM)
	key, csr, _ = pki.NewKeyAndCSR("ghost")
	certPEM, _, _ = ca.SignNestCSR(csr, "ghost")
	cfg = pki.PinnedTLS(caHash, pki.HostOf(hubURL))
	pair, _ = tls.X509KeyPair([]byte(certPEM), []byte(key))
	cfg.Certificates = []tls.Certificate{pair}
	if _, err := nestClient(cfg).ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: "ghost"})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unenrolled nest: %v", err)
	}

	// Revoking the enrolled Nest: its own certificate stops working, its
	// assignment stream ends and it is listed as revoked.
	cred, ok := loadNestCredential(filepath.Join(dir, "nest"), hubURL)
	if !ok || cred.CertPEM == "" {
		t.Fatal("no saved nest certificate")
	}
	ownCfg, err := pki.ClientTLS(cred.CAPEM, cred.CertPEM, cred.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	own := nestClient(ownCfg)
	if _, err := own.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: cred.NestID})); err != nil {
		t.Fatalf("own certificate before revoke: %v", err)
	}
	if out := cli(0, "nests", "revoke", cred.NestID); !strings.Contains(out, "revoked") {
		t.Fatal(out)
	}
	if _, err := own.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: cred.NestID})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked certificate: %v", err)
	}
	waitUntil(t, "revoked listed", 10*time.Second, func() bool { return strings.Contains(cli(0, "nests"), "revoked") })
	cli(1, "nests", "revoke", "no-such-nest")

	cancel()
	<-nestDone
	<-hubDone
}

// A Hub must not serve bearer tokens over plain HTTP beyond loopback.
func TestHubRefusesPlainHTTPOffLoopback(t *testing.T) {
	t.Setenv("AGEN_ADMIN_TOKEN", "x")
	t.Setenv("AGEN_HOME", t.TempDir())
	var errOut safeBuf
	if code := Main(context.Background(), []string{"hub", "serve", "--listen", "0.0.0.0:0"}, &safeBuf{}, &errOut); code != 1 || !strings.Contains(errOut.String(), "--tls") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, a := range []string{"127.0.0.1:1", "localhost:1", "[::1]:1"} {
		if !loopback(a) {
			t.Fatal(a)
		}
	}
	if loopback(":7070") || loopback("10.0.0.1:1") {
		t.Fatal("non-loopback treated as loopback")
	}
}
