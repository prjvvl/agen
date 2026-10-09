package hub

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/prjvvl/agen/platform/internal/pki"
	"github.com/prjvvl/agen/platform/internal/store"
)

// EnableTLS makes this Hub a TLS server for distributed fleets
// (docs/architecture.md §10): it loads (or creates) the CA shared by all Hub
// replicas through the Store, issues its server certificate for hosts, and
// from then on Nests must enrol with a CSR and call NestService with the
// client certificate they got (bearer nest tokens alone are refused). API
// clients keep using bearer tokens over TLS.
func (h *Hub) EnableTLS(ctx context.Context, hosts []string) (*tls.Config, error) {
	stored, err := h.Store.EnsureCA(ctx, func() (store.CA, error) {
		c, k, err := pki.NewCA()
		return store.CA{CertPEM: c, KeyPEM: k}, err
	})
	if err != nil {
		return nil, err
	}
	ca, err := pki.Load(stored.CertPEM, stored.KeyPEM)
	if err != nil {
		return nil, err
	}
	cert, err := ca.ServerCert(hosts)
	if err != nil {
		return nil, err
	}
	h.CA = ca
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		// Verified when presented (Nests); API clients present none.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  ca.Pool(),
		NextProtos: []string{"h2", "http/1.1"},
	}, nil
}

type peerCertKey struct{}

// withPeerCert exposes the verified client certificate's fingerprint to the
// NestService handlers.
func withPeerCert(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.PeerCertificates) > 0 {
			fp := pki.Fingerprint(r.TLS.PeerCertificates[0].Raw)
			r = r.WithContext(context.WithValue(r.Context(), peerCertKey{}, fp))
		}
		next.ServeHTTP(w, r)
	})
}

func peerCert(ctx context.Context) string {
	fp, _ := ctx.Value(peerCertKey{}).(string)
	return fp
}
