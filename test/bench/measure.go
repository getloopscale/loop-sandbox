package main

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"time"
)

// Report is everything one run measured and where it measured it.
type Report struct {
	MeasuredAt   string            `json:"measured_at"`
	Driver       string            `json:"driver"`
	Image        string            `json:"image"`
	Host         *Host             `json:"host,omitempty"`
	Versions     map[string]string `json:"versions"`
	Measurements []Measurement     `json:"measurements"`
	Skipped      []Skipped         `json:"skipped,omitempty"`
	Density      *Density          `json:"density,omitempty"`
}

// Measurement is one timed operation.
type Measurement struct {
	Name    string  `json:"name"`
	What    string  `json:"what"`
	Summary Summary `json:"summary"`
}

// Skipped is a measurement the driver cannot serve.
type Skipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Density is how many sandboxes were alive at once and what they cost.
type Density struct {
	Target int `json:"target"`
	// Reached is how many sandboxes were alive at once, each having run a command.
	Reached int `json:"reached"`
	// StoppedBy is the error that ended the ramp before the target, when one did.
	StoppedBy string `json:"stopped_by,omitempty"`
	// MemoryPerSandboxMiB is the drop of the host's available memory divided by
	// Reached. It is absent when the API is not on this host.
	MemoryPerSandboxMiB *float64 `json:"memory_per_sandbox_mib,omitempty"`
}

func measure(ctx context.Context, c *client, opts options) (*Report, error) {
	driver, caps, err := c.capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the capabilities: %w", err)
	}
	report := &Report{
		MeasuredAt: time.Now().UTC().Format(time.RFC3339),
		Driver:     driver,
		Image:      opts.image,
		Versions:   map[string]string{"bench_go": runtime.Version()},
	}
	for k, v := range opts.meta {
		report.Versions[k] = v
	}
	if opts.localHost {
		host, err := readHost()
		if err != nil {
			return nil, err
		}
		report.Host = host
	}

	create, firstExec, err := measureCreate(ctx, c, opts, nil)
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	report.Measurements = append(report.Measurements,
		Measurement{Name: "create", What: "POST /v1/sandboxes until the 201 answer, one sandbox at a time", Summary: summarize(create)},
		Measurement{Name: "create_first_exec", What: "the first command (true) in a sandbox that was just created", Summary: summarize(firstExec)},
	)

	if caps["snapshots"] {
		ms, err := measureSnapshots(ctx, c, opts)
		if err != nil {
			return nil, fmt.Errorf("snapshots: %w", err)
		}
		report.Measurements = append(report.Measurements, ms...)
	} else {
		report.Skipped = append(report.Skipped, Skipped{Name: "snapshot_restore", Reason: "the driver does not support snapshots"})
	}

	if caps["fork"] {
		m, err := measureFork(ctx, c, opts)
		if err != nil {
			return nil, fmt.Errorf("fork: %w", err)
		}
		report.Measurements = append(report.Measurements, m)
	} else {
		report.Skipped = append(report.Skipped, Skipped{Name: "fork", Reason: "the driver does not support fork"})
	}

	if opts.density > 0 {
		d, err := measureDensity(ctx, c, opts)
		if err != nil {
			return nil, fmt.Errorf("density: %w", err)
		}
		report.Density = d
	}
	return report, nil
}

// cleanupContext outlives a cancelled run, so that an interrupted benchmark
// still destroys what it created.
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Minute)
}

func destroyAll(c *client, ids []string) error {
	ctx, cancel := cleanupContext()
	defer cancel()
	var first error
	for _, id := range ids {
		if err := c.destroy(ctx, id); err != nil && first == nil {
			first = fmt.Errorf("destroy sandbox %s: %w", id, err)
		}
	}
	return first
}

// measureCreate creates opts.samples sandboxes one after the other, each with
// the extra fields, and times the creation and the first command of each.
func measureCreate(ctx context.Context, c *client, opts options, extra map[string]any) (create, firstExec []time.Duration, err error) {
	for range opts.samples {
		sb, took, err := c.create(ctx, opts.image, extra)
		if err != nil {
			return nil, nil, err
		}
		execTook, execErr := c.execTrue(ctx, sb.ID)
		if destroyErr := destroyAll(c, []string{sb.ID}); execErr == nil {
			execErr = destroyErr
		}
		if execErr != nil {
			return nil, nil, execErr
		}
		create, firstExec = append(create, took), append(firstExec, execTook)
	}
	return create, firstExec, nil
}

func measureSnapshots(ctx context.Context, c *client, opts options) (out []Measurement, err error) {
	source, _, err := c.create(ctx, opts.image, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		if destroyErr := destroyAll(c, []string{source.ID}); err == nil {
			err = destroyErr
		}
	}()
	if _, err := c.execTrue(ctx, source.ID); err != nil {
		return nil, err
	}

	var take []time.Duration
	var snapshotIDs []string
	defer func() {
		cleanup, cancel := cleanupContext()
		defer cancel()
		for _, id := range snapshotIDs {
			if _, delErr := c.do(cleanup, http.MethodDelete, "/v1/snapshots/"+id, nil, nil, http.StatusNoContent); delErr != nil && err == nil {
				err = fmt.Errorf("delete snapshot %s: %w", id, delErr)
			}
		}
	}()
	for i := range opts.samples {
		var snap struct {
			ID string `json:"id"`
		}
		took, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+source.ID+"/snapshots",
			map[string]any{"name": fmt.Sprintf("bench-%d-%d", time.Now().UnixNano(), i)}, &snap, http.StatusCreated)
		if err != nil {
			return nil, err
		}
		take, snapshotIDs = append(take, took), append(snapshotIDs, snap.ID)
	}

	restore, firstExec, err := measureCreate(ctx, c, opts, map[string]any{"snapshot": snapshotIDs[0]})
	if err != nil {
		return nil, err
	}
	return []Measurement{
		{Name: "snapshot_create", What: "POST /v1/sandboxes/{id}/snapshots of a running sandbox until the 201 answer", Summary: summarize(take)},
		{Name: "snapshot_restore", What: "POST /v1/sandboxes with a snapshot until the 201 answer", Summary: summarize(restore)},
		{Name: "snapshot_restore_first_exec", What: "the first command (true) in a sandbox restored from a snapshot", Summary: summarize(firstExec)},
	}, nil
}

func measureFork(ctx context.Context, c *client, opts options) (m Measurement, err error) {
	source, _, err := c.create(ctx, opts.image, nil)
	if err != nil {
		return Measurement{}, err
	}
	ids := []string{source.ID}
	defer func() {
		if destroyErr := destroyAll(c, ids); err == nil {
			err = destroyErr
		}
	}()
	if _, err := c.execTrue(ctx, source.ID); err != nil {
		return Measurement{}, err
	}
	var samples []time.Duration
	for range opts.samples {
		var fork sandbox
		took, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+source.ID+"/fork", nil, &fork, http.StatusCreated)
		if err != nil {
			return Measurement{}, err
		}
		ids = append(ids, fork.ID)
		if _, err := c.execTrue(ctx, fork.ID); err != nil {
			return Measurement{}, err
		}
		samples = append(samples, took)
	}
	return Measurement{Name: "fork", What: "POST /v1/sandboxes/{id}/fork of a running sandbox until the 201 answer; earlier forks stay alive", Summary: summarize(samples)}, nil
}

// measureDensity creates sandboxes without destroying them until the target or
// the first failure. A sandbox counts once it has run a command.
func measureDensity(ctx context.Context, c *client, opts options) (d *Density, err error) {
	d = &Density{Target: opts.density}
	var before int64
	if opts.localHost {
		if before, err = availableMemoryKiB(); err != nil {
			return nil, err
		}
	}
	var ids []string
	defer func() {
		if destroyErr := destroyAll(c, ids); err == nil {
			err = destroyErr
		}
	}()
	for d.Reached < opts.density {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sb, _, createErr := c.create(ctx, opts.image, nil)
		if createErr != nil {
			d.StoppedBy = createErr.Error()
			break
		}
		ids = append(ids, sb.ID)
		if _, execErr := c.execTrue(ctx, sb.ID); execErr != nil {
			d.StoppedBy = execErr.Error()
			break
		}
		d.Reached++
	}
	if opts.localHost && d.Reached > 0 {
		after, err := availableMemoryKiB()
		if err != nil {
			return nil, err
		}
		perSandbox := float64(before-after) / 1024 / float64(d.Reached)
		perSandbox = float64(int(perSandbox*10+0.5)) / 10
		d.MemoryPerSandboxMiB = &perSandbox
	}
	return d, nil
}
