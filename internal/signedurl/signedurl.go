// Package signedurl issues and verifies the tokens of signed file URLs: a URL
// that lets its holder upload or download one file of one sandbox, without an
// API key, until it expires.
package signedurl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MinKeyLen is the shortest signing key accepted, in bytes.
const MinKeyLen = 32

// The operations a token can grant.
const (
	OpDownload = "download"
	OpUpload   = "upload"
)

// Verification errors. A caller answers both the same way; they differ for logs
// and tests.
var (
	ErrInvalid = errors.New("signed url: invalid token")
	ErrExpired = errors.New("signed url: expired token")
)

// domainTag separates these signatures from any other use of the same key.
const domainTag = "loopsandbox-file-url-v1\n"

// Claims is what a token grants: Operation on Path of SandboxID, which belongs
// to TenantID, until ExpiresAt.
type Claims struct {
	TenantID  string    `json:"t"`
	SandboxID string    `json:"s"`
	Path      string    `json:"p"`
	Operation string    `json:"o"`
	ExpiresAt time.Time `json:"-"`
	Expiry    int64     `json:"e"` // ExpiresAt in seconds since the Unix epoch
}

// Signer signs and verifies tokens with one HMAC-SHA256 key.
type Signer struct{ key []byte }

// New returns a Signer. The key must have at least MinKeyLen bytes.
func New(key []byte) (*Signer, error) {
	if len(key) < MinKeyLen {
		return nil, fmt.Errorf("signed url: the signing key has %d bytes, the minimum is %d", len(key), MinKeyLen)
	}
	return &Signer{key: append([]byte(nil), key...)}, nil
}

func (s *Signer) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(domainTag))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// Sign returns the token for c. Only ExpiresAt is read for the expiry.
func (s *Signer) Sign(c Claims) (string, error) {
	if c.Operation != OpDownload && c.Operation != OpUpload {
		return "", fmt.Errorf("signed url: unknown operation %q", c.Operation)
	}
	c.Expiry = c.ExpiresAt.Unix()
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("signed url: encode claims: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload)), nil
}

// Verify returns the claims of a token this Signer issued that has not expired
// at now. The signature is checked before anything in the token is trusted.
func (s *Signer) Verify(token string, now time.Time) (*Claims, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return nil, ErrInvalid
	}
	// Strict decoding: a token has exactly one spelling, so it cannot be
	// rewritten into another string that also verifies.
	strict := base64.RawURLEncoding.Strict()
	got, err := strict.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(payload)) {
		return nil, ErrInvalid
	}
	raw, err := strict.DecodeString(payload)
	if err != nil {
		return nil, ErrInvalid
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrInvalid
	}
	c.ExpiresAt = time.Unix(c.Expiry, 0).UTC()
	if !now.Before(c.ExpiresAt) {
		return nil, ErrExpired
	}
	return &c, nil
}
