package near

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
)

// KeyPair is an ed25519 key pair in NEAR's string format ("ed25519:<base58>").
type KeyPair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// GenerateKey returns a fresh random ed25519 key pair.
func GenerateKey() (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Public: pub, Private: priv}, nil
}

// ParseSecretKey reads "ed25519:<base58 of 64 bytes>" (the near-cli credential format).
// A 32-byte seed is also accepted.
func ParseSecretKey(s string) (KeyPair, error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(s), "ed25519:")
	if !ok {
		return KeyPair{}, errors.New("secret key must start with ed25519:")
	}
	raw, err := Base58Decode(body)
	if err != nil {
		return KeyPair{}, err
	}
	var priv ed25519.PrivateKey
	switch len(raw) {
	case ed25519.PrivateKeySize:
		priv = ed25519.PrivateKey(raw)
	case ed25519.SeedSize:
		priv = ed25519.NewKeyFromSeed(raw)
	default:
		return KeyPair{}, errors.New("secret key has the wrong length")
	}
	return KeyPair{Public: priv.Public().(ed25519.PublicKey), Private: priv}, nil
}

// PublicKeyString returns "ed25519:<base58>".
func (k KeyPair) PublicKeyString() string { return "ed25519:" + Base58Encode(k.Public) }

// SecretKeyString returns "ed25519:<base58 of 64 bytes>".
func (k KeyPair) SecretKeyString() string { return "ed25519:" + Base58Encode(k.Private) }

// Sign signs msg with the private key.
func (k KeyPair) Sign(msg []byte) []byte { return ed25519.Sign(k.Private, msg) }

// ParsePublicKey reads "ed25519:<base58>".
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	body, ok := strings.CutPrefix(s, "ed25519:")
	if !ok {
		return nil, errors.New("public key must start with ed25519:")
	}
	raw, err := Base58Decode(body)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("public key has the wrong length")
	}
	return ed25519.PublicKey(raw), nil
}
