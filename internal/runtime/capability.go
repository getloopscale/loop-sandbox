package runtime

// Capability names one thing a sandbox runtime can do beyond the lifecycle. The set
// is the vocabulary of GET /v1/capabilities and of the conformance suite: a client
// asks which capabilities the active driver supports instead of discovering a 501.
type Capability string

// The capabilities a driver can support. Exec and files are part of the Driver
// interface, so every driver has them.
const (
	CapExec          Capability = "exec"
	CapFiles         Capability = "files"
	CapFileOps       Capability = "file_ops"
	CapFileWatch     Capability = "file_watch"
	CapExecStream    Capability = "exec_stream"
	CapProcesses     Capability = "processes"
	CapLogs          Capability = "logs"
	CapMetrics       Capability = "metrics"
	CapSessions      Capability = "sessions"
	CapSessionStream Capability = "session_stream"
	CapPTY           Capability = "pty"
	CapFork          Capability = "fork"
	CapPause         Capability = "pause"
	CapSnapshots     Capability = "snapshots"
	CapVolumes       Capability = "volumes"
	CapBrowser       Capability = "browser"
	CapTemplateBuild Capability = "template_build"
)

// AllCapabilities returns every known capability in a stable order.
func AllCapabilities() []Capability {
	return []Capability{
		CapExec, CapFiles, CapFileOps, CapFileWatch, CapExecStream, CapProcesses, CapLogs, CapMetrics, CapSessions, CapSessionStream, CapPTY,
		CapFork, CapPause, CapSnapshots, CapVolumes, CapBrowser, CapTemplateBuild,
	}
}

// CapabilityGate is an optional Driver interface for a driver that implements a
// capability's Go interface but refuses it at run time. The fake driver is the
// case: it has the session methods, and they return an error unless a test opted in.
// Without the gate, Supports would report a capability the driver then rejects.
type CapabilityGate interface {
	CapabilityEnabled(c Capability) bool
}

// Supports reports whether d can serve capability c. It is derived from the
// optional interfaces the driver implements, so a driver cannot declare a
// capability it has no code for.
func Supports(d Driver, c Capability) bool {
	if !implements(d, c) {
		return false
	}
	if gate, ok := d.(CapabilityGate); ok {
		return gate.CapabilityEnabled(c)
	}
	return true
}

func implements(d Driver, c Capability) bool {
	var ok bool
	switch c {
	case CapExec, CapFiles:
		ok = d != nil
	case CapFileOps:
		_, ok = d.(FileOpsDriver)
	case CapFileWatch:
		_, ok = d.(WatchDriver)
	case CapExecStream:
		_, ok = d.(StreamingExecDriver)
	case CapProcesses:
		_, ok = d.(ProcessDriver)
	case CapLogs:
		_, ok = d.(LogsDriver)
	case CapMetrics:
		_, ok = d.(MetricsDriver)
	case CapSessions:
		_, ok = d.(SessionDriver)
	case CapSessionStream:
		_, ok = d.(StreamingSessionDriver)
	case CapPTY:
		_, ok = d.(PTYDriver)
	case CapFork:
		_, ok = d.(ForkDriver)
	case CapPause:
		_, ok = d.(PauseDriver)
	case CapSnapshots:
		_, ok = d.(SnapshotDriver)
	case CapVolumes:
		_, ok = d.(VolumeDriver)
	case CapBrowser:
		_, ok = d.(BrowserDriver)
	case CapTemplateBuild:
		_, ok = d.(TemplateBuilder)
	}
	return ok
}

// Supported returns the capabilities d supports, in AllCapabilities order.
func Supported(d Driver) []Capability {
	out := make([]Capability, 0, len(AllCapabilities()))
	for _, c := range AllCapabilities() {
		if Supports(d, c) {
			out = append(out, c)
		}
	}
	return out
}

// ProcessLocal is an optional Driver interface for a driver that keeps its
// sandboxes in the memory of the process that created them. Only that process can
// reach or destroy them, so the TTL loops must run inside it and it cannot be
// replicated.
type ProcessLocal interface {
	ProcessLocal() bool
}

// IsProcessLocal reports whether d keeps its sandboxes in process memory.
func IsProcessLocal(d Driver) bool {
	pl, ok := d.(ProcessLocal)
	return ok && pl.ProcessLocal()
}
