// Package tenantprov generates the per-tenant Kubernetes provisioning manifests
// (M10): a dedicated Namespace, a least-privilege RBAC Role/binding, a ResourceQuota,
// and — the security-critical one — a NetworkPolicy that denies egress by default and
// blocks the dangerous ranges (loopback, private, link-local, and the cloud metadata
// IMDS endpoint). The NetworkPolicy denylist is derived from the SAME egress.Policy
// that gates app-level dials, so the two never drift.
package tenantprov

import (
	"fmt"
	"strings"

	"github.com/getloopscale/loop-sandbox/internal/egress"
)

// Options parameterises tenant provisioning.
type Options struct {
	TenantID     string
	CPULimit     string // ResourceQuota cpu (e.g. "8")
	MemoryLimit  string // ResourceQuota memory (e.g. "16Gi")
	MaxSandboxes string // ResourceQuota pods (e.g. "100")
}

func (o Options) withDefaults() Options {
	if o.CPULimit == "" {
		o.CPULimit = "8"
	}
	if o.MemoryLimit == "" {
		o.MemoryLimit = "16Gi"
	}
	if o.MaxSandboxes == "" {
		o.MaxSandboxes = "100"
	}
	return o
}

// Namespace returns the tenant's namespace name, or an error if the tenant id cannot
// form a valid RFC-1123 DNS label (k8s namespace names are ≤63 chars, must start and
// end alphanumeric). Fail-fast at generation, not late at `kubectl apply`.
func Namespace(tenantID string) (string, error) {
	name := "lsb-tenant-" + sanitize(tenantID)
	name = strings.Trim(name, "-")
	if name == "" {
		return "", fmt.Errorf("tenantprov: tenant id %q sanitises to an empty name", tenantID)
	}
	if len(name) > 63 {
		return "", fmt.Errorf("tenantprov: tenant id %q produces a name > 63 chars (%q)", tenantID, name)
	}
	return name, nil
}

// Manifests renders the tenant provisioning YAML. policy supplies the egress denylist
// for the NetworkPolicy (single source of truth). Returns an error on invalid input.
func Manifests(policy *egress.Policy, opts Options) (string, error) {
	if opts.TenantID == "" {
		return "", fmt.Errorf("tenantprov: tenant id is required")
	}
	if policy == nil {
		return "", fmt.Errorf("tenantprov: egress policy is required")
	}
	o := opts.withDefaults()
	ns, err := Namespace(o.TenantID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, namespaceTmpl, ns, o.TenantID)
	fmt.Fprintf(&b, quotaTmpl, ns, o.MaxSandboxes, o.CPULimit, o.MemoryLimit)
	fmt.Fprintf(&b, rbacTmpl, ns, ns)
	b.WriteString(networkPolicy(ns, policy))
	return b.String(), nil
}

// networkPolicy renders a DENY-ALL-egress NetworkPolicy. The ENFORCED control is
// `egress: []` — it blocks ALL egress (including IMDS/private), which is the correct
// default for a sandbox running untrusted code. The blocked-CIDR list is emitted as a
// NON-ENFORCING annotation: it is NOT a k8s rule, it documents the ranges that MUST
// remain blocked (as an ipBlock/except, IPv4 and IPv6 split into separate rules) IF
// egress is ever selectively opened for a tenant. The app-level egress.Policy
// (ControlFunc) enforces the same denylist for any control-plane-initiated outbound.
func networkPolicy(ns string, policy *egress.Policy) string {
	var denylist strings.Builder
	for _, cidr := range policy.BlockedCIDRs() {
		fmt.Fprintf(&denylist, "      - %s\n", cidr)
	}
	return fmt.Sprintf(networkPolicyTmpl, ns, denylist.String())
}

// sanitize keeps a tenant id safe as a k8s name segment (lowercase alnum + '-'),
// collapsing any run of separators to a single '-' (so path traversal / metacharacters
// cannot produce long dash runs or break the YAML).
func sanitize(s string) string {
	var out strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				out.WriteRune('-')
				prevDash = true
			}
		}
	}
	return out.String()
}

const namespaceTmpl = `apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    loopsandbox.dev/tenant: "%s"
---
`

const quotaTmpl = `apiVersion: v1
kind: ResourceQuota
metadata:
  name: sandbox-quota
  namespace: %s
spec:
  hard:
    pods: "%s"
    requests.cpu: "%s"
    requests.memory: "%s"
---
`

const rbacTmpl = `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: sandbox-worker
  namespace: %s
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log"]
    verbs: ["get", "list", "create", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: sandbox-worker
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: sandbox-worker
subjects:
  - kind: ServiceAccount
    name: sandbox-worker
---
`

// networkPolicyTmpl ENFORCES a total egress denial via `egress: []` with the Egress
// policyType — this is what actually blocks IMDS/private ranges (by blocking ALL
// egress). The blocked-cidrs annotation is NON-ENFORCING documentation of the denylist
// to preserve if egress is ever selectively opened.
const networkPolicyTmpl = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: sandbox-deny-egress
  namespace: %s
  annotations:
    # NON-ENFORCING: documents the denylist to keep blocked IF egress is ever opened.
    # The ENFORCED control is the empty egress rule below (total deny).
    loopsandbox.dev/blocked-cidrs: |
%s
spec:
  podSelector: {}
  policyTypes:
    - Egress
  egress: []
`
