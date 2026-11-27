package domain

import "time"

// AuditEvent records a mutating operation (TRD §5.6). Every create/destroy/policy
// change emits one, carrying who/when/what without leaking secrets (TRD §10.7).
type AuditEvent struct {
	ID           string
	TenantID     string
	ActorType    string // "api_key"
	ActorID      string // API key id (never the secret)
	Action       string // "sandbox.create", "sandbox.destroy", ...
	ResourceType string // "sandbox"
	ResourceID   string
	RequestID    string
	Timestamp    time.Time
}

// NewAuditEvent builds an audit event; fail-fast on missing tenant/action.
func NewAuditEvent(id, tenantID, actorID, action, resourceType, resourceID, requestID string, now time.Time) (*AuditEvent, error) {
	if tenantID == "" || action == "" {
		return nil, ErrInvalidArgument
	}
	return &AuditEvent{
		ID:           id,
		TenantID:     tenantID,
		ActorType:    "api_key",
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		RequestID:    requestID,
		Timestamp:    now,
	}, nil
}
