package agent

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/getloopscale/loop-sandbox/internal/agentproto"
)

// procDir is where the kernel's process filesystem is mounted. A variable so the
// parsing is testable with fixture files.
var procDir = "/proc"

// nanosPerTick converts the CPU times of /proc/stat, in units of 1/100 s on
// every Linux architecture Go supports, to nanoseconds.
const nanosPerTick = 10_000_000

// stats reports the resource use of the machine the agent runs in. In a microVM
// that machine is the sandbox.
func (h *Handler) stats() *agentproto.Response {
	cpu, err := cpuNanos(procDir + "/stat")
	if err != nil {
		return &agentproto.Response{Err: "stats: " + err.Error()}
	}
	used, total, err := memoryBytes(procDir + "/meminfo")
	if err != nil {
		return &agentproto.Response{Err: "stats: " + err.Error()}
	}
	return &agentproto.Response{Stats: &agentproto.Stats{
		CPUNanos: cpu, MemoryUsed: used, MemoryTotal: total, Processes: h.procs.running(),
	}}
}

// cpuNanos is the CPU time spent on anything but idling, from the first line of
// /proc/stat: "cpu user nice system idle iowait irq softirq steal ...".
func cpuNanos(statPath string) (uint64, error) {
	raw, err := os.ReadFile(statPath) //nolint:gosec // a fixed kernel file
	if err != nil {
		return 0, err
	}
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, fmt.Errorf("unexpected first line of %s: %q", statPath, line)
	}
	var busy uint64
	for i, f := range fields[1:] {
		if i == 3 || i == 4 { // idle and iowait
			continue
		}
		if i > 7 { // guest times are already counted in user and nice
			break
		}
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: field %d: %w", statPath, i+1, err)
		}
		busy += v
	}
	return busy * nanosPerTick, nil
}

// memoryBytes is the memory in use (total minus what the kernel says is
// available) and the total, from /proc/meminfo.
func memoryBytes(meminfoPath string) (used, total uint64, err error) {
	f, err := os.Open(meminfoPath) //nolint:gosec // a fixed kernel file
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()
	var available uint64
	var haveTotal, haveAvailable bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text()) // "MemTotal:  1014736 kB"
		if len(fields) < 2 {
			continue
		}
		kb, perr := strconv.ParseUint(fields[1], 10, 64)
		if perr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = kb<<10, true
		case "MemAvailable:":
			available, haveAvailable = kb<<10, true
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if !haveTotal || !haveAvailable || available > total {
		return 0, 0, fmt.Errorf("%s: MemTotal or MemAvailable missing or inconsistent", meminfoPath)
	}
	return total - available, total, nil
}
