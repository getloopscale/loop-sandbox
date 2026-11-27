package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/auth"
	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/persistence/memory"
)

func setup(t *testing.T) (*auth.APIKeyAuthenticator, *memory.APIKeyStore, string) {
	t.Helper()
	tenants := memory.NewTenants()
	keys := memory.NewAPIKeys()
	now := time.Unix(0, 0).UTC()
	tn, _ := domain.NewTenant("tenant-a", "acme", domain.PlanPro, now)
	_ = tenants.SaveTenant(context.Background(), tn)
	raw, key, _ := domain.GenerateAPIKey("k1", "tenant-a", "ci", []string{"sandbox:execute"}, now)
	_ = keys.SaveAPIKey(context.Background(), key)
	return auth.NewAPIKeyAuthenticator(keys, tenants), keys, raw
}

func TestAuthenticate_valid_key_resolves_tenant(t *testing.T) {
	a, _, raw := setup(t)
	p, err := a.Authenticate(context.Background(), "Bearer "+raw)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.TenantID != "tenant-a" || p.KeyID != "k1" {
		t.Errorf("principal = %+v", p)
	}
	if !p.HasScope("sandbox:execute") {
		t.Error("principal missing scope")
	}
}

func TestAuthenticate_unknown_key_is_unauthorized(t *testing.T) {
	a, _, _ := setup(t)
	if _, err := a.Authenticate(context.Background(), "Bearer lsb_deadbeef"); err != auth.ErrUnauthorized {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthenticate_garbage_is_unauthorized(t *testing.T) {
	a, _, _ := setup(t)
	if _, err := a.Authenticate(context.Background(), "Bearer not-a-key"); err != auth.ErrUnauthorized {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthenticate_revoked_key_is_unauthorized(t *testing.T) {
	a, keys, raw := setup(t)
	_ = keys.Revoke(context.Background(), "tenant-a", "k1", time.Now())
	if _, err := a.Authenticate(context.Background(), "Bearer "+raw); err != auth.ErrUnauthorized {
		t.Errorf("revoked key err = %v, want ErrUnauthorized", err)
	}
}

func TestAuthenticate_suspended_tenant_is_unauthorized(t *testing.T) {
	tenants := memory.NewTenants()
	keys := memory.NewAPIKeys()
	now := time.Unix(0, 0).UTC()
	tn, _ := domain.NewTenant("tenant-a", "acme", domain.PlanPro, now)
	tn.Status = domain.TenantSuspended
	_ = tenants.SaveTenant(context.Background(), tn)
	raw, key, _ := domain.GenerateAPIKey("k1", "tenant-a", "", nil, now)
	_ = keys.SaveAPIKey(context.Background(), key)
	a := auth.NewAPIKeyAuthenticator(keys, tenants)

	if _, err := a.Authenticate(context.Background(), "Bearer "+raw); err != auth.ErrUnauthorized {
		t.Errorf("suspended tenant err = %v, want ErrUnauthorized", err)
	}
}
