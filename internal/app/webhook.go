package app

import (
	"context"
	"time"
)

// WebhookEvent is a sandbox lifecycle event delivered to a configured webhook (M20 agent-native
// DX). Agent orchestrators subscribe to these to react to sandbox state changes.
type WebhookEvent struct {
	Type      string    `json:"type"` // sandbox.created | sandbox.exec | sandbox.destroyed
	TenantID  string    `json:"tenant_id"`
	SandboxID string    `json:"sandbox_id"`
	Timestamp time.Time `json:"timestamp"`
}

// WebhookSink delivers lifecycle events (M20). The real impl POSTs JSON to a configured URL
// (best-effort, async, egress-guarded); tests inject a recording fake. Emit MUST NOT block the
// caller's hot path — implementations dispatch asynchronously.
type WebhookSink interface {
	Emit(ctx context.Context, e WebhookEvent)
}

// SetWebhookSink installs the lifecycle-webhook sink (composition root only, M20).
func (s *Service) SetWebhookSink(w WebhookSink) { s.hooks = w }

// emitWebhook delivers a lifecycle event when a sink is wired (nil-safe, best-effort).
func (s *Service) emitWebhook(ctx context.Context, typ, tenantID, sandboxID string) {
	if s.hooks == nil {
		return
	}
	s.hooks.Emit(ctx, WebhookEvent{Type: typ, TenantID: tenantID, SandboxID: sandboxID, Timestamp: s.clock.Now()})
}
