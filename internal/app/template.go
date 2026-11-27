package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/getloopscale/loop-sandbox/internal/domain"
	"github.com/getloopscale/loop-sandbox/internal/enforcement"
	"github.com/getloopscale/loop-sandbox/internal/tracing"
)

// ErrTemplatesUnsupported is returned when the template registry is not wired (M17).
var ErrTemplatesUnsupported = errors.New("template registry not available in this deployment")

// ErrTemplateNotPinned is returned when a template's image ref is not digest-pinned (M17). The
// registry enforces digest-pinning so a template always resolves to an immutable artifact.
var ErrTemplateNotPinned = errors.New("template image must be digest-pinned (…@sha256:…)")

// ErrTemplateScanFailed is returned when a template fails the vulnerability scan gate and
// cannot be published public (M17).
var ErrTemplateScanFailed = errors.New("template failed the vulnerability scan gate")

// TemplateScanner scans an image/rootfs ref for vulnerabilities — the publish gate (M17). The
// real implementation wraps trivy (composition root); tests inject a fake. Reports whether the
// artifact is clean enough to publish.
type TemplateScanner interface {
	Scan(ctx context.Context, imageRef string) (passed bool, err error)
}

// RegisterTemplate records a template in the registry (M17). The image ref MUST be
// digest-pinned (immutability). Publishing PUBLIC requires passing the vulnerability scan gate
// (a private template may register unscanned). Cross-tenant is naturally isolated: a template
// is stored under the actor's tenant.
func (s *Service) RegisterTemplate(ctx context.Context, actor Actor, name, imageRef, version string, public bool) (*domain.Template, error) {
	ctx, end := tracing.Start(ctx, "template.register", actor.TenantID, name)
	defer end()
	if s.tmpls == nil {
		return nil, ErrTemplatesUnsupported
	}
	if !domain.ValidVolumeName(name) { // same safe-identifier charset as volumes
		return nil, domain.ErrInvalidArgument
	}
	digest, ok := pinnedDigest(imageRef)
	if !ok {
		return nil, ErrTemplateNotPinned
	}
	now := s.clock.Now()
	// Plan-quota gate: bound how many templates a tenant may register (M17 review — prevents
	// unbounded registry growth). Metered on success below.
	if err := s.enforce(ctx, actor.TenantID, enforcement.MetricTemplateCreations, 1, now); err != nil {
		return nil, err
	}
	t, err := domain.NewTemplate("tpl_"+s.ids.New(), actor.TenantID, name, imageRef, now)
	if err != nil {
		return nil, err
	}
	t.Digest = digest
	if version != "" {
		t.Version = version
	}
	// Scan gate — a PUBLIC template must pass the vulnerability scan before publish.
	if public {
		if s.scan == nil {
			return nil, ErrTemplateScanFailed // cannot publish without a scanner to vouch for it
		}
		passed, serr := s.scan.Scan(ctx, imageRef)
		if serr != nil {
			return nil, serr
		}
		if !passed {
			// Not persisted — a failed-scan template simply does not exist in the registry.
			return nil, ErrTemplateScanFailed
		}
		t.ScanStatus = domain.ScanPassed
		t.Public = true
	}
	if err := s.tmpls.SaveTemplate(ctx, t); err != nil {
		return nil, err
	}
	s.meter(ctx, actor.TenantID, domain.MetricTemplateCreations, now)
	s.audit.Record(actor.TenantID, actor.KeyID, "template.register", "template", t.ID, actor.RequestID, now)
	return t, nil
}

// ListTemplates returns the tenant's own templates plus all public ones (M17).
func (s *Service) ListTemplates(ctx context.Context, actor Actor) ([]*domain.Template, error) {
	if s.tmpls == nil {
		return nil, ErrTemplatesUnsupported
	}
	return s.tmpls.ListTemplates(ctx, actor.TenantID)
}

// GetTemplate resolves a template visible to the actor (owned or public); not-found otherwise.
func (s *Service) GetTemplate(ctx context.Context, actor Actor, id string) (*domain.Template, error) {
	if s.tmpls == nil {
		return nil, ErrTemplatesUnsupported
	}
	return s.tmpls.GetTemplate(ctx, actor.TenantID, id)
}

// CreateSandboxFromTemplate provisions a sandbox from a registered template (M17). The
// template's pinned image ref becomes the sandbox image. Cross-tenant/unknown template is
// not-found.
func (s *Service) CreateSandboxFromTemplate(ctx context.Context, actor Actor, templateID string, ttl time.Duration) (*domain.Sandbox, error) {
	return s.createFromTemplate(ctx, actor, templateID, ttl, sandboxAttributes{})
}

func (s *Service) createFromTemplate(ctx context.Context, actor Actor, templateID string, ttl time.Duration, attrs sandboxAttributes) (*domain.Sandbox, error) {
	if s.tmpls == nil {
		return nil, ErrTemplatesUnsupported
	}
	t, err := s.tmpls.GetTemplate(ctx, actor.TenantID, templateID)
	if err != nil {
		return nil, err // ErrNotFound for unknown / not-visible
	}
	// A template built by the microVM builder has a LOCAL ROOTFS image ref (…/x.ext4@sha256:…):
	// provision by booting that rootfs. An OCI-image template (name@sha256:…) provisions by
	// image ref (the container driver's path). This closes the loop so a built template
	// actually boots its own customized rootfs (M17), not the default one.
	if path, ok := builtRootfsPath(t.ImageRef); ok {
		return s.createSandbox(ctx, actor, "", path, "", ttl, attrs)
	}
	return s.createSandbox(ctx, actor, t.ImageRef, "", "", ttl, attrs)
}

// builtRootfsPath returns the local rootfs path of a built-template ref (…/x.ext4[@sha256:…]),
// or false when the ref is an OCI image reference.
func builtRootfsPath(ref string) (string, bool) {
	bare := ref
	if i := strings.LastIndex(bare, "@sha256:"); i >= 0 {
		bare = bare[:i]
	}
	if strings.HasPrefix(bare, "/") && strings.HasSuffix(bare, ".ext4") {
		return bare, true
	}
	return "", false
}

// pinnedDigest extracts the sha256 digest from a digest-pinned ref (…@sha256:<64-hex>). It
// requires EXACTLY one "@sha256:" and 64 lowercase-hex chars with nothing trailing, so a
// crafted ref cannot smuggle garbage past the pin check.
func pinnedDigest(ref string) (string, bool) {
	i := strings.Index(ref, "@sha256:")
	if i < 0 || i != strings.LastIndex(ref, "@sha256:") {
		return "", false
	}
	hex := ref[i+len("@sha256:"):]
	if len(hex) != 64 {
		return "", false
	}
	for _, c := range hex {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return "", false
		}
	}
	return "sha256:" + hex, true
}
