package tenantprov_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getloopscale/loop-sandbox/internal/egress"
	"github.com/getloopscale/loop-sandbox/internal/tenantprov"
)

func TestManifests_denies_egress_and_documents_imds_block(t *testing.T) {
	p, _ := egress.New()
	out, err := tenantprov.Manifests(p, tenantprov.Options{TenantID: "tnt_abc"})
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	// Namespace, quota, RBAC, and the deny-egress NetworkPolicy are all present.
	for _, want := range []string{
		"kind: Namespace", "name: lsb-tenant-tnt-abc", // '_' sanitised to '-' (k8s names)
		"kind: ResourceQuota", "kind: Role", "kind: NetworkPolicy",
		"policyTypes:\n    - Egress", "egress: []", // deny-all egress
	} {
		if !strings.Contains(out, want) {
			t.Errorf("manifests missing %q", want)
		}
	}
	// The M10 acceptance criterion: IMDS is in the blocked-CIDR set.
	if !strings.Contains(out, "169.254.169.254/32") {
		t.Errorf("NetworkPolicy does not block IMDS 169.254.169.254/32")
	}
	// And the private/loopback ranges too.
	for _, cidr := range []string{"10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16"} {
		if !strings.Contains(out, cidr) {
			t.Errorf("blocked-CIDR annotation missing %s", cidr)
		}
	}
}

func TestManifests_extra_blocked_cidr_flows_through(t *testing.T) {
	p, _ := egress.New("100.64.0.0/16") // control-plane subnet
	out, _ := tenantprov.Manifests(p, tenantprov.Options{TenantID: "t"})
	if !strings.Contains(out, "100.64.0.0/16") {
		t.Errorf("extra control-plane CIDR not propagated to the NetworkPolicy")
	}
}

func TestManifests_requires_tenant_and_policy(t *testing.T) {
	if _, err := tenantprov.Manifests(nil, tenantprov.Options{TenantID: "t"}); err == nil {
		t.Error("nil policy accepted, want error")
	}
	p, _ := egress.New()
	if _, err := tenantprov.Manifests(p, tenantprov.Options{}); err == nil {
		t.Error("empty tenant accepted, want error")
	}
}

func TestNamespace_sanitises_and_validates(t *testing.T) {
	cases := []struct {
		id      string
		want    string
		wantErr bool
	}{
		{"tnt_abc", "lsb-tenant-tnt-abc", false},
		{"UPPER", "lsb-tenant-upper", false},           // lowercased
		{"tnt/../../etc", "lsb-tenant-tnt-etc", false}, // traversal collapsed to single dash
		{"a--b", "lsb-tenant-a-b", false},              // dash run collapsed
		{"trailing_", "lsb-tenant-trailing", false},    // trailing separator trimmed
		{strings.Repeat("x", 80), "", true},            // > 63 chars → error
	}
	for _, c := range cases {
		got, err := tenantprov.Namespace(c.id)
		if c.wantErr {
			if err == nil {
				t.Errorf("Namespace(%q) = %q, want error", c.id, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Namespace(%q): %v", c.id, err)
		}
		if got != c.want {
			t.Errorf("Namespace(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// The generated manifests must be VALID YAML (every document parses) — not just
// contain substrings. Proves a nasty tenant id cannot break the manifest structure.
func TestManifests_are_valid_yaml(t *testing.T) {
	p, _ := egress.New()
	out, err := tenantprov.Manifests(p, tenantprov.Options{TenantID: "tnt/../weird\tid"})
	if err != nil {
		t.Fatalf("Manifests: %v", err)
	}
	for i, doc := range strings.Split(out, "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Errorf("doc %d is not valid YAML: %v\n%s", i, err, doc)
		}
		if m["kind"] == nil {
			t.Errorf("doc %d has no kind: %s", i, doc)
		}
	}
}
