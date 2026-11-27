package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Host is the machine the numbers were measured on.
type Host struct {
	CPUModel    string `json:"cpu_model"`
	LogicalCPUs int    `json:"logical_cpus"`
	MemoryMiB   int64  `json:"memory_mib"`
	Kernel      string `json:"kernel"`
	Arch        string `json:"arch"`
}

func readHost() (*Host, error) {
	total, err := meminfoKiB("MemTotal")
	if err != nil {
		return nil, err
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return nil, fmt.Errorf("read the kernel release: %w", err)
	}
	model, err := cpuModel()
	if err != nil {
		return nil, err
	}
	return &Host{
		CPUModel:    model,
		LogicalCPUs: runtime.NumCPU(),
		MemoryMiB:   total / 1024,
		Kernel:      strings.TrimSpace(string(kernel)),
		Arch:        runtime.GOARCH,
	}, nil
}

func cpuModel() (string, error) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", fmt.Errorf("read the CPU model: %w", err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), ":"); ok && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(value), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read the CPU model: %w", err)
	}
	return "unknown", nil // not every architecture has a "model name" line
}

// availableMemoryKiB is what the kernel estimates a new workload can use.
func availableMemoryKiB() (int64, error) { return meminfoKiB("MemAvailable") }

func meminfoKiB(field string) (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", field, err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok || key != field {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %s in /proc/meminfo: %w", field, err)
		}
		return n, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", field, err)
	}
	return 0, fmt.Errorf("/proc/meminfo has no %s", field)
}
