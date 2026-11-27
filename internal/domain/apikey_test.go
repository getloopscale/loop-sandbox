package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

func TestGenerateAPIKey_hash_differs_from_plaintext_and_verifies(t *testing.T) {
	raw, key, err := domain.GenerateAPIKey("k1", "tenant-a", "ci", []string{"sandbox:execute"}, t0)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if !strings.HasPrefix(raw, "lsb_") {
		t.Errorf("raw key %q missing lsb_ prefix", raw)
	}
	// The stored hash must NOT be the plaintext (TRD §5.2).
	if key.KeyHash == raw || strings.Contains(key.KeyHash, raw) {
		t.Fatal("plaintext leaked into stored hash")
	}
	// Re-hashing the presented raw key reproduces the stored hash (auth lookup).
	if domain.HashAPIKey(raw) != key.KeyHash {
		t.Error("HashAPIKey(raw) != stored KeyHash — auth lookup would fail")
	}
	// Prefix is a display fragment, never the full secret.
	if len(key.Prefix) >= len(raw) {
		t.Error("prefix should be a short fragment, not the whole key")
	}
}

func TestGenerateAPIKey_is_unique(t *testing.T) {
	r1, _, _ := domain.GenerateAPIKey("k1", "t", "", nil, t0)
	r2, _, _ := domain.GenerateAPIKey("k2", "t", "", nil, t0)
	if r1 == r2 {
		t.Fatal("two generated keys collided — insufficient entropy")
	}
}

func TestAPIKey_revoked_and_scopes(t *testing.T) {
	_, key, _ := domain.GenerateAPIKey("k1", "t", "", []string{"sandbox:execute"}, t0)
	if key.Revoked() {
		t.Error("new key should not be revoked")
	}
	if !key.HasScope("sandbox:execute") {
		t.Error("key should have sandbox:execute scope")
	}
	if key.HasScope("admin") {
		t.Error("key must not have unassigned scope")
	}
	rev := t0.Add(time.Hour)
	key.RevokedAt = &rev
	if !key.Revoked() {
		t.Error("key should be revoked after RevokedAt set")
	}
}

func TestLooksLikeAPIKey(t *testing.T) {
	_, key, _ := domain.GenerateAPIKey("k1", "t", "", nil, t0)
	_ = key
	if domain.LooksLikeAPIKey("bearer-garbage") {
		t.Error("non-lsb key should not look like an API key")
	}
	if !domain.LooksLikeAPIKey("lsb_abcdefgh") {
		t.Error("lsb-prefixed key should look like an API key")
	}
}
