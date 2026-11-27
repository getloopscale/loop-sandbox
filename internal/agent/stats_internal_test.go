package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCPUNanosCountsEverythingButIdleIOWaitAndGuest(t *testing.T) {
	// user=100 nice=5 system=50 idle=9000 iowait=40 irq=3 softirq=2 steal=1 guest=70 guest_nice=7
	stat := fixture(t, "cpu  100 5 50 9000 40 3 2 1 70 7\ncpu0 100 5 50 9000 40 3 2 1 70 7\n")

	got, err := cpuNanos(stat)

	if want := uint64(100+5+50+3+2+1) * 10_000_000; err != nil || got != want {
		t.Errorf("cpuNanos = %d, %v; want %d", got, err, want)
	}
}

func TestCPUNanosRejectsAFileThatIsNotProcStat(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "other": "intr 1 2 3\n", "not a number": "cpu a b c d e\n"} {
		t.Run(name, func(t *testing.T) {
			if _, err := cpuNanos(fixture(t, content)); err == nil {
				t.Error("no error for a malformed file")
			}
		})
	}
}

func TestMemoryBytesIsTotalMinusAvailable(t *testing.T) {
	meminfo := fixture(t, "MemTotal:        1000 kB\nMemFree:          100 kB\nMemAvailable:     600 kB\nBuffers: 5 kB\n")

	used, total, err := memoryBytes(meminfo)

	if err != nil || total != 1000<<10 || used != 400<<10 {
		t.Errorf("used = %d total = %d err = %v; want 400 KiB of 1000 KiB", used, total, err)
	}
}

func TestMemoryBytesRejectsMissingOrInconsistentFields(t *testing.T) {
	cases := map[string]string{
		"no available":         "MemTotal: 1000 kB\n",
		"no total":             "MemAvailable: 600 kB\n",
		"available over total": "MemTotal: 100 kB\nMemAvailable: 600 kB\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := memoryBytes(fixture(t, content)); err == nil {
				t.Error("no error for a malformed file")
			}
		})
	}
}
