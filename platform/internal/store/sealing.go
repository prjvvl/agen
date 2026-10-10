package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Hub secrets in the Store (the CA key, the call-token key) are sealed with
// a key-encryption key only Hubs hold (AGEN_HUB_KEK), so the Store alone
// (a backup, a leaked admin DSN) does not give them away.

const sealedPrefix = "enc1:"

// ErrNoKEK: a sealed secret was found but this process has no KEK.
var ErrNoKEK = errors.New("the Hub's keys in the Store are encrypted: set AGEN_HUB_KEK (the same value on every Hub)")

// SetKEK enables sealing with a key derived from secret ("" disables it).
func (s *Store) SetKEK(secret string) { s.kek = deriveKEK(secret) }

// SetPreviousKEK lets this Store still open values sealed with an older KEK
// (AGEN_HUB_KEK_PREVIOUS), so Hubs keep working while agen hub rotate-kek
// runs. New values are always sealed with the current KEK.
func (s *Store) SetPreviousKEK(secret string) { s.prevKEK = deriveKEK(secret) }

func deriveKEK(secret string) []byte {
	if secret == "" {
		return nil
	}
	k := sha256.Sum256([]byte("agen-hub-kek/v1:" + secret))
	return k[:]
}

func (s *Store) seal(plain string) (string, error) {
	if s.kek == nil {
		return plain, nil
	}
	block, err := aes.NewCipher(s.kek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return sealedPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (s *Store) unseal(v string) (string, error) {
	enc, ok := strings.CutPrefix(v, sealedPrefix)
	if !ok {
		return v, nil
	}
	if s.kek == nil && s.prevKEK == nil {
		return "", ErrNoKEK
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	for _, k := range [][]byte{s.kek, s.prevKEK} {
		if k == nil {
			continue
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return "", err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return "", err
		}
		if len(raw) < gcm.NonceSize() {
			return "", errors.New("sealed secret is corrupt")
		}
		if plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil); err == nil {
			return string(plain), nil
		}
	}
	return "", errors.New("cannot decrypt the Hub's keys: AGEN_HUB_KEK differs from the one they were sealed with")
}

// sealInPlace encrypts a stored plaintext secret once a KEK is configured
// (upgrading an existing Store).
func (s *Store) sealInPlace(ctx context.Context, stored, query string) {
	if s.kek == nil || strings.HasPrefix(stored, sealedPrefix) {
		return
	}
	if sealed, err := s.seal(stored); err == nil {
		// Only if still the same plaintext (another Hub may have sealed it).
		_, _ = s.db.ExecContext(ctx, query, sealed, stored)
	}
}

// RotateKEK re-seals every sealed secret (the Hub's CA and call-token keys,
// platform secrets, notification signing secrets) from the current KEK to
// next, in one transaction, and switches this Store to next. Plaintext
// values are sealed too. Every Hub must then run with the new AGEN_HUB_KEK.
func (s *Store) RotateKEK(ctx context.Context, next string) (int, error) {
	if next == "" {
		return 0, errors.New("the new KEK is empty")
	}
	to := &Store{db: s.db, dialect: s.dialect}
	to.SetKEK(next)
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cols := []struct{ table, key, col string }{
			{"hub_ca", "CAST(id AS TEXT)", "key_pem"},
			{"hub_token_key", "CAST(id AS TEXT)", "private_key"},
			{"platform_secrets", "namespace || '/' || name", "value"},
			{"notification_targets", "namespace || '/' || name", "secret"},
		}
		for _, c := range cols {
			rows, err := tx.QueryContext(ctx, "SELECT "+c.key+", "+c.col+" FROM "+c.table)
			if err != nil {
				return err
			}
			type row struct{ key, val string }
			var all []row
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.key, &r.val); err != nil {
					rows.Close()
					return err
				}
				all = append(all, r)
			}
			rows.Close()
			for _, r := range all {
				plain, err := s.unseal(r.val)
				if err != nil {
					return fmt.Errorf("%s: %w", c.table, err)
				}
				sealed, err := to.seal(plain)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, "UPDATE "+c.table+" SET "+c.col+" = $1 WHERE "+c.key+" = $2", sealed, r.key); err != nil {
					return err
				}
				n++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.kek = to.kek
	return n, nil
}
