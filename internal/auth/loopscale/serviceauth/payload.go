// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The LoopScale Authors

// Package serviceauth provides the dual-control service authentication
// primitives shared by the API engine and the cloud proxy: canonical
// payload construction, Ed25519 sign/verify, replay-window validation,
// and nonce-store deduplication.
//
// Design RFC: .claude/knowledge-base/decisions/2026-05-13-service-auth-ed25519-adoption.md
// Threat model: cloud→engine service-auth path previously trusted
// identity headers behind a single shared secret. A leak of the shared
// secret (CI log, kubectl describe, sealed-secret misconfig) yielded
// full tenant escalation. Dual-control requires BOTH the shared secret
// AND an Ed25519 signature; a single leak no longer enables impersonation.
//
// The canonical payload includes method+path so a captured signature
// for one route cannot be replayed against another route.
package serviceauth

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PayloadInput is the canonical input for signature construction and
// verification. The shape of this struct + the CanonicalPayload format
// are wire-protocol contracts — must not change without a coordinated
// API/cloud rollout.
type PayloadInput struct {
	UserID    string // X-Loop-Runtime-User-ID
	TenantID  string // X-Loop-Runtime-Tenant
	Timestamp int64  // X-Loop-Runtime-Timestamp (Unix epoch seconds)
	Nonce     string // X-Loop-Runtime-Nonce
	Method    string // HTTP method (GET/POST/...)
	Path      string // HTTP path (e.g., /api/v1/builds) — NOT including query string
}

// CanonicalPayload produces the byte-exact payload that the Ed25519
// signature is computed over. Format (newline-delimited, ASCII):
//
//	user_id \n tenant_id \n timestamp \n nonce \n method " " path
//
// Stability: this format is a wire-protocol contract. ANY change
// invalidates every existing signature and forces a flag-day rollout.
// The shape is documented in
// `.claude/knowledge-base/decisions/2026-05-13-service-auth-ed25519-adoption.md`.
//
// Callers MUST first call ValidateInputs to ensure no field contains
// the delimiter ('\n') or other ambiguity-creating characters. Sign()
// already does this; verifiers and tests should as well.
func CanonicalPayload(in PayloadInput) string {
	var b strings.Builder
	// Pre-allocate a reasonable capacity. Worst-case overshoot is fine.
	b.Grow(len(in.UserID) + len(in.TenantID) + 20 + len(in.Nonce) + len(in.Method) + len(in.Path) + 5)
	b.WriteString(in.UserID)
	b.WriteByte('\n')
	b.WriteString(in.TenantID)
	b.WriteByte('\n')
	b.WriteString(strconv.FormatInt(in.Timestamp, 10))
	b.WriteByte('\n')
	b.WriteString(in.Nonce)
	b.WriteByte('\n')
	b.WriteString(in.Method)
	b.WriteByte(' ')
	b.WriteString(in.Path)
	return b.String()
}

// ErrInvalidPayloadInput indicates the input violates one of the
// canonical-payload integrity rules (empty required field, or contains
// a delimiter character that would allow field-confusion).
//
// This error is wrapped with the specific field name so audit logs can
// trace which header was malformed. Audit identifier: `service-auth`
// appears in every wrap so grep finds them.
var ErrInvalidPayloadInput = errors.New("service-auth: invalid payload input")

// ValidateInputs enforces the integrity invariants for canonical payload
// construction:
//
//  1. All textual fields are non-empty.
//  2. No textual field contains '\n', '\t', or '\r' — those characters
//     would allow field-confusion forgery where a malicious caller
//     embeds the delimiter in user_id to forge a different tenant_id.
//
// Sign() calls this automatically. Direct callers of CanonicalPayload
// (e.g., for offline diagnostic purposes) MUST call it themselves.
func ValidateInputs(in PayloadInput) error {
	checks := []struct {
		field string
		value string
	}{
		{"user_id", in.UserID},
		{"tenant_id", in.TenantID},
		{"nonce", in.Nonce},
		{"method", in.Method},
		{"path", in.Path},
	}
	for _, c := range checks {
		if c.value == "" {
			return fmt.Errorf("%w: %s is empty", ErrInvalidPayloadInput, c.field)
		}
		if strings.ContainsAny(c.value, "\n\r\t") {
			return fmt.Errorf("%w: %s contains control character", ErrInvalidPayloadInput, c.field)
		}
	}
	return nil
}
