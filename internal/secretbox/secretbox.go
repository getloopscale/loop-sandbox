// Package secretbox encrypts small values at rest with AES-256-GCM. The
// ciphertext is bound to the place it is stored in: opening it with other
// associated data fails, so a row copied to another tenant or name is refused.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeySize is the length of the key in bytes.
const KeySize = 32

// ErrOpen is returned when a ciphertext cannot be opened: it was sealed with
// another key, bound to other associated data, cut short or altered.
var ErrOpen = errors.New("secretbox: cannot open the ciphertext")

// Box seals and opens values with one key.
type Box struct {
	aead  cipher.AEAD
	keyID string
}

// New returns a Box for key, which must be KeySize random bytes.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: the key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	// The id names the key without revealing it: a hash of a domain tag and the key.
	sum := sha256.Sum256(append([]byte("loopsandbox secretbox key id\x00"), key...))
	return &Box{aead: aead, keyID: hex.EncodeToString(sum[:6])}, nil
}

// KeyID identifies the key, so a stored ciphertext can say which key sealed it.
func (b *Box) KeyID() string { return b.keyID }

// Seal encrypts plaintext and binds it to associated. The result starts with a
// random nonce, so sealing the same value twice gives different bytes.
func (b *Box) Seal(plaintext, associated []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: read a nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, associated), nil
}

// Open decrypts what Seal returned for the same associated data.
func (b *Box) Open(sealed, associated []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrOpen
	}
	plaintext, err := b.aead.Open(nil, sealed[:n], sealed[n:], associated)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}
