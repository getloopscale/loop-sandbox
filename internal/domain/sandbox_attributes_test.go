package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
)

func runningSandbox(t *testing.T, now time.Time) *domain.Sandbox {
	t.Helper()
	sb, err := domain.NewSandbox("sbx_1", "tenant-a", "busybox", time.Minute, now)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	if err := sb.TransitionTo(domain.StateRunning, now); err != nil {
		t.Fatalf("TransitionTo running: %v", err)
	}
	return sb
}

func TestValidateMetadataAcceptsLabelLikeKeys(t *testing.T) {
	err := domain.ValidateMetadata(map[string]string{"team": "search", "app.kubernetes.io/name": "agent-7", "run_id": ""})

	if err != nil {
		t.Errorf("ValidateMetadata = %v, want nil", err)
	}
}

func TestValidateMetadataRejectsInvalidInput(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i <= domain.MaxMetadataEntries; i++ {
		tooMany[strings.Repeat("k", i+1)] = "v"
	}
	cases := map[string]map[string]string{
		"empty key":         {"": "v"},
		"key with a space":  {"my key": "v"},
		"key starting dash": {"-key": "v"},
		"key too long":      {strings.Repeat("k", domain.MaxMetadataKeyLen+1): "v"},
		"value too long":    {"k": strings.Repeat("v", domain.MaxMetadataValueLen+1)},
		"value not UTF-8":   {"k": "\xff\xfe"},
		"too many entries":  tooMany,
	}
	for name, metadata := range cases {
		t.Run(name, func(t *testing.T) {
			err := domain.ValidateMetadata(metadata)

			var verr *domain.ValidationError
			if !errors.As(err, &verr) || verr.Field != "metadata" || !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("ValidateMetadata = %v, want a ValidationError on field metadata", err)
			}
		})
	}
}

func TestValidateEnvRejectsInvalidInput(t *testing.T) {
	cases := map[string]map[string]string{
		"name starting with a digit": {"1VAR": "v"},
		"name with a dash":           {"MY-VAR": "v"},
		"name with an equals sign":   {"A=B": "v"},
		"empty name":                 {"": "v"},
		"value with a NUL byte":      {"VAR": "a\x00b"},
		"value too long":             {"VAR": strings.Repeat("v", domain.MaxEnvValueLen+1)},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			err := domain.ValidateEnv(env)

			var verr *domain.ValidationError
			if !errors.As(err, &verr) || verr.Field != "env" {
				t.Errorf("ValidateEnv = %v, want a ValidationError on field env", err)
			}
		})
	}
}

func TestSetAttributesCopiesTheMaps(t *testing.T) {
	sb := runningSandbox(t, time.Unix(1000, 0))
	metadata := map[string]string{"team": "search"}
	env := map[string]string{"MODE": "test"}

	if err := sb.SetAttributes(metadata, env); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	metadata["team"] = "changed"
	env["MODE"] = "changed"

	if sb.Metadata["team"] != "search" || sb.Env["MODE"] != "test" {
		t.Errorf("sandbox saw the caller's later change: metadata=%v env=%v", sb.Metadata, sb.Env)
	}
}

func TestSetTTLReplacesTheExpiry(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	sb := runningSandbox(t, t0)
	later := t0.Add(30 * time.Second)

	if err := sb.SetTTL(10*time.Minute, later); err != nil {
		t.Fatalf("SetTTL: %v", err)
	}

	if want := later.Add(10 * time.Minute); !sb.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", sb.ExpiresAt, want)
	}
}

func TestSetTTLCanShortenTheLifetime(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	sb := runningSandbox(t, t0)

	if err := sb.SetTTL(time.Second, t0); err != nil {
		t.Fatalf("SetTTL: %v", err)
	}

	if !sb.Expired(t0.Add(2 * time.Second)) {
		t.Error("sandbox with a 1s TTL is not expired after 2s")
	}
}

func TestSetTTLRejectsANonPositiveDuration(t *testing.T) {
	sb := runningSandbox(t, time.Unix(1000, 0))

	err := sb.SetTTL(0, time.Unix(1000, 0))

	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("SetTTL(0) = %v, want ErrInvalidArgument", err)
	}
}

func TestSetTTLOnADestroyedSandboxIsAnInvalidTransition(t *testing.T) {
	t0 := time.Unix(1000, 0)
	sb := runningSandbox(t, t0)
	if err := sb.TransitionTo(domain.StateDestroyed, t0); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	err := sb.SetTTL(time.Minute, t0)

	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("SetTTL on a destroyed sandbox = %v, want ErrInvalidTransition", err)
	}
}

func TestMatchesMetadataRequiresEveryWantedLabel(t *testing.T) {
	sb := runningSandbox(t, time.Unix(1000, 0))
	if err := sb.SetAttributes(map[string]string{"team": "search", "env": "prod"}, nil); err != nil {
		t.Fatalf("SetAttributes: %v", err)
	}
	cases := []struct {
		name string
		want map[string]string
		ok   bool
	}{
		{"no filter", nil, true},
		{"one matching label", map[string]string{"team": "search"}, true},
		{"all labels", map[string]string{"team": "search", "env": "prod"}, true},
		{"different value", map[string]string{"team": "ads"}, false},
		{"missing key", map[string]string{"owner": "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sb.MatchesMetadata(c.want); got != c.ok {
				t.Errorf("MatchesMetadata(%v) = %v, want %v", c.want, got, c.ok)
			}
		})
	}
}

func TestComesAfterOrdersNewestFirstWithIDAsTieBreak(t *testing.T) {
	t0 := time.Unix(1000, 0)
	cursor := domain.SandboxCursor{CreatedAt: t0, ID: "sbx_m"}
	cases := []struct {
		name      string
		createdAt time.Time
		id        string
		after     bool
	}{
		{"older sandbox", t0.Add(-time.Second), "sbx_z", true},
		{"newer sandbox", t0.Add(time.Second), "sbx_a", false},
		{"same instant, smaller id", t0, "sbx_a", true},
		{"same instant, larger id", t0, "sbx_z", false},
		{"the cursor itself", t0, "sbx_m", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sb := &domain.Sandbox{ID: c.id, CreatedAt: c.createdAt}

			if got := sb.ComesAfter(cursor); got != c.after {
				t.Errorf("ComesAfter = %v, want %v", got, c.after)
			}
		})
	}
}

func TestValidPlanAcceptsOnlyKnownPlans(t *testing.T) {
	for plan, want := range map[domain.Plan]bool{
		domain.PlanStarter: true, domain.PlanPro: true, domain.PlanTeam: true,
		"": false, "enterprise": false, "Starter": false,
	} {
		if got := domain.ValidPlan(plan); got != want {
			t.Errorf("ValidPlan(%q) = %v, want %v", plan, got, want)
		}
	}
}

func TestValidateAPIKeyRequestAcceptsScopesTheCallerHolds(t *testing.T) {
	both := []string{domain.ScopeSandboxExecute, domain.ScopeAPIKeysManage}

	err := domain.ValidateAPIKeyRequest("ci", both, both)

	if err != nil {
		t.Errorf("ValidateAPIKeyRequest = %v, want nil", err)
	}
}

func TestValidateAPIKeyRequestRefusesAScopeTheCallerLacks(t *testing.T) {
	err := domain.ValidateAPIKeyRequest("ci", []string{domain.ScopeAPIKeysManage}, []string{domain.ScopeSandboxExecute})

	if !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestValidateAPIKeyRequestRejectsInvalidInput(t *testing.T) {
	both := []string{domain.ScopeSandboxExecute, domain.ScopeAPIKeysManage}
	cases := map[string]struct {
		name   string
		scopes []string
		field  string
	}{
		"empty name":     {"", both, "name"},
		"name too long":  {strings.Repeat("n", domain.MaxAPIKeyNameLen+1), both, "name"},
		"name not UTF-8": {"\xff", both, "name"},
		"no scopes":      {"ci", nil, "scopes"},
		"unknown scope":  {"ci", []string{"root"}, "scopes"},
		"repeated scope": {"ci", []string{domain.ScopeSandboxExecute, domain.ScopeSandboxExecute}, "scopes"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := domain.ValidateAPIKeyRequest(c.name, c.scopes, both)

			var verr *domain.ValidationError
			if !errors.As(err, &verr) || verr.Field != c.field {
				t.Errorf("err = %v, want a ValidationError on field %s", err, c.field)
			}
		})
	}
}
