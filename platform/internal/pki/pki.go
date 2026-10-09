// Package pki is the Hub's certificate authority (docs/architecture.md §10):
// it signs Nest client certificates at enrolment and the Hub's own TLS
// certificate. A joining Nest pins the CA by hash (from `agen join-token`)
// until it has the CA certificate.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"
)

// Authority is a loaded CA.
type Authority struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     crypto.Signer
	// NestLifetime is how long Nest certificates are valid (default
	// DefaultNestLifetime); Nests renew them before they expire.
	NestLifetime time.Duration
}

// DefaultNestLifetime of Nest client certificates.
const DefaultNestLifetime = 30 * 24 * time.Hour

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

func keyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// NewCA creates a CA (ECDSA P-256, 10 years) and returns its PEMs.
func NewCA() (certPEM, keyPEMOut string, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Agen Hub CA", Organization: []string{"agen"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return "", "", err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(kp), nil
}

// Load parses a stored CA.
func Load(certPEM, keyPEMIn string) (*Authority, error) {
	cb, _ := pem.Decode([]byte(certPEM))
	kb, _ := pem.Decode([]byte(keyPEMIn))
	if cb == nil || kb == nil {
		return nil, errors.New("pki: bad CA PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &Authority{Cert: cert, CertPEM: []byte(certPEM), key: key}, nil
}

// Fingerprint is "sha256:<hex>" of a certificate's DER.
func Fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(h[:])
}

// Hash identifies the CA (what a joining Nest pins).
func (a *Authority) Hash() string { return Fingerprint(a.Cert.Raw) }

// Pool is a cert pool holding the CA.
func (a *Authority) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(a.Cert)
	return p
}

// CheckCSR validates a PEM certificate request and its signature.
func CheckCSR(csrPEM string) error {
	b, _ := pem.Decode([]byte(csrPEM))
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return errors.New("not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return err
	}
	return csr.CheckSignature()
}

// SignNestCSR issues a client certificate (NestLifetime) for a Nest. The CSR's own
// subject is ignored: the CN is the Nest id. Returns the PEM and fingerprint.
func (a *Authority) SignNestCSR(csrPEM, nestID string) (string, string, error) {
	b, _ := pem.Decode([]byte(csrPEM))
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return "", "", errors.New("pki: csr_pem is not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return "", "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return "", "", fmt.Errorf("pki: bad CSR signature: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: nestID, OrganizationalUnit: []string{"agen-nest"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(a.nestLifetime()),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, csr.PublicKey, a.key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Fingerprint(der), nil
}

func (a *Authority) nestLifetime() time.Duration {
	if a.NestLifetime > 0 {
		return a.NestLifetime
	}
	return DefaultNestLifetime
}

// CertNotAfter returns the expiry of the first certificate in a PEM.
func CertNotAfter(certPEM string) (time.Time, error) {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil {
		return time.Time{}, errors.New("pki: not a PEM certificate")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

// ServerCert issues the Hub's TLS certificate (1 year) for host names/IPs;
// the chain includes the CA.
func (a *Authority) ServerCert(hosts []string) (tls.Certificate, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "agen-hub"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.Cert, &k.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, a.Cert.Raw}, PrivateKey: k}, nil
}

// NewKeyAndCSR creates a Nest key and CSR.
func NewKeyAndCSR(name string) (keyPEMOut, csrPEM string, err error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, k)
	if err != nil {
		return "", "", err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return "", "", err
	}
	return string(kp), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// PinnedTLS trusts a server whose chain contains the CA with this hash and
// whose leaf certificate that CA signed (bootstrap before the Nest has the CA
// certificate). It fails closed.
//
// host is the name or IP the client dialed; the server certificate must be
// valid for it (the TLS ServerName is empty when dialing an IP).
func PinnedTLS(caHash, host string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verification is done below, against the pinned CA
		VerifyConnection: func(cs tls.ConnectionState) error {
			var ca *x509.Certificate
			for _, c := range cs.PeerCertificates {
				if Fingerprint(c.Raw) == caHash {
					ca = c
				}
			}
			if ca == nil || len(cs.PeerCertificates) == 0 {
				return errors.New("pki: the server's CA does not match the pinned CA hash")
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			name := cs.ServerName
			if name == "" {
				name = host
			}
			if name == "" {
				return errors.New("pki: no server name to verify")
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: name,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		},
	}
}

// PEMFingerprint is Fingerprint of the first certificate in a PEM.
func PEMFingerprint(certPEM string) (string, error) {
	b, _ := pem.Decode([]byte(certPEM))
	if b == nil || b.Type != "CERTIFICATE" {
		return "", errors.New("pki: not a PEM certificate")
	}
	return Fingerprint(b.Bytes), nil
}

// HostOf returns the host (no port) of a URL, or "".
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ClientTLS trusts the CA and presents the Nest certificate (after enrolment).
func ClientTLS(caPEM, certPEM, keyPEMIn string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("pki: bad CA PEM")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	if certPEM != "" {
		pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEMIn))
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}
