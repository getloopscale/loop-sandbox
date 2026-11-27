package fake

import (
	"context"
	"fmt"

	"github.com/getloopscale/loop-sandbox/internal/egress"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
)

// network is the fake's record of sandbox networks. It enforces nothing: it
// keeps the policy each sandbox was given so a test can read it back.
type network struct {
	enabled  bool
	policies map[runtime.SandboxID]runtime.NetworkPolicy
	denials  map[runtime.SandboxID][]runtime.NetworkDenial
}

// EnableNetwork turns the "network" capability on. Call it before the driver is used.
func (d *Driver) EnableNetwork() *Driver {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.net.enabled = true
	return d
}

// NetworkPolicy returns the policy the sandbox has now, for assertions in tests.
func (d *Driver) NetworkPolicy(id runtime.SandboxID) runtime.NetworkPolicy {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.net.policies[id]
}

// RecordDenial adds a refused connection to the sandbox's list, for tests.
func (d *Driver) RecordDenial(id runtime.SandboxID, denial runtime.NetworkDenial) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.net.denials == nil {
		d.net.denials = map[runtime.SandboxID][]runtime.NetworkDenial{}
	}
	d.net.denials[id] = append(d.net.denials[id], denial)
}

// setPolicy is called with d.mu held.
func (d *Driver) setPolicy(id runtime.SandboxID, policy runtime.NetworkPolicy) {
	if d.net.policies == nil {
		d.net.policies = map[runtime.SandboxID]runtime.NetworkPolicy{}
	}
	d.net.policies[id] = policy
}

// UpdateNetwork implements runtime.NetworkDriver.
func (d *Driver) UpdateNetwork(_ context.Context, id runtime.SandboxID, policy runtime.NetworkPolicy) error {
	if _, err := egress.ParseRules(policy.AllowOut, policy.DenyOut); err != nil {
		return fmt.Errorf("%w: %w", runtime.ErrInvalidRequest, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[id]; !ok {
		return runtime.ErrSandboxNotFound
	}
	d.setPolicy(id, policy)
	return nil
}

// NetworkDenials implements runtime.NetworkDriver.
func (d *Driver) NetworkDenials(_ context.Context, id runtime.SandboxID, limit int) ([]runtime.NetworkDenial, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sandboxes[id]; !ok {
		return nil, runtime.ErrSandboxNotFound
	}
	all := d.net.denials[id]
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return append([]runtime.NetworkDenial(nil), all...), nil
}

var _ runtime.NetworkDriver = (*Driver)(nil)
