package runtime_test

import (
	"testing"

	"github.com/getloopscale/loop-sandbox/internal/runtime"
	"github.com/getloopscale/loop-sandbox/internal/runtime/fake"
)

func TestSupportsReportsCoreCapabilitiesForEveryDriver(t *testing.T) {
	d := fake.New()

	for _, c := range []runtime.Capability{runtime.CapExec, runtime.CapFiles} {
		if !runtime.Supports(d, c) {
			t.Errorf("Supports(%q) = false, want true: it is part of the Driver interface", c)
		}
	}
}

func TestSupportsHidesHostExecutingCapabilitiesOfDefaultFakeDriver(t *testing.T) {
	d := fake.New()

	for _, c := range []runtime.Capability{runtime.CapSessions, runtime.CapSessionStream, runtime.CapExecStream, runtime.CapPTY, runtime.CapBrowser} {
		if runtime.Supports(d, c) {
			t.Errorf("Supports(%q) = true on a fake driver that did not opt in; the route would answer an error", c)
		}
	}
}

func TestSupportsReportsSessionsWhenFakeDriverOptedIn(t *testing.T) {
	d := fake.NewWithSessions()

	if !runtime.Supports(d, runtime.CapSessions) {
		t.Error("Supports(sessions) = false on a fake driver created with sessions")
	}
}

func TestSupportedListsOnlyKnownCapabilitiesInStableOrder(t *testing.T) {
	got := runtime.Supported(fake.NewWithSessions())

	pos := map[runtime.Capability]int{}
	for i, c := range runtime.AllCapabilities() {
		pos[c] = i
	}
	last := -1
	for _, c := range got {
		i, known := pos[c]
		if !known {
			t.Fatalf("Supported returned unknown capability %q", c)
		}
		if i <= last {
			t.Fatalf("Supported is not in AllCapabilities order: %v", got)
		}
		last = i
	}
}
