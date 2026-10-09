package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agenv1 "github.com/prjvvl/agen/platform/gen/agen/v1"
	"github.com/prjvvl/agen/platform/internal/calltoken"
)

// Call-token lifetimes: users get a token per Resolve; agents' Managers
// cache theirs, and it must outlast a Hub outage for A2A to keep working.
const (
	userCallTokenTTL  = 10 * time.Minute
	agentCallTokenTTL = time.Hour
)

// keyGrace: a new call-token key is published (GetTokenKeys) at once but
// used for signing only once it is this old, so Managers (refreshing every
// minute) know it before any token signed with it arrives. keyringTTL is
// how long a Hub keeps the keys it read.
// minRotationAge: a key must sign for longer than the agent token lifetime
// before the next rotation retires the key before it.
var (
	keyGrace       = 5 * time.Minute
	keyringTTL     = time.Minute
	minRotationAge = agentCallTokenTTL + 2*keyGrace
)

type tokenKeyring struct {
	signing ed25519.PrivateKey
	public  map[string]ed25519.PublicKey // published: verify live tokens
	records map[string]ed25519.PublicKey // incl. retired: verify requester records
	read    time.Time
}

// keyring returns the call-token keys, creating the first one in the Store
// on first use and re-reading them every keyringTTL (rotation).
func (h *Hub) keyring(ctx context.Context) (*tokenKeyring, error) {
	h.tokenMu.Lock()
	defer h.tokenMu.Unlock()
	if h.keys != nil && time.Since(h.keys.read) < keyringTTL {
		return h.keys, nil
	}
	if _, err := h.Store.EnsureTokenKey(ctx, newTokenSeed); err != nil {
		return nil, err
	}
	rows, err := h.Store.TokenKeys(ctx)
	if err != nil {
		return nil, err
	}
	kr := &tokenKeyring{public: map[string]ed25519.PublicKey{}, records: map[string]ed25519.PublicKey{}, read: time.Now()}
	for _, r := range rows {
		seed, err := base64.StdEncoding.DecodeString(r.Seed)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("hub token key %d is corrupt", r.ID)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		kr.records[calltoken.KeyID(pub)] = pub
		if r.RetiredMs > 0 {
			continue
		}
		kr.public[calltoken.KeyID(pub)] = pub
		// Oldest first: the last key old enough to be known everywhere
		// signs; the very first key signs at once.
		if kr.signing == nil || time.Since(time.UnixMilli(r.CreatedMs)) >= keyGrace {
			kr.signing = priv
		}
	}
	h.keys = kr
	return kr, nil
}

func newTokenSeed() (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(seed), nil
}

// tokenKey returns the current call-token signing key.
func (h *Hub) tokenKey(ctx context.Context) (ed25519.PrivateKey, error) {
	kr, err := h.keyring(ctx)
	if err != nil {
		return nil, err
	}
	return kr.signing, nil
}

// issueCallToken adds a call token for ns/name to a Resolve response.
func (h *Hub) issueCallToken(ctx context.Context, out *agenv1.ResolveResponse, sub, inst, ns, name string, ttl time.Duration) error {
	key, err := h.tokenKey(ctx)
	if err != nil {
		return connectErr(err)
	}
	tok, exp := calltoken.Sign(key, calltoken.Claims{Sub: sub, Inst: inst, Aud: ns + "/" + name}, ttl, time.Now())
	out.Token, out.TokenExpiresAt = tok, timestamppb.New(exp)
	return nil
}

// tokenUser returns the user principal of a call token the Hub issued for
// aud (expiry ignored: it records who made a call), or "".
func (h *Hub) tokenUser(ctx context.Context, token, aud string) string {
	kr, err := h.keyring(ctx)
	if err != nil {
		return ""
	}
	c, err := calltoken.VerifyIssued(token, kr.records, aud)
	if err != nil {
		return ""
	}
	user, _ := strings.CutPrefix(c.Sub, "user:")
	if user == c.Sub {
		return ""
	}
	return user
}

// GetTokenKeys returns the public call-token keys to a Nest.
func (n *NestAPI) GetTokenKeys(ctx context.Context, req *connect.Request[agenv1.GetTokenKeysRequest]) (*connect.Response[agenv1.GetTokenKeysResponse], error) {
	if err := requireNest(ctx, req.Msg.NestId); err != nil {
		return nil, err
	}
	kr, err := n.hub.keyring(ctx)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &agenv1.GetTokenKeysResponse{}
	for kid, pub := range kr.public {
		out.Keys = append(out.Keys, &agenv1.TokenKey{Kid: kid, PublicKey: pub})
	}
	return connect.NewResponse(out), nil
}

// MinTokenKeyRotationAge is how old the newest call-token key must be before
// another rotation (agen hub rotate-token-key without --force).
func MinTokenKeyRotationAge() time.Duration { return minRotationAge }
