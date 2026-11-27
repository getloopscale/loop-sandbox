package loopscale_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale/serviceauth"

	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/auth/loopscale"
)

// signedForward mints a valid edge assertion for (method, path) at ts, returning
// the headers the sandbox verifier consumes.
func signedForward(t *testing.T, priv ed25519.PrivateKey, userID, tenantID, nonce, method, path string, ts int64) loopscale.ForwardHeaders {
	t.Helper()
	in := serviceauth.PayloadInput{UserID: userID, TenantID: tenantID, Timestamp: ts, Nonce: nonce, Method: method, Path: path}
	sig, err := serviceauth.Sign(priv, in)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return loopscale.ForwardHeaders{UserID: userID, TenantID: tenantID, Timestamp: strconv.FormatInt(ts, 10), Nonce: nonce, Signature: sig}
}

func newVerifier(t *testing.T, pub ed25519.PublicKey) *loopscale.ForwardAuthVerifier {
	t.Helper()
	return loopscale.NewForwardAuthVerifier([]ed25519.PublicKey{pub}, 30*time.Second, serviceauth.NewInMemoryNonceStore(time.Minute))
}

func TestForwardAuth_valid_assertion_passes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())

	got, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.TenantID != "tenant-a" || got.UserID != "usr-1" {
		t.Errorf("assertion = %+v, want tenant-a/usr-1", got)
	}
}

func TestForwardAuth_tampered_signature_fails_closed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())
	h.Signature = h.Signature[:len(h.Signature)-2] + "AA" // corrupt

	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestForwardAuth_swapped_tenant_fails_closed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())
	h.TenantID = "tenant-b" // signature was over tenant-a → verification must fail

	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("swapped tenant err = %v, want ErrUnauthorized", err)
	}
}

func TestForwardAuth_swapped_path_fails_closed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())
	// replaying the signature against a DIFFERENT path must fail (binds method+path).
	if _, err := v.Verify(context.Background(), "DELETE", "/v1/sandboxes/victim", h, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("swapped path err = %v, want ErrUnauthorized", err)
	}
}

func TestForwardAuth_stale_timestamp_fails_closed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	old := now.Add(-5 * time.Minute)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", old.Unix())

	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("stale err = %v, want ErrUnauthorized (outside replay window)", err)
	}
}

func TestForwardAuth_replayed_nonce_fails_closed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())

	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); err != nil {
		t.Fatalf("first use: %v", err)
	}
	// same nonce again → replay rejected.
	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("replay err = %v, want ErrUnauthorized", err)
	}
}

func TestForwardAuth_untrusted_key_fails_closed(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	_, attackerPriv, _ := ed25519.GenerateKey(nil) // NOT in the trust set
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, attackerPriv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())

	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("untrusted-key err = %v, want ErrUnauthorized", err)
	}
}

func TestForwardAuth_malformed_timestamp_fails_closed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())
	for _, bad := range []string{"", "abc", "1e9", "  "} {
		h.Timestamp = bad
		if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); !errors.Is(err, auth.ErrUnauthorized) {
			t.Errorf("timestamp %q → err %v, want ErrUnauthorized", bad, err)
		}
	}
}

func TestForwardAuth_same_nonce_different_users_both_pass(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	v := newVerifier(t, pub)
	now := time.Unix(1_000_000, 0)
	// Same nonce, DIFFERENT users → both accepted (nonce is keyed per user).
	h1 := signedForward(t, priv, "usr-1", "tenant-a", "shared-nonce", "POST", "/v1/sandboxes", now.Unix())
	h2 := signedForward(t, priv, "usr-2", "tenant-a", "shared-nonce", "POST", "/v1/sandboxes", now.Unix())
	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h1, now); err != nil {
		t.Fatalf("usr-1: %v", err)
	}
	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h2, now); err != nil {
		t.Errorf("usr-2 with same nonce should pass (per-user keying): %v", err)
	}
}

func TestForwardAuth_replay_window_boundaries(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	// tolerance = 30s
	now := time.Unix(1_000_000, 0)
	for _, tc := range []struct {
		name   string
		offset time.Duration
		ok     bool
	}{
		{"exactly_at_tolerance_past", -30 * time.Second, true},
		{"just_past_tolerance", -31 * time.Second, false},
		{"future_beyond_tolerance", 31 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVerifier(t, pub) // fresh nonce store per case
			ts := now.Add(tc.offset).Unix()
			h := signedForward(t, priv, "usr-1", "tenant-a", "n-"+tc.name, "POST", "/v1/sandboxes", ts)
			_, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now)
			if tc.ok && err != nil {
				t.Errorf("offset %v → err %v, want pass", tc.offset, err)
			}
			if !tc.ok && !errors.Is(err, auth.ErrUnauthorized) {
				t.Errorf("offset %v → err %v, want ErrUnauthorized", tc.offset, err)
			}
		})
	}
}

func TestForwardAuth_key_rotation_previous_key_trusted(t *testing.T) {
	curPub, _, _ := ed25519.GenerateKey(nil)
	prevPub, prevPriv, _ := ed25519.GenerateKey(nil)
	_, attackerPriv, _ := ed25519.GenerateKey(nil)
	// Trust set = [current, previous] (rotation window).
	v := loopscale.NewForwardAuthVerifier([]ed25519.PublicKey{curPub, prevPub}, 30*time.Second, serviceauth.NewInMemoryNonceStore(time.Minute))
	now := time.Unix(1_000_000, 0)

	// A forward signed with the PREVIOUS key still validates.
	h := signedForward(t, prevPriv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())
	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); err != nil {
		t.Errorf("previous key should still be trusted: %v", err)
	}
	// A third, untrusted key does not.
	hb := signedForward(t, attackerPriv, "usr-1", "tenant-a", "n2", "POST", "/v1/sandboxes", now.Unix())
	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", hb, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("untrusted key err = %v, want ErrUnauthorized", err)
	}
}

func TestForwardAuth_nil_nonce_store_fails_closed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	// A misconfigured verifier with no nonce store must fail closed, not skip replay.
	v := loopscale.NewForwardAuthVerifier([]ed25519.PublicKey{pub}, 30*time.Second, nil)
	now := time.Unix(1_000_000, 0)
	h := signedForward(t, priv, "usr-1", "tenant-a", "n1", "POST", "/v1/sandboxes", now.Unix())
	if _, err := v.Verify(context.Background(), "POST", "/v1/sandboxes", h, now); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("nil nonce store err = %v, want ErrUnauthorized (fail-closed)", err)
	}
}
