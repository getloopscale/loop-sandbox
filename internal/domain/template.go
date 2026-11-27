package domain

import (
	"context"
	"time"
)

// Scan status values for a registered template (M17 publish gate).
const (
	ScanUnscanned = "unscanned"
	ScanPassed    = "passed"
	ScanFailed    = "failed"
)

// Template is a registered, reusable sandbox base a sandbox can be created from (M17
// registry). It carries a version and a content digest (digest-pinning) and a scan status
// (a template may only be PUBLISHED public once it passes the vulnerability scan gate).
type Template struct {
	ID         string
	TenantID   string // owner; "" for a built-in / public template
	Name       string
	ImageRef   string // OCI ref or built rootfs identifier
	Version    string // caller-supplied version label (e.g. "1", "2026.07"); defaults to "1"
	Digest     string // sha256:… content digest (digest-pin); empty until built/registered
	Public     bool   // visible to all tenants (built-ins + explicitly published)
	ScanStatus string // ScanUnscanned | ScanPassed | ScanFailed
	CreatedAt  time.Time
}

// NewTemplate constructs a template. Fail-fast on missing id/name. Version defaults to "1";
// scan status starts unscanned.
func NewTemplate(id, tenantID, name, imageRef string, now time.Time) (*Template, error) {
	if id == "" || name == "" {
		return nil, ErrInvalidArgument
	}
	return &Template{
		ID: id, TenantID: tenantID, Name: name, ImageRef: imageRef,
		Version: "1", ScanStatus: ScanUnscanned, CreatedAt: now,
	}, nil
}

// TemplateStore persists templates (tenant-scoped; public templates are visible to all).
type TemplateStore interface {
	SaveTemplate(ctx context.Context, t *Template) error
	GetTemplate(ctx context.Context, tenantID, id string) (*Template, error)
	// ListTemplates returns the tenant's own templates plus all public ones.
	ListTemplates(ctx context.Context, tenantID string) ([]*Template, error)
}
