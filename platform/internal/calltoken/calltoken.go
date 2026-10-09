// Package calltoken issues and verifies A2A call tokens: short-lived,
// Hub-signed (Ed25519) credentials naming who calls (a user principal or an
// agent deployment) and which deployment they may call. Gateways verify them
// offline with the Hub's public keys, so calls keep working while the Hub is
// down (architecture §10).
//
// Format: "agen1." + base64url(JSON claims) + "." + base64url(signature over
// the first two parts).
package calltoken

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const prefix = "agen1"

// Claims is what a token asserts.
type Claims struct {
	// Sub is the caller: "user:<principal id>" or "agent:<ns>/<deployment>".
	Sub string `json:"sub"`
	// Inst is the calling instance (agents), for audit.
	Inst string `json:"inst,omitempty"`
	// Aud is the deployment the token may call: "<ns>/<deployment>".
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	// Kid names the signing key.
	Kid string `json:"kid"`
}

// Agent reports whether the caller is an agent (whose lineage metadata the
// Gateway may trust), and which deployment.
func (c Claims) Agent() (ns, dep string, ok bool) {
	rest, ok := strings.CutPrefix(c.Sub, "agent:")
	if !ok {
		return "", "", false
	}
	ns, dep, ok = strings.Cut(rest, "/")
	return ns, dep, ok && ns != "" && dep != ""
}

// KeyID is a short stable name for a public key.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

var b64 = base64.RawURLEncoding

// Sign issues a token for c (Iat, Exp and Kid are filled in).
func Sign(key ed25519.PrivateKey, c Claims, ttl time.Duration, now time.Time) (string, time.Time) {
	exp := now.Add(ttl)
	c.Iat, c.Exp, c.Kid = now.Unix(), exp.Unix(), KeyID(key.Public().(ed25519.PublicKey))
	payload, _ := json.Marshal(c)
	signed := prefix + "." + b64.EncodeToString(payload)
	return signed + "." + b64.EncodeToString(ed25519.Sign(key, []byte(signed))), exp
}

// Errors from Verify.
var (
	ErrMalformed = errors.New("calltoken: malformed token")
	ErrUnknown   = errors.New("calltoken: signed by an unknown key")
	ErrSignature = errors.New("calltoken: bad signature")
	ErrExpired   = errors.New("calltoken: expired")
	ErrAudience  = errors.New("calltoken: not valid for this deployment")
)

// Verify checks a token against the known public keys (by key id), its
// expiry (with a small clock-skew allowance) and its audience.
func Verify(token string, keys map[string]ed25519.PublicKey, aud string, now time.Time) (Claims, error) {
	return verify(token, keys, aud, &now)
}

// VerifyIssued checks that a token was issued by the Hub for aud, ignoring
// its expiry: a stored record of who made a call (runs.requested_by) that a
// host cannot forge.
func VerifyIssued(token string, keys map[string]ed25519.PublicKey, aud string) (Claims, error) {
	return verify(token, keys, aud, nil)
}

func verify(token string, keys map[string]ed25519.PublicKey, aud string, now *time.Time) (Claims, error) {
	var c Claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != prefix {
		return c, ErrMalformed
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return c, ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return c, ErrMalformed
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, ErrMalformed
	}
	pub, ok := keys[c.Kid]
	if !ok {
		return c, ErrUnknown
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return c, ErrSignature
	}
	const skew = 60
	if now != nil && (now.Unix() > c.Exp+skew || now.Unix() < c.Iat-skew) {
		return c, ErrExpired
	}
	if c.Aud != aud {
		return c, fmt.Errorf("%w (token is for %s)", ErrAudience, c.Aud)
	}
	return c, nil
}
