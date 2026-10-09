package pki

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testCA(t *testing.T) *Authority {
	t.Helper()
	certPEM, keyPEM, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := Load(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// serve starts an HTTPS server with a certificate from ca for hosts.
func serve(t *testing.T, ca *Authority, hosts []string) *httptest.Server {
	t.Helper()
	cert, err := ca.ServerCert(hosts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func get(cfg *tls.Config, url string) error {
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := c.Get(url)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestPinnedTLSChecksCAAndHost(t *testing.T) {
	ca := testCA(t)
	good := serve(t, ca, []string{"127.0.0.1"})
	if err := get(PinnedTLS(ca.Hash(), HostOf(good.URL)), good.URL); err != nil {
		t.Fatalf("pinned CA, right host: %v", err)
	}
	// Same CA, but the certificate is for another name: dialing an IP (no
	// SNI server name) must still check the host.
	other := serve(t, ca, []string{"other.example"})
	if err := get(PinnedTLS(ca.Hash(), HostOf(other.URL)), other.URL); err == nil {
		t.Fatal("certificate for another host accepted")
	}
	// Another CA.
	if err := get(PinnedTLS(testCA(t).Hash(), HostOf(good.URL)), good.URL); err == nil || !strings.Contains(err.Error(), "pinned CA") {
		t.Fatalf("foreign CA: %v", err)
	}
	// A Nest (client-auth) certificate from the right CA is no server cert.
	key, csr, _ := NewKeyAndCSR("n")
	nestPEM, _, err := ca.SignNestCSR(csr, "n")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := tls.X509KeyPair([]byte(nestPEM+string(ca.CertPEM)), []byte(key))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()
	if err := get(PinnedTLS(ca.Hash(), HostOf(srv.URL)), srv.URL); err == nil {
		t.Fatal("nest certificate accepted as a server certificate")
	}
}

func TestPEMFingerprint(t *testing.T) {
	ca := testCA(t)
	fp, err := PEMFingerprint(string(ca.CertPEM))
	if err != nil || fp != ca.Hash() {
		t.Fatalf("%s %v", fp, err)
	}
	if _, err := PEMFingerprint("junk"); err == nil {
		t.Fatal("junk accepted")
	}
}
