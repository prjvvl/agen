package cli

import (
	"context"
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
)

// Nest certificates are short-lived and renewed over the current mTLS
// connection before they expire; the old certificate stops working and the
// Nest keeps running work past the first certificate's expiry.
func TestNestCertificateRenewal(t *testing.T) {
	bin := hostBin(t)
	dir := t.TempDir()
	storeURL := "sqlite:" + filepath.ToSlash(filepath.Join(dir, "agen.db"))
	const admin = "admin-token-for-renewal-test"
	t.Setenv("AGEN_ADMIN_TOKEN", admin)
	t.Setenv("AGEN_HOME", dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hubOut := &safeBuf{}
	hubDone := make(chan int, 1)
	go func() {
		hubDone <- Main(ctx, []string{"hub", "serve", "--tls", "--listen", "127.0.0.1:0", "--store", storeURL, "--nest-cert-lifetime", "20s"}, hubOut, &safeBuf{})
	}()
	waitUntil(t, "hub", 30*time.Second, func() bool { return strings.Contains(hubOut.String(), "CA sha256:") })
	hubURL := regexp.MustCompile(`https://\S+`).FindString(hubOut.String())
	caHash := regexp.MustCompile(`sha256:[0-9a-f]{64}`).FindString(hubOut.String())
	cli := func(code int, args ...string) string {
		return agen(t, code, append(args, "--hub", hubURL, "--token", admin, "--ca-hash", caHash)...)
	}
	join := strings.SplitN(cli(0, "join-token"), "\n", 2)[0]
	nestDir := filepath.Join(dir, "nest")
	nestErr := &safeBuf{}
	nestDone := make(chan int, 1)
	go func() {
		nestDone <- Main(ctx, []string{"nest", "run", "--hub", hubURL, "--join-token", join, "--ca-hash", caHash, "--name", "renewing",
			"--store", storeURL, "--data-dir", nestDir, "--gateway-listen", "127.0.0.1:0", "--host-bin", bin, "--cert-renew-before", "14s"}, &safeBuf{}, nestErr)
	}()
	waitUntil(t, "nest enrolled", 30*time.Second, func() bool { return strings.Contains(cli(0, "nests"), "renewing") })
	// The Nest saves its credential right after enrolling, which can be a
	// moment after the Hub lists it.
	var first nestCredential
	waitUntil(t, "nest certificate saved", 10*time.Second, func() bool {
		c, ok := loadNestCredential(nestDir, hubURL)
		first = c
		return ok && c.CertPEM != ""
	})
	firstExpiry, _ := pki.CertNotAfter(first.CertPEM)

	// Renewed (and saved) before it expires.
	var renewed nestCredential
	waitUntil(t, "certificate renewal", 20*time.Second, func() bool {
		c, ok := loadNestCredential(nestDir, hubURL)
		renewed = c
		return ok && c.CertPEM != first.CertPEM && c.KeyPEM != first.KeyPEM
	})
	if exp, _ := pki.CertNotAfter(renewed.CertPEM); !exp.After(firstExpiry) {
		t.Fatalf("renewed certificate expires %s, first %s", exp, firstExpiry)
	}
	// The old certificate no longer authenticates the Nest.
	oldCfg, err := pki.ClientTLS(first.CAPEM, first.CertPEM, first.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig, tr.ForceAttemptHTTP2 = oldCfg, true
	old := agenv1connect.NewNestServiceClient(&http.Client{Transport: tr, Timeout: 10 * time.Second}, hubURL, connect.WithProtoJSON())
	// It stays valid only until the Nest uses the new one (next heartbeat).
	waitUntil(t, "old certificate retired", 10*time.Second, func() bool {
		_, err := old.ReportStatus(ctx, connect.NewRequest(&agenv1.ReportStatusRequest{NestId: first.NestID}))
		return connect.CodeOf(err) == connect.CodeUnauthenticated
	})

	// Past the first certificate's expiry the Nest still takes work.
	time.Sleep(time.Until(firstExpiry) + 2*time.Second)
	cli(0, "deploy", filepath.Join(repoRoot(), "examples", "bundles", "hello"), "--replicas", "1")
	if out := cli(0, "run", "hello", "after expiry"); strings.TrimSpace(out) != "Hello! Nice to meet you." {
		t.Fatalf("run after the first certificate expired: %q\n%s", out, nestErr.String())
	}
	if !strings.Contains(nestErr.String(), "nest certificate renewed") {
		t.Fatalf("no renewal logged:\n%s", nestErr.String())
	}
	cancel()
	<-nestDone
	<-hubDone
}
