package app

import "github.com/getloopscale/loop-sandbox/internal/runtime"

// CapabilityStatus says whether the active runtime driver supports one capability.
type CapabilityStatus struct {
	Name      runtime.Capability
	Supported bool
}

// Capabilities returns the driver name and, for every known capability, whether the
// active driver supports it. Clients use it to tell "not supported by this
// deployment" apart from a failure before they call the route.
func (s *Service) Capabilities() (driver string, caps []CapabilityStatus) {
	all := runtime.AllCapabilities()
	caps = make([]CapabilityStatus, 0, len(all))
	for _, c := range all {
		caps = append(caps, CapabilityStatus{Name: c, Supported: s.supports(c)})
	}
	return s.driver.Name(), caps
}

// Supports reports whether this deployment can serve capability c. Handlers that
// commit to a response before calling the use case (a streamed body) ask first, so
// an unsupported capability still answers 501.
func (s *Service) Supports(c runtime.Capability) bool { return s.supports(c) }

// supports is the single answer to "can this deployment serve capability c": the
// driver must support it and the stores it needs must be wired. Every use case that
// depends on an optional capability asks here, so what GET /v1/capabilities reports
// and what the routes do cannot drift apart.
func (s *Service) supports(c runtime.Capability) bool {
	if c == runtime.CapVolumes && s.vols == nil {
		return false
	}
	return runtime.Supports(s.driver, c)
}
