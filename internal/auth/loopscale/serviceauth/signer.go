// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The LoopScale Authors

package serviceauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// ErrSignatureInvalid is returned when an Ed25519 signature fails
// verification — either because the bytes do not match or because the
// signature does not bind to the provided payload. Wrapped errors carry
// no detail about *why* the verification failed beyond this sentinel, so
// audit logs do not leak which trusted key the signature was attempted
// against (key-distinguishing oracle resistance).
var ErrSignatureInvalid = errors.New("service-auth: signature invalid")

// ErrNoTrustedKeys is returned when Verify is called with an empty
// trust set. Per RFC Q5 decision, public-key fetch failure → fail-closed.
// If the API cannot load the public key from ConfigMap (ESO sync issue,
// startup race, deliberate operator action), every signed request fails
// rather than silently bypassing the dual-control check.
var ErrNoTrustedKeys = errors.New("service-auth: no trusted public keys configured")

// Sign produces a base64-encoded Ed25519 signature over the canonical
// payload derived from `in`. Returns an error if the input fails
// ValidateInputs — Sign refuses to produce a signature over malformed
// inputs because such a signature would be a forgery primitive.
//
// The private key is `ed25519.PrivateKey`; in production this is
// materialized by ESO into a K8s Secret (mounted as a file the cloud
// proxy reads at startup). Per RFC, the private key is held ONLY by
// the cloud proxy — no other binary should accept it.
func Sign(priv ed25519.PrivateKey, in PayloadInput) (string, error) {
	if err := ValidateInputs(in); err != nil {
		return "", err
	}
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("service-auth: invalid private key size %d (want %d)", len(priv), ed25519.PrivateKeySize)
	}
	payload := CanonicalPayload(in)
	sig := ed25519.Sign(priv, []byte(payload))
	return base64.StdEncoding.EncodeToString(sig), nil
}

// Verify checks `signature` (base64-encoded) against the canonical
// payload derived from `in`, accepting any public key in `trustedKeys`.
//
// Trust-set semantics: the slice represents the dual-key rotation
// window. Typical configuration during a rotation is
// `[currentPub, previousPub]` — a signature from either key validates.
// Once rotation completes, `previousPub` is dropped from the trust set
// and signatures from the old private key no longer validate.
//
// Returns:
//   - ErrNoTrustedKeys if `trustedKeys` is empty (fail-closed, RFC Q5).
//   - ErrSignatureInvalid if no key in the trust set validates the
//     signature, or if the input fails ValidateInputs.
//   - A wrapped error for base64-decoding failures (malformed header).
//
// Constant-time: ed25519.Verify is constant-time per the stdlib
// guarantees; iterating the trust set leaks at most O(N) on key-set
// size, never on the secret bits of any key.
func Verify(trustedKeys []ed25519.PublicKey, signature string, in PayloadInput) error {
	if len(trustedKeys) == 0 {
		return ErrNoTrustedKeys
	}
	if err := ValidateInputs(in); err != nil {
		return err
	}
	sigBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("service-auth: signature is not valid base64: %w", err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("%w: wrong signature length %d", ErrSignatureInvalid, len(sigBytes))
	}
	payload := []byte(CanonicalPayload(in))
	for _, pub := range trustedKeys {
		if len(pub) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(pub, payload, sigBytes) {
			return nil
		}
	}
	return ErrSignatureInvalid
}
