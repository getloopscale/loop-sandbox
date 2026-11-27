// Package loopscale is the LoopScale authentication adapter for the sandbox. It
// implements the auth.TenantAuthenticator seam (DIP) for requests forwarded by the
// LoopScale edge: the edge authenticates a loopscale_live_* consumer key, then forwards to the
// sandbox a LoopScale JWT (tenant_id + scope) inside an Ed25519-signed service assertion
// (LoopScale contracts/serviceauth). The sandbox trusts the forward ONLY if the signature
// verifies (fail-closed), and cross-checks the JWT tenant against the signed tenant —
// the single tenant-identity resolution point.
package loopscale

import (
	"context"
	"crypto/ed25519"
	"strconv"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale/serviceauth"

	"github.com/getloopscale/loop-sandbox/internal/auth"
)

// ForwardHeaders carries the X-Loop-Runtime-* service-assertion headers off a request.
type ForwardHeaders struct {
	UserID    string // X-Loop-Runtime-User-ID
	TenantID  string // X-Loop-Runtime-Tenant
	Timestamp string // X-Loop-Runtime-Timestamp (unix seconds, string)
	Nonce     string // X-Loop-Runtime-Nonce
	Signature string // X-Loop-Runtime-Signature (base64 Ed25519)
}

// Assertion is the verified edge identity extracted from a signed forward.
type Assertion struct {
	UserID   string
	TenantID string
}

// ForwardAuthVerifier verifies the Ed25519-signed service assertion the LoopScale edge
// attaches to a forwarded request, with replay-window + nonce protection.
type ForwardAuthVerifier struct {
	trustedKeys []ed25519.PublicKey
	replay      *serviceauth.ReplayWindow
	nonces      serviceauth.NonceStore
}

// NewForwardAuthVerifier builds a verifier. trustedKeys is the edge signing-key
// trust set (supports [current, previous] rotation). tolerance bounds clock skew /
// staleness; nonces guards against replay within that window.
func NewForwardAuthVerifier(trustedKeys []ed25519.PublicKey, tolerance time.Duration, nonces serviceauth.NonceStore) *ForwardAuthVerifier {
	return &ForwardAuthVerifier{
		trustedKeys: trustedKeys,
		replay:      serviceauth.NewReplayWindow(tolerance),
		nonces:      nonces,
	}
}

// Verify checks the signed assertion over (method, path, headers) at time now.
// EVERY failure returns auth.ErrUnauthorized with no distinguishing detail
// (fail-closed — a verification error must never fall through to trust).
func (v *ForwardAuthVerifier) Verify(ctx context.Context, method, path string, h ForwardHeaders, now time.Time) (*Assertion, error) {
	ts, err := strconv.ParseInt(h.Timestamp, 10, 64)
	if err != nil {
		return nil, auth.ErrUnauthorized
	}
	in := serviceauth.PayloadInput{
		UserID:    h.UserID,
		TenantID:  h.TenantID,
		Timestamp: ts,
		Nonce:     h.Nonce,
		Method:    method,
		Path:      path,
	}
	// 1. Signature: proves the request came from the trusted edge and binds the
	//    tenant/user/method/path (ValidateInputs inside also rejects delimiter forgery).
	if err := serviceauth.Verify(v.trustedKeys, h.Signature, in); err != nil {
		return nil, auth.ErrUnauthorized
	}
	// 2. Staleness: reject assertions outside the replay window.
	if err := v.replay.Validate(time.Unix(ts, 0), now); err != nil {
		return nil, auth.ErrUnauthorized
	}
	// 3. Nonce: reject a replayed assertion within the window. A missing nonce store
	//    is a fail-CLOSED misconfiguration — never silently skip replay protection.
	if v.nonces == nil {
		return nil, auth.ErrUnauthorized
	}
	if err := v.nonces.Claim(ctx, h.UserID, h.Nonce); err != nil {
		return nil, auth.ErrUnauthorized
	}
	return &Assertion{UserID: h.UserID, TenantID: h.TenantID}, nil
}
