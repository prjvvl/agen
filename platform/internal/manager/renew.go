package manager

import (
	"context"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/pki"
)

// maybeRenewCert renews the Nest's client certificate over the current mTLS
// connection when less than CertRenewBefore remains. The key is new each
// time and never leaves the Nest.
func (m *Manager) maybeRenewCert(ctx context.Context) {
	cur := m.Credential()
	if cur.CertPEM == "" {
		return
	}
	notAfter, err := pki.CertNotAfter(cur.CertPEM)
	if err != nil || time.Until(notAfter) > m.cfg.CertRenewBefore {
		return
	}
	key, csr, err := pki.NewKeyAndCSR(m.cfg.Name)
	if err != nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := m.hub().RenewCertificate(c, connect.NewRequest(&agenv1.RenewCertificateRequest{NestId: m.nestID, CsrPem: csr}))
	if err != nil {
		m.cfg.Log.Warn("certificate renewal failed; will retry", "expires", notAfter, "err", err)
		return
	}
	if fp, err := pki.PEMFingerprint(resp.Msg.CaPem); err != nil || (m.cfg.CAHash != "" && fp != m.cfg.CAHash) {
		m.cfg.Log.Error("certificate renewal returned an unexpected CA; keeping the current certificate")
		return
	}
	// Save first: if saving fails, keep using the current certificate (the
	// Hub accepts it until the new one is used) and retry later.
	next := m.Credential()
	next.CertPEM, next.KeyPEM, next.CAPEM = resp.Msg.CertificatePem, key, resp.Msg.CaPem
	if m.cfg.OnCredential != nil {
		if err := m.cfg.OnCredential(next); err != nil {
			m.cfg.Log.Error("could not save the renewed certificate; keeping the current one", "err", err)
			return
		}
	}
	m.credMu.Lock()
	m.cfg.CertPEM, m.cfg.KeyPEM, m.cfg.CAPEM = next.CertPEM, next.KeyPEM, next.CAPEM
	token := m.cfg.NestToken
	m.credMu.Unlock()
	m.setToken(token) // new client certificate for every Hub call
	exp, _ := pki.CertNotAfter(next.CertPEM)
	m.cfg.Log.Info("nest certificate renewed", "expires", exp)
}
