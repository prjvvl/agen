package manager

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
)

// tokenKeysFile keeps the Hub's call-token public keys, so a Nest that
// restarts while the Hub is down can still verify A2A callers.
const tokenKeysFile = "token-keys.json"

// TokenKeys returns the Hub's public keys for A2A call tokens, by key id.
func (m *Manager) TokenKeys() map[string]ed25519.PublicKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokenKeys
}

// refreshTokenKeys fetches the keys from the Hub (and saves them); if the Hub
// is unreachable it keeps the ones it has, loading the saved ones at first.
func (m *Manager) refreshTokenKeys(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := m.hub().GetTokenKeys(c, connect.NewRequest(&agenv1.GetTokenKeysRequest{NestId: m.nestID}))
	if err != nil {
		m.mu.Lock()
		have := len(m.tokenKeys) > 0
		m.mu.Unlock()
		if !have {
			m.loadTokenKeys()
		}
		if ctx.Err() == nil {
			m.cfg.Log.Warn("could not fetch call-token keys from the hub; using the cached ones", "err", err)
		}
		return
	}
	keys := map[string]ed25519.PublicKey{}
	saved := map[string][]byte{}
	for _, k := range r.Msg.Keys {
		if len(k.PublicKey) == ed25519.PublicKeySize {
			keys[k.Kid] = ed25519.PublicKey(k.PublicKey)
			saved[k.Kid] = k.PublicKey
		}
	}
	m.mu.Lock()
	m.tokenKeys = keys
	m.mu.Unlock()
	if b, err := json.Marshal(saved); err == nil {
		_ = os.MkdirAll(m.cfg.DataDir, 0o700)
		_ = os.WriteFile(filepath.Join(m.cfg.DataDir, tokenKeysFile), b, 0o600)
	}
}

func (m *Manager) loadTokenKeys() {
	b, err := os.ReadFile(filepath.Join(m.cfg.DataDir, tokenKeysFile))
	if err != nil {
		return
	}
	var saved map[string][]byte
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	keys := map[string]ed25519.PublicKey{}
	for kid, pub := range saved {
		if len(pub) == ed25519.PublicKeySize {
			keys[kid] = ed25519.PublicKey(pub)
		}
	}
	m.mu.Lock()
	m.tokenKeys = keys
	m.mu.Unlock()
}
