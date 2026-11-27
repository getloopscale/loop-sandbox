package app

import (
	"context"
	"errors"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/egress"
	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// Errors of the network use cases. The API maps the first to 501 and the second
// to 409.
var (
	ErrNetworkUnsupported  = errors.New("sandbox networks are not supported by this runtime")
	ErrSandboxHasNoNetwork = errors.New("the sandbox has no network")
)

// Limits of one read of the refused connections.
const (
	DefaultNetworkDenials = 100
	MaxNetworkDenials     = 1000
)

// validateNetwork refuses rules the runtime could not enforce, and any rules at
// all on a runtime without sandbox networks: accepting them there would promise
// a restriction nothing applies.
func (s *Service) validateNetwork(policy runtime.NetworkPolicy) error {
	if !s.supports(runtime.CapNetwork) {
		return ErrNetworkUnsupported
	}
	return validateRules(policy)
}

func validateRules(policy runtime.NetworkPolicy) error {
	if _, err := egress.ParseRules(policy.AllowOut, policy.DenyOut); err != nil {
		return &domain.ValidationError{Field: "network", Reason: err.Error()}
	}
	return nil
}

func (s *Service) mapNetworkErr(err error) error {
	if errors.Is(err, runtime.ErrNoNetwork) {
		return ErrSandboxHasNoNetwork
	}
	return s.mapRuntimeErr(err)
}

// UpdateSandboxNetwork replaces the outbound rules of the actor's running
// sandbox. They apply to the connections it makes from then on.
func (s *Service) UpdateSandboxNetwork(ctx context.Context, actor Actor, id string, policy runtime.NetworkPolicy) (*domain.Sandbox, error) {
	ctx, end := tracing.Start(ctx, "sandbox.update_network", actor.TenantID, id)
	defer end()
	nd, ok := s.driver.(runtime.NetworkDriver)
	if !ok || !s.supports(runtime.CapNetwork) {
		return nil, ErrNetworkUnsupported
	}
	if err := validateRules(policy); err != nil {
		return nil, err
	}
	// Serialized with destroy and pause on the same sandbox, and with another
	// update, so the stored rules are the ones the runtime enforces.
	unlock := s.locks.lock("sb:" + id)
	defer unlock()
	sb, err := s.ownedSandbox(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if sb.State != domain.StateRunning {
		return nil, domain.ErrInvalidTransition
	}
	if err := nd.UpdateNetwork(ctx, runtime.SandboxID(id), policy); err != nil {
		return nil, s.mapNetworkErr(err)
	}
	if store, ok := s.repo.(domain.NetworkStore); ok {
		if err := store.SetSandboxNetwork(ctx, actor.TenantID, id, policy.AllowOut, policy.DenyOut); err != nil {
			return nil, err
		}
	}
	sb.NetworkAllowOut = append([]string(nil), policy.AllowOut...)
	sb.NetworkDenyOut = append([]string(nil), policy.DenyOut...)
	s.audit.Record(actor.TenantID, actor.KeyID, "sandbox.update_network", "sandbox", id, actor.RequestID, s.clock.Now())
	return sb, nil
}

// SandboxNetworkDenials returns the most recent connections the policy of the
// actor's sandbox refused, oldest first. A limit of zero means
// DefaultNetworkDenials.
func (s *Service) SandboxNetworkDenials(ctx context.Context, actor Actor, id string, limit int) ([]runtime.NetworkDenial, error) {
	ctx, end := tracing.Start(ctx, "sandbox.network_denials", actor.TenantID, id)
	defer end()
	nd, ok := s.driver.(runtime.NetworkDriver)
	if !ok || !s.supports(runtime.CapNetwork) {
		return nil, ErrNetworkUnsupported
	}
	if limit == 0 {
		limit = DefaultNetworkDenials
	}
	if limit < 0 || limit > MaxNetworkDenials {
		return nil, &domain.ValidationError{Field: "limit", Reason: "must be between 1 and 1000"}
	}
	if _, err := s.ownedSandbox(ctx, actor, id); err != nil {
		return nil, err
	}
	denials, err := nd.NetworkDenials(ctx, runtime.SandboxID(id), limit)
	if err != nil {
		return nil, s.mapNetworkErr(err)
	}
	return denials, nil
}
